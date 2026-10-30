"""仅用小型测试样本检查汇总，不启动采样器、引擎或服务器。

设置 PULSE_PRODUCTION_SUMMARY_TEST_TMP 可将测试样本保留在源码 checkout 之外。
"""
import contextlib
import csv
import hashlib
import io
import json
import math
import os
from pathlib import Path
import tempfile
import types
import unittest
from unittest.mock import MagicMock, patch

import summarize_production as summary


TEST_ROOT = Path(os.environ.get('PULSE_PRODUCTION_SUMMARY_TEST_TMP',
                 Path(__file__).resolve().parents[1] / 'results' / 'test-production-summary'))
GOLDEN = 1164970750538294679  # 来自独立采样器测试的 uint64 参考值。


def manifest(experiment, repeats=1):
    identity = dict(engines='pulse,eino,temporal', modes='plain,observe',
                    shapes='chain,wide,diamonds', nodes='1,10,100,1000,10000', rounds='1,256',
                    repeats=repeats, parallelism=4, gomaxprocs=4,
                    binary_sha256='a'*64, temporal_sha256='b'*64)
    if experiment == 'resources':
        identity.update(modes='plain', rounds='1', fixed_iterations=1)
    elif experiment == 'checkpoint':
        identity.update(engines='pulse', modes='checkpoint', rounds='1', fixed_iterations=1)
    elif experiment == 'recovery':
        identity.update(modes='plain', shapes='chain', nodes='10,100,1000', rounds=256)
        return identity
    return dict(identity=identity)


def sample(experiment='native', **changes):
    value = dict(engine='pulse', mode='checkpoint' if experiment == 'checkpoint' else 'plain',
                 shape='chain', nodes=1, rounds=1, repeat=0, status='ok',
                 requested_iterations=1, iterations=1, first_completed=True,
                 first_ns=200, run_ns=100.0, checksum=GOLDEN,
                 job_cpu_s=0.0, sampled_tree_peak_rss_bytes=None,
                 alloc_bytes_per_run=None, alloc_objects_per_run=None)
    if experiment == 'recovery':
        value = dict(engine='pulse', nodes=10, repeat=0, prefix=5, kind='runtime_kill',
                     status='ok', recovery_wall_s=.02, operation_ns=1_000_000,
                     checksum=summary.oracle('chain', 10, 256))
    value.update(changes)
    return value


def normal(experiment='native', **changes):
    source = manifest(experiment, repeats=20)
    return summary.normalize(sample(experiment, **changes), experiment, source.get('identity', source))


