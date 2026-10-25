import importlib.util
import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("parallel_acceptance", Path(__file__).with_name("snapshot_parallel_e2e.py"))
parallel = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(parallel)


class ParallelAdmissionTests(unittest.TestCase):
    def test_concurrent_space_reservation_precedes_authentication_or_source_creation(self):
        config = {"CONSOLE_SNAPSHOT_MAX_BYTES":str(2<<30), "CONSOLE_SNAPSHOT_MAX_EXPANDED_BYTES":str(21<<28),
                  "ONLYBOXES_DEV_BOXLITE_PROFILE":"release", "WORKER_PERSISTENCE_CACHE_MAX_BYTES":"0",
                  "WORKER_TERMINAL_MAX_ACTIVE_SESSIONS":"4", "WORKER_PERSISTENCE_MAX_INFLIGHT":"4", "WORKER_TERMINAL_EXEC_MAX_INFLIGHT":"2", "WORKER_TERMINAL_EXEC_DOCKER_IMAGE":"python@sha256:"+"a"*64}
        for host, daemon in ((0, "999999999999 4096"), (1<<40, "0 4096")):
            with tempfile.TemporaryDirectory() as root, patch.dict(os.environ, {"ONLYBOXES_DEV_ENV":"fixture"}, clear=True), patch("sys.argv", ["snapshot_parallel_e2e.py"]), patch.object(parallel.dev, "STATE", Path(root)), patch.object(parallel.dev, "env_file", return_value=config), patch.object(parallel.os, "statvfs", return_value=SimpleNamespace(f_bavail=host, f_frsize=4096)), patch.object(parallel.dev, "run", return_value=daemon), patch.object(parallel.dev, "console_auth") as auth:
                with self.assertRaisesRegex(RuntimeError, "filesystem requires"):
                    parallel.main()
                auth.assert_not_called()

    def test_overlap_requires_simultaneously_open_operation_intervals(self):
        sequential = [{"started":i, "finished":i+1} for i in range(4)]
        concurrent = [{"started":i/10, "finished":1+i/10} for i in range(4)]
        self.assertEqual(parallel.peak_overlap(sequential), 1)
        self.assertEqual(parallel.peak_overlap(concurrent), 4)

    def test_post_suspend_barrier_requires_a_new_heartbeat_from_each_online_worker(self):
        workers = [{"node_id":"a", "status":"online", "last_seen_at":"2026-10-25T00:00:02Z"},
                   {"node_id":"b", "status":"online", "last_seen_at":"2026-10-25T00:00:01Z"}]
        threshold = parallel.datetime.datetime.fromisoformat("2026-10-25T00:00:01+00:00").timestamp()
        self.assertFalse(parallel.fresh_heartbeats(workers, ["a", "b"], threshold))
        workers[1]["last_seen_at"] = "2026-10-25T00:00:02Z"
        self.assertTrue(parallel.fresh_heartbeats(workers, ["a", "b"], threshold))
        workers[1]["status"] = "offline"
        self.assertFalse(parallel.fresh_heartbeats(workers, ["a", "b"], threshold))
        workers[1]["last_seen_at"] = "invalid"
        self.assertFalse(parallel.fresh_heartbeats(workers, ["a", "b"], threshold))

    def test_native_journal_supports_deduplicated_history_and_rejects_unsafe_inputs(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root)/"journal.json"
            data = {"history":"a" * 70000}
            path.write_text(json.dumps(data))
            path.chmod(0o600)
            self.assertEqual(parallel.read_journal(path), data)
            link = Path(root)/"alias"
            link.symlink_to(path)
            with self.assertRaises(OSError):
                parallel.read_journal(link)
            path.chmod(0o644)
            with self.assertRaisesRegex(RuntimeError, "ownership or size"):
                parallel.read_journal(path)
            path.chmod(0o600)
            with path.open("r+b") as stream:
                stream.truncate((64<<20)+1)
            with self.assertRaisesRegex(RuntimeError, "ownership or size"):
                parallel.read_journal(path)


if __name__ == "__main__":
    unittest.main()
