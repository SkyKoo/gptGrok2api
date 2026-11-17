#!/usr/bin/env python3
"""只处理 Adobe profile 内固定的可重建缓存；凭据、会话与注册账本不在范围内。"""
from __future__ import annotations
import errno
import json
import os
from pathlib import Path
import re
import stat
import sys
import time

CACHE_DIRS = (
    'Default/Cache', 'Default/Code Cache', 'Default/GPUCache',
    'Default/DawnGraphiteCache', 'Default/DawnWebGPUCache',
    'ShaderCache', 'GrShaderCache', 'GraphiteDawnCache',
)


class Refused(Exception):
    pass


def require(value, code):
    if not value:
        raise Refused(code)


def identity(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_size, info.st_mtime_ns, info.st_ctime_ns, info.st_nlink)


def open_directory(path):
    """逐级 openat，任何父目录链接均拒绝；不依赖 resolve 后的字符串路径。"""
    path = Path(path)
    require(path.is_absolute() and '..' not in path.parts, 'unsafe_profile_path')
    fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.parts[1:]:
            following = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = following
        return fd
    except BaseException:
        os.close(fd)
        raise


def stable_lock(run_fd, lock_fd):
    expected = os.fstat(lock_fd)
    current = os.stat('.lock', dir_fd=run_fd, follow_symlinks=False)
    require(stat.S_ISREG(current.st_mode) and current.st_nlink == 1
            and identity(current) == identity(expected), 'profile_lock_changed')


def mountpoints():
    require(sys.platform == 'linux', 'unsupported_platform')
    rows = Path('/proc/self/mountinfo').read_text().splitlines()
    return [Path(re.sub(r'\\([0-7]{3})', lambda m: chr(int(m.group(1), 8)), row.split()[4])) for row in rows]


def no_browser_references(browser, *, protected=(), proc=Path('/proc')):
    """关闭返回后仍检查存活进程的 user-data-dir 与文件引用；不输出进程参数。"""
    require(sys.platform == 'linux', 'unsupported_platform')
    browser_info = browser.stat()
    browser_identity = (browser_info.st_dev, browser_info.st_ino)
    for entry in proc.iterdir():
        if not entry.name.isdigit() or int(entry.name) == os.getpid():
            continue
        try:
            command = (entry / 'cmdline').read_bytes().split(b'\0')
            for index, raw in enumerate(command):
                value = None
                if raw.startswith(b'--user-data-dir='):
                    value = os.fsdecode(raw.split(b'=', 1)[1])
                elif raw == b'--user-data-dir' and index + 1 < len(command):
                    value = os.fsdecode(command[index + 1])
                if value is not None:
                    target = Path(os.path.abspath(value))
                    require(target != browser and browser not in target.parents, 'browser_still_running')
                    # 宿主机CLI看到的是容器内argv路径；通过该进程的root比较真实inode。
                    try:
                        actual = (entry / 'root' / target.as_posix().lstrip('/')).stat()
                    except FileNotFoundError:
                        pass
                    else:
                        require((actual.st_dev, actual.st_ino) != browser_identity, 'browser_still_running')
            links = [entry / 'cwd', entry / 'root', entry / 'exe', *(entry / 'fd').iterdir()]
            for link in links:
                try:
                    raw = os.readlink(link).removesuffix(' (deleted)')
                except FileNotFoundError:
                    continue
                if raw.startswith('/'):
                    target = Path(raw)
                    require(target != browser and browser not in target.parents, 'browser_still_running')
                    try:
                        actual = link.stat()
                    except FileNotFoundError:
                        continue
                    require((actual.st_dev, actual.st_ino) not in protected, 'browser_still_running')
        except FileNotFoundError:
            continue  # 进程/FD已退出，不再持有引用。
        except OSError as error:
            raise Refused('process_check_unavailable') from error


def scan_tree(fd, device, deadline):
    require(time.monotonic() < deadline, 'cache_cleanup_budget')
    before = os.fstat(fd)
    require(before.st_dev == device and stat.S_ISDIR(before.st_mode), 'cache_filesystem_changed')
    children, files, size = {}, 0, 0
    for name in sorted(os.listdir(fd)):
        require(time.monotonic() < deadline, 'cache_cleanup_budget')
        info = os.stat(name, dir_fd=fd, follow_symlinks=False)
        require(info.st_dev == device, 'cache_filesystem_changed')
        require(stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode), 'unsafe_cache_entry')
        if stat.S_ISDIR(info.st_mode):
            child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            try:
                require(identity(os.fstat(child)) == identity(info), 'cache_tree_changed')
                node = scan_tree(child, device, deadline)
            finally:
                os.close(child)
            files += node['files']; size += node['bytes']
        else:
            node = {'identity': identity(info), 'files': 1, 'bytes': info.st_size}
            files += 1; size += info.st_size
        require(identity(os.stat(name, dir_fd=fd, follow_symlinks=False)) == identity(info), 'cache_tree_changed')
        children[name] = node
    require(identity(os.fstat(fd)) == identity(before), 'cache_tree_changed')
    return {'identity': identity(before), 'children': children, 'files': files, 'bytes': size}


