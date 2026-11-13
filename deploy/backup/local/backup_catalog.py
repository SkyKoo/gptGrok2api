#!/usr/bin/env python3
"""只读登记已知、成功的 CFM 恢复点，供精确保留计划使用。

本工具不删除文件、不运行备份或修改服务。未知布局和缺乏成功证据的目录始终保留。
登记时核对发布校验和及 PostgreSQL 导出目录；执行器再次完整校验归档，登记不等于恢复测试。
"""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

FORMAT = "ogg-backup-catalog-v1"
NAME = re.compile(r"[A-Za-z0-9_.-]{1,160}\Z")
HASH = re.compile(r"[a-f0-9]{64}\Z")
SERVICES = ("gptgrok2api",)
INTERRUPTED = set()
TZ = dt.timezone(dt.timedelta(hours=8))


class CatalogError(RuntimeError):
    pass


def require(ok, message):
    if not ok:
        raise CatalogError(message)


def regular(path):
    """拒绝目录和路径中的符号链接，避免把其他服务文件登记为备份。"""
    path = Path(path)
    return (path.is_absolute() and path.resolve() == path and path.is_file()
            and stat.S_ISREG(path.lstat().st_mode))


def signature(path):
    require(regular(path), "Artifact is not an ordinary file")
    s = path.stat()
    return [s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns, s.st_mode]


def sha(path):
    before = signature(path)
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(block)
    require(signature(path) == before, "File changed while hashing")
    return h.hexdigest()


def load(path, optional=False):
    if optional and not path.exists():
        return None
    require(regular(path) and path.stat().st_size <= 16 * 1024 * 1024,
            "Missing, unsafe or oversized proof")
    value = json.loads(path.read_text())
    require(isinstance(value, dict), "Proof must be an object")
    return value


def stamp(name):
    match = re.search(r"(20\d{6}T\d{6}Z)", name)
    require(match is not None, "Backup has no unambiguous UTC timestamp")
    return dt.datetime.strptime(match[1], "%Y%m%dT%H%M%SZ").replace(tzinfo=dt.timezone.utc).isoformat()


def proof(path):
    return {"path": str(path), "sha256": sha(path)}


def success(document, service, backup, adobe=False):
    if document.get("backup") != str(backup):
        return False
    if service == "lumora":
        return (document.get("stage") == "success" and document.get("new_health") == "healthy"
                and document.get("old_stopped") is True)
    return (document.get("status") == "success" and document.get("phase") == "success"
            and (not adobe or document.get("final_backup_verified") is True))


def external_proofs(root):
    """只查既定 operations 下的直接发布状态，不递归展开历史元数据引用。"""
    found = {}
    operation_root = root / "operations"
    if not operation_root.exists() or operation_root.resolve() != operation_root:
        return found
    for directory in sorted(operation_root.iterdir()):
        if not directory.is_dir() or directory.resolve() != directory:
            continue
        path = directory / "release-state.json"
        if not regular(path):
            continue
        try:
            doc = load(path)
        except (CatalogError, ValueError, OSError):
            continue
        backup = doc.get("backup")
        if isinstance(backup, str) and Path(backup).parent == root / "backups":
            found.setdefault(backup, []).append((path, doc))
    return found


def attestations(path):
    if path is None:
        return {}
    doc = load(path)
    require(doc.get("format") == "ogg-backup-attestations-v1", "Unknown attestation format")
    result = {}
    for item in doc.get("backups", []):
        key = (item.get("service"), item.get("name"))
        require(key[0] in SERVICES and NAME.fullmatch(key[1] or "") and key not in result,
                "Invalid or duplicate attestation")
        require(item.get("success") is True and item.get("bindings"), "Unbound success attestation")
        require(HASH.fullmatch(item.get("source_evidence_sha256", "")), "Missing original evidence hash")
        result[key] = (path, item)
    return result


def attested_proof(item, backup):
    if item is None:
        return None
    path, doc = item
    for name, digest in doc["bindings"].items():
        require(NAME.fullmatch(name) and HASH.fullmatch(digest), "Invalid attestation binding")
        require(sha(backup / name) == digest, "Attested backup metadata changed")
    return proof(path)


