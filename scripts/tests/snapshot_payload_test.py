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

    def seed_small(self, count=257):
        with patch.object(fixture.os, "sync") as sync:
            digest = fixture.payload(self.root, "seed", 31, small_files=count)
            sync.assert_called_once()
        return digest

    def test_small_file_data_metadata_links_and_manifest_are_verified(self):
        digest = self.seed_small()
        self.assertEqual(fixture.payload(self.root, "verify", 31, small_files=257), digest)
        self.assertNotEqual(hashlib.sha256((self.root/"payload").read_bytes()).hexdigest(), digest)
        file = self.root/"small-files/0001/00000100"
        file.write_bytes(b"x"*fixture.SMALL_BYTES)
        with self.assertRaises(AssertionError):
            fixture.payload(self.root, "verify", 31, small_files=257)

    def test_small_file_links_and_extra_entries_cannot_be_replaced_silently(self):
        self.seed_small(1)
        shard = self.root/"small-files/0000"
        alias = shard/"hardlink"
        original = (shard/"00000000").read_bytes()
        alias.unlink(); alias.write_bytes(original)
        with self.assertRaises(AssertionError): fixture.payload(self.root, "verify", 31, small_files=1)
        alias.unlink(); fixture.os.link(shard/"00000000", alias)
        (shard/"extra").write_bytes(b"unexpected")
        with self.assertRaises(AssertionError): fixture.payload(self.root, "verify", 31, small_files=1)
        (shard/"extra").unlink(); (shard/"shortcut").unlink(); (shard/"shortcut").symlink_to("other")
        with self.assertRaises(AssertionError): fixture.payload(self.root, "verify", 31, small_files=1)

    def test_self_consistent_manifest_tampering_changes_the_external_fingerprint(self):
        digest = self.seed_small(1)
        file = self.root/"small-files/0000/00000000"
        data = b"x"*fixture.SMALL_BYTES; file.write_bytes(data)
        manifest = self.root/"small-files.manifest"
        with manifest.open("r+b") as stream:
            stream.seek(16)
            stream.write(fixture.SMALL_RECORD.pack(0, 0o600, hashlib.sha256(data).digest()))
        self.assertNotEqual(fixture.payload(self.root,"verify",31,small_files=1),digest)

    def test_guest_inode_refusal_precedes_payload_creation(self):
        from types import SimpleNamespace
        for count, free in ((1,1), (65536,65536+256+8)):
            with patch.object(fixture.os,"statvfs",return_value=SimpleNamespace(f_favail=free)):
                with self.assertRaisesRegex(AssertionError,"guest inodes"):
                    fixture.payload(self.root,"seed",31,small_files=count)
        self.assertFalse(self.root.exists())

    def test_guest_disk_refusal_precedes_payload_creation(self):
        from types import SimpleNamespace
        with patch.object(fixture.os,"statvfs",return_value=SimpleNamespace(f_favail=10000,f_frsize=4096,f_bavail=1)):
            with self.assertRaisesRegex(AssertionError,"guest space"):
                fixture.payload(self.root,"seed",31,small_files=1)
        self.assertFalse(self.root.exists())


if __name__ == "__main__":
    unittest.main()
