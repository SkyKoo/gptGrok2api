"""恢复点登记的安全边界：成功证明、未知对象、引用范围与缓存失效。"""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("catalog", Path(__file__).with_name("backup_catalog.py"))
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)


class CatalogTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.base = Path(self.temp.name).resolve()
        self.pg = mock.patch.object(c, "verify_postgres_dump")
        self.pg.start()
        for name in c.SERVICES:
            (self.base / name / "backups").mkdir(parents=True)
            (self.base / name / "maintenance.lock").touch()

    def tearDown(self):
        self.pg.stop()
        self.temp.cleanup()

    def write(self, path, data):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(data))

    def cfm(self, name="rolling-20261112T010000Z-example", external=False):
        p = self.base / "gptgrok2api/backups" / name
        p.mkdir()
        names = ("snapshot.tar.gz", "postgres.dump", "postgres-globals.sql", "redis.rdb")
        for n in names:
            (p / n).write_bytes(b"fixture-" + n.encode())
        self.write(p / "checksums.json", {n:c.sha(p/n) for n in names})
        self.write(p / "complete", {"protocol":"cfm-single-writer-v1"})
        doc = {"backup":str(p), "status":"success", "phase":"success", "commit":"a"*40,
               "final_backup_verified":True}
        proof = (self.base / "gptgrok2api/operations/test/release-state.json") if external else p / "release.json"
        self.write(proof, doc)
        return p, proof

    def test_successful_cfm_and_manifest_hash_registration(self):
        p, proof = self.cfm()
        result = c.build_catalog(self.base)
        record = result["services"]["gptgrok2api"]["backups"][0]
        self.assertTrue(record["success"])
        self.assertEqual(record["proofs"], [c.proof(proof)])
        self.assertEqual(record["files"]["snapshot.tar.gz"], c.sha(p / "snapshot.tar.gz"))

    def test_adobe_requires_final_backup_verified(self):
        p, proof = self.cfm("adobe-test-20261112T010000Z", external=True)
        self.assertTrue(c.build_catalog(self.base)["services"]["gptgrok2api"]["backups"][0]["success"])
        doc = c.load(proof);doc["final_backup_verified"] = False;self.write(proof, doc)
        self.assertFalse(c.build_catalog(self.base)["services"]["gptgrok2api"]["backups"][0]["success"])

    def test_current_reference_does_not_inherit_history(self):
        p, proof = self.cfm()
        ancient = str(p.parent / "old-20200101T000000Z")
        self.write(self.base / "gptgrok2api/current.json", {"backup":str(p), "history":{"backup":ancient}})
        service = c.build_catalog(self.base)["services"]["gptgrok2api"]
        self.assertEqual(service["protected_paths"], [str(p)])
        self.assertEqual(service["reference_mode"], "explicit")


    def test_bad_attestation_binding_cannot_register_backup(self):
        p, proof = self.cfm();proof.unlink()
        attestation=self.base / "attestations.json"
        self.write(attestation, {"format":"ogg-backup-attestations-v1","backups":[{
            "service":"gptgrok2api","name":p.name,"success":True,
            "source_evidence_sha256":"a"*64,"bindings":{"checksums.json":"b"*64}}]})
        record=c.build_catalog(self.base,attestation)["services"]["gptgrok2api"]["backups"][0]
        self.assertFalse(record["success"])

    def test_attestation_bound_to_exact_backup_manifest(self):
        p, proof = self.cfm();proof.unlink()
        attestation=self.base / "attestations.json"
        self.write(attestation, {"format":"ogg-backup-attestations-v1","backups":[{
            "service":"gptgrok2api","name":p.name,"success":True,
            "source_evidence_sha256":"a"*64,"bindings":{"checksums.json":c.sha(p / "checksums.json")}}]})
        record=c.build_catalog(self.base,attestation)["services"]["gptgrok2api"]["backups"][0]
        self.assertTrue(record["success"])

    def test_artifact_cache_invalidates_on_content_change(self):
        p=self.base / "example";p.write_bytes(b"one")
        h, sig = c.artifact_hash(p,None,{})
        previous={"files":{"example":h},"_file_signatures":{"example":sig}}
        p.write_bytes(b"two")
        new, _ = c.artifact_hash(p,None,previous)
        self.assertNotEqual(new,h)

    def test_symlink_proof_is_not_followed(self):
        p, proof = self.cfm();saved=self.base / "saved.json"
        proof.rename(saved);proof.symlink_to(saved)
        record=c.build_catalog(self.base)["services"]["gptgrok2api"]["backups"][0]
        self.assertFalse(record["success"])

    def test_changed_artifact_cannot_borrow_recorded_checksum(self):
        p, proof = self.cfm()
        (p / "snapshot.tar.gz").write_bytes(b"unexpected replacement")
        record=c.build_catalog(self.base)["services"]["gptgrok2api"]["backups"][0]
        self.assertFalse(record["success"])

    def test_postgres_toc_failure_protects_backup(self):
        p, proof = self.cfm()
        with mock.patch.object(c, "verify_postgres_dump", side_effect=c.CatalogError("invalid dump")):
            record=c.build_catalog(self.base)["services"]["gptgrok2api"]["backups"][0]
        self.assertFalse(record["success"])


    def test_single_service_never_reads_other_service_root(self):
        self.cfm()
        result=c.build_catalog(self.base,services=["gptgrok2api"])
        self.assertEqual(set(result["services"]), {"gptgrok2api"})

    def test_duplicate_service_selection_rejected(self):
        with self.assertRaises(c.CatalogError):
            c.build_catalog(self.base,services=["gptgrok2api","gptgrok2api"])


    def test_current_release_blocks_retention_until_final_marker(self):
        p, proof = self.cfm()
        doc=c.load(proof);doc['retention_requires_final_verification']=True;self.write(proof,doc)
        self.write(self.base/'gptgrok2api/current.json',{'commit':'a'*40,'backup':str(p)})
        service=c.build_catalog(self.base)['services']['gptgrok2api']
        self.assertEqual(service['blocked_reason'],'current-release-final-verification-pending')
        self.assertFalse(service['backups'][0]['success'])
        self.write(p/'retention-ready.json',{'status':'verified','commit':'a'*40,'backup':str(p)})
        service=c.build_catalog(self.base)['services']['gptgrok2api']
        self.assertIsNone(service['blocked_reason'])
        self.assertTrue(service['backups'][0]['success'])

    def test_bootstrap_recovery_point_is_explicitly_pinned(self):
        self.cfm('rolling-20261104T035328Z-7a99dcecaf6c')
        record=c.build_catalog(self.base)['services']['gptgrok2api']['backups'][0]
        self.assertEqual(record['kind'],'migration')
        self.assertIn('single-writer-bootstrap-checkpoint',record['protected_reasons'])

    def test_other_service_is_rejected(self):
        with self.assertRaises(c.CatalogError):
            c.build_catalog(self.base,services=['lumora'])


if __name__ == "__main__":
    unittest.main()
