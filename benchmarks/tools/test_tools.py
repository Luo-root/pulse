"""Contract tests for retained benchmark evidence; no engines or network required."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


def load(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


matrix, summary = load("run_matrix"), load("summarize")
CASE = dict(engine="direct", shape="chain", nodes=1, rounds=1, mode="e2e", parallelism=1, procs=1)
EXPECTED = dict(checksum=1164970750538294679, edges=0, critical_path=1)
GOOD = dict(CASE, **EXPECTED, status="ok", iterations=2, ns_per_op=100, bytes_per_op=24, allocs_per_op=1)


class EvidenceTests(unittest.TestCase):
    def test_independent_golden(self):
        self.assertEqual(matrix.oracle("chain", 1, 1), EXPECTED)
        self.assertEqual(matrix.oracle("chain", 10000, 256)["checksum"], 6413444386843497948)
        self.assertEqual(matrix.oracle("wide", 10000, 256)["checksum"], 12525124291304054842)
        self.assertEqual(matrix.oracle("diamonds", 10000, 256),
                         dict(checksum=14469358313332578430, edges=13332, critical_path=6667))

    def test_reject_invalid_measurements_and_overwritten_identity(self):
        matrix.check_result(GOOD, CASE, EXPECTED)
        for override in (dict(checksum=0), dict(iterations=True), dict(iterations=1.5),
                         dict(iterations=float("inf")), dict(ns_per_op=0), dict(bytes_per_op=-1),
                         dict(ns_per_op=10**400), dict(sample=7), dict(suite="workflow"),
                         dict(expected={}), dict(wall_seconds=-1), dict(backend="file")):
            with self.subTest(override=override), self.assertRaises((ValueError, OverflowError)):
                matrix.check_result(dict(GOOD, **override), CASE, EXPECTED)

    def test_partial_failures_never_become_zero_measurements(self):
        records = [dict(GOOD, suite="dag", sample=1), dict(GOOD, suite="dag", sample=2, ns_per_op=300),
                   dict(CASE, suite="dag", sample=3, status="timeout")]
        row, = summary.summarize(records)
        self.assertEqual((row["samples_ok"], row["samples_planned"]), (2, 3))
        self.assertEqual(row["ns_per_op_median"], 200)
        failed, = summary.summarize([records[-1]])
        self.assertEqual(failed["ns_per_op_median"], "")

    def invoke(self, directory, fixture, resume=False):
        args = ["run_matrix", "--suite", "dag", "--module", str(directory), "--binary", str(directory / "mock"),
                "--out", str(directory / "results"), "--sizes", "1", "--rounds", "1", "--shapes", "chain",
                "--modes", "e2e", "--engines", "direct", "--repeats", "1", "--parallelism", "1", "--procs", "1"]
        if resume:
            args.append("--resume")
        with patch.object(sys, "argv", args), patch.object(matrix, "capture", return_value="metadata"), \
                patch.object(matrix, "sample_process", return_value=(0, json.dumps(fixture), "")), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            return matrix.main()

    def test_resume_retains_existing_failure(self):
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as folder:
            directory = Path(folder)
            (directory / "mock").write_bytes(b"unchanged binary")
            self.assertEqual(self.invoke(directory, dict(GOOD, status="error")), 1)
            self.assertEqual(self.invoke(directory, dict(GOOD, status="error"), resume=True), 1)
            manifest = json.loads((directory / "results" / "manifest.json").read_text())
            self.assertEqual(manifest["failed_samples"], 1)

    def test_invalid_raw_output_is_retained_and_cannot_be_resumed(self):
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as folder:
            directory = Path(folder)
            (directory / "mock").write_bytes(b"unchanged binary")
            with self.assertRaises(RuntimeError):
                self.invoke(directory, dict(GOOD, checksum=0))
            record, = map(json.loads, (directory / "results" / "samples.jsonl").read_text().splitlines())
            self.assertEqual(record["status"], "invalid_result")
            self.assertEqual(json.loads(record["stdout"])["checksum"], 0)
            self.assertEqual(record["expected"], EXPECTED)
            with self.assertRaises(SystemExit) as caught:
                self.invoke(directory, GOOD, resume=True)
            self.assertNotEqual(caught.exception.code, 0)


if __name__ == "__main__":
    unittest.main()
