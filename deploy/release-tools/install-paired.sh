#!/usr/bin/env bash
# 独立安装配套发布模板；不调用旧安装器、不发布或重启业务。
set -euo pipefail

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  cat <<'HELP'
用法：./deploy/release-tools/install-paired.sh SSH别名 运行根目录
      ./deploy/release-tools/install-paired.sh --package OUTPUT.tar.gz
仅安装 <运行根目录>/tools/paired-deploy/versions/<内容SHA>，校验后原子更新 current。
保留历史版本，不覆盖旧 deploy/rolling 工具，不触发发布、容器操作或备份清理。
--package 仅生成新离线归档，输出内容SHA；不连接服务器，也不覆盖已有文件。
HELP
  exit 0
fi
cfm_mode=install
cfm_output=
if [[ ${1:-} == --package ]]; then
  [[ $# == 2 ]] || { echo '--package 必须指定一个新输出文件。' >&2; exit 2; }
  cfm_mode=package
  cfm_output=$2
else
  [[ $# == 2 ]] || { echo '请指定 SSH 别名和运行根目录。' >&2; exit 2; }
  cfm_target=$1
  cfm_runtime_root=$2
  [[ $cfm_runtime_root == /* && $cfm_runtime_root != / ]] || {
    echo '运行根目录必须是绝对路径。' >&2; exit 2;
  }
  [[ $cfm_target =~ ^[A-Za-z0-9][A-Za-z0-9_.@-]*$ ]] || { echo 'SSH 别名格式无效。' >&2; exit 2; }
fi
cfm_source_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cfm_runner='import sys; program=sys.argv.pop(1); exec(compile(program,"<paired-installer>","exec"))'
cfm_program=$(cat <<'PY_PAIRED_INSTALL'
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tarfile
import tempfile
import uuid

sys.dont_write_bytecode = True

TEMPLATES = ('common.py', 'release.py', 'preflight.py', 'verify.py', 'state-guard.py',
             'coordinator.py', 'worker-release.py', 'build.py',
             'build-worker.py', 'worker-image-probe.py', 'snapshot.py',
             'capture-services.py', 'verify-other-services.py')
FILES = ('paired-deploy.py', 'snapshot_integrity.py', 'paired_phases.py', 'PAIRED.md',
         *('paired/' + name for name in TEMPLATES))
MANIFEST = 'install-manifest.json'
EXPECTED = set(FILES) | {MANIFEST}
RUNNER = 'import sys; program=sys.argv.pop(1); exec(compile(program,"<paired-installer>","exec"))'


def require(value, message):
    if not value:
        raise RuntimeError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def identity(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def regular_bytes(path, owner=None):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor, 'rb') as stream:
        before = os.fstat(stream.fileno())
        require(stat.S_ISREG(before.st_mode) and before.st_nlink == 1
                and before.st_size <= 2 * 1024 * 1024, 'Input is linked, special or too large')
        require(owner is None or before.st_uid == owner and not before.st_mode & 0o022,
                'Input owner or permissions changed')
        raw = stream.read(2 * 1024 * 1024 + 1)
        require(identity(before) == identity(os.fstat(stream.fileno()))
                == identity(path.lstat()) and len(raw) == before.st_size,
                'Input changed while reading')
    return raw


def manifest_for(files, names=None):
    names = tuple(FILES if names is None else names)
    body = {'format': 1, 'files': {name: digest(files[name]) for name in sorted(names)}}
    version = digest(json.dumps(body, sort_keys=True, separators=(',', ':')).encode())
    return dict(body, content_sha256=version)


def package(source, output):
    source, output = Path(source), Path(output)
    require(source.resolve() == source.absolute() and not (source / 'paired').is_symlink(),
            'Package source must not be linked')
    files = {name: regular_bytes(source / name) for name in FILES}
    for name, raw in files.items():
        if name.endswith('.py'):
            compile(raw, name, 'exec')
    manifest = manifest_for(files)
    files[MANIFEST] = (json.dumps(manifest, sort_keys=True, indent=2) + '\n').encode()
    with tarfile.open(output, 'x:gz', format=tarfile.USTAR_FORMAT) as archive:
        for name, raw in sorted(files.items()):
            member = tarfile.TarInfo(name)
            member.size, member.mode, member.mtime = len(raw), 0o600, 0
            archive.addfile(member, io.BytesIO(raw))
    return {'archive': str(output), 'content_sha256': manifest['content_sha256'],
            'files': len(FILES), 'deployment_started': False}


def verify_tree(directory, version, owner, names=None):
    directory = Path(directory)
    require(re.fullmatch('[0-9a-f]{64}', version), 'Invalid content SHA')
    for path in (directory, directory / 'paired'):
        info = path.lstat()
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == owner and not info.st_mode & 0o022,
                'Package directory owner, type or permissions changed')
    if names is None:
        manifest_path = directory / MANIFEST
        require(manifest_path.is_file() and not manifest_path.is_symlink(),
                'Unexpected or missing package path')
        manifest = json.loads(regular_bytes(manifest_path, owner))
        names = tuple(manifest.get('files', {}).keys())
    require(names and len(names) == len(set(names)), 'Package file list is invalid')
    for name in names:
        relative = Path(name)
        require(isinstance(name, str) and name not in (MANIFEST, '', '.', '..')
                and relative.as_posix() == name and not relative.is_absolute()
                and '..' not in relative.parts, 'Package file path is invalid')
    expected = set(names) | {MANIFEST}
    paths = {path.relative_to(directory).as_posix(): path for path in directory.rglob('*')}
    # A host-side Python hook can create validation bytecode in the temporary
    # staging tree between the unprivileged and root checks. It is never part of
    # the archive and is accepted only in this exact directory with regular,
    # non-writable .pyc files owned by the staging user.
    transient = set()
    cache = directory / 'paired' / '__pycache__'
    if cache.exists() or cache.is_symlink():
        info = cache.lstat()
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == owner
                and not info.st_mode & 0o022, 'Validation bytecode directory changed')
        transient.add('paired/__pycache__')
        for path in cache.iterdir():
            info = path.lstat()
            require(path.suffix == '.pyc' and stat.S_ISREG(info.st_mode)
                    and info.st_uid == owner and not info.st_mode & 0o022,
                    'Unexpected validation bytecode')
            transient.add(path.relative_to(directory).as_posix())
    require(set(paths) - transient == expected | {'paired'},
            'Unexpected or missing package path')
    files = {name: regular_bytes(directory / name, owner) for name in expected}
    manifest = json.loads(files[MANIFEST])
    require(set(manifest) == {'format', 'files', 'content_sha256'}
            and set(manifest.get('files', {})) == set(names)
            and manifest == manifest_for(files, names) and manifest['content_sha256'] == version,
            'Package manifest or content digest differs')
    for name in names:
        if name.endswith('.py'):
            compile(files[name], name, 'exec')
    return files


def unpack(stream, directory, version):
    directory = Path(directory)
    (directory / 'paired').mkdir(mode=0o700)
    seen, total = set(), 0
    with tarfile.open(fileobj=stream, mode='r|gz') as archive:
        for member in archive:
            require(member.name in EXPECTED and member.name not in seen
                    and member.isfile() and not member.issparse()
                    and 0 <= member.size <= 2 * 1024 * 1024,
                    'Archive has an unexpected, linked, duplicate or oversized member')
            total += member.size
            require(total <= 8 * 1024 * 1024, 'Archive exceeds the tool package budget')
            with archive.extractfile(member) as incoming:
                raw = incoming.read(member.size + 1)
            require(len(raw) == member.size, 'Archive member is truncated')
            descriptor = os.open(directory / member.name,
                                 os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(descriptor, 'wb') as output:
                output.write(raw)
            seen.add(member.name)
    require(seen == EXPECTED, 'Archive is missing expected tool files')
    return verify_tree(directory, version, os.geteuid())


def fsync_dir(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def ensure_directory(path, owner):
    path.mkdir(mode=0o755, exist_ok=True)
    info = path.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == owner and not info.st_mode & 0o022,
            'Installation directory is linked, writable or owned elsewhere')


def install_version(staging, destination, version, staging_owner):
    staging, destination = Path(staging), Path(destination)
    # Re-read and verify every unprivileged input into memory before root writes.
    files = verify_tree(staging, version, staging_owner)
    require(destination.is_absolute() and destination.parent.is_dir()
            and destination.parent.resolve() == destination.parent,
            'Installation parent is missing or linked')
    owner = os.geteuid()
    parent = destination.parent.lstat()
    require(parent.st_uid == owner and not parent.st_mode & 0o022,
            'Installation parent ownership or permissions changed')
    ensure_directory(destination, owner)
    versions = destination / 'versions'
    ensure_directory(versions, owner)
    descriptor = os.open(destination / '.install.lock', os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(descriptor)
        require(stat.S_ISREG(info.st_mode) and info.st_uid == owner
                and info.st_nlink == 1 and not info.st_mode & 0o077, 'Installation lock changed')
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        current = destination / 'current'
        previous = None
        if current.exists() or current.is_symlink():
            require(current.is_symlink(), 'Current tool pointer is not a symlink')
            previous = os.readlink(current)
            require(re.fullmatch('versions/[0-9a-f]{64}', previous), 'Current tool pointer is unexpected')
            verify_tree(destination / previous, previous.split('/')[1], owner)
        target = versions / version
        if target.exists() or target.is_symlink():
            verify_tree(target, version, owner)
        else:
            with tempfile.TemporaryDirectory(prefix='.install-', dir=versions) as temporary:
                candidate = Path(temporary) / 'candidate'
                candidate.mkdir(mode=0o700)
                (candidate / 'paired').mkdir(mode=0o700)
                for name, raw in sorted(files.items()):
                    with (candidate / name).open('xb') as output:
                        output.write(raw)
                        output.flush()
                        os.fchmod(output.fileno(), 0o644)
                        os.fsync(output.fileno())
                (candidate / 'paired').chmod(0o755)
                candidate.chmod(0o755)
                fsync_dir(candidate / 'paired')
                fsync_dir(candidate)
                verify_tree(candidate, version, owner)
                os.rename(candidate, target)
                fsync_dir(versions)
        # Even an existing immutable version must pass again before switching.
        verify_tree(target, version, owner)
        pointer = destination / ('.current-' + uuid.uuid4().hex)
        try:
            os.symlink('versions/' + version, pointer)
            os.replace(pointer, current)
            fsync_dir(destination)
        finally:
            if pointer.is_symlink():
                pointer.unlink()
        return {'installed': str(target), 'content_sha256': version, 'previous': previous,
                'current': 'versions/' + version, 'deployment_started': False}
    finally:
        os.close(descriptor)


def destination(path):
    root = Path(path)
    require(root.is_absolute() and root != Path('/') and '..' not in root.parts,
            'Runtime root must be an absolute service directory')
    return root / 'tools' / 'paired-deploy'


def main():
    os.umask(0o077)
    action = sys.argv[1]
    if action == 'package':
        print(json.dumps(package(Path(sys.argv[2]), Path(sys.argv[3]))))
    elif action == 'receive':
        require(os.geteuid() != 0, 'Receive and unpack must use the ordinary SSH account')
        version, root = sys.argv[2:4]
        destination(root)
        with tempfile.TemporaryDirectory(prefix='cfm-paired-install-') as temporary:
            staging = Path(temporary)
            unpack(sys.stdin.buffer, staging, version)
            subprocess.run(['sudo', '-n', 'python3', '-B', '-c', RUNNER, program, 'install',
                            str(staging), version, str(os.geteuid()), root], check=True)
    elif action == 'install':
        require(os.geteuid() == 0, 'Only the verified copy step runs as root')
        staging_owner = int(sys.argv[4])
        require(staging_owner > 0, 'Staging must belong to the ordinary SSH account')
        print(json.dumps(install_version(Path(sys.argv[2]), destination(sys.argv[5]), sys.argv[3], staging_owner)))
    else:
        raise RuntimeError('Unknown installer action')


if __name__ == '__main__':
    main()
PY_PAIRED_INSTALL
)

if [[ $cfm_mode == package ]]; then
  python3 -c "$cfm_runner" "$cfm_program" package "$cfm_source_dir" "$cfm_output"
  exit 0
fi
umask 077
cfm_local_staging=$(mktemp -d)
trap 'rm -rf -- "$cfm_local_staging"' EXIT
python3 -c "$cfm_runner" "$cfm_program" package "$cfm_source_dir" "$cfm_local_staging/package.tar.gz" > "$cfm_local_staging/package.json"
cfm_content_sha=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["content_sha256"])' "$cfm_local_staging/package.json")
cfm_remote_command=$(python3 -c 'import shlex,sys; print(shlex.join(["python3","-c",sys.argv[1],sys.argv[2],"receive",sys.argv[3],sys.argv[4]]))' "$cfm_runner" "$cfm_program" "$cfm_content_sha" "$cfm_runtime_root")
ssh -o BatchMode=yes -- "$cfm_target" "$cfm_remote_command" < "$cfm_local_staging/package.tar.gz"
