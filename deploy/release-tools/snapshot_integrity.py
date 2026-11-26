"""CFM 冻结快照校验：合并内容读取，不省略元数据、归档或真实恢复验证。

调用方必须持有业务写入围栏，并保持快照私有。preserve_layout 只修复已有副本的
元数据及空目录，不复制缺失文件。它返回可原子持久化的 manifest；write_archive
可在解除流量保持后独立执行，但必须成功后才能登记完整备份。

所有公共操作接受无参 checkpoint 回调及累计计数用的 stats 字典。计数同时包含
总量和 phases 分项；content_bytes_read / content_files_read / content_read_calls
描述内容校验读取，不把 stat 遍历误算成内容读取。失败保留不完整产物供调查。
"""
from contextlib import contextmanager
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
import gzip
import hashlib
import os
from pathlib import Path, PurePosixPath
import shutil
import stat
import subprocess
import tarfile


CHUNK = 1024 * 1024
SOURCE_DIRECTORIES = ('data', 'logs')
SOURCE_FILES = ('.env', 'compose.yaml', 'current.json')
CFM_RUNTIME_FILES = frozenset(('.cfm-writer.lock', '.cfm-legacy-instance.json',
                               '.cfm-gateway.json', 'cfm-maintenance'))
_NOFOLLOW = os.O_NOFOLLOW
_DIRECTORY = os.O_DIRECTORY


class IntegrityError(RuntimeError):
    """快照内容、路径身份、元数据或归档格式不满足准入要求。"""


@dataclass
class Capture:
    """内容清单与同次采集的路径身份；身份只用于当前冻结窗口，不能跨恢复比较。"""
    manifest: dict
    identities: dict
    selected: bool = False


def _require(condition, message):
    if not condition:
        raise IntegrityError(message)


def _tick(checkpoint):
    if checkpoint is not None:
        checkpoint()


def _count(stats, phase, key, amount=1):
    if stats is not None:
        stats[key] = stats.get(key, 0) + amount
        counters = stats.setdefault('phases', {}).setdefault(phase, {})
        counters[key] = counters.get(key, 0) + amount


def identity(info):
    """排除会被正常读取改变的 atime，保留无法用 utime 伪造回去的 ctime。"""
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def _row(info):
    _require(stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode),
             'Backup refuses symlinks or special files')
    result = {'type': 'directory' if stat.S_ISDIR(info.st_mode) else 'file',
              'uid': info.st_uid, 'gid': info.st_gid,
              'mode': stat.S_IMODE(info.st_mode), 'mtime_ns': info.st_mtime_ns}
    if result['type'] == 'file':
        result['size'] = info.st_size
    return result


def _without_hash(row):
    return {key: value for key, value in row.items() if key != 'sha256'}


def _parts(name):
    _require(isinstance(name, str) and name != '', 'Invalid manifest path')
    if name == '.':
        return ()
    value = PurePosixPath(name)
    _require(not value.is_absolute() and '..' not in value.parts and
             value.as_posix() == name and all(part not in ('', '.') for part in value.parts),
             'Invalid manifest path')
    return value.parts


def _validate_manifest(expected):
    _require(isinstance(expected, dict) and '.' in expected, 'Manifest root is missing')
    for name, row in expected.items():
        parts = _parts(name)
        _require(isinstance(row, dict) and row.get('type') in ('file', 'directory'),
                 'Invalid manifest entry')
        keys = {'type', 'uid', 'gid', 'mode', 'mtime_ns'}
        if row['type'] == 'file':
            keys |= {'size', 'sha256'}
            _require(isinstance(row.get('size'), int) and row['size'] >= 0 and
                     isinstance(row.get('sha256'), str) and len(row['sha256']) == 64 and
                     all(char in '0123456789abcdef' for char in row['sha256']),
                     'Invalid manifest file digest or size')
        _require(set(row) == keys, 'Unexpected manifest metadata')
        _require(all(type(row[key]) is int for key in ('uid', 'gid', 'mode', 'mtime_ns'))
                 and row['uid'] >= 0 and row['gid'] >= 0 and 0 <= row['mode'] <= 0o7777,
                 'Invalid manifest metadata')
        if parts:
            parent = PurePosixPath(*parts[:-1]).as_posix()
            _require(parent in expected and expected[parent].get('type') == 'directory',
                     'Manifest parent directory is missing')
    _require(expected['.']['type'] == 'directory' and expected['.']['mode'] == 0o700,
             'Private snapshot root permission must be 0700')