class ProductionSummaryTests(unittest.TestCase):
    def setUp(self):
        TEST_ROOT.mkdir(parents=True, exist_ok=True)
        self.root = Path(tempfile.mkdtemp(prefix=self._testMethodName + '-', dir=TEST_ROOT))

    def dataset(self, experiment, records=None, metadata=None):
        directory = self.root / experiment
        directory.mkdir()
        if records is None:
            records = [sample(experiment)]
        data = ''.join(json.dumps(row, allow_nan=False) + '\n' for row in records).encode('utf-8')
        (directory / 'raw.jsonl').write_bytes(data)
        value = manifest(experiment) if metadata is None else metadata
        value['raw_sha256'] = hashlib.sha256(data).hexdigest()
        value['records'] = len(records)
        (directory / 'manifest.json').write_text(json.dumps(value), encoding='utf-8')
        return directory

    def invoke(self, out, no_plots=True):
        args = [item for experiment in summary.EXPERIMENTS
                for item in ('--' + experiment, str(self.root / experiment))] + ['--out', str(out)]
        if no_plots:
            args.append('--no-plots')
        with contextlib.redirect_stdout(io.StringIO()):
            return summary.main(args)

    def test_uint64_csv_is_exact_and_missing_metrics_stay_blank(self):
        for experiment in summary.EXPERIMENTS:
            self.dataset(experiment)
        original = {p: p.read_bytes() for p in self.root.glob('*/raw.jsonl')}
        out = self.root / 'summary'
        self.assertEqual(self.invoke(out), 0)
        with (out / 'summary.csv').open(encoding='utf-8', newline='') as stream:
            rows = list(csv.DictReader(stream))
        row = next(r for r in rows if r['experiment'] == 'native')
        self.assertEqual(row['checksum'], str(GOLDEN))
        self.assertEqual(row['alloc_bytes_per_run_median'], '')
        self.assertEqual(row['sampled_tree_peak_rss_bytes_median'], '')
        self.assertEqual(float(row['job_cpu_s_median']), 0.0)
        self.assertEqual(row['first_completed_total'], '1')
        self.assertEqual(original, {p: p.read_bytes() for p in original})
        identity = json.loads((out / 'identity.json').read_text(encoding='utf-8'))
        self.assertEqual((identity['records'], identity['groups']), (4, 4))
        self.assertEqual(identity['inputs']['native']['raw_sha256'], hashlib.sha256(original[self.root / 'native' / 'raw.jsonl']).hexdigest())
        self.assertIsNone(identity['inputs']['native']['versions']['pulse'])
        self.assertEqual({p.name for p in out.iterdir()}, {'summary.csv', 'identity.json'})

    def test_all_statuses_progress_and_checksum_error_remain_visible(self):
        records = [normal(repeat=i, status=status, run_ns=100 if i == 0 else 999,
                          iterations=3 if i == 0 else 0, requested_iterations=3,
                          checksum=GOLDEN if i == 0 else None,
                          error='reuse checksum mismatch' if status == 'error' else '')
                   for i, status in enumerate(summary.STATUSES)]
        row, = summary.summarize(records, {'native': {'repeats': 6}})
        for status in summary.STATUSES:
            self.assertEqual(row['status_' + status], 1)
        self.assertEqual((row['run_ns_n'], row['run_ns_median']), (1, 100))
        self.assertEqual((row['planned_iterations_total'], row['completed_iterations_total']), (18, 3))
        self.assertEqual((row['first_completed_total'], row['first_completed_known']), (6, 6))
        self.assertEqual(row['checksum_errors'], 1)
        self.assertEqual(json.loads(row['errors']), {'reuse checksum mismatch': 1})
        failed, = summary.summarize(records[1:], {'native': {'repeats': 6}})
        self.assertEqual((failed['run_ns_n'], failed['run_ns_median'], failed['checksum']), (0, '', ''))

    def test_batch_means_have_equal_repeat_weight_and_observations_keep_coverage(self):
        records = [normal(repeat=0, iterations=1, requested_iterations=1,
                          run_ns=100, observations={'lifecycle_callbacks': 10}),
                   normal(repeat=1, iterations=9, requested_iterations=9,
                          run_ns=300, observations={'lifecycle_callbacks': 50})]
        row, = summary.summarize(records, {'native': {'repeats': 3}})
        self.assertEqual((row['run_ns_median'], row['run_ns_min'], row['run_ns_max']), (200, 100, 300))
        self.assertEqual(row['samples_missing'], 1)
        self.assertEqual(json.loads(row['observations'])['lifecycle_callbacks'],
                         {'n': 2, 'median': 30, 'min': 10, 'max': 50})
        self.assertIn('Surfaces differ', summary.SCOPES['observations'])

    def test_all_workload_and_experiment_identities_are_separate(self):
        records = []
        for engine in summary.ENGINES:
            for mode in ('plain', 'observe'):
                for shape in summary.SHAPES:
                    for rounds in (1, 256):
                        records.append(normal(engine=engine, mode=mode, shape=shape, rounds=rounds,
                                              checksum=summary.oracle(shape, 1, rounds)))
        records += [normal('resources'), normal('checkpoint')]
        identities = {e: manifest(e).get('identity', manifest(e)) for e in summary.EXPERIMENTS}
        rows = summary.summarize(records, identities)
        self.assertEqual(len(rows), 38)
        self.assertTrue(all(row['samples_recorded'] == 1 for row in rows))

    def test_recovery_failures_get_controller_identity_and_kinds_do_not_merge(self):
        source = manifest('recovery')
        rows = [summary.normalize(dict(engine='temporal', nodes=100, repeat=0,
                    status='error', error='interrupted', server_restart=restart), 'recovery', source)
                for restart in (False, True)]
        rows += [normal('recovery', engine='eino', kind='active_interrupt')]
        aggregates = summary.summarize(rows, {'recovery': source})
        self.assertEqual(len(aggregates), 3)
        self.assertEqual({r['kind'] for r in aggregates}, {'runtime_kill', 'server_restart', 'active_interrupt'})
        for row in rows[:2]:
            self.assertEqual((row['shape'], row['mode'], row['rounds'], row['prefix']), ('chain', 'plain', 256, 10))
        for row in aggregates:
            if row['engine'] == 'temporal':
                self.assertEqual(row['recovery_wall_s_median'], '')
                self.assertEqual(row['first_completed_total'], '')

    def test_explicit_recovery_prefix_is_part_of_group(self):
        rows = summary.summarize([normal('recovery'), normal('recovery', prefix=4)],
                                 {'recovery': {'repeats': 1}})
        self.assertEqual({r['prefix'] for r in rows}, {4, 5})

    def test_bad_checksum_and_fake_success_are_rejected_before_output(self):
        for experiment in summary.EXPERIMENTS:
            self.dataset(experiment, [sample(experiment, checksum=0)] if experiment == 'native' else None)
        out = self.root / 'bad-summary'
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            self.invoke(out)
        self.assertFalse(out.exists())
        for change in (dict(iterations=0), dict(first_completed=False), dict(requested_iterations=2),
                       dict(first_ns=None), dict(run_ns=0), dict(error='real error')):
            with self.subTest(change=change), self.assertRaises(ValueError):
                normal(**change)

    def test_invalid_numbers_and_status_are_rejected(self):
        changes = [dict(checksum=True), dict(checksum=float(GOLDEN)), dict(checksum=-1),
                   dict(checksum=1 << 64), dict(run_ns=float('nan')), dict(run_ns=float('inf')),
                   dict(run_ns=True), dict(run_ns=-1), dict(run_ns=10**400),
                   dict(iterations=1.5), dict(first_ns=False), dict(observations={'bad': True}),
                   dict(first_completed=1), dict(status='unknown'), dict(nodes=True)]
        for change in changes:
            with self.subTest(change=change), self.assertRaises((ValueError, OverflowError)):
                normal(**change)
        with self.assertRaisesRegex(ValueError, 'non-finite'):
            summary.decode('{"extra":NaN}')

    def test_resources_and_checkpoint_require_equal_fixed_work(self):
        for experiment in ('resources', 'checkpoint'):
            source = manifest(experiment)['identity']
            for fixed in ((None, 2, True, 1.0) if experiment == 'resources' else (2, True, 1.0)):
                with self.subTest(experiment=experiment, fixed=fixed), self.assertRaisesRegex(ValueError, 'fixed_iterations'):
                    summary.normalize(sample(experiment), experiment, dict(source, fixed_iterations=fixed))
            with self.assertRaisesRegex(ValueError, 'requested_iterations'):
                summary.normalize(sample(experiment, iterations=2, requested_iterations=2), experiment, source)
        automatic = dict(manifest('checkpoint')['identity'], fixed_iterations=None)
        self.assertEqual(summary.normalize(sample('checkpoint'), 'checkpoint', automatic)['iterations'], 1)
        with self.assertRaisesRegex(ValueError, 'requested_iterations'):
            summary.normalize(sample('checkpoint', requested_iterations=2), 'checkpoint', automatic)
        with self.assertRaisesRegex(ValueError, 'only Pulse'):
            normal('checkpoint', engine='eino')

    def test_different_input_binary_is_rejected_before_any_report(self):
        for experiment in summary.EXPERIMENTS:
            metadata = manifest(experiment)
            if experiment == 'resources':
                metadata['identity']['binary_sha256'] = 'c'*64
            self.dataset(experiment, metadata=metadata)
        out = self.root / 'rejected'
        with self.assertRaisesRegex(ValueError, 'joint sampling identity mismatch'):
            self.invoke(out)
        self.assertFalse(out.exists())

    def test_manifest_hash_counts_matrix_and_duplicate_protection(self):
        duplicate = self.dataset('native', [sample(), sample()])
        with self.assertRaisesRegex(ValueError, 'duplicate sample'):
            summary.load_dataset('native', duplicate)
        for field, value, error in (('raw_sha256', 'wrong', 'raw_sha256'), ('records', 99, 'record count'),
                                    ('statuses', {'error': 1}, 'status counts')):
            root = self.root / field
            root.mkdir()
            data = (json.dumps(sample()) + '\n').encode('utf-8')
            (root / 'raw.jsonl').write_bytes(data)
            metadata = manifest('native')
            metadata[field] = value
            (root / 'manifest.json').write_text(json.dumps(metadata), encoding='utf-8')
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, error):
                summary.load_dataset('native', root)
        with self.assertRaisesRegex(ValueError, 'outside manifest'):
            summary.normalize(sample(nodes=5), 'native', manifest('native')['identity'])
        with self.assertRaisesRegex(ValueError, 'manifest identity mismatch'):
            normal(parallelism=8)

    def test_matrix_formats_preserve_history_and_reject_duplicates(self):
        formats = (dict(engines=' pulse ', modes=' plain ', shapes=' chain ', nodes=' 01 ', rounds=' 01 '),
                   dict(engines=['pulse'], modes=['plain'], shapes=['chain'], nodes=[1], rounds=[1]),
                   dict(engines='pulse', modes='plain', shapes='chain', nodes=1, rounds=1))
        for index, matrix in enumerate(formats):
            with self.subTest(matrix=matrix):
                metadata = manifest('resources')
                metadata['identity'].update(matrix)
                root = self.root / str(index)
                self.root = root
                root.mkdir()
                directory = self.dataset('resources', metadata=metadata)
                before = {p: p.read_bytes() for p in directory.iterdir()}
                records, identity, details = summary.load_dataset('resources', directory)
                self.assertEqual(details['matrix']['nodes'], matrix['nodes'])
                self.assertEqual(summary.summarize(records, {'resources': identity})[0]['status_ok'], 1)
                self.assertEqual(before, {p: p.read_bytes() for p in before})
                for field, values in (('engines', ('pulse, pulse', ['pulse', 'pulse'])),
                                      ('nodes', ('1, 01', [1, '01']))):
                    for value in values:
                        with self.subTest(field=field, value=value), self.assertRaisesRegex(ValueError, 'duplicate'):
                            summary.normalize(sample('resources'), 'resources', dict(identity, **{field: value}))
                self.root = root.parent

    def test_verified_config_is_used_and_historical_records_still_load(self):
        identity = manifest('resources')['identity']
        config = dict(engine='pulse', mode='plain', shape='chain', nodes=1, rounds=1, parallelism=4)
        verified = summary.normalize(sample('resources', config=config), 'resources', identity)
        historical = summary.normalize(sample('resources'), 'resources', identity)
        self.assertEqual((verified['parallelism'], historical['parallelism']), (4, 4))
        self.assertEqual((verified['gomaxprocs'], historical['gomaxprocs']), (4, 4))
        for bad in (None, {}, dict(config, parallelism=1), dict(config, parallelism=True),
                    dict(config, parallelism=4.0), dict(config, nodes=True)):
            with self.subTest(config=bad), self.assertRaisesRegex(ValueError, 'config'):
                summary.normalize(sample('resources', config=bad), 'resources', identity)
        for value in (1, True, 4.0):
            with self.subTest(parallelism=value), self.assertRaisesRegex(ValueError, 'parallelism'):
                summary.normalize(sample('resources', config=config, parallelism=value), 'resources', identity)
        directory = self.dataset('resources', [sample('resources', config=config)])
        records, _, _ = summary.load_dataset('resources', directory)
        self.assertEqual(records[0]['parallelism'], config['parallelism'])
        # 无 config 的旧记录继续按历史清单解释，不回写原始证据。
        legacy = self.dataset('native')
        self.assertEqual(summary.load_dataset('native', legacy)[0][0]['parallelism'], 4)

    def test_versions_come_from_retained_stdout_and_are_not_inferred(self):
        directory = self.dataset('native', [sample(case_dir='cases/fixture')])
        case = directory / 'cases' / 'fixture'
        case.mkdir(parents=True)
        payload = dict(modules={path: 'v0.0.0-FIXTURE-' + name for path, name in summary.PRIMARY_MODULES.items()},
                       toolchain='go-FIXTURE')
        stdout = case / 'stdout.json'
        stdout.write_text(json.dumps(payload), encoding='utf-8')
        _, _, details = summary.load_dataset('native', directory)
        evidence = details['versions']['pulse']
        self.assertEqual(evidence['value'], 'v0.0.0-FIXTURE-pulse')
        self.assertIn('cases/fixture/stdout.json@sha256=' + hashlib.sha256(stdout.read_bytes()).hexdigest(), evidence['source'])
        self.assertIsNone(details['versions']['temporal_server_version'])

    def test_recovery_stdout_versions_and_manifest_versions(self):
        directory = self.dataset('recovery')
        case = directory / 'pulse-10-0-0'
        case.mkdir()
        (case / '01-resume.json').write_text(json.dumps({'modules': {'github.com/Luo-root/pulse': 'v-FIXTURE'}}), encoding='utf-8')
        _, _, details = summary.load_dataset('recovery', directory)
        self.assertEqual(details['versions']['pulse']['value'], 'v-FIXTURE')
        self.assertIn('01-resume.json', details['versions']['pulse']['source'])
        metadata = manifest('native')
        metadata['identity']['modules'] = {'github.com/cloudwego/eino': 'v-FIXTURE-MANIFEST'}
        other = self.dataset('native', metadata=metadata)
        _, _, details = summary.load_dataset('native', other)
        self.assertEqual(details['versions']['eino']['source'], 'manifest.json#identity#modules.github.com/cloudwego/eino')

    def test_existing_output_is_refused_and_inputs_are_not_written(self):
        for experiment in summary.EXPERIMENTS:
            self.dataset(experiment)
        out = self.root / 'already-there'
        out.mkdir()
        sentinel = out / 'retain.txt'
        sentinel.write_bytes(b'keep existing output')
        with self.assertRaisesRegex(ValueError, 'already exists'):
            self.invoke(out)
        self.assertEqual(sentinel.read_bytes(), b'keep existing output')
        self.assertEqual({p.name for p in out.iterdir()}, {'retain.txt'})
        with self.assertRaisesRegex(ValueError, 'separate from input'):
            self.invoke(self.root / 'native' / 'nested-output')

    def test_plot_data_has_gaps_not_failure_zero_and_preserves_real_zero(self):
        records = [normal(nodes=1), normal(nodes=10, status='deadline', checksum=None, iterations=0)]
        rows = summary.summarize(records, {'native': {'repeats': 1}})
        xs, ys, low, high = summary.series(rows, 'pulse', 'run_ns', .001)
        self.assertEqual(xs, [1, 10])
        self.assertEqual(ys[0], .1)
        self.assertTrue(math.isnan(ys[1]))
        self.assertEqual(summary.series(rows[:1], 'pulse', 'job_cpu_s')[1], [0.0])
        self.assertEqual((low, high), ([0, 0], [0, 0]))
        with self.assertRaisesRegex(ValueError, 'multiple identities'):
            summary.series(rows + rows, 'pulse', 'run_ns')

    def test_error_reports_are_retained_but_exit_unsuccessfully(self):
        for experiment in summary.EXPERIMENTS:
            self.dataset(experiment, [sample(experiment, status='error', error='reuse checksum mismatch', checksum=None)]
                         if experiment == 'native' else None)
        out = self.root / 'failure-report'
        self.assertEqual(self.invoke(out), 1)
        self.assertTrue((out / 'summary.csv').is_file())
        self.assertTrue((out / 'identity.json').is_file())

    def test_four_plot_routes_without_rendering_or_loading_matplotlib(self):
        # 用少量数据和内存中的绘图替身执行路由；父进程可能正在计时生产样本，
        # 因此有意推迟实际渲染。
        figures = []
        def subplots(nrows, ncols, **options):
            figure = MagicMock()
            axes = [[MagicMock() for _ in range(ncols)] for _ in range(nrows)]
            for axis_row in axes:
                for axis in axis_row:
                    axis.get_legend_handles_labels.return_value = (['handle'], ['label'])
            figures.append((figure, axes))
            return figure, axes
        pyplot = types.SimpleNamespace(rcParams={}, subplots=subplots, close=MagicMock())
        matplotlib = types.SimpleNamespace(use=MagicMock(), pyplot=pyplot)
        ticker = types.SimpleNamespace(ScalarFormatter=MagicMock())
        records = [normal(job_cpu_s=999), normal(nodes=10, status='deadline', checksum=None, iterations=0),
                   normal('resources', job_cpu_s=.125, sampled_tree_peak_rss_bytes=1024**2),
                   normal('resources', repeat=1, job_cpu_s=0, sampled_tree_peak_rss_bytes=1024**2),
                   normal('resources', repeat=2, job_cpu_s=.125, sampled_tree_peak_rss_bytes=1024**2),
                   normal('checkpoint'), normal('recovery')]
        identities = {e: manifest(e).get('identity', manifest(e)) for e in summary.EXPERIMENTS}
        rows = summary.summarize(records, identities)
        with patch.dict('sys.modules', {'matplotlib': matplotlib, 'matplotlib.pyplot': pyplot,
                                      'matplotlib.ticker': ticker}):
            names = summary.draw(rows, self.root)
        self.assertEqual(names, ['native_reuse.png', 'resources.png', 'observability.png', 'recovery.png'])
        self.assertEqual(len(figures), 4)
        for (figure, axes), name in zip(figures, names):
            self.assertEqual(figure.savefig.call_args.args[0], self.root / name)
        # 资源对比不能使用刻意设置为不等值的原生运行 CPU 总量。
        cpu_call = figures[1][1][0][0].errorbar.call_args
        self.assertEqual(cpu_call.args[:2], ([1], [.125]))
        self.assertEqual(cpu_call.kwargs['yerr'][0], [.125])
        self.assertEqual(figures[1][1][0][0].set_yscale.call_args.args, ('symlog',))
        self.assertEqual(figures[1][1][0][0].set_ylim.call_args.kwargs, {'bottom': 0})
        rss_call = figures[1][1][1][0].errorbar.call_args
        self.assertEqual(rss_call.args[:2], ([1], [1.0]))
        self.assertFalse(any(self.root.glob('*.png')))

    def test_no_plots_mode_does_not_import_matplotlib(self):
        for experiment in summary.EXPERIMENTS:
            self.dataset(experiment)
        with patch.object(summary, 'draw', side_effect=AssertionError('plotting forbidden during fixture checks')):
            self.assertEqual(self.invoke(self.root / 'no-plots'), 0)


if __name__ == '__main__':
    unittest.main()
