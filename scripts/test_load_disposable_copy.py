"""Regression checks for retaining load evidence after an unavailable server."""
import contextlib
import importlib.util
import io
import json
import pathlib
import tempfile
import threading
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("load", pathlib.Path(__file__).with_name("load-disposable-copy.py"))
load = importlib.util.module_from_spec(spec)
spec.loader.exec_module(load)


class FakeClient:
    def __init__(self, failure):
        self.failure = failure
        self.token = "synthetic"
        self.rows = {}
        self.lock = threading.Lock()

    def login(self, *_):
        pass

    def request(self, method, path, body=None):
        with self.lock:
            if "?" in path:
                return 200, {}
            if method == "POST":
                if self.failure == "write":
                    raise ConnectionRefusedError()
                rid = str(len(self.rows) + 1)
                self.rows[rid] = {"id": rid, **body}
                return 200, dict(self.rows[rid])
            rid = path.rsplit("/", 1)[-1]
            if method == "PATCH":
                self.rows[rid].update(body)
            if method == "DELETE":
                if self.failure == "cleanup":
                    raise ConnectionResetError()
                del self.rows[rid]
                return 204, None
            return 200, dict(self.rows[rid])


class LoadEvidenceTests(unittest.TestCase):
    def run_load(self, failure):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            password = root / "password"
            password.write_text("synthetic")
            output = root / "report.json"
            argv = ["load", "--candidate", "http://127.0.0.1:1", "--label", "regression", "--email", "test@example.invalid", "--password-file", str(password), "--output", str(output), "--iterations", "1", "--allow-synthetic-writes"]
            with mock.patch("sys.argv", argv), mock.patch.object(load.comparison, "Client", return_value=FakeClient(failure)), contextlib.redirect_stdout(io.StringIO()):
                code = load.main()
            return code, json.loads(output.read_text())

    def test_cleanup_disconnect_keeps_all_measured_rounds(self):
        code, report = self.run_load("cleanup")
        self.assertEqual(code, 1)
        self.assertFalse(report["passed"])
        self.assertEqual(len(report["rounds"]), 3)
        self.assertEqual(report["cleanupPendingRecords"], 25)
        self.assertEqual(report["cleanupTransportError"], "ConnectionResetError")
        self.assertRegex(report["runPrefix"], r"^load-[0-9a-f]{16}$")

    def test_unavailable_writer_fails_without_later_rounds(self):
        code, report = self.run_load("write")
        self.assertEqual(code, 1)
        self.assertFalse(report["completed"])
        self.assertFalse(report["passed"])
        self.assertEqual(len(report["rounds"]), 1)
        self.assertEqual(report["rounds"][0]["failures"], {"transport:ConnectionRefusedError": 1})

    def test_success_requires_measurement_and_cleanup(self):
        code, report = self.run_load(None)
        self.assertEqual(code, 0)
        self.assertTrue(report["completed"])
        self.assertTrue(report["passed"])
        self.assertEqual(report["cleanupFailures"], 0)


if __name__ == "__main__":
    unittest.main()