def _open_directory_path(path):
    """逐级 openat 固定父目录，拒绝路径任意一层软链接（包括最终根目录）。"""
    absolute = os.path.abspath(os.fspath(path))
    descriptor = os.open('/', os.O_RDONLY | _DIRECTORY | _NOFOLLOW)
    try:
        for part in Path(absolute).parts[1:]:
            child = os.open(part, os.O_RDONLY | _DIRECTORY | _NOFOLLOW, dir_fd=descriptor)
            os.close(descriptor)
            descriptor = child
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


class _Tree:
    def __init__(self, path):
        self.path = Path(os.path.abspath(os.fspath(path)))
        self.fd = None

    def __enter__(self):
        self.fd = _open_directory_path(self.path)
        self.root_identity = os.fstat(self.fd)
        return self

    def check_root(self):
        check = _open_directory_path(self.path)
        try:
            current = os.fstat(check)
            _require((current.st_dev, current.st_ino) ==
                     (self.root_identity.st_dev, self.root_identity.st_ino),
                     'Backup root or its parent was replaced')
        finally:
            os.close(check)

    def __exit__(self, *unused):
        os.close(self.fd)

    @contextmanager
    def open(self, name):
        parts = _parts(name)
        descriptor = os.dup(self.fd)
        try:
            for index, part in enumerate(parts):
                flags = os.O_RDONLY | _NOFOLLOW | os.O_NONBLOCK
                if index < len(parts) - 1:
                    flags |= _DIRECTORY
                child = os.open(part, flags, dir_fd=descriptor)
                os.close(descriptor)
                descriptor = child
            _row(os.fstat(descriptor))
            yield descriptor
        finally:
            os.close(descriptor)

    @contextmanager
    def parent(self, name):
        parts = _parts(name)
        _require(parts, 'Cannot mutate the snapshot root through its parent')
        with self.open(PurePosixPath(*parts[:-1]).as_posix()) as descriptor:
            _require(stat.S_ISDIR(os.fstat(descriptor).st_mode), 'Parent is not a directory')
            yield descriptor, parts[-1]


def _walk(tree, selected, checkpoint, stats, phase, *, prune_directory=None):
    """每层目录仅 scandir 一次；在已固定的父 fd 下打开并复核每个子项。"""
    def visit(name, descriptor):
        _tick(checkpoint)
        before = os.fstat(descriptor)
        _row(before)
        _count(stats, phase, 'scanned_entries')
        yield name, descriptor, before
        if stat.S_ISDIR(before.st_mode):
            with os.scandir(descriptor) as iterator:
                children = sorted(iterator, key=lambda item: item.name)
            for child in children:
                info = child.stat(follow_symlinks=False)
                _row(info)
                child_name = child.name if name == '.' else name + '/' + child.name
                flags = os.O_RDONLY | _NOFOLLOW | os.O_NONBLOCK
                if stat.S_ISDIR(info.st_mode):
                    if prune_directory is not None and prune_directory(child_name):
                        continue
                    flags |= _DIRECTORY
                child_fd = os.open(child.name, flags, dir_fd=descriptor)
                try:
                    _require(identity(os.fstat(child_fd)) == identity(info),
                             'Backup path was replaced before inspection')
                    yield from visit(child_name, child_fd)
                    _require(identity(os.stat(child.name, dir_fd=descriptor,
                                              follow_symlinks=False)) == identity(info),
                             'Backup path was replaced during inspection')
                finally:
                    os.close(child_fd)
        _require(identity(os.fstat(descriptor)) == identity(before),
                 'Backup input changed while inspected')

    if not selected:
        yield from visit('.', tree.fd)
    else:
        # 源根目录含其他发布目录；仅采集业务范围，仍固定其 inode。
        for name in SOURCE_DIRECTORIES + SOURCE_FILES:
            try:
                info = os.stat(name, dir_fd=tree.fd, follow_symlinks=False)
            except FileNotFoundError:
                _require(name in SOURCE_FILES, 'Business directory missing')
                continue
            row = _row(info)
            _require(row['type'] == ('directory' if name in SOURCE_DIRECTORIES else 'file'),
                     'Unexpected business path type')
            flags = os.O_RDONLY | _NOFOLLOW | os.O_NONBLOCK
            if row['type'] == 'directory':
                flags |= _DIRECTORY
            descriptor = os.open(name, flags, dir_fd=tree.fd)
            try:
                _require(identity(os.fstat(descriptor)) == identity(info),
                         'Business path was replaced before inspection')
                yield from visit(name, descriptor)
                _require(identity(os.stat(name, dir_fd=tree.fd, follow_symlinks=False)) == identity(info),
                         'Business path was replaced during inspection')
            finally:
                os.close(descriptor)
    tree.check_root()