def artifact_hash(path, expected, prior):
    identity = signature(path)
    old = prior.get("_file_signatures", {}).get(path.name)
    previous_hash = prior.get("files", {}).get(path.name, "")
    if old == identity and HASH.fullmatch(previous_hash) and prior.get("_content_hashes_verified") is True:
        actual = previous_hash
    else:
        actual = sha(path)
    if expected is not None:
        require(HASH.fullmatch(expected) and expected == actual, "Recorded checksum does not match artifact")
    return actual, identity


def verify_postgres_dump(service, path):
    """仅让现有 PG 客户端读取导出目录，不连接数据库，不执行恢复写入。"""
    before = signature(path)
    with path.open("rb") as source:
        result = subprocess.run(["docker", "exec", "-i", service + "-postgres-1", "pg_restore", "--list"],
                                stdin=source, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=120)
    require(result.returncode == 0 and signature(path) == before, "PostgreSQL dump TOC validation failed")


def daily_record(backup):
    require(not (backup / "failed.json").exists(), "Daily backup has an unresolved failure")
    entry = load(backup / "catalog-entry.json")
    state = load(backup / "recovery-point.json")
    complete = load(backup / "complete")
    require(state.get("format") == "ogg-daily-proof-v1"
            and complete.get("format") == "ogg-daily-complete-v1", "Unknown daily format")
    require(complete.get("proof_sha256") == sha(backup / "recovery-point.json"), "Daily proof changed")
    require(all(state.get(key) is True for key in
                ("completed", "success", "archive_verified", "metadata_verified")), "Daily backup incomplete")
    require(state.get("service") == backup.parent.parent.name and state.get("kind") == "daily",
            "Daily service identity mismatch")
    require(entry.get("name") == backup.name and entry.get("kind") == "daily"
            and entry.get("completed") is True and entry.get("success") is True,
            "Daily catalog identity mismatch")
    require(entry.get("created_at") == state.get("created_at"), "Daily timestamp mismatch")
    require(entry.get("proofs") == [proof(backup / "recovery-point.json")], "Daily proof path mismatch")
    for name, digest in entry.get("files", {}).items():
        require(NAME.fullmatch(name) and HASH.fullmatch(digest) and regular(backup / name),
                "Unsafe daily artifact")
    require(entry.get("files", {}).get("recovery-point.json") == complete["proof_sha256"],
            "Daily artifact proof differs")
    require(entry.get("files", {}).get("complete") == sha(backup / "complete"), "Daily completion changed")
    for name, digest in state.get("checksums", {}).items():
        require(entry["files"].get(name) == digest, "Daily checksum mismatch")
    require(set(state.get("checksums", {})) >= {"backup.tar.gz", "manifest.json"},
            "Daily checksums missing")
    require(load(backup / "checksums.json") == state["checksums"], "Daily checksum file differs")
    require(entry["files"].get("checksums.json") == sha(backup / "checksums.json"),
            "Daily checksum file hash differs")
    required = ["payload/service/.env", "payload/service/compose.yaml", "payload/service/current.json",
                "payload/database/postgres.dump"]
    if state["service"] == "gptgrok2api":
        required += ["payload/database/redis.rdb", "payload/database/postgres-globals.sql",
                     "payload/service/data/files", "payload/service/data/log_input_images"]
    else:
        required += ["payload/images", "payload/references"]
    require(entry.get("archives") == [{"path": "backup.tar.gz", "required_roots": ["payload"],
                                       "required_members": required}], "Daily archive scope differs")
    return entry


