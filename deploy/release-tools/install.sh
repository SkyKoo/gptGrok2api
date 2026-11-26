#!/usr/bin/env bash
# 从 CFM 仓库安装发布工具；不触发构建或容器重启。
set -euo pipefail

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  echo '用法：./deploy/release-tools/install.sh SSH别名 运行根目录'
  echo '更新 <运行根目录>/tools，保留旧工具备份，不修改运行配置、数据和容器。'
  exit 0
fi
[[ $# == 2 ]] || { echo '请指定 SSH 别名和运行根目录。' >&2; exit 2; }
target=$1
runtime_root=$2
[[ $target =~ ^[A-Za-z0-9][A-Za-z0-9_.@-]*$ ]] || { echo 'SSH 别名格式无效。' >&2; exit 2; }
python3 -c 'from pathlib import Path; import sys; p = Path(sys.argv[1]); sys.exit(not p.is_absolute() or p == Path("/") or ".." in p.parts)' "$runtime_root"
source_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bash -n "$source_dir/deploy.sh"
python3 -c 'import ast, pathlib, sys; ast.parse(pathlib.Path(sys.argv[1]).read_text())' "$source_dir/deploy.py"
python3 -c 'import ast,pathlib,sys; ast.parse(pathlib.Path(sys.argv[1]).read_text())' "$source_dir/rolling-deploy.py"

remote_command=$(cat <<'REMOTE'
set -eu
runtime_root=$1
umask 077
staging=$(mktemp -d)
trap 'rm -rf -- "$staging"' EXIT
tar -xzf - -C "$staging"
sudo -n true
bash -n "$staging/deploy.sh"
python3 -c 'import ast, pathlib, sys; ast.parse(pathlib.Path(sys.argv[1]).read_text())' "$staging/deploy.py"
if ! sudo -n test -d "$runtime_root"; then
  sudo -n install -d -o root -g root -m 755 "$runtime_root"
fi
sudo -n install -d -o root -g root -m 700 "$runtime_root/backups"
if sudo -n test -d "$runtime_root/tools"; then
  backup="$runtime_root/backups/tools-$(date -u +%Y%m%dT%H%M%SZ)-$$"
  sudo -n cp -a "$runtime_root/tools" "$backup"
  echo "旧工具备份：$backup"
fi
sudo -n install -d -o root -g root -m 755 "$runtime_root/tools"
sudo -n install -o root -g root -m 755 "$staging/deploy.sh" "$runtime_root/tools/deploy.sh"
sudo -n install -o root -g root -m 644 "$staging/deploy.py" "$staging/rolling-deploy.py" "$staging/compose.yaml" "$runtime_root/tools/"
echo "工具安装完成：$runtime_root/tools/deploy.sh"
REMOTE
)

remote_command=$(python3 -c 'import shlex, sys; print(shlex.join(["bash", "-c", sys.argv[1], "install", sys.argv[2]]))' "$remote_command" "$runtime_root")
COPYFILE_DISABLE=1 tar --format=ustar -C "$source_dir" -czf - deploy.sh deploy.py rolling-deploy.py compose.yaml |
  ssh -o BatchMode=yes -- "$target" "$remote_command"