def _hash_fd(descriptor, checkpoint, stats, phase):
    digest = hashlib.sha256()
    count = 0
    _count(stats, phase, 'content_files_read')
    while True:
        _tick(checkpoint)
        block = os.read(descriptor, CHUNK)
        _count(stats, phase, 'content_read_calls')
        if not block:
            break
        digest.update(block)
        count += len(block)
        _count(stats, phase, 'content_bytes_read', len(block))
    return count, digest.hexdigest()


def _capture(tree, selected, checkpoint, stats, phase, *, content=True, durable=False):
    manifest, identities = {}, {}
    for name, descriptor, before in _walk(tree, selected, checkpoint, stats, phase):
        row = _row(before)
        if content and row['type'] == 'file':
            count, digest = _hash_fd(descriptor, checkpoint, stats, phase)
            _require(count == before.st_size, 'Backup input size changed while inspected')
            row['sha256'] = digest
        if durable:
            os.fsync(descriptor)
            _count(stats, phase, 'fsync_calls')
        manifest[name] = row
        identities[name] = identity(before)
    return Capture(manifest, identities, selected)


def capture_layout(root, *, selected=False, checkpoint=None, stats=None):
    """采集一次内容清单及身份；selected=True 仅选业务目录和部署配置。"""
    with _Tree(root) as tree:
        result = _capture(tree, selected, checkpoint, stats, 'capture')
        stable = _capture(tree, selected, checkpoint, stats, 'capture_stability', content=False)
        _require(stable.identities == result.identities, 'Backup tree changed during capture')
        return result


def verify_identity(root, capture, *, checkpoint=None, stats=None):
    """在同一冻结窗口内复核完整路径集合和身份；不读取内容，不适用于跨重启信任。"""
    _require(isinstance(capture, Capture), 'verify_identity requires a current Capture')
    with _Tree(root) as tree:
        current = _capture(tree, capture.selected, checkpoint, stats,
                           'identity_verify', content=False)
        _require(current.identities == capture.identities, 'Backup tree identity changed')
    return {'entries': len(capture.manifest), 'identity_verified': True}


def fsync_layout(root, capture, *, checkpoint=None, stats=None):
    """使已校验副本落盘并复核身份，不再读取文件内容；不接受跨重启身份缓存。"""
    _require(isinstance(capture, Capture), 'fsync_layout requires a current Capture')
    with _Tree(root) as tree:
        result = _capture(tree, capture.selected, checkpoint, stats,
                          'fsync', content=False, durable=True)
        _require(result.identities == capture.identities, 'Snapshot changed before durable seal')
        stable = _capture(tree, capture.selected, checkpoint, stats,
                          'fsync_stability', content=False)
        _require(stable.identities == capture.identities, 'Snapshot changed during durable seal')
    return {'entries': len(capture.manifest), 'durable': True, 'identity_verified': True}


def _apply_metadata(descriptor, wanted, stats, phase):
    actual = os.fstat(descriptor)
    if (actual.st_uid, actual.st_gid) != (wanted['uid'], wanted['gid']):
        os.fchown(descriptor, wanted['uid'], wanted['gid'])
        _count(stats, phase, 'metadata_updates')
        actual = os.fstat(descriptor)  # chown 可能清除 setuid/setgid。
    if stat.S_IMODE(actual.st_mode) != wanted['mode']:
        os.fchmod(descriptor, wanted['mode'])
        _count(stats, phase, 'metadata_updates')
    if actual.st_mtime_ns != wanted['mtime_ns']:
        os.utime(descriptor, ns=(wanted['mtime_ns'], wanted['mtime_ns']))
        _count(stats, phase, 'metadata_updates')


