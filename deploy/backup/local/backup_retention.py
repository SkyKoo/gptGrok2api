#!/usr/bin/env python3
"""Plan and execute bounded retention for explicitly verified CFM backups only.

``plan`` is read-only. ``execute`` requires the exact plan SHA-256, a fresh plan,
unchanged catalog/runtime, all deployment locks, and complete archive validation.
Unknown directories and incomplete/unproven records are always retained. This
tool never removes raw snapshots from recovery points selected for retention.

Catalog format: ogg-backup-catalog-v1, service gptgrok2api only.
Registration is an explicit assertion by the release/daily-backup adapter that
the supplied proof records a successful, recoverable backup; layout alone is
never success evidence. A full restore is performed by the adapter/rehearsal,
not by this tool. Here archives, manifests and their immutable proof are checked.
"""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
from decimal import Decimal
import fcntl
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import subprocess
import sys
import tarfile
from zoneinfo import ZoneInfo

ROOTS = {"gptgrok2api": Path("/opt/gptgrok2api/backups")}
TZ = ZoneInfo("Asia/Shanghai")
FORMAT = "ogg-backup-retention-plan-v1"
HASH = re.compile(r"[0-9a-f]{64}\Z")
SAFE_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,159}\Z")
MAX_PLAN_AGE = dt.timedelta(hours=1)
AUDIT_NAMES = {"release.json", "current.json", "previous.json", "checksums.json",
               "SHA256SUMS", "complete", "recovery-point.json", "manifest.json",
               "snapshot-metadata.json", "baseline.json", "business-before.json",
               "retention-ready.json", "catalog-entry.json"}


class Refusal(RuntimeError):
    """Unsafe, incomplete or changed evidence; no automatic fallback."""


