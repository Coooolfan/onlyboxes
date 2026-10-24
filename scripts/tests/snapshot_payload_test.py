import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("payload_fixture",Path(__file__).with_name("snapshot_payload.py"))
fixture = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(fixture)


class PayloadTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)/"guest"
    def tearDown(self):
        self.temp.cleanup()
    def test_streams_bounded_chunks_and_verifies_every_byte_and_mode(self):
        sizes = []
        def random_bytes(size):
            sizes.append(size)
            return bytes([len(sizes)])*size
        total = 2*fixture.CHUNK+17
        with patch.object(fixture.os,"urandom",side_effect=random_bytes):
            digest = fixture.payload(self.root,"seed",total)
        self.assertEqual(sizes,[fixture.CHUNK,fixture.CHUNK,17])
        self.assertEqual(digest,hashlib.sha256((self.root/"payload").read_bytes()).hexdigest())
        self.assertEqual(fixture.payload(self.root,"verify",total),digest)
        with (self.root/"payload").open("r+b") as stream:
            stream.seek(fixture.CHUNK+7); stream.write(b"x")
        self.assertNotEqual(fixture.payload(self.root,"verify",total),digest)
        (self.root/"payload").chmod(0o600)
        with self.assertRaises(AssertionError): fixture.payload(self.root,"verify",total)
    def test_truncation_wrong_link_and_mutation_checks(self):
        digest = fixture.payload(self.root,"seed",31)
        (self.root/"remove-me").unlink()
        (self.root/"new-file").write_bytes(b"created after restore")
        (self.root/"modified-file").write_text("second version")
        self.assertEqual(fixture.payload(self.root,"verify",31,True),digest)
        with self.assertRaises(AssertionError): fixture.payload(self.root,"verify",31)
        (self.root/"link").unlink(); (self.root/"link").symlink_to("modified-file")
        with self.assertRaises(AssertionError): fixture.payload(self.root,"verify",31,True)
        (self.root/"link").unlink(); (self.root/"link").symlink_to("payload")
        (self.root/"payload").write_bytes(b"short")
        with self.assertRaises(AssertionError): fixture.payload(self.root,"verify",31,True)


if __name__ == "__main__":
    unittest.main()
