import importlib.util
import hashlib
import json
import os
from pathlib import Path
import tempfile
import time
import unittest

SPEC = importlib.util.spec_from_file_location("cache_fixture", Path(__file__).with_name("snapshot_cache_fixture.py"))
fixture = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(fixture)


class CacheFaultFixtureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        os.chmod(self.root, 0o700)
        (self.root / "artifacts").mkdir(mode=0o700)
        self.file = self.root / "artifacts/archive.tar"
        self.data = b"unique immutable snapshot bytes"
        self.file.write_bytes(self.data)
        self.file.chmod(0o600)
        self.sha = hashlib.sha256(self.data).hexdigest()
        self.op = {"operation_id":"op", "kind":"source", "phase":"finalized", "verified":True,
                   "snapshot_committed":True,"remote_verified":True,"cache_until_unix_ms":int(time.time()*1000)+60000,
                   "artifact":"archive.tar", "archive":{"snapshot_id":"snapshot","sha256":self.sha,"size_bytes":len(self.data)}}
        self.journal = self.root / "journal.json"
        self.save()

    def tearDown(self):
        self.temp.cleanup()

    def save(self):
        self.journal.write_text(json.dumps({"worker_id":"worker", "operations":{"op":self.op},
                                          "private_spec":{"env":{"TOKEN":"private-fixture"}}}))
        self.journal.chmod(0o600)

    def probe(self, action, worker="worker", snapshot="snapshot", sha=None):
        return fixture.probe(self.root,"op",worker,action,snapshot,sha or self.sha)

    def test_corruption_changes_only_one_byte_and_preserves_metadata_and_size(self):
        saved = self.journal.read_bytes()
        result = self.probe("corrupt")
        self.assertFalse(result["valid"])
        self.assertEqual(len(self.file.read_bytes()),len(self.data))
        self.assertEqual(sum(a!=b for a,b in zip(self.data,self.file.read_bytes())),1)
        self.assertEqual(self.journal.read_bytes(),saved)
        self.assertEqual(self.probe("source-corrupt")["sha256"],result["sha256"])
        self.assertNotIn("private-fixture",json.dumps(result))

    def test_missing_cache_and_valid_target_require_positive_evidence(self):
        saved = self.journal.read_bytes()
        self.assertFalse(self.probe("evict")["present"])
        self.assertFalse(self.probe("source-missing")["present"])
        self.assertEqual(self.journal.read_bytes(),saved)
        self.file.write_bytes(self.data); self.file.chmod(0o600)
        self.op.update(kind="target",phase="active"); self.save()
        self.assertTrue(self.probe("target-valid")["valid"])

    def test_uncommitted_unverified_expired_or_held_sources_cannot_be_mutated(self):
        original = self.op.copy()
        for change in ({"snapshot_committed":False},{"remote_verified":False},{"verified":False},
                       {"phase":"preparing"},{"phase":"frozen"},{"phase":"committed"},
                       {"kind":"target","phase":"active"},{"cache_until_unix_ms":0}):
            self.op = {**original,**change}; self.save()
            with self.assertRaises(AssertionError): self.probe("corrupt")
            self.assertEqual(self.file.read_bytes(),self.data)

    def test_wrong_identity_or_archive_refuses_mutation(self):
        for arguments in ({"worker":"foreign"},{"snapshot":"foreign"},{"sha":"0"*64}):
            with self.assertRaises(AssertionError): self.probe("corrupt",**arguments)
            self.assertEqual(self.file.read_bytes(),self.data)
        self.file.write_bytes(b"x"*len(self.data))
        with self.assertRaises(AssertionError): self.probe("corrupt")

    def test_links_and_path_escape_cannot_mutate_other_files(self):
        outside = self.root / "outside"
        outside.write_bytes(self.data); outside.chmod(0o600)
        self.file.unlink(); self.file.symlink_to(outside)
        with self.assertRaises(OSError): self.probe("corrupt")
        self.file.unlink(); os.link(outside,self.file)
        with self.assertRaises(AssertionError): self.probe("corrupt")
        self.op["artifact"] = "../outside"; self.save()
        with self.assertRaises(AssertionError): self.probe("corrupt")
        self.assertEqual(outside.read_bytes(),self.data)


if __name__ == "__main__":
    unittest.main()
