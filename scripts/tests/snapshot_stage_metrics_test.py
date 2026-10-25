import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("stage_metrics", Path(__file__).with_name("snapshot_stage_metrics.py"))
metrics = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(metrics)


def receipt(message, attempt="attempt", **values):
    value = {"message":message, "operation_id":"op", "attempt_id":attempt, "stage":"export", "worker":"worker"}
    if message == metrics.FINISH:
        value.update(elapsed_us=123, succeeded=True)
    return {**value, **values}


class StageMetricTests(unittest.TestCase):
    def test_profile_lock_serializes_distinct_labels_without_replacing_the_inode(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"profile.lock"
            first = metrics.profile_lock(path)
            try:
                inode = path.stat().st_ino
                with self.assertRaises(BlockingIOError):
                    metrics.profile_lock(path)
            finally:
                first.close()
            with metrics.profile_lock(path):
                self.assertEqual(path.stat().st_ino, inode)
                self.assertEqual(path.stat().st_size, 0)

    def test_failed_or_repeated_api_attempts_cannot_be_hidden_by_the_final_success(self):
        checks = [{"check":"suspend", "status":"succeeded"}, {"check":"resume", "status":"succeeded"}]
        self.assertEqual(set(metrics.lifecycle_checks({"checks":checks})), {"suspend", "resume"})
        for previous in ({"check":"resume", "status":"failed"}, {"check":"suspend", "status":"succeeded"}):
            with self.assertRaisesRegex(RuntimeError, "one successful client lifecycle"):
                metrics.lifecycle_checks({"checks":[previous, *checks]})

    def test_success_requires_paired_receipts_for_the_exact_worker_and_operation(self):
        events = [receipt(metrics.START), receipt(metrics.FINISH), receipt(metrics.START, "unrelated", operation_id="other")]
        value = metrics.summarize(events, "worker", "op", {"export"})
        self.assertEqual(value["export"]["attempts"], 1)
        for worker, operation in (("other", "op"), ("worker", "other")):
            with self.assertRaises(RuntimeError):
                metrics.summarize(events, worker, operation, {"export"})
        for events in ([receipt(metrics.START)], [receipt(metrics.FINISH)],
                       [receipt(metrics.START), receipt(metrics.FINISH, stage="import")]):
            with self.assertRaisesRegex(RuntimeError, "matching start/finish"):
                metrics.summarize(events, "worker", "op", set())

    def test_retries_remain_separate_attempts_and_failed_only_stage_cannot_pass(self):
        events = [receipt(metrics.START), receipt(metrics.FINISH, succeeded=False),
                  receipt(metrics.START, "retry"), receipt(metrics.FINISH, "retry", elapsed_us=456)]
        value = metrics.summarize(events, "worker", "op", {"export"})["export"]
        self.assertEqual((value["attempts"], value["succeeded"], value["failed"], value["elapsed_us_sum"]), (2, 1, 1, 579))
        with self.assertRaisesRegex(RuntimeError, "required successful"):
            metrics.summarize(events[:2], "worker", "op", {"export"})

    def test_log_reader_omits_private_fields_and_refuses_invalid_completions(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"worker.log"
            fields = receipt(metrics.FINISH)
            fields["url"] = "private-grant"
            path.write_text('noise\n[]\n' + json.dumps({"fields":fields}) + '\n'); path.chmod(0o600)
            self.assertNotIn("private-grant", json.dumps(list(metrics.stage_events(path))))
            for bad in ({"elapsed_us":-1}, {"elapsed_us":True}, {"succeeded":"true"}, {"stage":"unknown"}):
                path.write_text(json.dumps({"fields":{**fields, **bad}}))
                with self.assertRaises(RuntimeError):
                    list(metrics.stage_events(path))
            path.chmod(0o644)
            with self.assertRaisesRegex(RuntimeError, "private owned"):
                list(metrics.stage_events(path))

    def test_disk_accounting_deduplicates_aliases_without_following_outside_links(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)/"owned"; root.mkdir()
            outside = Path(directory)/"outside"; outside.write_bytes(b"x"*100000)
            data = root/"data"; data.write_bytes(b"guest")
            os.link(data, root/"alias")
            link = root/"external"; link.symlink_to(outside)
            value = metrics.disk_accounting({"owned":root})
            self.assertFalse(value["raced_with_filesystem_change"])
            self.assertEqual(value["roots"]["owned"]["inodes"], 3)
            self.assertEqual(value["total_logical_bytes"], root.lstat().st_size+data.lstat().st_size+link.lstat().st_size)
            self.assertEqual(value["total_allocated_bytes"], sum(path.lstat().st_blocks*512 for path in (root,data,link)))

    def test_large_private_metrics_can_update_but_links_and_public_files_cannot(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"metrics.json"
            metrics.write_metrics(path, {"samples":"x"*100000})
            metrics.write_metrics(path, {"verified":True})
            self.assertEqual(json.loads(path.read_text()), {"verified":True})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            alias = Path(directory)/"alias"; os.link(path, alias)
            with self.assertRaisesRegex(RuntimeError, "unsafe"):
                metrics.write_metrics(path, {"changed":True})
            alias.unlink(); path.chmod(0o644)
            with self.assertRaisesRegex(RuntimeError, "unsafe"):
                metrics.write_metrics(path, {"changed":True})

    def test_monitor_keeps_receipts_across_worker_log_replacement(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(metrics.dev, "STATE", Path(directory)):
            path = Path(directory)/"service.log"
            path.write_text('\n'.join(json.dumps({"fields":receipt(message)}) for message in (metrics.START,metrics.FINISH))); path.chmod(0o600)
            monitor = metrics.Monitor({"worker":"service"}, {})
            monitor.capture(False)
            path.write_text("new Worker startup\n")
            monitor.capture(False)
            value = metrics.summarize(monitor.events.values(), "worker", "op", {"export"})
            self.assertEqual(value["export"]["elapsed_us_sum"], 123)

    def test_existing_evidence_prevents_child_session_creation(self):
        config = {"ONLYBOXES_DEV_BOXLITE_PROFILE":"release"}
        with patch.dict(os.environ, {"ONLYBOXES_DEV_ENV":"fixture"}, clear=True), patch("sys.argv", ["snapshot_stage_metrics.py"]), patch.object(metrics.dev, "env_file", return_value=config), patch.object(Path, "exists", return_value=True), patch.object(metrics.subprocess, "run") as child:
            with self.assertRaisesRegex(RuntimeError, "evidence already exists"):
                metrics.main()
            child.assert_not_called()

    def test_native_audit_requires_exact_owner_archive_and_independent_removed_disks(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(metrics.dev, "STATE", Path(directory)):
            root = Path(directory)/"service"/metrics.hashlib.sha256(b"worker").hexdigest()[:32]
            (root/"journal").mkdir(parents=True)
            path = root/"journal/journal.json"
            archive = {"snapshot_id":"snapshot", "sha256":"a"*64}
            source = {"kind":"source", "phase":"finalized", "snapshot_committed":True, "remote_verified":True, "archive":archive, "box_id":"original", "scoped_session_id":"obx:owner:session"}
            target = {"kind":"target", "phase":"active", "archive":archive, "box_id":"restored", "scoped_session_id":"obx:owner:session"}
            state = {"worker_id":"worker", "operations":{"suspend":source, "resume":target}}
            def save():
                path.write_text(json.dumps(state)); path.chmod(0o600)
            save()
            result = {"account_id":"owner", "session_id":"session"}
            checks = {"suspend":{"operation_id":"suspend"}, "resume":{"operation_id":"resume"}}
            def audit():
                return metrics.audit_native_lifecycle(result, {"worker":"service"}, checks, "worker", "worker")
            self.assertTrue(audit()["independent_native_instances_and_disks_removed"])
            for key, value in (("box_id","original"), ("scoped_session_id","obx:foreign:session"), ("archive",{"snapshot_id":"old"})):
                original = target[key]; target[key] = value; save()
                with self.assertRaises(RuntimeError): audit()
                target[key] = original
            save()
            disk = root/"sdk-home/boxes/restored/disks/disk.qcow2"
            disk.parent.mkdir(parents=True); disk.write_bytes(b"unique data")
            with self.assertRaisesRegex(RuntimeError, "disk remains"):
                audit()
            self.assertEqual(disk.read_bytes(), b"unique data")


if __name__ == "__main__":
    unittest.main()