def preserve_layout(source, snapshot, *, checkpoint=None, stats=None, capture_sink=None,
                    source_content_verified=False):
    """冻结后校验已有副本并返回已落盘 manifest。

    The normal path hashes both source and target. When
    ``source_content_verified`` is true, the caller has just completed the
    frozen copy with per-file identity checks, so only the target content is
    hashed here and the source is checked by identity.

    可选 capture_sink(snapshot_capture, source_capture) 保存本次身份，供同一冻结
    窗口的 seal 在数据库导出后只做身份复核。跨进程恢复必须重新读取并验证内容。
    """
    with _Tree(source) as source_tree, _Tree(snapshot) as target_tree:
        _require(source_tree.path != target_tree.path and
                 target_tree.path not in source_tree.path.parents and
                 not any(target_tree.path == source_tree.path / name or
                         source_tree.path / name in target_tree.path.parents
                         for name in SOURCE_DIRECTORIES), 'Source and snapshot paths overlap')
        expected = _capture(source_tree, True, checkpoint, stats, 'preserve_source',
                            content=not source_content_verified)
        existing = _capture(target_tree, False, checkpoint, stats, 'preserve_target_stat', content=False)
        _require(existing.manifest['.']['mode'] == 0o700, 'Private snapshot root permission changed')
        # 同 inode 的副本会把元数据修复写回源，必须在任何改动前拒绝。
        source_inodes = {value[:2] for value in expected.identities.values()}
        _require(not any(value[:2] in source_inodes for value in existing.identities.values()),
                 'Snapshot shares an inode with the frozen source')
        extra = set(existing.manifest) - set(expected.manifest) - {'.'}
        for name in sorted(extra, key=lambda item: len(_parts(item)), reverse=True):
            _tick(checkpoint)
            _require(existing.manifest[name]['type'] == 'directory', 'Unexpected snapshot file remains')
            with target_tree.parent(name) as (parent, basename):
                # 删除子空目录会改变其父 mtime/ctime/size；仍要求 inode、类型、权限和
                # 所有者保持，rmdir 自身保证不会删除任何非空目录。
                _require(identity(os.stat(basename, dir_fd=parent, follow_symlinks=False))[:5] ==
                         existing.identities[name][:5], 'Obsolete snapshot directory changed')
                os.rmdir(basename, dir_fd=parent)
        for name, wanted in sorted(expected.manifest.items(), key=lambda item: (len(_parts(item[0])), item[0])):
            _tick(checkpoint)
            if name not in existing.manifest:
                _require(wanted['type'] == 'directory', 'Snapshot file is missing')
                with target_tree.parent(name) as (parent, basename):
                    os.mkdir(basename, 0o700, dir_fd=parent)
            else:
                _require(existing.manifest[name]['type'] == wanted['type'], 'Snapshot path type changed')
        # 在目标内容 hash 前修复元数据，避免本工具更改 ctime 掩盖外部写入。
        for name, wanted in sorted(expected.manifest.items(), key=lambda item: len(_parts(item[0])), reverse=True):
            _tick(checkpoint)
            with target_tree.open(name) as descriptor:
                _require(_row(os.fstat(descriptor))['type'] == wanted['type'], 'Snapshot path type changed')
                _require(identity(os.fstat(descriptor))[:2] not in source_inodes,
                         'Snapshot was replaced by a source hard link')
                _apply_metadata(descriptor, wanted, stats, 'preserve_metadata')
        final = _capture(target_tree, False, checkpoint, stats, 'preserve_target')
        if source_content_verified:
            source_metadata = {key: _without_hash(row) for key, row in expected.manifest.items()}
            target_metadata = {key: _without_hash(row) for key, row in final.manifest.items()
                               if key != '.'}
            _require(target_metadata == source_metadata,
                     'Snapshot metadata differs from frozen source')
            # The legacy frozen copy has already read each source file and
            # rejected any identity change around that read. Bind those source
            # identities to the target content digests for later state guards.
            source_capture = Capture(final.manifest, expected.identities, selected=True)
        else:
            _require({key: row for key, row in final.manifest.items() if key != '.'} == expected.manifest,
                     'Snapshot content or metadata differs from frozen source')
            source_capture = expected
        # 文件和目录均落盘；两个复核只有 stat，没有额外内容读取。
        fsync_layout(snapshot, final, checkpoint=checkpoint, stats=stats)
        stable_source = _capture(source_tree, True, checkpoint, stats, 'preserve_source_stability', content=False)
        _require(stable_source.identities == expected.identities, 'Frozen source changed during preservation')
        target_tree.check_root()
        if capture_sink is not None:
            capture_sink(final, source_capture)
        return final.manifest


