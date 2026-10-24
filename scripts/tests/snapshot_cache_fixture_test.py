import importlib.util
import hashlib
import fcntl
import json
import os
from pathlib import Path
import select
import shutil
import subprocess
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

    def boxlite(self):
        self.op.update(scoped_session_id="obx:owner:session", generation=1)
        identity = json.dumps([self.op["scoped_session_id"],"op",1],separators=(",", ":")).encode()
        self.op["artifact"] = hashlib.sha256(identity).hexdigest()+".boxlite"
        path = self.root/"artifacts"/self.op["artifact"]
        self.file.rename(path); self.file = path; self.save()

    def test_boxlite_cache_lock_is_retained_and_busy_cache_is_not_modified(self):
        self.boxlite()
        lock_path = Path(str(self.file)+".lock")
        lock_path.touch(mode=0o600)
        inode = lock_path.stat().st_ino
        with lock_path.open("r+b") as lock:
            fcntl.flock(lock,fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                fixture.probe(self.root,"op","worker","corrupt","snapshot",self.sha,"boxlite")
            self.assertEqual(self.file.read_bytes(),self.data)
        result = fixture.probe(self.root,"op","worker","corrupt","snapshot",self.sha,"boxlite")
        self.assertFalse(result["valid"])
        self.assertEqual(lock_path.stat().st_ino,inode)
        self.assertEqual(lock_path.read_bytes(),b"")

    def test_boxlite_rejects_noncanonical_archive_and_invalid_lock(self):
        self.boxlite()
        self.op["generation"] = 2; self.save()
        with self.assertRaises(AssertionError):
            fixture.probe(self.root,"op","worker","evict","snapshot",self.sha,"boxlite")
        self.op["generation"] = 1; self.save()
        lock_path = Path(str(self.file)+".lock")
        lock_path.write_bytes(b"foreign-state"); lock_path.chmod(0o600)
        with self.assertRaises(AssertionError):
            fixture.probe(self.root,"op","worker","evict","snapshot",self.sha,"boxlite")
        self.assertEqual(self.file.read_bytes(),self.data)
        self.assertEqual(lock_path.read_bytes(),b"foreign-state")

    @unittest.skipUnless(shutil.which("rustc"), "Rust compiler required for cross-process cache lock interoperability")
    def test_rust_owned_lock_blocks_python_fault_until_release(self):
        self.boxlite()
        lock_path = Path(str(self.file)+".lock")
        lock_path.touch(mode=0o600)
        inode = lock_path.stat().st_ino
        source = self.root/"lock.rs"
        source.write_text('''use std::{fs::OpenOptions,io::{self,Read,Write}};
fn main(){
    let path=std::env::args().nth(1).unwrap();
    let file=OpenOptions::new().read(true).write(true).open(path).unwrap();
    file.try_lock().unwrap();
    println!("ready"); io::stdout().flush().unwrap();
    let mut byte=[0]; io::stdin().read_exact(&mut byte).unwrap();
    drop(file);
}
''')
        binary = self.root/"lock"
        subprocess.run(["rustc",str(source),"-o",str(binary)],check=True,capture_output=True,timeout=30)
        child = subprocess.Popen([str(binary),str(lock_path)],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        try:
            self.assertTrue(select.select([child.stdout],[],[],5)[0],"Rust lock holder did not become ready")
            self.assertEqual(child.stdout.readline(),b"ready\n")
            with self.assertRaises(BlockingIOError):
                fixture.probe(self.root,"op","worker","corrupt","snapshot",self.sha,"boxlite")
            self.assertEqual(self.file.read_bytes(),self.data)
        finally:
            child.communicate(input=b"x",timeout=10)
        self.assertEqual(child.returncode,0)
        result = fixture.probe(self.root,"op","worker","evict","snapshot",self.sha,"boxlite")
        self.assertFalse(result["present"])
        self.assertEqual(lock_path.stat().st_ino,inode)
        self.assertEqual(lock_path.read_bytes(),b"")


if __name__ == "__main__":
    unittest.main()
