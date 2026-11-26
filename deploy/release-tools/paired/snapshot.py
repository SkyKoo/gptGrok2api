"""Split durable raw backup from resumable archive completion."""
import hashlib
import json
import os
from pathlib import Path
import stat
import uuid

import common
import snapshot_integrity as integrity

verify_archive = integrity.verify_archive
restore_archive = integrity.restore_archive
DATABASE_FILES = ('postgres.dump', 'postgres-globals.sql', 'redis.rdb')
RAW_IDENTITY = 'raw-identity.json'


def boot_identity():
    """身份缓存只在同一次内核启动内可复用；不支持的平台退回内容校验。"""
    try:
        return str(uuid.UUID(Path('/proc/sys/kernel/random/boot_id').read_text().strip()))
    except (OSError, ValueError):
        return None


def layout(root):
    return integrity.capture_layout(root).manifest


def source_layout(root):
    return integrity.capture_layout(root, selected=True).manifest


def file_proof(path, *, durable=False, checkpoint=None):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        before = os.fstat(descriptor)
        common.require(stat.S_ISREG(before.st_mode), 'Backup evidence must be regular')
        digest = hashlib.sha256()
        while True:
            if checkpoint is not None:
                checkpoint()
            block = os.read(descriptor, 1024 * 1024)
            if not block:
                break
            digest.update(block)
            if checkpoint is not None:
                checkpoint()
        if durable:
            if checkpoint is not None:
                checkpoint()
            os.fsync(descriptor)
            if checkpoint is not None:
                checkpoint()
        after = os.fstat(descriptor)
        common.require(integrity.identity(before) == integrity.identity(after)
                       == integrity.identity(Path(path).lstat()), 'Backup evidence changed')
        return {'sha256': digest.hexdigest(), 'identity': list(integrity.identity(after))}
    finally:
        os.close(descriptor)


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def load_receipt(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor) as stream:
        before = os.fstat(stream.fileno())
        common.require(stat.S_ISREG(before.st_mode), 'Receipt must be a regular file')
        value = json.load(stream)
        common.require(integrity.identity(before) == integrity.identity(os.fstat(stream.fileno()))
                       == integrity.identity(Path(path).lstat()), 'Receipt changed while reading')
    return value


def verify_proofs(work, proofs):
    for name, proof in proofs.items():
        common.require(list(integrity.identity((work / name).lstat())) == proof['identity'],
                       'Sealed backup evidence changed')


def verify_raw_snapshot(work, manifest, manifest_sha256, receipt=None, *, checkpoint=None, stats=None):
    """复用受摘要保护的同启动身份清单；缺失或变化时重新验证全部内容。"""
    root = Path(work) / 'snapshot'
    boot = boot_identity()
    if (isinstance(receipt, dict) and receipt.get('format') == 1
            and receipt.get('protocol') == common.PROTOCOL
            and boot is not None and receipt.get('boot_id') == boot
            and receipt.get('root') == str(root.absolute())
            and receipt.get('manifest_sha256') == manifest_sha256):
        identities = receipt.get('identities')
        if (isinstance(identities, dict) and set(identities) == set(manifest)
                and all(isinstance(row, list) and len(row) == 8
                        and all(type(value) is int for value in row)
                        for row in identities.values())):
            capture = integrity.Capture(manifest, {name: tuple(row) for name, row in identities.items()})
            try:
                integrity.verify_identity(root, capture, checkpoint=checkpoint, stats=stats)
                return {'method': 'sealed_identity', 'entries': len(manifest)}
            except integrity.IntegrityError:
                # A replaced inode or changed ctime is not proof of corruption.
                # Re-read the actual bytes and require the exact frozen manifest.
                pass
    capture = integrity.capture_layout(root, checkpoint=checkpoint, stats=stats)
    common.require(capture.manifest == manifest, 'Raw snapshot differs from frozen manifest')
    return {'method': 'full_content', 'entries': len(manifest)}


def business_file_digests(manifest, runtime_files=()):
    runtime_files = integrity.CFM_RUNTIME_FILES.union(runtime_files)
    return {name.removeprefix('data/'): row['sha256'] for name, row in manifest.items()
            if name.startswith('data/') and row['type'] == 'file'
            and name.removeprefix('data/') not in runtime_files}