def require(condition, message):
    if not condition:
        raise Refusal(message)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def digest(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def now():
    return dt.datetime.now(TZ)


def timestamp(value):
    result = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    require(result.tzinfo is not None, "Timestamp lacks timezone")
    return result


def identity(info):
    return [info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_size, info.st_mtime_ns, info.st_ctime_ns]


def real_path(path):
    path = Path(path)
    require(path.is_absolute() and path.resolve() == path, "Non-canonical or linked path")
    return path


def project_path(value):
    path = real_path(value)
    require(any(path.is_relative_to(root.parent) for root in ROOTS.values()),
            "Evidence outside the CFM project is forbidden")
    return path


def relative_path(root, name):
    require(isinstance(name, str) and name and not name.startswith("/"), "Invalid relative path")
    parts = PurePosixPath(name).parts
    require(parts and ".." not in parts and "." not in parts, "Unsafe relative path")
    path = root.joinpath(*parts)
    require(path.resolve().is_relative_to(root) and not path.is_symlink(), "Linked artifact path")
    return path


def sha(path):
    path = real_path(path)
    before = path.lstat()
    require(stat.S_ISREG(before.st_mode), "Artifact is not a regular file")
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(block)
    require(identity(before) == identity(path.lstat()), "Artifact changed during hashing")
    return result.hexdigest()


def fingerprint(root):
    """Freeze all metadata, including directories; never follow links."""
    result = hashlib.sha256()
    total = count = 0
    unsafe = False
    first = root.lstat()
    todo = [root]
    while todo:
        path = todo.pop()
        before = path.lstat()
        name = str(path.relative_to(root))
        row = [name, identity(before)]
        if stat.S_ISLNK(before.st_mode):
            unsafe = True
            row.append(os.readlink(path))
        elif stat.S_ISDIR(before.st_mode):
            if before.st_dev != first.st_dev:
                unsafe = True
            else:
                todo.extend(sorted(path.iterdir(), reverse=True))
        elif not stat.S_ISREG(before.st_mode):
            unsafe = True
        elif before.st_nlink != 1:
            # Cross-backup hardlinks would mutate retained inode metadata on unlink.
            unsafe = True
        result.update(canonical(row) + b"\n")
        total += before.st_blocks * 512
        count += 1
    require(identity(first) == identity(root.lstat()), "Backup changed during inventory")
    return {"tree_sha256": result.hexdigest(), "root_identity": identity(first),
            "allocated_bytes": total, "entries": count, "unsafe": unsafe}


def runtime():
    """Read Docker identity/health/mounts without emitting env or credentials."""
    def command(args):
        process = subprocess.run(args, capture_output=True, check=False, timeout=120)
        require(process.returncode == 0, "Runtime inspection failed")
        return process.stdout
    ids = command(["docker", "ps", "-aq"]).decode().split()
    objects = json.loads(command(["docker", "inspect", *ids])) if ids else []
    containers = []
    for item in objects:
        state = item["State"]
        containers.append({"id": item["Id"], "image": item["Image"],
                           "status": state["Status"], "started_at": state["StartedAt"],
                           "restarts": item["RestartCount"], "oom": state["OOMKilled"],
                           "health": state.get("Health", {}).get("Status"),
                           "mounts": item["Mounts"]})
    return sorted(containers, key=lambda value: value["id"])


def guard_state(service):
    result, refs = {}, set(service.get("protected_paths", []))
    for value in service.get("guard_files", []):
        path = project_path(value)
        require(path.stat().st_size <= 16 * 1024 * 1024, "Oversized live guard file")
        result[str(path)] = sha(path)
        # Guard files are current runtime/configuration only, never history trees.
        if service.get("reference_mode", "conservative") != "explicit":
            text = path.read_text(errors="replace")
            pattern = r"(?:" + "|".join(re.escape(str(root)) for root in ROOTS.values()) + r")/[A-Za-z0-9_.-]+"
            refs.update(re.findall(pattern, text))
    return result, sorted(refs)


def record_valid(record):
    """An unproven registration remains protected instead of becoming eligible."""
    return (record.get("completed") is True and record.get("success") is True
            and record.get("kind") in {"release", "daily", "migration"}
            and bool(record.get("proofs")) and bool(record.get("files"))
            and bool(record.get("archives")))


def proof_state(record):
    result = {}
    for item in record.get("proofs", []):
        expected = item.get("sha256", "")
        require(HASH.fullmatch(expected), "Invalid proof hash")
        actual = sha(project_path(item["path"]))
        require(actual == expected, "Success/restore proof changed")
        result[item["path"]] = actual
    return result


def overlap(first, second):
    first, second = Path(first), Path(second)
    return first == second or first in second.parents or second in first.parents


def build_plan(catalog, at=None, runtime_snapshot=None):
    at = at or now()
    require(catalog.get("format") == "ogg-backup-catalog-v1", "Unsupported catalog format")
    require(catalog.get("services"), "Empty catalog")
    require(at.tzinfo is not None, "Plan time lacks timezone")
    live = runtime() if runtime_snapshot is None else runtime_snapshot
    mounts = [m["Source"] for item in live for m in item["mounts"] if m.get("Source")]
    plan = {"format": FORMAT, "as_of": at.isoformat(), "timezone": str(TZ),
            "catalog_sha256": digest(catalog), "runtime": live,
            "policy": {"latest_successful_releases": 3, "calendar_days": 7,
                       "minimum_recoverable": 3}, "services": {}}
    for name, service in catalog["services"].items():
        require(name in ROOTS and service["root"] == str(ROOTS[name]), "Unexpected backup root")
        root = real_path(service["root"])
        require(root.is_dir(), "Backup root missing")
        guards, refs = guard_state(service)
        registrations = {}
        for record in service.get("backups", []):
            key = record["name"]
            require(SAFE_NAME.fullmatch(key) and key not in registrations, "Invalid/duplicate backup registration")
            registrations[key] = record
        rows = []
        children = sorted(root.iterdir())
        links = [str(child.resolve()) for child in children if child.is_symlink()]
        for child in children:
            record = registrations.get(child.name)
            row = {"name": child.name, "path": str(child), "action": "keep", "reasons": [],
                   "fingerprint": fingerprint(child), "eligible": False}
            if not child.is_dir() or child.is_symlink() or row["fingerprint"]["unsafe"]:
                row["reasons"].append("linked-special-or-not-directory")
            elif record is None or not record_valid(record):
                row["reasons"].append("unknown-incomplete-or-unproven")
            else:
                stamp = timestamp(record["created_at"])
                require(stamp <= at + dt.timedelta(minutes=5), "Backup timestamp is in the future")
                row.update(eligible=True, created_at=stamp.isoformat(), kind=record["kind"],
                           proofs=proof_state(record), record_sha256=digest(record))
                row["reasons"].extend(record.get("protected_reasons", []))
                if record["kind"] == "migration" or child.name.startswith("schema-"):
                    row["reasons"].append("migration-recovery-point")
            if any(overlap(child, reference) for reference in refs):
                row["reasons"].append("current-reference")
            if any(overlap(child, target) for target in links):
                row["reasons"].append("symlink-target")
            if any(overlap(child, mount) for mount in mounts):
                row["reasons"].append("container-mount")
            rows.append(row)
        require(set(registrations) <= {p.name for p in children}, "Registered backup disappeared")
        candidates = sorted((r for r in rows if r["eligible"]),
                            key=lambda r: (timestamp(r["created_at"]), r["name"]), reverse=True)
        releases = [r for r in candidates if r["kind"] == "release"]
        for row in releases[:3]:
            row["reasons"].append("latest-three-successful-releases")
        daily = set()
        first_day = at.astimezone(TZ).date() - dt.timedelta(days=6)
        today = at.astimezone(TZ).date()
        for row in candidates:
            day = timestamp(row["created_at"]).astimezone(TZ).date()
            if first_day <= day <= today and day not in daily:
                row["reasons"].append("daily-within-seven-calendar-days")
                daily.add(day)
        # Even catalogs with only daily points retain at least three valid points.
        for row in candidates[:3]:
            row["reasons"].append("minimum-three-recoverable")
        if len(candidates) < 3:
            for row in candidates:
                row["reasons"].append("insufficient-recovery-points-no-deletion")
        for row in rows:
            row["reasons"] = sorted(set(row["reasons"]))
            if row["eligible"] and not row["reasons"]:
                row["action"] = "delete"
        plan["services"][name] = {"root": str(root), "guards": guards, "references": refs,
                                  "rows": rows, "missing_days": [
                                      (first_day + dt.timedelta(days=i)).isoformat()
                                      for i in range(7) if first_day + dt.timedelta(days=i) not in daily]}
    return plan


def verify_archive(root, spec):
    archive_path = relative_path(root, spec["path"])
    expected = None
    if spec.get("metadata_file"):
        expected = json.loads(relative_path(root, spec["metadata_file"]).read_text())
    seen, layout, roots = set(), {}, set()
    with gzip.open(archive_path, "rb") as stream:
        with tarfile.open(fileobj=stream, mode="r|") as archive:
            for member in archive:
                parts = PurePosixPath(member.name).parts
                require(parts and not member.name.startswith("/") and ".." not in parts,
                        "Unsafe archive member")
                name = PurePosixPath(*parts).as_posix()
                require(name not in seen and (member.isfile() or member.isdir()),
                        "Duplicate, linked or special archive member")
                seen.add(name)
                roots.add(parts[0])
                row = {"type": "file" if member.isfile() else "directory", "uid": member.uid,
                       "gid": member.gid, "mode": member.mode & 0o7777,
                       "mtime_ns": int(Decimal(member.pax_headers.get("mtime", str(member.mtime))) * 10**9)}
                if member.isfile():
                    h, count = hashlib.sha256(), 0
                    with archive.extractfile(member) as source:
                        for block in iter(lambda: source.read(1024 * 1024), b""):
                            if expected is not None:
                                h.update(block)
                            count += len(block)
                    require(count == member.size, "Truncated archive member")
                    row.update(size=count, sha256=h.hexdigest())
                if expected is not None:
                    require(parts[0] == "snapshot", "Unexpected metadata archive root")
                    layout[PurePosixPath(*parts[1:]).as_posix()] = row
        while stream.read(1024 * 1024):
            pass  # Force gzip CRC/EOF verification even past tar end-of-archive.
    require(seen, "Empty archive")
    require(set(spec.get("required_roots", [])) <= roots, "Required archive root missing")
    require(set(spec.get("required_members", [])) <= seen, "Required archive member missing")
    require(spec.get("required_roots") or spec.get("required_members"), "Archive layout is unspecified")
    if expected is not None:
        require(layout == expected, "Archive content/permissions differ from manifest")
    return {"members": len(seen), "metadata_verified": expected is not None}


def verify_record(root, record):
    require(record_valid(record), "Unproven backup")
    proof_state(record)
    files = record["files"]
    for name, expected in files.items():
        require(HASH.fullmatch(expected), "Invalid artifact hash")
        require(sha(relative_path(root, name)) == expected, "Backup artifact checksum mismatch")
    verified = {}
    for spec in record["archives"]:
        require(spec["path"] in files, "Archive hash missing from catalog")
        if spec.get("metadata_file"):
            require(spec["metadata_file"] in files, "Archive manifest hash missing")
        verified[spec["path"]] = verify_archive(root, spec)
    return verified


@contextlib.contextmanager
def acquire_locks(catalog):
    with contextlib.ExitStack() as stack:
        paths = set()
        for service in catalog["services"].values():
            root = real_path(service["root"])
            paths.update(service.get("locks", []))
            paths.update(str(p) for p in root.parent.glob("*.lock"))
            require(str(root.parent / "maintenance.lock") in paths, "Maintenance lock is required")
        for name in sorted(paths):
            path = real_path(name)
            require(path.is_file(), "Deployment lock missing")
            handle = stack.enter_context(path.open("r+"))
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as exc:
                raise Refusal("Deployment/backup lock busy; skip this run") from exc
        yield


def mounted_paths():
    return [re.sub(r"\\([0-7]{3})", lambda m: chr(int(m[1], 8)), line.split()[4])
            for line in Path("/proc/self/mountinfo").read_text().splitlines()]


def ensure_unused(targets):
    for target in targets:
        require(not any(Path(m) == target or target in Path(m).parents for m in mounted_paths()),
                "Backup contains a mount")
    for process in Path("/proc").iterdir():
        if not process.name.isdigit() or int(process.name) == os.getpid():
            continue
        try:
            paths = [process / "cwd", process / "root", *list((process / "fd").iterdir())]
        except FileNotFoundError:
            continue
        for path in paths:
            try:
                destination = Path(os.readlink(path).removesuffix(" (deleted)"))
            except FileNotFoundError:
                continue
            except OSError as exc:
                if exc.errno == 22:  # Descriptor is no longer a symlink.
                    continue
                raise
            require(not any(destination == target or target in destination.parents for target in targets),
                    "Backup is referenced by a live process")


def write_private(path, data):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())