def remove_tree(parent, name, expected, guard, result, progress=lambda _: None):
    """只用已打开父fd删除；链接替换不能把删除操作引向profile外部。"""
    guard()
    info = os.stat(name, dir_fd=parent, follow_symlinks=False)
    require(identity(info) == expected['identity'], 'cache_tree_changed')
    if stat.S_ISREG(info.st_mode):
        os.unlink(name, dir_fd=parent)
        result['removedFiles'] += 1; result['removedBytes'] += info.st_size
        if result['removedFiles'] % 64 == 0:
            progress(result)
        return
    require(stat.S_ISDIR(info.st_mode), 'unsafe_cache_entry')
    child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
    try:
        require(identity(os.fstat(child)) == expected['identity'], 'cache_tree_changed')
        require(set(os.listdir(child)) == set(expected['children']), 'cache_tree_changed')
        for basename, row in expected['children'].items():
            remove_tree(child, basename, row, guard, result, progress)
        guard()
        # 子项删除会改变父mtime/ctime；目录身份、类型和所有者仍必须一致。
        require(identity(os.stat(name, dir_fd=parent, follow_symlinks=False))[:5] == expected['identity'][:5],
                'cache_tree_changed')
        os.rmdir(name, dir_fd=parent)
    finally:
        os.close(child)


def maintain(run, *, apply=False, lock_fd=None, budget_ms=30000, progress=lambda _: None):
    result = {'status': 'blocked', 'code': '', 'files': 0, 'bytes': 0, 'removedFiles': 0, 'removedBytes': 0}
    opened, plans, anchors = [], [], []
    deadline = time.monotonic() + min(max(budget_ms, 100), 30000) / 1000
    mutated = False
    try:
        run = Path(run)
        require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]{0,79}', run.name) is not None, 'unsafe_profile_path')
        run_fd = open_directory(run); opened.append(run_fd)
        run_info = os.fstat(run_fd)
        require(run_info.st_uid == os.geteuid() and stat.S_IMODE(run_info.st_mode) == 0o700,
                'profile_permissions_unsafe')
        if apply:
            require(lock_fd is not None, 'profile_lock_required')
            stable_lock(run_fd, lock_fd)
        else:
            try:
                os.stat('.lock', dir_fd=run_fd, follow_symlinks=False)
            except FileNotFoundError:
                pass
            else:
                raise Refused('profile_busy')
        try:
            browser_fd = os.open('browser', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=run_fd)
        except FileNotFoundError:
            result.update(status='cleaned' if apply else 'eligible', code='no_browser_profile')
            return result
        opened.append(browser_fd)
        browser_info = os.fstat(browser_fd)
        require(browser_info.st_uid == os.geteuid() and stat.S_IMODE(browser_info.st_mode) == 0o700,
                'profile_permissions_unsafe')
        device = browser_info.st_dev
        require(device == os.fstat(run_fd).st_dev, 'cache_filesystem_changed')
        browser = run / 'browser'
        # 即使同device的bind mount也拒绝，不能只依赖st_dev。
        if sys.platform == 'linux':
            require(not any(m == run or run in m.parents for m in mountpoints()), 'profile_contains_mount')
        for singleton in ('SingletonLock', 'SingletonSocket', 'SingletonCookie'):
            try:
                os.stat(singleton, dir_fd=browser_fd, follow_symlinks=False)
            except FileNotFoundError:
                pass
            else:
                raise Refused('browser_singleton_present')
        for relative in CACHE_DIRS:
            parts = relative.split('/')
            parent = browser_fd
            try:
                for component in parts[:-1]:
                    above = parent
                    parent = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=above)
                    opened.append(parent)
                    require(os.fstat(parent).st_dev == device, 'cache_filesystem_changed')
                    anchors.append((above, component, os.fstat(parent)))
                target = os.open(parts[-1], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
            except FileNotFoundError:
                continue
            opened.append(target)
            node = scan_tree(target, device, deadline)
            plans.append((parent, parts[-1], node))
            result['files'] += node['files']; result['bytes'] += node['bytes']
        protected = {(browser_info.st_dev, browser_info.st_ino)}
        def collect(node):
            protected.add(node['identity'][:2])
            for child in node.get('children', {}).values():
                collect(child)
        for _, _, node in plans:
            collect(node)
        no_browser_references(browser, protected=protected)
        def guard():
            require(time.monotonic() < deadline, 'cache_cleanup_budget')
            stable_lock(run_fd, lock_fd)
            require(identity(run.lstat())[:5] == identity(run_info)[:5], 'profile_directory_changed')
            require(identity(os.stat('browser', dir_fd=run_fd, follow_symlinks=False))[:5]
                    == identity(browser_info)[:5], 'profile_directory_changed')
            for parent, name, info in anchors:
                require(identity(os.stat(name, dir_fd=parent, follow_symlinks=False))[:5]
                        == identity(info)[:5], 'profile_directory_changed')
        if apply:
            guard()
            # 完整白名单先预检，再开始任何删除；未知布局不会部分放行。
            for parent, name, node in plans:
                guard()
                mutated = True
                remove_tree(parent, name, node, guard, result, progress)
                progress(result)
            result.update(status='cleaned', code='cache_removed')
        else:
            result.update(status='eligible', code='cache_report')
    except BaseException as error:
        code = str(error) if isinstance(error, Refused) else 'cache_inspection_failed'
        result.update(status='partial' if mutated else 'blocked', code=code)
    finally:
        for fd in reversed(opened):
            os.close(fd)
    return result


if __name__ == '__main__':
    try:
        request = json.load(sys.stdin)
        def progress(row):
            print(json.dumps({'progress': True, **row}, separators=(',', ':')), flush=True)
        result = maintain(request['runDir'], apply=request.get('apply') is True,
                          lock_fd=3 if request.get('apply') is True else None,
                          budget_ms=request.get('budgetMs', 30000), progress=progress)
    except BaseException:
        result = {'status': 'blocked', 'code': 'cache_inspection_failed', 'files': 0, 'bytes': 0,
                  'removedFiles': 0, 'removedBytes': 0}
    print(json.dumps(result, separators=(',', ':')), flush=True)
