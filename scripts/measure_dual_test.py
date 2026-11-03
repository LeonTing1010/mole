import importlib.util
import io
import pathlib
import threading
import time
import unittest
from unittest import mock
import urllib.parse
import tempfile
import contextlib

spec = importlib.util.spec_from_file_location("measure_dual", pathlib.Path(__file__).with_name("measure-dual.py"))
measure = importlib.util.module_from_spec(spec)
spec.loader.exec_module(measure)


class MeasureTests(unittest.TestCase):
    # Quality/what: transport comparisons must hold total concurrency constant.
    def test_cli_compares_equal_total_connections(self):
        # Why: four single-leg versus eight dual-leg flows overstated dual gains.
        report = {"aggregate_mbps": 1, "elapsed_seconds": 1,
                  "legs": {}, "new_enobufs_lines": 0, "complete": True}
        with tempfile.TemporaryDirectory() as directory:
            args = ["measure-dual.py", "--output", directory + "/report.json"]
            with mock.patch.object(measure.sys, "argv", args), \
                    mock.patch.object(measure, "run_phase", return_value=report.copy()) as run, \
                    mock.patch.object(measure.time, "sleep"), \
                    contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(measure.main(), 0)
            self.assertEqual([len(c.args[1]) * c.args[4] for c in run.call_args_list],
                             [8, 8, 8])

    # Quality/what-x-what: concurrent legs share a budget and a common clock.
    def test_concurrent_legs_share_budget_and_measured_interval(self):
        # Why: adding sequential rates would invent bandwidth that never occurred.
        barrier = threading.Barrier(2)

        class Opener:
            def open(self, url, timeout):
                size = int(urllib.parse.parse_qs(urllib.parse.urlsplit(url).query)["bytes"][0])
                barrier.wait(timeout=2)
                response = io.BytesIO(b"x" * size)
                response.status = 200
                response.headers = {"Content-Length": str(size)}
                return response

        report = measure.run_phase("Both", [8098, 8099], 1, 128 << 10, 1,
                                   request_bytes=64 << 10, opener_factory=Opener)
        self.assertEqual(sum(v["bytes"] for v in report["legs"].values()), 128 << 10)
        self.assertTrue(report["budget_exhausted"])
        self.assertEqual(report["new_enobufs_lines"], 0)
        self.assertAlmostEqual(report["aggregate_mbps"], (128 << 10) * 8 / report["elapsed_seconds"] / 1e6)

    # Quality/what: connection failures must remain explicit, never successful zeroes.
    def test_failure_is_reported(self):
        # Why: an unreachable listener is an invalid measurement, not zero capacity.
        class Opener:
            def open(self, url, timeout):
                raise OSError("connection refused")

        report = measure.run_phase("Reality", [8098], 1, 64 << 10, 1, opener_factory=Opener)
        self.assertEqual(report["legs"][8098]["errors"], ["connection refused"])
        self.assertEqual(report["legs"][8098]["completed"], 0)

    def test_deadline_does_not_count_reserved_budget_as_delivered(self):
        class Response(io.BytesIO):
            status = 200
            headers = {"Content-Length": "65536"}
            def read(self, size):
                time.sleep(.04)
                return super().read(min(size, 1024))
        class Opener:
            def open(self, url, timeout):
                return Response(b"x" * 65536)
        report = measure.run_phase("slow", [8098], .01, 65536, 1, opener_factory=Opener)
        self.assertEqual(report["budget_reserved_bytes"], 65536)
        self.assertTrue(report["budget_reserved_exhausted"])
        self.assertFalse(report["budget_exhausted"])
        self.assertFalse(report["complete"])
        self.assertEqual(report["stop_reason"], "deadline")
        self.assertEqual(report["legs"][8098]["timed_out"], 1)
        self.assertEqual(report["legs"][8098]["errors"], [])
        self.assertLess(report["delivered_bytes"], 65536)

    def test_short_body_is_explicit_failure(self):
        class Opener:
            def open(self, url, timeout):
                response = io.BytesIO(b"short")
                response.status = 200
                response.headers = {"Content-Length": "65536"}
                return response
        report = measure.run_phase("short", [8098], 1, 65536, 1, opener_factory=Opener)
        self.assertEqual(report["stop_reason"], "request_error")
        self.assertFalse(report["complete"])
        self.assertEqual(report["legs"][8098]["failed"], 1)

    def test_cli_returns_failure_for_error_free_partial_transfer(self):
        report = {"aggregate_mbps": 1, "elapsed_seconds": 1, "legs": {},
                  "new_enobufs_lines": 0, "complete": False, "stop_reason": "deadline"}
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(measure.sys, "argv", ["measure-dual.py", "--output", directory + "/r.json"]), \
                    mock.patch.object(measure, "run_phase", return_value=report), \
                    contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(measure.main(), 2)


if __name__ == "__main__":
    unittest.main()
