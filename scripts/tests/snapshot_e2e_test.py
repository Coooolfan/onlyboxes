import importlib.util
import os
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("snapshot_e2e",Path(__file__).with_name("snapshot_e2e.py"))
acceptance = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(acceptance)


class LargeFixtureAdmissionTests(unittest.TestCase):
    def test_headroom_refusal_precedes_authentication_or_session_creation(self):
        config = {"CONSOLE_SNAPSHOT_MAX_BYTES":str(2<<30),"CONSOLE_SNAPSHOT_MAX_EXPANDED_BYTES":str(3<<30),"WORKER_TERMINAL_EXEC_DOCKER_IMAGE":"python@sha256:"+"a"*64}
        for host_free, daemon_free in ((0,"999999999999 4096"),(1<<40,"0 4096")):
            with self.subTest(host_free=host_free), patch.dict(os.environ,{},clear=True), patch("sys.argv",["snapshot_e2e.py","--runtime","docker","--payload-mib","1024"]), patch.object(acceptance.dev,"env_file",return_value=config), patch.object(acceptance.os,"statvfs",return_value=SimpleNamespace(f_bavail=host_free,f_frsize=4096)), patch.object(acceptance.dev,"run",return_value=daemon_free), patch.object(acceptance.dev,"console_auth") as auth:
                with self.assertRaisesRegex(RuntimeError,"filesystem requires"):
                    acceptance.main()
                auth.assert_not_called()
    def test_long_fixture_lease_cannot_be_silently_clamped_by_worker(self):
        with patch.dict(os.environ, {}, clear=True), patch("sys.argv", ["snapshot_e2e.py", "--lease-ttl-sec", "7200", "--operation-wait-sec", "3600"]), patch.object(acceptance.dev, "env_file", return_value={"WORKER_TERMINAL_LEASE_MAX_SEC":"1800"}), patch.object(acceptance.dev, "console_auth") as auth:
            with self.assertRaisesRegex(RuntimeError, "exceeds configured Worker maximum"):
                acceptance.main()
            auth.assert_not_called()

    def test_quota_retry_accepts_only_the_same_payload_runtime_and_failed_suspend(self):
        import json
        evidence = {"runtime":"boxlite", "payload_bytes":5<<30, "session_id":"retained-session", "account_id":"owner", "sha256":"a"*64,
                    "checks":[{"check":"suspend", "status":"failed", "error":{"code":"snapshot_quota_exceeded"}}]}
        with patch.object(acceptance.dev, "read_private", return_value=json.dumps(evidence)):
            self.assertEqual(acceptance.quota_retry_evidence(Path("fixture"), "boxlite", 5<<30), evidence)
            for runtime, size in (("docker", 5<<30), ("boxlite", 1<<30)):
                with self.assertRaisesRegex(RuntimeError, "matching private evidence"):
                    acceptance.quota_retry_evidence(Path("fixture"), runtime, size)
        for last in ({"check":"resume", "status":"failed", "error":{"code":"snapshot_quota_exceeded"}},
                     {"check":"suspend", "status":"succeeded"},
                     {"check":"suspend", "status":"failed", "error":{"code":"snapshot_invalid"}}):
            with patch.object(acceptance.dev, "read_private", return_value=json.dumps({**evidence, "checks":[last]})):
                with self.assertRaisesRegex(RuntimeError, "matching private evidence"):
                    acceptance.quota_retry_evidence(Path("fixture"), "boxlite", 5<<30)

    def test_expiry_restart_refuses_a_clamped_or_long_lease_before_authentication(self):
        for scenario in ("worker-expired-restart", "initial-expired-delete"):
            for minimum, requested in ((60, 30), (1, 61), (1, 900)):
                with patch.dict(os.environ, {}, clear=True), patch("sys.argv", ["snapshot_e2e.py", "--scenario", scenario, "--lease-ttl-sec", str(requested)]), patch.object(acceptance.dev, "env_file", return_value={"WORKER_TERMINAL_LEASE_MIN_SEC":str(minimum)}), patch.object(acceptance.dev, "console_auth") as auth:
                    with self.assertRaisesRegex(RuntimeError, "unclamped lease"):
                        acceptance.main()
                    auth.assert_not_called()

    def test_explicit_archive_and_expanded_budgets_are_required(self):
        with patch.dict(os.environ,{},clear=True), patch("sys.argv",["snapshot_e2e.py","--payload-mib","1024"]), patch.object(acceptance.dev,"env_file",return_value={}), patch.object(acceptance.dev,"console_auth") as auth:
            with self.assertRaisesRegex(RuntimeError,"explicit archive/expanded limits"):
                acceptance.main()
            auth.assert_not_called()

    def test_cache_contention_refuses_disabled_cache_or_unobservable_logs_before_auth(self):
        for cache, log_format in (("0", "json"), (str(4<<30), "text")):
            config = {"CONSOLE_SNAPSHOT_MAX_BYTES":str(2<<30), "WORKER_PERSISTENCE_CACHE_MAX_BYTES":cache, "WORKER_LOG_FORMAT":log_format}
            with patch.dict(os.environ, {}, clear=True), patch("sys.argv", ["snapshot_e2e.py", "--scenario", "cache-copy-eviction", "--payload-mib", "1024"]), patch.object(acceptance.dev, "env_file", return_value=config), patch.object(Path, "is_file", return_value=True), patch.object(Path, "exists", return_value=False), patch.object(acceptance.os, "access", return_value=True), patch.object(acceptance.dev, "console_auth") as auth:
                with self.assertRaisesRegex(RuntimeError, "retained producer cache and JSON info logs"):
                    acceptance.main()
                auth.assert_not_called()

    def test_initial_expiry_requires_observable_logs_before_authentication(self):
        with patch.dict(os.environ, {}, clear=True), patch("sys.argv", ["snapshot_e2e.py", "--scenario", "initial-expired-delete", "--lease-ttl-sec", "30"]), patch.object(acceptance.dev, "env_file", return_value={"WORKER_LOG_FORMAT":"text"}), patch.object(Path, "exists", return_value=False), patch.object(acceptance.dev, "console_auth") as auth:
            with self.assertRaisesRegex(RuntimeError, "requires JSON info logs"):
                acceptance.main()
            auth.assert_not_called()


if __name__ == "__main__":
    unittest.main()