class _HashingReader:
    def __init__(self, stream, checkpoint=None, stats=None, phase=None):
        self.stream, self.checkpoint, self.stats, self.phase = stream, checkpoint, stats, phase
        self.digest, self.count = hashlib.sha256(), 0
        if phase:
            _count(stats, phase, 'content_files_read')

    def read(self, size=-1):
        _tick(self.checkpoint)
        data = self.stream.read(size)
        self.digest.update(data)
        self.count += len(data)
        if self.phase:
            _count(self.stats, self.phase, 'content_read_calls')
            _count(self.stats, self.phase, 'content_bytes_read', len(data))
        return data


class _HashingWriter:
    def __init__(self, stream):
        self.stream, self.digest, self.count = stream, hashlib.sha256(), 0

    def write(self, data):
        written = self.stream.write(data)
        _require(written == len(data), 'Incomplete archive write')
        self.digest.update(data)
        self.count += written
        return written

    def flush(self):
        self.stream.flush()


class _ObservedReader:
    """保留 tar 流预读尾部，以验证 tar EOF 后没有被隐藏的非零成员数据。"""
    def __init__(self, stream, checkpoint):
        self.stream, self.checkpoint, self.count = stream, checkpoint, 0
        self.previous, self.last = b'', b''

    def read(self, size=-1):
        _tick(self.checkpoint)
        data = self.stream.read(size)
        self.count += len(data)
        if data:
            self.previous, self.last = self.last, data
        return data

    def finish(self, consumed, last_member_end):
        # tarfile 容忍缺失结束块的 tar；完整备份要求真正存在两个 512 字节零块。
        _require(consumed == last_member_end + tarfile.BLOCKSIZE,
                 'Archive is missing its end marker')
        tail = self.previous + self.last
        pending = self.count - last_member_end
        _require(0 <= pending <= len(tail), 'Invalid archive stream position')
        _require(not pending or not any(tail[-pending:]), 'Nonzero data after archive EOF')
        # 必须读至 gzip EOF，验证 CRC/长度/截断；tar 的两个零块并不保证 gzip 完整。
        while True:
            data = self.read(CHUNK)
            if not data:
                _require(self.count >= last_member_end + 2 * tarfile.BLOCKSIZE,
                         'Archive is missing its second end marker')
                return
            _require(not any(data), 'Nonzero data after archive EOF')


def _tar_name(member):
    name = member.name
    _require(name == 'snapshot' or name.startswith('snapshot/'), 'Unexpected archive member path')
    relative = '.' if name == 'snapshot' else name[len('snapshot/'):]
    _parts(relative)
    return relative


def _tar_row(member):
    _require((member.isfile() or member.isdir()) and not member.issparse(),
             'Linked or special archive member')
    try:
        value = Decimal(member.pax_headers.get('mtime', str(member.mtime))) * 1000000000
        _require(value.is_finite() and value == value.to_integral_value(), 'Invalid archive timestamp')
        timestamp = int(value)
    except (InvalidOperation, ValueError, OverflowError) as error:
        raise IntegrityError('Invalid archive timestamp') from error
    row = {'type': 'file' if member.isfile() else 'directory', 'uid': member.uid,
           'gid': member.gid, 'mode': member.mode, 'mtime_ns': timestamp}
    if member.isfile():
        row['size'] = member.size
    else:
        _require(member.size == 0, 'Directory archive member has payload')
    return row


@contextmanager
def _archive_input(path):
    path = Path(os.path.abspath(os.fspath(path)))
    with _Tree(path.parent) as parent:
        descriptor = os.open(path.name, os.O_RDONLY | _NOFOLLOW | os.O_NONBLOCK, dir_fd=parent.fd)
        with os.fdopen(descriptor, 'rb') as stream:
            before = os.fstat(stream.fileno())
            _require(stat.S_ISREG(before.st_mode), 'Archive must be a regular file')
            yield stream
            _require(identity(os.fstat(stream.fileno())) == identity(before) and
                     identity(os.stat(path.name, dir_fd=parent.fd, follow_symlinks=False)) == identity(before),
                     'Archive changed during verification')
            parent.check_root()