def verify_backup(work, *, runtime_files=(), checkpoint=None, stats=None):
    """独立验收仍完整回读归档；压缩包摘要由同一次读取产生。"""
    work = Path(work)
    checks = load_receipt(work / 'checksums.json')
    required = {*DATABASE_FILES, 'snapshot-metadata.json', 'snapshot.tar.gz'}
    report_name = 'archive-report.json'
    allowed = (required, required | {RAW_IDENTITY}, required | {report_name},
               required | {RAW_IDENTITY, report_name})
    common.require(isinstance(checks, dict) and set(checks) in allowed,
                   'Frozen backup checksum set differs')
    complete = load_receipt(work / 'complete')
    common.require(complete.get('protocol') == common.PROTOCOL and complete.get('metadata_verified') is True,
                   'Frozen backup metadata evidence missing')
    # Hash small evidence and database dumps once. The large archive is hashed
    # by verify_archive while it checks every member and the gzip/tar endings.
    proofs = {name: file_proof(work / name, checkpoint=checkpoint)
              for name in checks if name != 'snapshot.tar.gz'}
    common.require(all(proof['sha256'] == checks[name] for name, proof in proofs.items()),
                   'Frozen backup checksums changed')
    manifest = load_receipt(work / 'snapshot-metadata.json')
    receipt = load_receipt(work / RAW_IDENTITY) if RAW_IDENTITY in checks else None
    verify_proofs(work, proofs)
    archive_identity = integrity.identity((work / 'snapshot.tar.gz').lstat())
    if report_name in checks:
        report = load_receipt(work / report_name)
        report_proof = proofs[report_name]
        common.require(report.get('format') == 1 and report.get('content_verified') is True
                       and report.get('manifest_sha256') == checks['snapshot-metadata.json']
                       and report.get('sha256') == checks['snapshot.tar.gz']
                       and report.get('archive_identity') == list(archive_identity)
                       and report.get('entries') == len(manifest),
                       'Frozen archive report differs')
        archive = {key: report[key] for key in ('entries', 'content_verified', 'sha256', 'size')}
    else:
        archive = integrity.verify_archive(work / 'snapshot.tar.gz', manifest, checkpoint=checkpoint, stats=stats)
        common.require(archive['sha256'] == checks['snapshot.tar.gz'], 'Frozen archive checksum changed')
    if complete.get('archive_mode') == 'direct':
        raw = {'method': 'direct_archive', 'entries': len(manifest)}
    else:
        raw = verify_raw_snapshot(work, manifest, checks['snapshot-metadata.json'], receipt,
                                  checkpoint=checkpoint, stats=stats)
    verify_proofs(work, proofs)
    common.require(integrity.identity((work / 'snapshot.tar.gz').lstat()) == archive_identity,
                   'Archive changed after verification')
    files = business_file_digests(manifest, runtime_files)
    return {'checksums_verified': sorted(checks), 'archive': archive, 'raw_snapshot': raw, 'files': files}


