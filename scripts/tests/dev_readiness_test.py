import contextlib
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import shlex
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import uuid
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location("dev_readiness", Path(__file__).resolve().parents[1] / "dev-sandbox.py")
dev = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(dev)


class GuardReadinessTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="obx-ready-", dir="/tmp")
        self.control = Path(self.temp.name)
        self.worker = "fixture-worker"
        self.store = str(uuid.uuid4())
        self.identity = {"worker_id": self.worker, "store_id": self.store, "epoch_id": str(uuid.uuid4()),
                         "process": {"pid": os.getpid(), "host_id": str(uuid.uuid4()),
                                     "boot_id": str(uuid.uuid4()), "process_namespace": "host", "start_time": 1}}

    def tearDown(self):
        self.temp.cleanup()

    def exchange(self, transform=lambda value: value, raw=None):
        endpoint = self.control / "control.sock"
        endpoint.unlink(missing_ok=True)
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(str(endpoint))
        os.chmod(endpoint, 0o600)
        server.listen(1)
        server.settimeout(2)
        failures = []
        def reply():
            try:
                with server.accept()[0] as client:
                    client.settimeout(2)
                    with client.makefile("rb") as stream:
                        size = int.from_bytes(stream.read(4), "big")
                        request = json.loads(stream.read(size))
                    self.assertEqual(request["command"], {"action": "ping"})
                    self.assertIsNone(request["expected"])
                    value = {"version": 2, "request_id": request["request_id"],
                             "identity": copy.deepcopy(self.identity), "reply": {"type": "ready"}}
                    data = raw if raw is not None else json.dumps(transform(value)).encode()
                    client.sendall(len(data).to_bytes(4, "big") + data)
            except OSError as error:
                if raw is None or len(raw) <= 16384:
                    failures.append(error)
            except BaseException as error:
                failures.append(error)
        thread = threading.Thread(target=reply)
        thread.start()
        try:
            result = dev.guard_identity(self.control, self.worker, self.store, os.getpid())
        finally:
            thread.join(timeout=3)
            server.close()
        self.assertFalse(thread.is_alive())
        if failures:
            raise failures[0]
        return result

    def test_actual_ping_confirms_nonce_worker_store_epoch_and_owned_child(self):
        self.assertEqual(self.exchange(), self.identity)
        for field, value in [("request_id", "old"), ("version", 1), ("version", 2.0), ("reply", {"type": "rejected"})]:
            with self.subTest(field=field, value=value):
                self.assertIsNone(self.exchange(lambda reply: {**reply, field: value}))
        for field, value in [("worker_id", "other"), ("store_id", str(uuid.uuid4())), ("epoch_id", "bad")]:
            def change(reply):
                reply["identity"][field] = value
                return reply
            with self.subTest(field=field):
                self.assertIsNone(self.exchange(change))
        for field, value in [("pid", os.getpid()+1), ("pid", True), ("start_time", 0), ("boot_id", "bad"), ("process_namespace", "unknown")]:
            def change(reply):
                reply["identity"]["process"][field] = value
                return reply
            with self.subTest(field=field):
                self.assertIsNone(self.exchange(change))

    def test_stale_socket_malformed_replies_and_duplicate_fields_never_mean_ready(self):
        endpoint = self.control / "control.sock"
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.bind(str(endpoint))
        os.chmod(endpoint, 0o600)
        listener.close()
        original_inode = endpoint.stat().st_ino
        self.assertIsNone(dev.guard_identity(self.control, self.worker, self.store, os.getpid(), .1))
        self.assertEqual(endpoint.stat().st_ino, original_inode)
        for raw in (b"[1]", b"{", b'{"version":2,"version":2}', b"{}", b"x"*16385):
            with self.subTest(raw_size=len(raw)):
                self.assertIsNone(self.exchange(raw=raw))

    def test_a_slow_partial_reply_obeys_one_total_deadline(self):
        endpoint = self.control / "control.sock"
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.bind(str(endpoint))
        os.chmod(endpoint, 0o600)
        listener.listen(1)
        def slow():
            with listener.accept()[0] as client:
                client.recv(16384)
                for part in (b"\x00", b"\x00", b"\x00", b"\x10"):
                    try:
                        client.sendall(part)
                    except OSError:
                        break
                    time.sleep(.1)
        thread = threading.Thread(target=slow)
        thread.start()
        try:
            started = time.monotonic()
            self.assertIsNone(dev.guard_identity(self.control, self.worker, self.store, os.getpid(), .15))
            self.assertLess(time.monotonic() - started, .8)
        finally:
            thread.join(timeout=2)
            listener.close()
        self.assertFalse(thread.is_alive())