def make_record(service, backup, external, attested, prior):
    require(service == "gptgrok2api", "Only CFM backup records are allowed")
    record = {"name": backup.name, "kind": "release", "completed": False, "success": False,
              "protected_reasons": [], "proofs": [], "files": {}, "archives": []}
    if service == "gptgrok2api" and backup.name == "rolling-20261104T035328Z-7a99dcecaf6c":
        record.update(kind="migration", protected_reasons=["single-writer-bootstrap-checkpoint"])
    if backup.name.startswith("schema-") or "pre-migration" in backup.name:
        record.update(kind="migration", protected_reasons=["migration-recovery-point"])
        return record
    if (service, backup.name) in INTERRUPTED:
        record["protected_reasons"] = ["documented-interrupted-backup"]
        return record
    try:
        if backup.name.startswith("daily-"):
            return daily_record(backup)
        record["created_at"] = stamp(backup.name)
        proofs, source = [], None
        local = load(backup / "release.json", optional=True)
        if local is not None and success(local, service, backup):
            if local.get("retention_requires_final_verification") is True:
                record["_release_commit"] = local.get("commit", local.get("release"))
                record["_pending_final_verification"] = True
                ready = load(backup / "retention-ready.json")
                require(ready.get("status") == "verified" and ready.get("backup") == str(backup)
                        and ready.get("commit") == local.get("commit", local.get("release")), "Final release verification missing")
                proofs.append(proof(backup / "retention-ready.json"))
                record["_pending_final_verification"] = False
            proofs.append(proof(backup / "release.json")); source = local
        if not proofs and service == "gptgrok2api":
            for path, doc in external.get(str(backup), []):
                if success(doc, service, backup, adobe=True):
                    proofs.append(proof(path)); source = doc
                    break
        if not proofs:
            approved = attested_proof(attested, backup)
            if approved:
                proofs.append(approved)
                source = attested[1]
        if not proofs:
            record["protected_reasons"].append("no-confirmed-success-proof")
            return record
        checks = {}
        if service == "gptgrok2api":
            complete = load(backup / "complete")
            checks = load(backup / "checksums.json")
            required = {"snapshot.tar.gz", "postgres.dump", "postgres-globals.sql", "redis.rdb"}
            require(complete.get("protocol") == "cfm-single-writer-v1" and required <= checks.keys(),
                    "Unsupported CFM completion format")
            names = required | {"checksums.json", "complete"}
            archive = {"path": "snapshot.tar.gz", "required_roots": ["snapshot"],
                       "required_members": ["snapshot/.env", "snapshot/compose.yaml", "snapshot/current.json"]}
            if complete.get("metadata_verified") is True:
                require("snapshot-metadata.json" in checks, "CFM metadata checksum missing")
                names.add("snapshot-metadata.json"); archive["metadata_file"] = "snapshot-metadata.json"
        else:
            names = {"media.tar.gz", "postgres.dump", ".env", "compose.yaml", "current.json", "release.json"}
            archive = {"path": "media.tar.gz", "required_roots": ["media-0", "media-1"], "required_members": []}
        # 保留配置、摘要与原发布证据的哈希，但不把原始业务文件名写入目录清单。
        for name in ("previous.json", "baseline.json", "business-before.json", "compose.cfm.yaml",
                     "compose.sh", "Caddyfile", "release.json", "retention-ready.json"):
            if regular(backup / name):
                names.add(name)
        identities, hashes = {}, {}
        for name in sorted(names):
            hashes[name], identities[name] = artifact_hash(backup / name, checks.get(name), prior)
        if not (prior.get("_postgres_toc_verified") is True and prior.get("_content_hashes_verified") is True
                and prior.get("_file_signatures", {}).get("postgres.dump") == identities["postgres.dump"]
                and prior.get("files", {}).get("postgres.dump") == hashes["postgres.dump"]):
            verify_postgres_dump(service, backup / "postgres.dump")
        record.update(completed=True, success=True, proofs=proofs, files=hashes, archives=[archive],
                      _file_signatures=identities, _content_hashes_verified=True, _postgres_toc_verified=True,
                      _release_commit=source.get("commit", source.get("release")),
                      _previous_commit=source.get("previous_commit", source.get("previous")))
    except (CatalogError, ValueError, KeyError, OSError, TypeError, subprocess.SubprocessError):
        # 不输出可能含凭据的解析异常，未知/变化记录交由人工核对。
        record.update(completed=False, success=False, files={}, archives=[], proofs=[])
        record["protected_reasons"].append("unrecognized-incomplete-or-changed-evidence")
    return record


