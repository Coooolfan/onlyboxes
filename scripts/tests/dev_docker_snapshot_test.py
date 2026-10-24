import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("dev_docker_snapshot", Path(__file__).resolve().parents[1] / "dev-docker-snapshot.py")
adapter = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(adapter)


class DockerNodeVolumeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="onlyboxes-docker-dev-test-")
        self.root = Path(self.temp.name)
        self.values = {"WORKER_ID": "worker-a"}
        self.service = "worker-docker-snapshot"
        self.volume = {"Name": "onlyboxes-snapshot-node-" + adapter.dev.NAMESPACE + "-worker-a", "CreatedAt": "2026-10-24T00:00:00Z", "Mountpoint": "/var/lib/docker/volumes/node/_data", "Driver": "local", "Options": {}, "Labels": {"onlyboxes.dev.namespace": adapter.dev.NAMESPACE, "onlyboxes.dev.service": self.service, "onlyboxes.worker_id": "worker-a"}}

    def tearDown(self):
        self.temp.cleanup()

    def test_existing_volume_is_pinned_and_replacement_or_missing_volume_is_rejected(self):
        with patch.object(adapter, "inspect", return_value=self.volume), patch.object(adapter.dev, "run") as run:
            first = adapter.node_volume(self.service, self.values, self.root)
            self.assertEqual(first, (self.volume["Name"], self.volume["Mountpoint"]))
            self.assertEqual(adapter.node_volume(self.service, self.values, self.root), first)
            run.assert_not_called()
        saved = (self.root / "docker-volume.json").read_bytes()
        for changed in (None, {**self.volume, "CreatedAt": "2026-10-25T00:00:00Z"}, {**self.volume, "Labels": {}}, {**self.volume, "Labels": None}, {**self.volume, "Options": {"device": "/untrusted"}}):
            with patch.object(adapter, "inspect", return_value=changed), patch.object(adapter.dev, "run") as run:
                with self.assertRaises(RuntimeError):
                    adapter.node_volume(self.service, self.values, self.root)
                run.assert_not_called()
                self.assertEqual((self.root / "docker-volume.json").read_bytes(), saved)

    def test_lost_create_reply_can_pin_only_matching_owned_volume(self):
        with patch.object(adapter, "inspect", side_effect=[None, self.volume]), patch.object(adapter.dev, "run") as run:
            adapter.node_volume(self.service, self.values, self.root)
            self.assertEqual(run.call_count, 1)
            self.assertEqual(run.call_args.args[0][:3], ["docker", "volume", "create"])
        saved = json.loads((self.root / "docker-volume.json").read_text())
        self.assertEqual(saved["CreatedAt"], self.volume["CreatedAt"])

    def test_linux_uses_native_worker_and_excludes_console_storage_credentials(self):
        settings = {"WORKER_TERMINAL_EXEC_DOCKER_IMAGE": "python@sha256:" + "a"*64, "CONSOLE_SNAPSHOT_STORE_SK": "fixture-private", "CONSOLE_INITIAL_ADMIN_API_KEY": "fixture-private", "CONSOLE_GRPC_ADDR": "127.0.0.1:15054", "PATH": os.environ.get("PATH", "")}
        with patch.dict(os.environ, settings, clear=True), patch.object(adapter.sys, "platform", "linux"), patch.object(adapter, "build", return_value=(self.root/"worker-docker", "sha256:"+"b"*64)), patch.object(adapter.dev, "provision", return_value={**self.values, "WORKER_SECRET":"fixture-only"}), patch.object(adapter.dev, "node_paths", return_value=(self.root,self.root/"control")), patch.object(adapter, "node_volume") as volume, patch.object(adapter.dev, "supervise_worker", return_value=0) as supervise:
            self.assertEqual(adapter.worker(self.service), 0)
        volume.assert_not_called()
        command, environment, _, _ = supervise.call_args.args
        self.assertEqual(command, [str(self.root/"worker-docker")])
        self.assertEqual(environment["WORKER_CONSOLE_GRPC_TARGET"], "127.0.0.1:15054")
        self.assertEqual(environment["WORKER_SNAPSHOT_TMP_DIR"], str(self.root/"journal"))
        self.assertNotIn("CONSOLE_INITIAL_ADMIN_API_KEY", environment)
        self.assertNotIn("CONSOLE_SNAPSHOT_STORE_SK", environment)


if __name__ == "__main__":
    unittest.main()
