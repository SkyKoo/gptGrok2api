"""Retention behavior and destructive-operation refusal boundaries."""
import copy
import datetime as dt
import fcntl
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("backup_retention", Path(__file__).with_name("backup_retention.py"))
engine = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(engine)
AT = dt.datetime(2026, 11, 13, 20, tzinfo=engine.TZ)


class RetentionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.root = self.base / "gptgrok2api/backups"
        self.root.mkdir(parents=True)
        (self.root.parent / "maintenance.lock").touch()
        self.proofs = self.root.parent / "proofs"
        self.proofs.mkdir()
        self.patch = mock.patch.object(engine, "ROOTS", {"gptgrok2api": self.root})
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.catalog = {"format": "ogg-backup-catalog-v1", "generated_at": AT.isoformat(), "services": {
            "gptgrok2api": {"root": str(self.root), "locks": [str(self.root.parent / "maintenance.lock")],
                           "guard_files": [], "protected_paths": [], "backups": []}}}

    @property
    def service(self):
        return self.catalog["services"]["gptgrok2api"]

    def add(self, name, when, kind="release", success=True, pin=None):
        root = self.root / name
        root.mkdir()
        archive = root / "snapshot.tar.gz"
        with tarfile.open(archive, "w:gz") as output:
            for member in ("snapshot/.env", "snapshot/data/test.json", "snapshot/compose.yaml"):
                payload = b"fixture"
                item = tarfile.TarInfo(member)
                item.size, item.uid, item.gid, item.mode = len(payload), 1000, 1000, 0o600
                output.addfile(item, io.BytesIO(payload))
        (root / "release.json").write_text(json.dumps({"status": "success", "fixture": name}))
        (root / "snapshot").mkdir()
        (root / "snapshot/data.bin").write_bytes(b"raw duplicate fixture")
        proof = self.proofs / (name + ".json")
        proof.write_text(json.dumps({"status": "success", "restore_verified": True}))
        record = {"name": name, "created_at": when.isoformat(), "kind": kind,
                  "completed": True, "success": success, "protected_reasons": [pin] if pin else [],
                  "proofs": [{"path": str(proof), "sha256": engine.sha(proof)}],
                  "files": {"snapshot.tar.gz": engine.sha(archive)},
                  "archives": [{"path": "snapshot.tar.gz", "required_roots": ["snapshot"],
                                "required_members": ["snapshot/.env", "snapshot/compose.yaml"]}]}
        self.service["backups"].append(record)
        return record

    def plan(self, at=AT):
        return engine.build_plan(self.catalog, at=at, runtime_snapshot=[])

    def rows(self, plan):
        return {r["name"]: r for r in plan["services"]["gptgrok2api"]["rows"]}

    def four(self):
        for index in range(4):
            self.add("release-" + str(index), AT - dt.timedelta(days=10, hours=index))

    def run_execute(self, plan, audit=None, at=AT):
        with mock.patch.object(engine, "runtime", return_value=[]), mock.patch.object(engine, "ensure_unused"):
            return engine.execute(self.catalog, plan, audit or self.base / "audit.jsonl", at=at)

    def test_latest_releases_and_daily_are_union(self):
        for index in range(5):
            self.add("today-" + str(index), AT - dt.timedelta(minutes=index))
        self.add("yesterday", AT - dt.timedelta(days=1), kind="daily")
        rows = self.rows(self.plan())
        self.assertEqual({n for n, r in rows.items() if r["action"] == "keep"},
                         {"today-0", "today-1", "today-2", "yesterday"})

    def test_seven_calendar_days_in_shanghai_not_168_hours(self):
        for index in range(3):
            self.add("recent-" + str(index), AT - dt.timedelta(minutes=index))
        self.add("boundary-kept", dt.datetime(2026, 11, 6, 16, tzinfo=dt.timezone.utc))
        self.add("boundary-old", dt.datetime(2026, 11, 6, 15, 59, tzinfo=dt.timezone.utc))
        rows = self.rows(self.plan())
        self.assertEqual(rows["boundary-kept"]["action"], "keep")
        self.assertEqual(rows["boundary-old"]["action"], "delete")

    def test_sorting_compares_absolute_time_across_timezones(self):
        for index in range(4):
            when = AT - dt.timedelta(days=10, hours=index)
            self.add("release-" + str(index), when.astimezone(dt.timezone.utc) if index == 0 else when)
        rows = self.rows(self.plan())
        self.assertEqual(rows["release-0"]["action"], "keep")
        self.assertEqual(rows["release-3"]["action"], "delete")

    def test_unknown_incomplete_migration_and_unique_are_retained(self):
        self.four()
        unknown = self.root / "unknown"
        unknown.mkdir()
        self.add("incomplete", AT - dt.timedelta(days=20), success=False)
        self.add("schema-checkpoint", AT - dt.timedelta(days=20), kind="migration")
        self.add("sole-recovery", AT - dt.timedelta(days=20), pin="unique-recovery-point")
        rows = self.rows(self.plan())
        for name in ("unknown", "incomplete", "schema-checkpoint", "sole-recovery"):
            self.assertEqual(rows[name]["action"], "keep")
        self.assertEqual(rows["release-3"]["action"], "delete")

    def test_fewer_than_three_points_never_deletes(self):
        self.add("old", AT - dt.timedelta(days=40))
        self.add("older", AT - dt.timedelta(days=41))
        self.assertTrue(all(r["action"] == "keep" for r in self.rows(self.plan()).values()))

    def test_current_reference_and_container_mount_protect(self):
        self.four()
        config = self.root.parent / "current.json"
        config.write_text(json.dumps({"backup": str(self.root / "release-3")}))
        self.service["guard_files"] = [str(config)]
        rows = self.rows(self.plan())
        self.assertIn("current-reference", rows["release-3"]["reasons"])
        self.service["guard_files"] = []
        live = [{"mounts": [{"Source": str(self.root / "release-3/snapshot")}]}]
        rows = self.rows(engine.build_plan(self.catalog, AT, live))
        self.assertIn("container-mount", rows["release-3"]["reasons"])

    def test_explicit_references_do_not_inherit_historical_report_refs(self):
        self.four()
        config = self.root.parent / "current.json"
        config.write_text(json.dumps({"old_audit": {"backup": str(self.root / "release-3")}}))
        self.service.update(guard_files=[str(config)], reference_mode="explicit", protected_paths=[])
        self.assertEqual(self.rows(self.plan())["release-3"]["action"], "delete")

    def test_symlink_inside_backup_and_symlink_to_backup_protect(self):
        self.four()
        (self.root / "release-3/symlink").symlink_to(self.base)
        self.assertEqual(self.rows(self.plan())["release-3"]["action"], "keep")
        (self.root / "release-3/symlink").unlink()
        (self.root / "selected").symlink_to(self.root / "release-3")
        self.assertIn("symlink-target", self.rows(self.plan())["release-3"]["reasons"])

    def test_proof_mutation_refuses_plan(self):
        self.four()
        (self.proofs / "release-3.json").write_text("changed")
        with self.assertRaisesRegex(engine.Refusal, "proof changed"):
            self.plan()

    def test_plan_expiry_and_day_rollover_refuse_before_audit(self):
        self.four()
        plan = self.plan()
        with self.assertRaisesRegex(engine.Refusal, "expired"):
            self.run_execute(plan, at=AT + dt.timedelta(hours=2))
        earlier = AT.replace(hour=23, minute=50)
        plan = self.plan(earlier)
        with self.assertRaisesRegex(engine.Refusal, "calendar-day"):
            self.run_execute(plan, at=earlier + dt.timedelta(minutes=20))
        self.assertFalse((self.base / "audit.jsonl").exists())

    def test_tree_drift_refuses_before_deleting(self):
        self.four()
        plan = self.plan()
        (self.root / "release-3/snapshot/new-file").write_text("changed")
        with self.assertRaisesRegex(engine.Refusal, "state changed"):
            self.run_execute(plan)
        self.assertTrue((self.root / "release-3").exists())

    def test_corrupt_retained_archive_blocks_all_deletions(self):
        self.four()
        # Freeze a new plan after the corruption, so only full validation catches it.
        (self.root / "release-0/snapshot.tar.gz").write_bytes(b"not gzip")
        plan = self.plan()
        with self.assertRaisesRegex(engine.Refusal, "checksum mismatch"):
            self.run_execute(plan)
        self.assertTrue((self.root / "release-3").exists())
        self.assertIn('"event":"failed"', (self.base / "audit.jsonl").read_text())

    def test_corrupt_candidate_archive_blocks_deletion(self):
        self.four()
        (self.root / "release-3/snapshot.tar.gz").write_bytes(b"not gzip")
        plan = self.plan()
        with self.assertRaisesRegex(engine.Refusal, "checksum mismatch"):
            self.run_execute(plan)
        self.assertTrue((self.root / "release-3").exists())

    def test_execution_preserves_audit_and_retained_raw(self):
        self.four()
        plan = self.plan()
        result = self.run_execute(plan)
        self.assertEqual(result["deleted"], [str(self.root / "release-3")])
        self.assertFalse((self.root / "release-3").exists())
        self.assertTrue((self.root / "release-0/snapshot/data.bin").exists())
        saved = self.base / "audit.jsonl.evidence/gptgrok2api/release-3/release.json"
        self.assertEqual(json.loads(saved.read_text())["fixture"], "release-3")
        self.assertEqual(saved.stat().st_mode & 0o777, 0o600)

    def test_archive_traversal_and_symlink_refused(self):
        record = self.add("bad-archive", AT)
        path = self.root / "bad-archive/snapshot.tar.gz"
        for member_name, typecode in (("../outside", tarfile.REGTYPE), ("snapshot/link", tarfile.SYMTYPE)):
            with tarfile.open(path, "w:gz") as output:
                member = tarfile.TarInfo(member_name)
                member.type = typecode
                output.addfile(member)
            with self.assertRaises(engine.Refusal):
                engine.verify_archive(path.parent, record["archives"][0])

    def test_archive_metadata_permission_mismatch_refused(self):
        record = self.add("metadata", AT)
        path = self.root / "metadata"
        manifest = {".env": {"type": "file", "mode": 0o777}}
        (path / "snapshot-metadata.json").write_text(json.dumps(manifest))
        record["archives"][0]["metadata_file"] = "snapshot-metadata.json"
        with self.assertRaisesRegex(engine.Refusal, "permissions differ"):
            engine.verify_archive(path, record["archives"][0])

    def test_busy_lock_refuses(self):
        with (self.root.parent / "maintenance.lock").open("r+") as handle:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(engine.Refusal, "lock busy"):
                with engine.acquire_locks(self.catalog):
                    self.fail("Lock was bypassed")

    def test_changed_catalog_refuses(self):
        self.four()
        plan = self.plan()
        self.service["protected_paths"] = [str(self.root / "release-3")]
        with self.assertRaisesRegex(engine.Refusal, "Catalog changed"):
            self.run_execute(plan)


if __name__ == "__main__":
    unittest.main()
