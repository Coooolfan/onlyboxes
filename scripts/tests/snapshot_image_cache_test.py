import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("image_cache", Path(__file__).with_name("snapshot_image_cache.py"))
cache = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(cache)
IMAGE = "python@sha256:" + "a"*64
MANIFEST = "sha256:" + "a"*64
CONFIG = "sha256:" + "b"*64
LAYER = "sha256:" + "c"*64


class CacheEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.home = Path(self.temp.name).resolve()
        (self.home/"db").mkdir()
        for name in ("layers", "manifests", "configs", "extracted", "disk-images"):
            (self.home/"images"/name).mkdir(parents=True)
        self.database = self.home/"db/boxlite.db"
        with sqlite3.connect(self.database) as db:
            db.executescript('CREATE TABLE schema_version(id INTEGER,version INTEGER); INSERT INTO schema_version VALUES(1,10); CREATE TABLE image_index(reference TEXT,manifest_digest TEXT,config_digest TEXT,layers TEXT,complete INTEGER);')

    def tearDown(self):
        self.temp.cleanup()

    def populate(self):
        with sqlite3.connect(self.database) as db:
            db.execute('INSERT INTO image_index VALUES(?,?,?,?,?)', (cache.canonical_reference(IMAGE), MANIFEST, CONFIG, json.dumps([LAYER]), 1))
        for directory,digest,suffix in (("manifests",MANIFEST,".json"),("configs",CONFIG,".json"),("layers",LAYER,".tar.gz")):
            (self.home/"images"/directory/(digest.replace(":","-")+suffix)).write_bytes(b"cached")

    def test_strict_cold_rejects_unindexed_files_and_unrelated_cached_rows(self):
        self.assertEqual(cache.cache_snapshot(self.home, IMAGE)["state"], "cold")
        blob = self.home/"images/layers/unindexed"
        blob.write_bytes(b"existing data")
        self.assertEqual(cache.cache_snapshot(self.home, IMAGE)["state"], "incomplete")
        blob.unlink()
        with sqlite3.connect(self.database) as db:
            db.execute('INSERT INTO image_index VALUES(?,?,?,?,?)', ('other',MANIFEST,CONFIG,'[]',0))
        self.assertEqual(cache.cache_snapshot(self.home, IMAGE)["state"], "incomplete")

    def test_warm_requires_the_exact_complete_reference_and_all_blob_files(self):
        self.populate()
        result = cache.cache_snapshot(self.home, IMAGE)
        self.assertEqual(result["state"], "warm")
        self.assertTrue(result["requested_complete_blobs_present"])
        self.assertEqual(result["requested_layer_count"], 1)
        self.assertEqual(cache.cache_snapshot(self.home, 'python@sha256:'+'d'*64)["state"], "incomplete")
        (self.home/"images/layers"/(LAYER.replace(":","-")+'.tar.gz')).unlink()
        self.assertEqual(cache.cache_snapshot(self.home, IMAGE)["state"], "incomplete")
        with sqlite3.connect(self.database) as db:
            db.execute('UPDATE image_index SET complete=0')
        self.assertEqual(cache.cache_snapshot(self.home, IMAGE)["state"], "incomplete")

    def test_probe_is_readonly_and_refuses_unsupported_schema_and_linked_blobs(self):
        self.populate()
        before = self.database.read_bytes()
        cache.cache_snapshot(self.home, IMAGE)
        self.assertEqual(self.database.read_bytes(), before)
        blob=self.home/"images/layers"/(LAYER.replace(":","-")+'.tar.gz')
        original=self.home/"original"; blob.rename(original); blob.symlink_to(original)
        with self.assertRaisesRegex(RuntimeError,"unsafe"):
            cache.cache_snapshot(self.home, IMAGE)
        self.assertEqual(original.read_bytes(),b"cached")
        with sqlite3.connect(self.database) as db:
            db.execute('UPDATE schema_version SET version=11')
        with self.assertRaisesRegex(RuntimeError,"unsupported"):
            cache.cache_snapshot(self.home, IMAGE)

    def test_digest_path_escape_is_rejected_before_filesystem_access(self):
        self.populate()
        with sqlite3.connect(self.database) as db:
            db.execute('UPDATE image_index SET layers=?',(json.dumps(['../../private']),))
        with self.assertRaisesRegex(RuntimeError,"invalid.*layers"):
            cache.cache_snapshot(self.home,IMAGE)
        for image in ('python:latest','python@sha256:../private'):
            with self.assertRaises(RuntimeError): cache.canonical_reference(image)

    def test_sdk_receipts_are_limited_to_the_restore_window_and_equivalent_refs(self):
        path=self.home/"worker.log"
        old=json.dumps({'fields':{'message':'Pulling image from registry: '+cache.canonical_reference(IMAGE)}})+'\n'
        path.write_text(old); path.chmod(0o600); offset=path.stat().st_size
        with path.open('a') as output:
            for message in ('Using cached image: '+cache.canonical_reference(IMAGE),'Pulling image from registry: unrelated','noise'):
                output.write(json.dumps({'fields':{'message':message}})+'\n')
        self.assertEqual(cache.sdk_cache_events(path,offset,{cache.canonical_reference(IMAGE)}),{'registry_pull':0,'cache_reuse':1})
        path.write_text('')
        with self.assertRaisesRegex(RuntimeError,'truncated'):
            cache.sdk_cache_events(path,offset,{cache.canonical_reference(IMAGE)})


if __name__ == '__main__':
    unittest.main()