def execute(catalog, plan, audit_path, at=None):
    clock = now if at is None else lambda: at
    at = clock()
    stamp = timestamp(plan["as_of"])
    require(plan.get("format") == FORMAT, "Unsupported plan format")
    require(dt.timedelta(0) <= at - stamp <= MAX_PLAN_AGE, "Plan expired or is from the future")
    require(at.astimezone(TZ).date() == stamp.astimezone(TZ).date(), "Plan crossed calendar-day boundary")
    require(plan["catalog_sha256"] == digest(catalog), "Catalog changed")
    audit_path = real_path(audit_path)
    require(not audit_path.exists(), "Audit already exists; inspect previous run before retry")
    require(all(not audit_path.is_relative_to(root) for root in ROOTS.values()), "Audit must be outside backup roots")
    audit_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    saved = audit_path.with_suffix(audit_path.suffix + ".evidence")
    require(not saved.exists(), "Evidence directory already exists")
    deleted = []
    with acquire_locks(catalog):
        fresh = build_plan(catalog, at=stamp)
        require(fresh == plan, "Plan/runtime/backup state changed; create a new plan")
        require(all(not c["oom"] and c["health"] in (None, "healthy")
                    for c in fresh["runtime"] if c["status"] == "running"), "Unhealthy runtime")
        targets = [Path(r["path"]) for s in plan["services"].values() for r in s["rows"] if r["action"] == "delete"]
        ensure_unused(targets)
        write_private(audit_path, b"")
        def event(kind, **data):
            with audit_path.open("ab") as stream:
                stream.write(canonical({"event": kind, "at": now().isoformat(), **data}) + b"\n")
                stream.flush()
                os.fsync(stream.fileno())
        event("started", plan_sha256=digest(plan), targets=[str(p) for p in targets])
        disk_root = Path(next(iter(catalog["services"].values()))["root"])
        before_free = shutil.disk_usage(disk_root).free
        try:
            for service_name, service_plan in plan["services"].items():
                records = {r["name"]: r for r in catalog["services"][service_name]["backups"]}
                verified_kept = 0
                for row in service_plan["rows"]:
                    if row["eligible"]:
                        result = verify_record(Path(row["path"]), records[row["name"]])
                        event("backup_verified", service=service_name, name=row["name"], archives=result,
                              action=row["action"])
                        verified_kept += row["action"] == "keep"
                require(verified_kept >= 3 or not any(r["action"] == "delete" for r in service_plan["rows"]),
                        "Fewer than three verified retained points")
            require(build_plan(catalog, at=stamp) == plan, "State changed during archive verification")
            require(clock().astimezone(TZ).date() == stamp.astimezone(TZ).date(),
                    "Verification crossed calendar-day boundary; create a new plan")
            ensure_unused(targets)
            saved.mkdir(mode=0o700)
            write_private(saved / "plan.json", canonical(plan) + b"\n")
            write_private(saved / "catalog.json", canonical(catalog) + b"\n")
            # Preserve small non-payload audit records for every deletion before any deletion.
            for target in targets:
                destination = saved / target.parent.parent.name / target.name
                destination.mkdir(mode=0o700, parents=True)
                for name in sorted(AUDIT_NAMES):
                    path = target / name
                    if path.exists():
                        require(path.is_file() and not path.is_symlink(), "Invalid audit artifact")
                        require(path.stat().st_size <= 64 * 1024 * 1024, "Oversized audit artifact")
                        write_private(destination / name, path.read_bytes())
                        require(sha(destination / name) == sha(path), "Audit copy differs")
            for target in targets:
                require(clock().astimezone(TZ).date() == stamp.astimezone(TZ).date(),
                        "Deletion crossed calendar-day boundary; create a new plan")
                service = target.parent.parent.name
                row = next(r for r in plan["services"][service]["rows"] if r["path"] == str(target))
                require(fingerprint(target) == row["fingerprint"], "Candidate changed before deletion")
                require(runtime() == plan["runtime"], "Runtime changed before deletion")
                for key, cfg in catalog["services"].items():
                    require(guard_state(cfg) == (plan["services"][key]["guards"], plan["services"][key]["references"]),
                            "Live references changed before deletion")
                ensure_unused([target])
                event("delete_started", path=str(target), fingerprint=row["fingerprint"])
                shutil.rmtree(target)
                deleted.append(str(target))
                event("deleted", path=str(target))
            for service in plan["services"].values():
                for row in service["rows"]:
                    path = Path(row["path"])
                    if row["action"] == "keep":
                        require(fingerprint(path) == row["fingerprint"], "Retained backup changed")
                    else:
                        require(not path.exists() and not path.is_symlink(), "Deletion incomplete")
                require({p.name for p in Path(service["root"]).iterdir()} ==
                        {r["name"] for r in service["rows"] if r["action"] == "keep"}, "Unexpected backup entries")
            require(runtime() == plan["runtime"], "Runtime changed after deletion")
            result = {"deleted": deleted, "freed_bytes": shutil.disk_usage(disk_root).free - before_free,
                      "runtime_unchanged": True, "retained_backups_unchanged": True}
            event("success", **result)
            return result
        except Exception as exc:
            event("failed", deleted=deleted, error_type=type(exc).__name__, error=str(exc))
            raise


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=("plan", "execute"), nargs="?", default="plan")
    parser.add_argument("--catalog", required=True, type=Path)
    parser.add_argument("--output", type=Path, help="Write a new plan (never overwrite)")
    parser.add_argument("--plan", type=Path)
    parser.add_argument("--expect-plan-sha256", help="SHA-256 of the exact plan file")
    parser.add_argument("--audit", type=Path, help="New private JSONL audit path outside backup roots")
    args = parser.parse_args()
    os.nice(15)
    catalog = json.loads(args.catalog.read_text())
    if args.command == "plan":
        result = build_plan(catalog)
        raw = json.dumps(result, ensure_ascii=False, indent=2).encode() + b"\n"
        if args.output:
            write_private(args.output, raw)
            print(json.dumps({"plan": str(args.output), "sha256": hashlib.sha256(raw).hexdigest(),
                              "deletions": sum(r["action"] == "delete" for s in result["services"].values() for r in s["rows"])}))
        else:
            sys.stdout.buffer.write(raw)
    else:
        require(args.plan and args.audit and args.expect_plan_sha256, "Execute requires plan, expected SHA-256 and audit")
        require(sha(args.plan.resolve()) == args.expect_plan_sha256, "Plan file hash mismatch")
        print(json.dumps(execute(catalog, json.loads(args.plan.read_text()), args.audit.resolve())))


if __name__ == "__main__":
    try:
        main()
    except (Refusal, OSError, ValueError, KeyError) as error:
        print(json.dumps({"status": "refused", "error": str(error)}), file=sys.stderr)
        raise SystemExit(1)
