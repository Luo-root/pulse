"""Contract tests for retained benchmark evidence; no engines or network required."""
import contextlib
import csv
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import MagicMock, patch


def load(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


matrix, summary, storage = load("run_matrix"), load("summarize"), load("storage_plot")
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


class StoragePlotTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.source, self.out = self.root / "growth.jsonl", self.root / "plots"
        # 只替代绘图依赖；输入、汇总、CSV 和输出文件路径仍执行真实代码。
        self.figure = MagicMock()
        self.figure.savefig.side_effect = lambda path, **options: path.write_text("<svg/>\n", encoding="utf-8")
        pyplot = types.SimpleNamespace(rcParams={}, close=MagicMock(), subplots=MagicMock(
            return_value=(self.figure, [MagicMock() for _ in range(3)])))
        self.plot_modules = {"matplotlib": types.SimpleNamespace(use=MagicMock(), pyplot=pyplot),
                             "matplotlib.pyplot": pyplot,
                             "matplotlib.ticker": types.SimpleNamespace(ScalarFormatter=MagicMock())}

    def record(self, amount=100, **changes):
        row = dict(mode="growth", engine="durable", backend="file", nodes=10,
                   shape="chain", rounds=1, parallelism=4, procs=4, sample=1,
                   status="ok", confirmed_steps=10,
                   storage=dict(logical_json_bytes=amount, final_record_json_bytes=amount,
                                db_bytes=amount, wal_bytes=0))
        row.update(changes)
        return row

    def invoke(self, records):
        self.source.write_text("".join(json.dumps(row) + "\n" for row in records), encoding="utf-8")
        args = ["storage_plot", str(self.source), "--out", str(self.out)]
        with patch.object(sys, "argv", args), patch.dict(sys.modules, self.plot_modules), \
                contextlib.redirect_stdout(io.StringIO()):
            storage.main()

    def test_mixed_workloads_are_rejected_before_any_output(self):
        for field, changed in (("rounds", 256), ("shape", "wide"), ("parallelism", 8), ("procs", 8)):
            with self.subTest(field=field):
                with self.assertRaisesRegex(ValueError, "line 2.*" + field):
                    self.invoke([self.record(100), self.record(1000, **{field: changed})])
                self.assertFalse(self.out.exists())
                self.figure.savefig.assert_not_called()
                self.assertEqual([json.loads(line)["storage"]["logical_json_bytes"]
                                  for line in self.source.read_text(encoding="utf-8").splitlines()], [100, 1000])

    def test_rejection_preserves_existing_reports_and_checks_all_records(self):
        self.out.mkdir()
        originals = {name: ("retain " + name).encode() for name in
                     ("storage-summary.csv", "storage-growth.svg", "storage-growth.png")}
        for name, content in originals.items():
            (self.out / name).write_bytes(content)
        # 不同引擎、后端、节点数以及未重复记录也不能混入另一套工作量。
        with self.assertRaisesRegex(ValueError, "line 2.*rounds"):
            self.invoke([self.record(), self.record(engine="go-workflows", backend="memory", nodes=100,
                                                   rounds=256, status="not_repeated")])
        self.assertEqual({path.name: path.read_bytes() for path in self.out.iterdir()}, originals)
        self.figure.savefig.assert_not_called()

    def test_missing_or_invalid_workload_identity_is_rejected(self):
        for field in ("shape", "rounds", "parallelism", "procs"):
            with self.subTest(missing=field):
                row = self.record()
                del row[field]
                with self.assertRaisesRegex(ValueError, "line 1.*" + field):
                    self.invoke([row])
                self.assertFalse(self.out.exists())
        for field, value in (("shape", ""), ("shape", None), ("rounds", None), ("rounds", "1"),
                             ("rounds", 1.0), ("parallelism", True), ("parallelism", 0), ("procs", -1)):
            with self.subTest(field=field, value=value):
                with self.assertRaisesRegex(ValueError, "line 1.*" + field):
                    self.invoke([self.record(**{field: value})])
                self.assertFalse(self.out.exists())
        self.figure.savefig.assert_not_called()

    def test_same_workload_preserves_groups_statuses_and_metric_scopes(self):
        complete = [self.record(amount, sample=i + 1) for i, amount in enumerate((100, 300, 500))]
        for row, db, wal in zip(complete, (1, 10, 100), (100, 10, 1)):
            row["storage"].update(db_bytes=db, wal_bytes=wal)
        self.invoke(complete + [self.record(9, status="timeout", confirmed_steps=3),
                               self.record(10000, status="not_repeated"),
                               self.record(600, nodes=100, confirmed_steps=100),
                               self.record(700, backend="memory"),
                               self.record(1100, engine="go-workflows")])
        with (self.out / "storage-summary.csv").open(encoding="utf-8", newline="") as stream:
            rows = list(csv.DictReader(stream))
        self.assertEqual(len(rows), 5)
        grouped = {(row["engine"], row["backend"], row["requested_nodes"], row["state"]): row for row in rows}
        full = grouped[("durable", "file", "10", "complete")]
        partial = grouped[("durable", "file", "10", "partial")]
        self.assertEqual((full["samples"], full["logical_json_bytes"], full["db_wal_bytes"]), ("3", "300", "101"))
        self.assertEqual(json.loads(full["statuses"]), {"ok": 3})
        self.assertEqual((partial["samples"], partial["logical_json_bytes"], partial["confirmed_steps"]), ("1", "9", "3"))
        self.assertEqual(json.loads(partial["statuses"]), {"timeout": 1})
        self.assertEqual(grouped[("durable", "file", "100", "complete")]["logical_json_bytes"], "600")
        self.assertEqual(grouped[("durable", "memory", "10", "complete")]["logical_json_bytes"], "700")
        self.assertEqual(grouped[("go-workflows", "file", "10", "complete")]["logical_json_bytes"], "1100")
        self.assertEqual(self.figure.savefig.call_count, 2)
        self.assertEqual({path.name for path in self.out.iterdir()},
                         {"storage-summary.csv", "storage-growth.svg", "storage-growth.png"})


if __name__ == "__main__":
    unittest.main()
