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

    def test_explicit_archive_and_expanded_budgets_are_required(self):
        with patch.dict(os.environ,{},clear=True), patch("sys.argv",["snapshot_e2e.py","--payload-mib","1024"]), patch.object(acceptance.dev,"env_file",return_value={}), patch.object(acceptance.dev,"console_auth") as auth:
            with self.assertRaisesRegex(RuntimeError,"explicit archive/expanded limits"):
                acceptance.main()
            auth.assert_not_called()


if __name__ == "__main__":
    unittest.main()
