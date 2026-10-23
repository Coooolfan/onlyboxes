import contextlib
import importlib.util
import io
import http.client
import json
import os
from pathlib import Path
import stat
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("dev_sandbox", Path(__file__).resolve().parents[1] / "dev-sandbox.py")
dev = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(dev)


class AdapterTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="onlyboxes-dev-test-")
        self.state = Path(self.temp.name)
        self.bind = patch.object(dev, "STATE", self.state)
        self.bind.start()

    def tearDown(self):
        self.bind.stop()
        self.temp.cleanup()

    def test_private_env_roundtrip_does_not_follow_links_or_expand_shell_values(self):
        path = self.state / "worker-boxlite.env"
        values = {"WORKER_SECRET": "space ' $(touch should-not-exist) `echo no`", "WORKER_ID": "worker-a"}
        dev.write_env(path, values)
        self.assertEqual(dev.env_file(path), values)
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        target = self.state / "untouched"
        target.write_text("preserved")
        path.unlink()
        path.symlink_to(target)
        with self.assertRaises(RuntimeError):
            dev.write_env(path, values)
        self.assertEqual(target.read_text(), "preserved")

    def test_workers_do_not_inherit_console_or_long_term_storage_credentials(self):
        with patch.dict(os.environ, {"CONSOLE_INITIAL_ADMIN_API_KEY": "private", "CONSOLE_SNAPSHOT_STORE_SK": "private", "MINIO_ROOT_PASSWORD": "private", "WORKER_PERSISTENCE_ENABLED": "true"}):
            result = dev.worker_environment()
        self.assertNotIn("CONSOLE_INITIAL_ADMIN_API_KEY", result)
        self.assertNotIn("CONSOLE_SNAPSHOT_STORE_SK", result)
        self.assertNotIn("MINIO_ROOT_PASSWORD", result)
        self.assertEqual(result["WORKER_PERSISTENCE_ENABLED"], "true")

    def test_store_readiness_retries_closed_http_connections(self):
        class Ready:
            status = 200
            def __enter__(self): return self
            def __exit__(self, *_args): pass
        with patch.object(dev.urllib.request, "urlopen", side_effect=[http.client.RemoteDisconnected(), Ready()]) as request, patch.object(dev.time, "sleep"):
            dev.wait_store({"CONSOLE_SNAPSHOT_STORE_ENDPOINT": "http://127.0.0.1:19000"})
        self.assertEqual(request.call_count, 2)

    def test_valid_worker_reuses_credentials_and_database_rebuild_retains_old_home(self):
        workers = []
        created = []

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def reply(self, value):
                data = json.dumps(value).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self):
                if self.path == "/api/v1/auth/session":
                    self.reply({"role": "admin"})
                else:
                    self.reply({"items": list(workers)})

            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                item = {"node_id": "worker-" + str(len(created) + 1), "worker_secret": "fixture-only"}
                created.append(item)
                workers.append(item)
                self.reply(item)

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with patch.dict(os.environ, {"CONSOLE_HTTP_ADDR": "127.0.0.1:" + str(server.server_port), "CONSOLE_INITIAL_ADMIN_API_KEY": "fixture-only"}), contextlib.redirect_stdout(io.StringIO()):
                first = dev.provision("worker-boxlite")
                root, _control = dev.node_paths("worker-boxlite", first)
                (root / "original-data").write_text("preserved")
                replay = dev.provision("worker-boxlite")
                self.assertEqual(replay, first)
                self.assertEqual(len(created), 1)
                workers.clear()
                replacement = dev.provision("worker-boxlite")
                self.assertNotEqual(replacement["WORKER_ID"], first["WORKER_ID"])
                self.assertEqual((root / "original-data").read_text(), "preserved")
                self.assertEqual(stat.S_IMODE((self.state / "worker-boxlite.env").stat().st_mode), 0o600)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
