import importlib.util
import json
import os
import socket
import tempfile
import threading
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

    def test_retained_volume_lookup_never_creates_or_rebinds_storage(self):
        with patch.object(adapter, "inspect", return_value=self.volume), patch.object(adapter.dev, "run") as run:
            with self.assertRaises(RuntimeError):
                adapter.retained_volume(self.service, self.values, self.root)
            adapter.dev.atomic_private(self.root/"docker-volume.json", json.dumps({key:self.volume[key] for key in ("Name","CreatedAt","Mountpoint","Driver")}))
            self.assertEqual(adapter.retained_volume(self.service, self.values, self.root)["Name"], self.volume["Name"])
            original = (self.root/"docker-volume.json").read_bytes()
            for changed in (None, {**self.volume, "CreatedAt":"replacement"}, {**self.volume, "Labels":{}}, {**self.volume, "Options":{"device":"foreign"}}):
                with patch.object(adapter, "inspect", return_value=changed):
                    with self.assertRaises(RuntimeError):
                        adapter.retained_volume(self.service, self.values, self.root)
            self.assertEqual((self.root/"docker-volume.json").read_bytes(), original)
            run.assert_not_called()

    def test_native_journal_evidence_never_outputs_environment_or_guard_token(self):
        worker = self.values["WORKER_ID"]
        root = self.root/self.service/adapter.hashlib.sha256(worker.encode()).hexdigest()[:32]
        adapter.dev.private_dir(root/"journal")
        state = {"worker_id":worker,"operations":{"op":{"operation_id":"op","kind":"source","phase":"preparing", "container_id":"a"*64,"restore_spec":{"env":{"SECRET":"must-not-appear"}}}},
                 "lease_guard":{"helper_id":"b"*64,"state_id":"c"*32,"token":"private-token-must-not-appear"}}
        adapter.dev.atomic_private(root/"journal/journal.json", json.dumps(state))
        adapter.dev.private_dir(root/"journal/artifacts")
        prefix="snapshot-"+adapter.hashlib.sha256(b"op").hexdigest()+"-"
        (root/"journal/artifacts"/(prefix+"interrupted.tar")).write_text("private-root-bytes")
        with patch.object(adapter.dev, "STATE", self.root), patch.object(adapter.sys, "platform", "linux"):
            evidence = adapter.snapshot_evidence(self.service, self.values, "op")
        self.assertEqual(evidence["operation"]["box_id"], "a"*64)
        self.assertEqual(evidence["operation"]["known_artifact_count"], 1)
        self.assertEqual(len(evidence["guard"]["binding_sha256"]), 64)
        encoded = json.dumps(evidence)
        self.assertNotIn("must-not-appear", encoded)
        self.assertNotIn("private-token", encoded)


class DockerWorkerCrashTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="obx-dc-", dir="/tmp")
        self.root = Path(self.temp.name)
        self.service, self.values = "worker-docker-snapshot", {"WORKER_ID":"fixture-worker"}
        self.binding = {"Name":"owned-volume", "Mountpoint":"/native/owned", "Driver":"local", "CreatedAt":"fixed"}
        self.worker = {"Id":"a"*64, "Name":"/onlyboxes-snapshot-worker-"+adapter.dev.NAMESPACE+"-fixture-worker", "Path":"/worker-docker",
                       "State":{"Running":True}, "HostConfig":{"AutoRemove":True},
                       "Config":{"Labels":{"onlyboxes.dev.namespace":adapter.dev.NAMESPACE,"onlyboxes.dev.service":self.service,"onlyboxes.worker_id":"fixture-worker"},"Env":["WORKER_ID=fixture-worker"]},
                       "Mounts":[{"Type":"volume","Name":"owned-volume","Destination":"/native/owned"}]}

    def tearDown(self):
        self.temp.cleanup()

    def test_wrong_role_or_scope_cannot_kill_a_container(self):
        for changed in (None, {**self.worker,"Path":"/runtime-lease-guard"}, {**self.worker,"Config":{"Labels":{},"Env":[]}}, {**self.worker,"Mounts":[]}, {**self.worker,"Id":"name-not-id"}):
            with self.subTest(changed=changed is None), patch.object(adapter.sys, "platform", "darwin"), patch.object(adapter.dev, "env_file", return_value=self.values), patch.object(adapter, "retained_volume", return_value=self.binding), patch.object(adapter,"inspect",return_value=changed), patch.object(adapter.dev,"run") as kill:
                with self.assertRaises(RuntimeError):
                    adapter.crash(self.service)
                kill.assert_not_called()

    def test_container_kill_waits_for_the_owned_cli_exit_receipt(self):
        for returncode in (137,0):
            endpoint = self.root/"worker.sock"
            endpoint.unlink(missing_ok=True)
            server = socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
            server.bind(str(endpoint)); os.chmod(endpoint,0o600); server.listen(1); server.settimeout(3)
            waiting, killed = threading.Event(), threading.Event()
            failures = []
            def supervisor():
                try:
                    with server.accept()[0] as client:
                        client.settimeout(3)
                        self.assertEqual(client.recv(16),b"wait\n")
                        waiting.set()
                        client.sendall(b"waiting\n")
                        self.assertTrue(killed.wait(3))
                        client.sendall(json.dumps({"pid":123,"returncode":returncode}).encode()+b"\n")
                except BaseException as error:
                    failures.append(error)
            thread = threading.Thread(target=supervisor)
            thread.start()
            def kill(command):
                self.assertTrue(waiting.wait(3))
                self.assertEqual(command,["docker","kill","--signal","KILL","a"*64])
                killed.set()
                return "a"*64
            try:
                with patch.object(adapter.sys,"platform","darwin"), patch.object(adapter.dev,"env_file",return_value=self.values), patch.object(adapter,"retained_volume",return_value=self.binding), patch.object(adapter,"inspect",return_value=self.worker), patch.object(adapter.dev,"node_paths",return_value=(self.root,self.root)), patch.object(adapter.dev,"run",side_effect=kill), patch("sys.stdout"):
                    if returncode==137:
                        adapter.crash(self.service)
                    else:
                        with self.assertRaises(RuntimeError):
                            adapter.crash(self.service)
            finally:
                thread.join(timeout=4)
                server.close()
            self.assertFalse(thread.is_alive())
            if failures:
                raise failures[0]


if __name__ == "__main__":
    unittest.main()