class ManagedProcessTests(unittest.TestCase):
    def test_signal_during_spawn_is_forwarded_after_the_direct_child_is_published(self):
        handlers = {}
        child = Mock(pid=123, returncode=None)
        child.poll.side_effect = lambda: child.returncode
        child.wait.side_effect = lambda: setattr(child, "returncode", 0) or 0
        previous = {sig: signal.getsignal(sig) for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP)}
        def install(sig, callback):
            handlers[sig] = callback
            return previous[sig]
        def spawn(*_args, **_kwargs):
            self.assertEqual(set(handlers), set(previous))
            handlers[signal.SIGTERM](signal.SIGTERM, None)
            return child
        with patch.object(dev.signal, "signal", side_effect=install), patch.object(dev.subprocess, "Popen", side_effect=spawn):
            with dev.ManagedProcess(["owned-child"], {}) as managed:
                self.assertTrue(managed.stopping)
                self.assertIs(managed.child, child)
                child.terminate.assert_called_once()
                child.wait()
        self.assertEqual(handlers, previous)
        child.kill.assert_not_called()

    def test_stop_before_spawn_creates_no_child_and_exceptions_join_before_restoring_handlers(self):
        def stopping(sig, callback):
            if sig == signal.SIGHUP:
                callback(sig, None)
            return signal.SIG_DFL
        # Stop restoration uses integer handlers, which are not callbacks.
        def guarded(sig, callback):
            if callable(callback):
                return stopping(sig, callback)
            return signal.SIG_DFL
        with patch.object(dev.signal, "signal", side_effect=guarded), patch.object(dev.subprocess, "Popen") as spawn:
            with dev.ManagedProcess(["unused"], {}) as managed:
                self.assertIsNone(managed.child)
            spawn.assert_not_called()
        child = Mock(returncode=None)
        child.poll.side_effect = lambda: child.returncode
        child.wait.side_effect = lambda: setattr(child, "returncode", 0) or 0
        with patch.object(dev.subprocess, "Popen", return_value=child):
            with self.assertRaisesRegex(RuntimeError, "fixture"):
                with dev.ManagedProcess(["owned"], {}):
                    raise RuntimeError("fixture")
        child.terminate.assert_called_once()
        child.wait.assert_called_once()
        child.kill.assert_not_called()