def build_catalog(base=Path("/opt"), attestation_path=None, previous=None, services=SERVICES):
    base = base.absolute()
    require(base.resolve() == base, "Base path contains a link")
    prior = previous or {}
    require(not prior or prior.get("format") == FORMAT, "Unknown previous catalog")
    approved = attestations(attestation_path)
    result = {"format": FORMAT, "generated_at": dt.datetime.now(TZ).isoformat(), "services": {}}
    require(services and len(set(services)) == len(services) and set(services) <= set(SERVICES),
            "Invalid service selection")
    for service in services:
        root = base / service
        backups = root / "backups"
        require(backups.is_dir() and backups.resolve() == backups, "Backup root missing or linked")
        guards = [root / name for name in ("current.json", "rolling-status.json", "previous.json",
                                           "compose.yaml", ".env") if regular(root / name)]
        guards += [p for p in root.glob("slot-*.json") if regular(p)]
        guards += [p for p in (root / "instances").glob("**/*.json") if regular(p)]
        protected = set()
        for path in guards:
            if path.suffix != ".json":
                continue
            try:
                doc = load(path)
            except (ValueError, CatalogError):
                raise CatalogError("Current runtime JSON cannot be parsed")
            # 只处理现行对象直接字段。递归历史字段是审计信息，不是永久保留链。
            for key in ("backup", "backup_dir", "backup_path"):
                value = doc.get(key)
                if isinstance(value, str) and Path(value).parent == backups:
                    protected.add(value)
        known = external_proofs(root) if service == "gptgrok2api" else {}
        old = {r["name"]: r for r in prior.get("services", {}).get(service, {}).get("backups", [])}
        records = []
        for child in sorted(backups.iterdir()):
            if child.is_dir() and child.resolve() == child and NAME.fullmatch(child.name):
                records.append(make_record(service, child, known, approved.get((service, child.name)),
                                           old.get(child.name, {})))
        current = load(root / "current.json", optional=True) or {}
        direct_commits = {current.get(key) for key in ("commit", "grok_browser_commit", "worker_commit")}
        latest = sorted((r for r in records if r.get("success")), key=lambda r:r["created_at"], reverse=True)
        for commit in direct_commits - {None}:
            matches = [r for r in latest if r.get("_release_commit") == commit]
            if matches:
                protected.add(str(backups / matches[0]["name"]))
                previous_commit = matches[0].get("_previous_commit")
                previous_matches = [r for r in latest if previous_commit and r.get("_release_commit") == previous_commit]
                if previous_matches:
                    protected.add(str(backups / previous_matches[0]["name"]))
        blocked_reason = None
        if any(r.get("_pending_final_verification") is True and r.get("_release_commit") == current.get("commit")
               for r in records):
            blocked_reason = "current-release-final-verification-pending"
            for record in records:
                record["success"] = False
                record["protected_reasons"].append(blocked_reason)
        result["services"][service] = {
            "root": str(backups), "locks": sorted(str(p) for p in root.glob("*.lock") if regular(p)),
            "guard_files": sorted({str(p) for p in guards}), "reference_mode": "explicit",
            "protected_paths": sorted(protected), "backups": records, "blocked_reason": blocked_reason}
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", type=Path, default=Path("/opt"))
    parser.add_argument("--service", choices=SERVICES, action="append",
                        help="只登记 CFM 项目")
    parser.add_argument("--attestations", type=Path)
    parser.add_argument("--previous", type=Path)
    parser.add_argument("--output", type=Path, help="私有输出文件；省略时打印脱敏目录清单")
    args = parser.parse_args()
    os.nice(15)
    previous = load(args.previous) if args.previous else None
    result = build_catalog(args.base, args.attestations, previous, args.service or SERVICES)
    text = json.dumps(result, ensure_ascii=False, indent=2) + "\n"
    if args.output:
        # 仅写本次输出，避免替换链接或已有私有证据。
        descriptor = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w") as stream:
            stream.write(text)
    else:
        sys.stdout.write(text)


if __name__ == "__main__":
    main()