def _direct_snapshot(legacy, root, work, host, *, restoring=False):
    """Use the rolling tool's single streaming archive for paired releases.

    Paired releases still seal database and metadata evidence before the
    handoff. The large business tree stays in one compressed archive instead
    of an expanded copy plus a second archive.
    """
    result = legacy.Snapshot(root, work, host, direct=True)
    legacy_finish = result.finish
    legacy_sync = result.sync

    def sync(frozen=False):
        value = legacy_sync(frozen=frozen)
        if frozen:
            result.source_capture = integrity.Capture(
                getattr(result, 'source_manifest', None) or result.frozen_manifest,
                result.direct_identities, selected=True)
        return value

    result.sync = sync

    def business_files():
        common.require(result.frozen_manifest is not None, 'Snapshot was not frozen')
        return business_file_digests(getattr(result, 'source_manifest', None) or result.frozen_manifest,
                                     legacy.RUNTIME_FILES)

    result.business_files = business_files

    def seal():
        common.require(result.frozen_manifest is not None
                       and (result.work / 'snapshot.tar.gz').is_file(),
                       'Frozen direct archive is missing')
        archive_identity = result._identity((result.work / 'snapshot.tar.gz').stat())
        proofs = {name: file_proof(result.work / name, durable=True,
                                   checkpoint=result.host.checkpoint)
                  for name in DATABASE_FILES}
        metadata_proof = file_proof(result.work / 'snapshot-metadata.json', durable=True,
                                    checkpoint=result.host.checkpoint)
        archive_fd = os.open(result.work / 'snapshot.tar.gz', os.O_RDONLY | os.O_NOFOLLOW)
        try:
            os.fsync(archive_fd)
        finally:
            os.close(archive_fd)
        common.require(result._identity((result.work / 'snapshot.tar.gz').stat())
                       == archive_identity,
                       'Direct archive changed before seal')
        proofs['snapshot-metadata.json'] = metadata_proof
        legacy.atomic_json(result.work / 'raw-seal.json', {
            'format': 1,
            'protocol': common.PROTOCOL,
            'archive_mode': 'direct',
            'media_included': getattr(result, 'media_included', True),
            'files': proofs,
            'manifest_sha256': metadata_proof['sha256'],
            'archive_identity': list(archive_identity),
            'metadata_verified': True,
            'raw_durable': True,
        })
        sync_directory(result.work)
        sync_directory(result.work.parent)
        verify_proofs(result.work, proofs)
        return {'raw_durable': True, 'metadata_verified': True}

    result.seal = seal

    def finish():
        seal_receipt = load_receipt(result.work / 'raw-seal.json')
        common.require(seal_receipt.get('format') == 1
                       and seal_receipt.get('protocol') == common.PROTOCOL
                       and seal_receipt.get('archive_mode') == 'direct'
                       and seal_receipt.get('metadata_verified') is True
                       and seal_receipt.get('raw_durable') is True,
                       'Durable direct snapshot receipt missing')
        files = seal_receipt.get('files')
        common.require(isinstance(files, dict)
                       and set(files) == set(DATABASE_FILES) | {'snapshot-metadata.json'},
                       'Direct backup evidence set differs')
        common.require(seal_receipt.get('manifest_sha256')
                       == files['snapshot-metadata.json']['sha256'],
                       'Direct backup manifest binding differs')
        common.require(seal_receipt.get('archive_identity')
                       == list(result._identity((result.work / 'snapshot.tar.gz').stat())),
                       'Direct archive identity changed')
        for name, proof in files.items():
            common.require(file_proof(result.work / name,
                                      checkpoint=result.host.checkpoint) == proof,
                          'Sealed direct evidence changed')
        value = legacy_finish()
        complete = load_receipt(result.work / 'complete')
        if complete.get('metadata_verified') is not True:
            complete['metadata_verified'] = True
            legacy.atomic_json(result.work / 'complete', complete)
        return value

    result.finish = finish
    return result


def make(legacy, root, work, host, *, restoring=False):
    if not restoring:
        return _direct_snapshot(legacy, root, work, host)

    result = _direct_snapshot(legacy, root, work, host, restoring=True)
    result.frozen_manifest = load_receipt(result.work / 'snapshot-metadata.json')
    common.require((result.work / 'raw-seal.json').is_file(),
                   'Direct raw backup seal is missing')
    result.media_included = load_receipt(result.work / 'raw-seal.json').get('media_included', True)
    result.direct_archive_identity = result._identity((result.work / 'snapshot.tar.gz').stat())
    return result


