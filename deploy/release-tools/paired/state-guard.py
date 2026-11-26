#!/usr/bin/env python3
"""Read-only, strict state guard for a CFM release with no schema or data migration.

The caller must hold ingress, freeze the previous writer, keep the browser
worker idle, and keep the activated candidate paused. This script never migrates,
restores, starts/stops containers, or changes the source data. Snapshots contain
compact row/schema digests, not database row contents; keep them private regardless.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import stat
import subprocess
import sys
import time
from datetime import datetime, timezone
from decimal import Decimal

import snapshot_integrity as integrity


FORMAT = "cfm-state-unchanged-v2"
DATABASE_LIMIT = 128 * 1024 * 1024
DATABASE_TIMEOUT = 180
TABLES = (
    "mailbox_schema_migrations", "mailboxes", "mailbox_registration_blocks",
    "mailbox_claims", "mailbox_events", "mailbox_registration_retirements",
    "mailbox_sources",
)
CHECK_TABLES = ("mailbox_registration_blocks", "mailbox_registration_retirements")
RUNTIME_FILES = integrity.CFM_RUNTIME_FILES


class VerificationError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise VerificationError(message)


def canonical(value):
    # Type tags distinguish a PostgreSQL numeric from a text or JSON container,
    # and Decimal avoids silently rounding metadata numbers through float64.
    def tagged(item):
        if isinstance(item, dict):
            return ["object", [[key, tagged(item[key])] for key in sorted(item)]]
        if isinstance(item, list):
            return ["array", [tagged(part) for part in item]]
        if isinstance(item, Decimal):
            return ["decimal", str(item)]
        if item is None:
            return ["null"]
        if isinstance(item, bool):
            return ["boolean", item]
        if isinstance(item, int):
            return ["integer", str(item)]
        if isinstance(item, str):
            return ["string", item]
        raise VerificationError("unexpected database JSON value type")
    return json.dumps(tagged(value), sort_keys=True, ensure_ascii=False,
                      separators=(",", ":"), allow_nan=False).encode()


def digest(value):
    return hashlib.sha256(value).hexdigest()


def row_key(table, row):
    if table == "mailbox_schema_migrations":
        value = row["version"]
    elif table in CHECK_TABLES:
        value = [row["mailbox_id"], row["platform"]]
    else:
        value = row["id"]
    return digest(canonical(value))


def table_state(table, rows):
    """Keep exact typed row multiset coverage without persisting every row hash."""
    require(isinstance(rows, list), "unexpected database table rows")
    keys, hashes = set(), []
    for row in rows:
        require(isinstance(row, dict), "unexpected database row")
        key = row_key(table, row)
        require(key not in keys, "duplicate database row identity")
        keys.add(key)
        hashes.append(hashlib.sha256(canonical(row)).digest())
    result = hashlib.sha256(b"cfm-table-rows-v2\0")
    for value in sorted(hashes):
        result.update(value)
    return {"rows": len(hashes), "sha256": result.hexdigest()}


def database_state(tables, schema):
    require(set(tables) == set(TABLES), "unexpected database table set")
    return {"tables": {table: table_state(table, tables[table]) for table in TABLES},
            "schema_sha256": digest(schema.encode())}


def command(argv, payload=None):
    try:
        result = subprocess.run(argv, input=payload, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=DATABASE_TIMEOUT, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise VerificationError("database inspection command did not complete") from error
    # Database errors can contain values or connection details. Never echo them.
    require(result.returncode == 0, "database inspection command failed")
    require(len(result.stdout) <= DATABASE_LIMIT, "database snapshot exceeds reviewed size")
    return result.stdout


def database_lines(process, count, *, timeout=DATABASE_TIMEOUT, limit=DATABASE_LIMIT):
    """Read bounded psql records while its exported snapshot transaction stays open."""
    deadline = time.monotonic() + timeout
    pending, result, total = bytearray(), [], 0
    with selectors.DefaultSelector() as selector:
        selector.register(process.stdout, selectors.EVENT_READ)
        while len(result) < count:
            remaining = deadline - time.monotonic()
            require(remaining > 0, "database snapshot response deadline exceeded")
            require(selector.select(remaining), "database snapshot response deadline exceeded")
            block = os.read(process.stdout.fileno(), 64 * 1024)
            require(block, "database snapshot transaction ended before inspection")
            total += len(block)
            require(total <= limit, "database snapshot exceeds reviewed size")
            # Split only the newly read block; rescanning a growing JSON line
            # on every read becomes quadratic for large databases.
            pieces = block.split(b"\n")
            for piece in pieces[:-1]:
                pending.extend(piece)
                result.append(bytes(pending))
                pending.clear()
                require(len(result) <= count, "unexpected database inspection response")
            pending.extend(pieces[-1])
        require(not pending, "unexpected database inspection response")
    return result


def close_database_snapshot(process):
    """Release the exporter without draining unbounded output after a rejected response."""
    try:
        if process.stdin is not None:
            process.stdin.close()
        if process.stdout is not None:
            process.stdout.close()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
    except (OSError, subprocess.SubprocessError) as error:
        raise VerificationError("database snapshot exporter cleanup failed") from error


def inspect_database(container):
    require(re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", container), "invalid container name")
    # Rows need no SQL sort: canonical row digests are sorted once in Python.
    members = ",".join("'%s',(SELECT coalesce(jsonb_agg(to_jsonb(t)),'[]'::jsonb) FROM public.%s t)"
                       % (table, table) for table in TABLES)
    sql = ("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; "
           "SET LOCAL timezone='UTC'; SET LOCAL statement_timeout='180s'; "
           "SET LOCAL idle_in_transaction_session_timeout='240s'; "
           "SELECT pg_export_snapshot(); "
           "SELECT jsonb_build_object('tables',jsonb_build_object(" + members + "),"
           "'relations',(SELECT jsonb_agg(c.relname ORDER BY c.relname) FROM pg_class c "
           "JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' "
           "AND c.relkind IN ('r','p')));\n").encode()
    process = None
    try:
        process = subprocess.Popen(
            ["docker", "exec", "-i", container, "sh", "-c",
             'exec psql -XAtq -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            bufsize=0)
        process.stdin.write(sql)
        process.stdin.flush()
        snapshot_id, raw = database_lines(process, 2)
        require(re.fullmatch(rb"[0-9A-Fa-f]+-[0-9A-Fa-f]+-[0-9]+", snapshot_id)
                and len(snapshot_id) <= 120, "invalid database snapshot identifier")
        payload = json.loads(raw, parse_float=Decimal)
        require(set(payload["relations"]) == set(TABLES), "unexpected public business table set")
        require(process.poll() is None, "database snapshot exporter ended early")
        # pg_dump imports the exact snapshot used for all table data. The exporter
        # stays open until pg_dump returns; no second table scan is needed.
        raw_schema = command(["docker", "exec", container, "sh", "-c",
            'exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --schema-only '
            '--no-owner --no-privileges --snapshot="$1"', 'cfm-schema-snapshot', snapshot_id.decode()])
        require(process.poll() is None, "database snapshot exporter ended early")
        schema = "\n".join(line for line in raw_schema.decode().splitlines()
                           if not line.startswith(("\\restrict ", "\\unrestrict ")))
        return database_state(payload["tables"], schema)
    except (OSError, subprocess.SubprocessError) as error:
        raise VerificationError("database inspection command did not complete") from error
    finally:
        if process is not None:
            close_database_snapshot(process)


def data_projection(manifest, *, source=False):
    """Project the sealed content proof; timestamps only govern same-capture stability."""
    result = {}
    for original, row in manifest.items():
        if source:
            if original != "data" and not original.startswith("data/"):
                continue
            name = "." if original == "data" else original.removeprefix("data/")
        else:
            name = original
        if name in RUNTIME_FILES:
            require(row["type"] == "file", "invalid protocol state file type")
            continue
        result[name] = {key: value for key, value in row.items() if key != "mtime_ns"}
    require("." in result and result["."]["type"] == "directory", "data directory missing")
    return result


def omit_browser_metrics(name):
    # Rolling snapshots exclude Chromium's regenerable metrics directories.
    return name.startswith("browser-worker/") and name.endswith("/BrowserMetrics")


def business_capture(root, *, stats=None):
    """Hash each business file once through anchored descriptors, then stat for stability."""
    data = Path(root) / "data"
    try:
        with integrity._Tree(data) as tree:
            manifest, identities = {}, {}
            for name, descriptor, info in integrity._walk(
                    tree, False, None, stats, "business_capture", prune_directory=omit_browser_metrics):
                row = integrity._row(info)
                if name in RUNTIME_FILES:
                    require(row["type"] == "file", "invalid protocol state file type")
                    continue
                if row["type"] == "file":
                    count, value = integrity._hash_fd(descriptor, None, stats, "business_capture")
                    require(count == info.st_size, "business file size changed during inspection")
                    row["sha256"] = value
                manifest[name] = row
                identities[name] = integrity.identity(info)
        captured = integrity.Capture(manifest, identities)
        verify_business_identity(root, captured, stats=stats)
        return captured
    except integrity.IntegrityError as error:
        raise VerificationError("business tree changed or contains unsupported paths") from error


def verify_business_identity(root, captured, *, stats=None):
    require(isinstance(captured, integrity.Capture) and not captured.selected,
            "business identity proof is missing")
    current = {}
    try:
        with integrity._Tree(Path(root) / "data") as tree:
            for name, _, info in integrity._walk(
                    tree, False, None, stats, "business_identity", prune_directory=omit_browser_metrics):
                row = integrity._row(info)
                if name in RUNTIME_FILES:
                    require(row["type"] == "file", "invalid protocol state file type")
                    continue
                current[name] = integrity.identity(info)
        expected = captured.identities
        require(set(current) == set(expected),
                "data changed while snapshot was captured; keep maintenance held")
        # Creating or removing the top-level maintenance marker changes the
        # data directory's size and timestamps. Keep its inode, type, mode and
        # ownership pinned while allowing those runtime-only directory fields.
        root_expected, root_current = expected["."], current["."]
        require(root_expected[:5] == root_current[:5],
                "data root identity changed while snapshot was captured")
        for name in expected:
            if name != ".":
                require(current[name] == expected[name],
                        "data changed while snapshot was captured; keep maintenance held")
    except integrity.IntegrityError as error:
        raise VerificationError("business tree changed or contains unsupported paths") from error


def projected_capture(source_capture):
    require(isinstance(source_capture, integrity.Capture) and source_capture.selected,
            "frozen source proof is missing")
    manifest, identities = {}, {}
    for name, row in source_capture.manifest.items():
        if name == "data" or name.startswith("data/"):
            key = "." if name == "data" else name.removeprefix("data/")
            if key in RUNTIME_FILES:
                require(row["type"] == "file", "invalid protocol state file type")
                continue
            manifest[key] = row
            identities[key] = source_capture.identities[name]
    require("." in manifest, "frozen source proof has no data directory")
    return integrity.Capture(manifest, identities)


def file_state(root):
    return data_projection(business_capture(root).manifest)


def capture(root, container, *, source_capture=None):
    started, stats = time.monotonic(), {}
    reused = source_capture is not None
    files = projected_capture(source_capture) if reused else business_capture(root, stats=stats)
    database_started = time.monotonic()
    database = inspect_database(container)
    database_elapsed = time.monotonic() - database_started
    # The post-database identity pass covers the complete interval from the
    # frozen proof through the database snapshot. A second pass before the
    # query only repeated the same metadata walk when the content proof was
    # already reused.
    verify_business_identity(root, files, stats=stats)
    return {"format": FORMAT, "captured_at": datetime.now(timezone.utc).isoformat(),
            "files": data_projection(files.manifest), "database": database,
            "metrics": {"elapsed_seconds": round(time.monotonic() - started, 6),
                        "database_seconds": round(database_elapsed, 6),
                        "content_files_read": stats.get("content_files_read", 0),
                        "content_bytes_read": stats.get("content_bytes_read", 0),
                        "source_manifest_reused": reused, "database_scans": 1, "schema_dumps": 1}}


def business_files(snapshot):
    require(snapshot.get("format") == FORMAT, "unknown snapshot format")
    return {name: row["sha256"] for name, row in snapshot["files"].items() if row["type"] == "file"}


def summary(snapshot, *, status="captured"):
    require(snapshot.get("format") == FORMAT, "unknown snapshot format")
    return {"status": status,
            "tables": {table: item["rows"] for table, item in snapshot["database"]["tables"].items()},
            "files_and_directories": len(snapshot["files"]), "metrics": snapshot.get("metrics", {})}


def verify(before, after):
    require(before.get("format") == FORMAT and after.get("format") == FORMAT, "unknown snapshot format")
    require(before["files"] == after["files"],
            "business file, directory, permission or content changed; no migration is allowed")
    require(before["database"] == after["database"],
            "database rows or schema changed; no migration is allowed")
    return dict(summary(after, status="verified"), schema_unchanged=True, rows_unchanged=True,
                files_and_directories_unchanged=len(before["files"]))


def private_write(path, value):
    path = Path(path)
    require(not path.exists() and not path.is_symlink(), "output already exists; do not overwrite evidence")
    require(path.parent.is_dir() and not path.parent.is_symlink(), "output parent must be an existing private directory")
    require(stat.S_IMODE(path.parent.stat().st_mode) & 0o077 == 0, "output directory must be private (0700)")
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2).encode() + b"\n")
        stream.flush()
        os.fsync(stream.fileno())


def load_private(path):
    path = Path(path)
    info = path.lstat()
    require(stat.S_ISREG(info.st_mode) and not stat.S_IMODE(info.st_mode) & 0o077,
            "before snapshot must be a private regular file")
    return json.loads(path.read_bytes())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("snapshot", "verify"))
    parser.add_argument("--root", type=Path, required=True, help="CFM deployment root containing data/")
    parser.add_argument("--postgres-container", required=True)
    parser.add_argument("--output", type=Path, required=True, help="new 0600 JSON snapshot in a 0700 directory")
    parser.add_argument("--before", type=Path, help="required for verify")
    args = parser.parse_args()
    require((args.action == "verify") == (args.before is not None), "--before is only required for verify")
    require(args.root.is_dir() and not args.root.is_symlink(), "deployment root missing or linked")
    root = args.root.resolve()
    require(not args.output.resolve().is_relative_to(root / "data"), "snapshot output must be outside business data")
    before = load_private(args.before) if args.before else None
    if before is not None:
        require(before.get("format") == FORMAT, "unknown snapshot format")
    snapshot = capture(root, args.postgres_container)
    private_write(args.output, snapshot)
    if args.action == "snapshot":
        report = summary(snapshot)
    else:
        report = verify(before, snapshot)
    print(json.dumps(report, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (VerificationError, OSError, ValueError, KeyError, TypeError) as error:
        # Only our own messages are safe; JSON/parser/OS exceptions may reveal values.
        message = str(error) if isinstance(error, VerificationError) else "snapshot inspection failed"
        print(json.dumps({"status": "rejected", "reason": message}), file=sys.stderr)
        sys.exit(1)