class ServiceReadinessTests(unittest.TestCase):
    def test_lease_pins_only_an_actual_ping_from_its_unreaped_child(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            values = {"WORKER_ID": "fixture", "WORKER_SECRET": "private-fixture"}
            child = Mock(pid=123, returncode=None)
            child.poll.side_effect = lambda: child.returncode
            child.wait.side_effect = lambda: setattr(child, "returncode", 0) or 0
            store = str(uuid.uuid4())
            with patch.object(dev, "STATE", root), patch.object(dev, "build_boxlite", return_value=root), patch.object(dev, "provision", return_value=values), patch.object(dev, "node_paths", return_value=(root, root)), patch.object(dev.subprocess, "Popen", return_value=child), patch.object(dev, "guard_identity", side_effect=[None, {"store_id":store}]) as probe, patch.object(dev.time, "sleep"), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(dev.lease("worker-boxlite"), 0)
            self.assertEqual(probe.call_count, 2)
            self.assertEqual(probe.call_args.args[:4], (root, "fixture", None, 123))
            self.assertEqual(dev.env_file(root / "worker-boxlite.env")["WORKER_PERSISTENCE_LEASE_STORE_ID"], store)
            self.assertEqual((root / "worker-boxlite.env").stat().st_mode & 0o777, 0o600)

    def test_worker_does_not_spawn_from_a_socket_inode_without_ready(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            values = {"WORKER_ID":"fixture", "WORKER_PERSISTENCE_LEASE_STORE_ID":str(uuid.uuid4())}
            with patch.object(dev, "build_boxlite", return_value=root), patch.object(dev, "env_file", return_value=values), patch.object(dev, "node_paths", return_value=(root,root)), patch.object(dev, "guard_identity", side_effect=[None, {"store_id":values["WORKER_PERSISTENCE_LEASE_STORE_ID"]}]) as probe, patch.object(dev, "supervise_worker", return_value=0) as spawn, patch.object(dev.time, "sleep") as sleep, contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(dev.worker("worker-boxlite"), 0)
            self.assertEqual(probe.call_count, 2)
            sleep.assert_called_once()
            spawn.assert_called_once()

    def test_child_exit_during_ping_cannot_pin_or_replace_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            store = str(uuid.uuid4())
            values = {"WORKER_ID":"fixture", "WORKER_SECRET":"private-fixture", "WORKER_PERSISTENCE_LEASE_STORE_ID":store}
            path = root / "worker-boxlite.env"
            dev.write_env(path, values)
            original = path.read_bytes()
            child = Mock(pid=123, returncode=None)
            child.poll.side_effect = lambda: child.returncode
            def dead_reply(*_args):
                child.returncode = 17
                return {"store_id":store}
            with patch.object(dev, "STATE", root), patch.object(dev, "build_boxlite", return_value=root), patch.object(dev, "provision", return_value=dict(values)), patch.object(dev, "node_paths", return_value=(root,root)), patch.object(dev.subprocess, "Popen", return_value=child), patch.object(dev, "guard_identity", side_effect=dead_reply), patch.object(dev.time, "sleep"), patch.object(dev, "write_env") as write:
                with self.assertRaisesRegex(RuntimeError, "returncode=17"):
                    dev.lease("worker-boxlite")
            write.assert_not_called()
            self.assertEqual(path.read_bytes(), original)
            child.kill.assert_not_called()

    def test_tmux_command_preserves_service_exit_status_through_tee(self):
        source = (Path(dev.__file__).parent / "dev.sh").read_text()
        start = source.index("wrapped_cmd() {")
        wrapped = source[start:source.index("\n}\n", start)+3]
        with tempfile.TemporaryDirectory(prefix="obx command ") as directory:
            root = Path(directory)
            environment = dict(os.environ, ENV_FILE=str(root/"config.env"), LOG_DIR=str(root), SESSION="fixture")
            (root / "config.env").write_text("")
            for code in (0, 7):
                command = shlex.join([sys.executable, "-c", f"import sys; print('fixture'); sys.exit({code})"])
                script = wrapped + "\nsvc_cmd() { printf '%s' " + shlex.quote(command) + "; }\neval \"$(wrapped_cmd fixture)\"\n"
                result = subprocess.run(["bash", "-c", script], env=environment, capture_output=True)
                self.assertEqual(result.returncode, code)
                self.assertEqual((root/"fixture.log").read_text(), "fixture\n")


class ServiceSelectionTests(unittest.TestCase):
    def actions(self, *arguments):
        source = (Path(dev.__file__).parent / "dev.sh").read_text()
        declarations = next(line for line in source.splitlines() if line.startswith("ALL_SERVICES=("))
        functions = []
        for name in ("is_valid_service", "resolve_services", "cmd_start", "cmd_stop", "cmd_restart"):
            start = source.index(name + "() {")
            functions.append(source[start:source.index("\n}\n", start)+3])
        # Exercise the real shell dispatch and expansion, with no tmux/services.
        harness = declarations + "\n" + "\n".join(functions) + """
require_tmux() { :; }
ensure_session() { :; }
cleanup_bootstrap() { :; }
session_exists() { return 1; }
sleep() { :; }
start_service() { printf 'ACTION start %s\\n' "$1"; }
stop_service() { printf 'ACTION stop %s\\n' "$1"; }
die() { exit 2; }
PREVIEW_HELPER=true
command="$1"
shift
"cmd_$command" "$@"
"""
        result = subprocess.run(["bash", "-euc", harness, "fixture", *arguments], capture_output=True, text=True, check=True)
        return [line.removeprefix("ACTION ") for line in result.stdout.splitlines() if line.startswith("ACTION ")]

    def test_worker_stop_and_restart_preserve_the_independent_companion(self):
        for worker in ("worker-boxlite", "worker-boxlite-peer"):
            with self.subTest(worker=worker):
                self.assertEqual(self.actions("start", worker), [f"start {worker}-lease", f"start {worker}"])
                self.assertEqual(self.actions("stop", worker), [f"stop {worker}"])
                self.assertEqual(self.actions("restart", worker), [f"stop {worker}", f"start {worker}-lease", f"start {worker}"])

    def test_explicit_companion_and_group_selection_still_operate_on_companions(self):
        self.assertEqual(self.actions("restart", "worker-boxlite-lease"), ["stop worker-boxlite-lease", "start worker-boxlite-lease"])
        self.assertEqual(self.actions("stop", "sandbox-persistence"), [
            "stop worker-boxlite-peer", "stop worker-boxlite-peer-lease", "stop worker-boxlite", "stop worker-boxlite-lease", "stop console", "stop snapshot-store"])
        self.assertEqual(self.actions("start", "worker-boxlite", "worker-boxlite", "worker-boxlite-lease"), ["start worker-boxlite-lease", "start worker-boxlite"])
        self.assertIn("stop worker-boxlite-lease", self.actions("stop"))
        self.assertIn("stop worker-boxlite-peer-lease", self.actions("stop"))


if __name__ == "__main__":
    unittest.main()