def _consume_archive(path, expected, checkpoint, stats, phase, consume=None):
    _validate_manifest(expected)
    seen = set()
    last_member_end = 0
    with _archive_input(path) as raw:
        compressed = _HashingReader(raw, checkpoint)
        with gzip.GzipFile(fileobj=compressed, mode='rb') as inflated:
            observed = _ObservedReader(inflated, checkpoint)
            with tarfile.open(fileobj=observed, mode='r|', bufsize=CHUNK) as archive:
                for member in archive:
                    _tick(checkpoint)
                    name = _tar_name(member)
                    _require(name not in seen, 'Duplicate archive member')
                    _require(name in expected, 'Unexpected archive member')
                    _require(_tar_row(member) == _without_hash(expected[name]),
                             'Archive metadata differs from frozen manifest')
                    seen.add(name)
                    last_member_end = member.offset_data + (
                        (member.size + tarfile.BLOCKSIZE - 1) // tarfile.BLOCKSIZE) * tarfile.BLOCKSIZE
                    if member.isfile():
                        with archive.extractfile(member) as stream:
                            content = _HashingReader(stream, checkpoint, stats, phase)
                            if consume is not None:
                                consume(name, content)
                            else:
                                while content.read(CHUNK):
                                    pass
                            _require(content.count == expected[name]['size'] and
                                     content.digest.hexdigest() == expected[name]['sha256'],
                                     'Archive content differs from frozen manifest')
                    elif consume is not None:
                        consume(name, None)
                consumed = archive.fileobj.tell()
            observed.finish(consumed, last_member_end)
        _require(seen == set(expected), 'Archive member set differs from frozen manifest')
        return {'entries': len(seen), 'metadata_verified': True, 'content_verified': True,
                'sha256': compressed.digest.hexdigest(), 'size': compressed.count}


def verify_archive(path, expected, *, checkpoint=None, stats=None):
    """独立回读归档，校验成员、内容、元数据、tar 尾部及 gzip EOF。"""
    return _consume_archive(path, expected, checkpoint, stats, 'archive_verify')


def write_archive(snapshot, path, expected, *, checkpoint=None, stats=None):
    """将写归档与快照内容校验合并；完成后独立回读一次，不重复读取压缩包求 SHA。"""
    _validate_manifest(expected)
    path = Path(os.path.abspath(os.fspath(path)))
    with _Tree(snapshot) as tree, _Tree(path.parent) as parent:
        _require(path != tree.path and tree.path not in path.parents, 'Archive must be outside snapshot')
        before = _capture(tree, False, checkpoint, stats, 'archive_stat', content=False)
        _require(before.manifest == {name: _without_hash(row) for name, row in expected.items()},
                 'Snapshot metadata changed before archive')
        def write_members(archive):
            for name, file_fd, info in _walk(tree, False, checkpoint, stats, 'archive_write_stat'):
                _require(name in expected and _row(info) == _without_hash(expected[name]),
                         'Snapshot metadata changed during archive')
                wanted = expected[name]
                member = tarfile.TarInfo('snapshot' if name == '.' else 'snapshot/' + name)
                member.type = tarfile.DIRTYPE if wanted['type'] == 'directory' else tarfile.REGTYPE
                member.uid, member.gid, member.mode = wanted['uid'], wanted['gid'], wanted['mode']
                member.mtime = wanted['mtime_ns'] // 1000000000
                member.pax_headers['mtime'] = format(Decimal(wanted['mtime_ns']) / 1000000000, '.9f')
                if wanted['type'] == 'file':
                    member.size = wanted['size']
                    with os.fdopen(os.dup(file_fd), 'rb') as stream:
                        content = _HashingReader(stream, checkpoint, stats, 'archive_write')
                        archive.addfile(member, content)
                    _require(content.count == wanted['size'] and
                             content.digest.hexdigest() == wanted['sha256'],
                             'Snapshot content changed during archive')
                else:
                    archive.addfile(member)

        descriptor = os.open(path.name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | _NOFOLLOW,
                             0o600, dir_fd=parent.fd)
        pigz = shutil.which('pigz')
        with os.fdopen(descriptor, 'wb') as raw:
            if pigz:
                compressor = subprocess.Popen(
                    [pigz, '-1', '-n', '-p', str(max(1, min(os.cpu_count() or 1, 4))), '-c'],
                    stdin=subprocess.PIPE, stdout=raw, stderr=subprocess.DEVNULL)
                try:
                    with tarfile.open(fileobj=compressor.stdin, mode='w|', format=tarfile.PAX_FORMAT,
                                      bufsize=CHUNK, copybufsize=CHUNK) as archive:
                        write_members(archive)
                    compressor.stdin.close()
                    _require(compressor.wait() == 0, 'Archive compressor failed')
                except BaseException:
                    if compressor.poll() is None:
                        compressor.kill()
                    compressor.wait()
                    raise
                raw.flush()
                os.fsync(raw.fileno())
                _count(stats, 'archive_write', 'fsync_calls')
                written_sha = written_size = None
            else:
                output = _HashingWriter(raw)
                with gzip.GzipFile(fileobj=output, mode='wb', compresslevel=1, mtime=0, filename='') as compressed:
                    with tarfile.open(fileobj=compressed, mode='w|', format=tarfile.PAX_FORMAT,
                                      bufsize=CHUNK, copybufsize=CHUNK) as archive:
                        write_members(archive)
                raw.flush()
                os.fsync(raw.fileno())
                _count(stats, 'archive_write', 'fsync_calls')
                written_sha, written_size = output.digest.hexdigest(), output.count
        os.fsync(parent.fd)
        _count(stats, 'archive_write', 'fsync_calls')
        stable = _capture(tree, False, checkpoint, stats, 'archive_stability', content=False)
        _require(stable.identities == before.identities, 'Snapshot changed during archive')
        parent.check_root()
    report = verify_archive(path, expected, checkpoint=checkpoint, stats=stats)
    if written_sha is not None:
        _require(report['sha256'] == written_sha and report['size'] == written_size,
                 'Archive changed after creation')
    return report


def restore_archive(path, expected, destination, *, checkpoint=None, stats=None):
    """解包时校验流，再全量读取真正落盘的恢复副本；从不覆盖已有目录。"""
    _validate_manifest(expected)
    destination = Path(os.path.abspath(os.fspath(destination)))
    with _Tree(destination.parent) as parent:
        os.mkdir(destination.name, 0o700, dir_fd=parent.fd)
        with _Tree(destination) as tree:
            def ensure_directory(name):
                parts = _parts(name)
                for count in range(1, len(parts) + 1):
                    current = PurePosixPath(*parts[:count]).as_posix()
                    _require(current in expected and expected[current]['type'] == 'directory',
                             'Unexpected restore parent')
                    with tree.parent(current) as (directory_fd, basename):
                        try:
                            os.mkdir(basename, 0o700, dir_fd=directory_fd)
                        except FileExistsError:
                            info = os.stat(basename, dir_fd=directory_fd, follow_symlinks=False)
                            _require(stat.S_ISDIR(info.st_mode), 'Restore parent is not a real directory')

            def extract(name, content):
                if content is None:
                    ensure_directory(name)
                    return
                parts = _parts(name)
                ensure_directory(PurePosixPath(*parts[:-1]).as_posix())
                with tree.parent(name) as (directory_fd, basename):
                    descriptor = os.open(basename, os.O_WRONLY | os.O_CREAT | os.O_EXCL | _NOFOLLOW,
                                         0o600, dir_fd=directory_fd)
                    with os.fdopen(descriptor, 'wb') as output:
                        created = os.fstat(output.fileno())
                        while True:
                            block = content.read(CHUNK)
                            if not block:
                                break
                            output.write(block)
                        output.flush()
                        current = os.stat(basename, dir_fd=directory_fd, follow_symlinks=False)
                        _require((current.st_dev, current.st_ino) == (created.st_dev, created.st_ino),
                                 'Restore file was replaced during extraction')

            report = _consume_archive(path, expected, checkpoint, stats, 'restore_extract', extract)
            for name, wanted in sorted(expected.items(), key=lambda item: len(_parts(item[0])), reverse=True):
                _tick(checkpoint)
                with tree.open(name) as descriptor:
                    _apply_metadata(descriptor, wanted, stats, 'restore_metadata')
            actual = _capture(tree, False, checkpoint, stats, 'restore_verify', durable=True)
            _require(actual.manifest == expected, 'Restored layout differs from frozen manifest')
            stable = _capture(tree, False, checkpoint, stats, 'restore_stability', content=False)
            _require(stable.identities == actual.identities, 'Restored layout changed during verification')
            tree.check_root()
        os.fsync(parent.fd)
        _count(stats, 'restore_verify', 'fsync_calls')
        parent.check_root()
    report.update(restored=True, durable=True, disk_content_verified=True)
    return report
