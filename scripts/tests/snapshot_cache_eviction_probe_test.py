"""Native production-transfer probe checks; build its release example first."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import select
import subprocess
import tempfile
import unittest

BINARY = Path(__file__).resolve().parents[2] / "worker/worker-boxlite/target/release/examples/snapshot_cache_eviction_probe"


@unittest.skipUnless(BINARY.is_file(), "build release snapshot_cache_eviction_probe first")
class EvictionProbeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="onlyboxes-cache-probe-")
        self.root = Path(self.temp.name).resolve()
        self.source = self.root / "source.boxlite"
        self.source.write_bytes(b"archive")
        self.source.chmod(0o600)
        self.lock = self.root / "source.boxlite.lock"
        self.lock.touch(mode=0o600)
        self.request = {"source":str(self.source), "wait_seconds":1, "archive":{
            "snapshot_id":"probe-test", "size_bytes":7, "expanded_bytes":4096,
            "format":"boxlite-archive-v1", "sha256":hashlib.sha256(b"archive").hexdigest()}}

    def tearDown(self):
        self.temp.cleanup()

    def test_live_cross_process_pin_defers_production_eviction_then_reclaims(self):
        with subprocess.Popen([str(BINARY)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) as child:
            try:
                self.request["wait_seconds"] = 10
                child.stdin.write(json.dumps(self.request) + "\n"); child.stdin.flush()
                self.assertTrue(select.select([child.stdout], [], [], 10)[0])
                self.assertEqual(json.loads(child.stdout.readline()), {"event":"armed"})
                inode = self.lock.stat().st_ino
                with self.lock.open("r+b") as pin:
                    fcntl.flock(pin, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    self.assertTrue(select.select([child.stdout], [], [], 10)[0])
                    self.assertEqual(json.loads(child.stdout.readline()), {"event":"busy_eviction_deferred"})
                    self.assertEqual(self.source.read_bytes(), b"archive")
                output, error = child.communicate("evict\n", timeout=10)
                self.assertEqual(child.returncode, 0)
                self.assertEqual(error, "")
                self.assertEqual(json.loads(output), {"event":"evicted_after_copy"})
                self.assertFalse(self.source.exists())
                self.assertEqual(self.lock.stat().st_ino, inode)
            finally:
                if child.poll() is None:
                    child.kill(); child.wait()

    def test_no_observed_writer_fails_without_deletion(self):
        result = subprocess.run([str(BINARY)], input=json.dumps(self.request) + "\n", capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(result.stdout), {"event":"armed"})
        self.assertEqual(self.source.read_bytes(), b"archive")

    def test_already_busy_source_cannot_be_claimed_as_a_restore_race(self):
        with self.lock.open("r+b") as pin:
            fcntl.flock(pin, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = subprocess.run([str(BINARY)], input=json.dumps(self.request) + "\n", capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertEqual(self.source.read_bytes(), b"archive")

    def test_alias_or_public_source_refuses_before_arming(self):
        other = self.root / "private-data"
        other.write_bytes(b"archive"); other.chmod(0o600)
        for kind in ("symlink", "hardlink", "public"):
            with self.subTest(kind=kind):
                self.source.unlink()
                if kind == "symlink":
                    self.source.symlink_to(other)
                elif kind == "hardlink":
                    os.link(other, self.source)
                else:
                    self.source.write_bytes(b"archive"); self.source.chmod(0o644)
                result = subprocess.run([str(BINARY)], input=json.dumps(self.request) + "\n", capture_output=True, text=True, timeout=10)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertNotIn(str(self.root), result.stderr)
                self.assertEqual(other.read_bytes(), b"archive")


if __name__ == "__main__":
    unittest.main()