def make_legacy(legacy, root, work, host, *, restoring=False):
    class VerifiedSnapshot(legacy.Snapshot):
        frozen_layout = None
        capture = None
        source_capture = None

        def sync(self, frozen=False):
            super().sync(frozen=frozen)
            if frozen:
                # The legacy frozen copy checks source identity before and after
                # every file read. Reuse that proof below so preserve_layout
                # only hashes the durable target copy once.
                def remember(value, source_value):
                    self.capture = value
                    self.source_capture = source_value
                self.frozen_layout = integrity.preserve_layout(
                    self.root, self.work / 'snapshot', checkpoint=self.host.checkpoint,
                    capture_sink=remember, source_content_verified=True)

        def business_files(self):
            common.require(self.frozen_layout is not None, 'Snapshot was not frozen')
            return business_file_digests(self.frozen_layout, legacy.RUNTIME_FILES)

        def seal(self):
            common.require(self.capture is not None, 'Snapshot was not frozen')
            common.require(not (self.work / 'raw-seal.json').exists(), 'Raw backup was already sealed')
            # preserve_layout already fsynced and verified every raw file/dir.
            # Keep the final identity check below after the evidence files are
            # written. The earlier duplicate pair only repeated the same full
            # metadata walk before seal construction.
            legacy.atomic_json(self.work / 'snapshot-metadata.json', self.frozen_layout)
            proofs = {name: file_proof(self.work / name, durable=True,
                                       checkpoint=self.host.checkpoint)
                      for name in DATABASE_FILES}
            manifest_proof = file_proof(self.work / 'snapshot-metadata.json', durable=True,
                                        checkpoint=self.host.checkpoint)
            legacy.atomic_json(self.work / RAW_IDENTITY,
                               {'format': 1, 'protocol': common.PROTOCOL, 'boot_id': boot_identity(),
                                'root': str((self.work / 'snapshot').absolute()),
                                'manifest_sha256': manifest_proof['sha256'],
                                'identities': self.capture.identities})
            proofs[RAW_IDENTITY] = file_proof(self.work / RAW_IDENTITY, durable=True,
                                              checkpoint=self.host.checkpoint)
            proofs['snapshot-metadata.json'] = manifest_proof
            integrity.verify_identity(self.work / 'snapshot', self.capture,
                                      checkpoint=self.host.checkpoint)
            integrity.verify_identity(self.root, self.source_capture,
                                      checkpoint=self.host.checkpoint)
            sync_directory(self.work)
            sync_directory(self.work.parent)
            verify_proofs(self.work, proofs)
            legacy.atomic_json(self.work / 'raw-seal.json',
                               {'format': 1, 'protocol': common.PROTOCOL, 'files': proofs,
                                'manifest_sha256': proofs['snapshot-metadata.json']['sha256'],
                                'metadata_verified': True, 'raw_durable': True})
            return {'raw_durable': True, 'metadata_verified': True}

        def finish(self):
            seal = load_receipt(self.work / 'raw-seal.json')
            common.require(seal.get('format') == 1 and seal.get('metadata_verified') is True
                           and seal.get('raw_durable') is True and seal.get('protocol') == common.PROTOCOL,
                           'Durable raw snapshot receipt missing')
            required = {*DATABASE_FILES, 'snapshot-metadata.json'}
            common.require(set(seal.get('files', {})) in (required, required | {RAW_IDENTITY}),
                           'Raw backup evidence set differs')
            common.require(seal.get('manifest_sha256') == seal['files']['snapshot-metadata.json']['sha256'],
                           'Raw backup manifest binding differs')
            for name, proof in seal['files'].items():
                common.require(file_proof(self.work / name, checkpoint=self.host.checkpoint) == proof,
                               'Sealed backup evidence changed')
            manifest = load_receipt(self.work / 'snapshot-metadata.json')
            archive = self.work / 'snapshot.tar.gz'
            if archive.exists() or archive.is_symlink():
                report = integrity.verify_archive(archive, manifest, checkpoint=self.host.checkpoint)
            else:
                # Interrupted temporary archives remain evidence. A resume creates
                # another unique temp; it never overwrites an unknown artifact.
                pending = self.work / ('snapshot-' + uuid.uuid4().hex + '.partial.tar.gz')
                report = integrity.write_archive(self.work / 'snapshot', pending, manifest,
                                                 checkpoint=self.host.checkpoint)
                os.link(pending, archive, follow_symlinks=False)
                pending.unlink()
                sync_directory(self.work)
            archive_report = dict(report, format=1,
                                  manifest_sha256=file_proof(self.work / 'snapshot-metadata.json',
                                                            checkpoint=self.host.checkpoint)['sha256'],
                                  archive_identity=list(integrity.identity(archive.lstat())))
            legacy.atomic_json(self.work / 'archive-report.json', archive_report)
            verify_proofs(self.work, seal['files'])
            checks = {name: value['sha256'] for name, value in seal['files'].items()}
            checks['snapshot.tar.gz'] = report['sha256']
            checks['archive-report.json'] = file_proof(
                self.work / 'archive-report.json', checkpoint=self.host.checkpoint)['sha256']
            if (self.work / 'complete').exists():
                common.require(load_receipt(self.work / 'checksums.json') == checks,
                               'Completed backup evidence differs')
                complete = load_receipt(self.work / 'complete')
                common.require(complete.get('protocol') == common.PROTOCOL
                               and complete.get('metadata_verified') is True, 'Completion receipt differs')
                return report
            legacy.atomic_json(self.work / 'checksums.json', checks)
            legacy.atomic_json(self.work / 'complete',
                               {'verified_at': legacy.now(), 'protocol': common.PROTOCOL,
                                'metadata_verified': True})
            return report

    if not restoring:
        return VerifiedSnapshot(root, work, host)
    result = VerifiedSnapshot.__new__(VerifiedSnapshot)
    result.root, result.work, result.host = Path(root), Path(work), host
    result.receipts = {}
    return result


def resume(legacy, root, work, host):
    """Archive only: no live data copy, database export, activation or routing."""
    if (Path(work) / 'snapshot').is_dir():
        result = make_legacy(legacy, root, work, host, restoring=True)
        result.frozen_layout = load_receipt(result.work / 'snapshot-metadata.json')
        result.capture = integrity.capture_layout(result.work / 'snapshot', checkpoint=host.checkpoint)
        common.require(result.capture.manifest == result.frozen_layout, 'Raw snapshot changed before resume')
        return result
    return make(legacy, root, work, host, restoring=True)
