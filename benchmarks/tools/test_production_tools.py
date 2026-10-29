"""有界采样器回归测试：仅使用模拟 CLI 载荷和短时受控进程。"""
import contextlib
import io
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import run_production as production
import run_recovery as recovery
import summarize_production as summary


TEST_ROOT = Path(os.environ.get('PULSE_PRODUCTION_TEST_TMP',
                 Path(__file__).resolve().parents[1] / 'results' / 'test-production-tools'))
CONFIG = dict(engine='pulse', mode='plain', shape='chain', nodes=1, rounds=1, parallelism=4)


def payload(**overrides):
    value = dict(config=dict(CONFIG), status='ok', iterations=1, requested_iterations=1,
                 first_completed=True, first_ns=100, run_ns=100.0,
                 checksum=1164970750538294679, alloc_bytes_per_run=None,
                 alloc_objects_per_run=None)
    value.update(overrides)
    return value


def observed(value=None, **overrides):
    result = dict(returncode=0, stdout=json.dumps(payload() if value is None else value), stderr='',
                  hard_timeout=False, wall_s=.01, job_cpu_s=.01,
                  sampled_tree_peak_rss_bytes=100, rss_polls=1, job_total_processes=1)
    result.update(overrides)
    return result


class SamplerTests(unittest.TestCase):
    def setUp(self):
        TEST_ROOT.mkdir(parents=True, exist_ok=True)
        # 将验证证据保留在指定目录，始终避开系统临时目录。
        self.directory = Path(tempfile.mkdtemp(prefix=self._testMethodName + '-', dir=TEST_ROOT))
        self.binary = self.directory / 'mock-cli.bin'
        self.binary.write_bytes(b'mock CLI; never executed\n')

    def invoke(self, out, sampler, resume=False, repeats=3, extra=()):
        args = ['run_production', '--binary', str(self.binary), '--temporal-bin', str(self.binary),
                '--out', str(out), '--nodes', '1', '--rounds', '1', '--shapes', 'chain',
                '--modes', 'plain', '--engines', 'pulse', '--repeats', str(repeats), '--iterations', '1']
        if resume:
            args.append('--resume')
        with patch.object(sys, 'argv', args + list(extra)), \
                patch.object(production, 'sample_process', side_effect=sampler), \
                contextlib.redirect_stdout(io.StringIO()):
            return production.main()

    def rows(self, out):
        return [json.loads(line) for line in (out / 'raw.jsonl').read_text(encoding='utf-8').splitlines()]

    def test_first_interruption_persists_identity_and_keeps_every_attempt(self):
        out = self.directory / 'interrupted'
        def interrupt(*args):
            manifest = json.loads((out / 'manifest.json').read_text(encoding='utf-8'))
            self.assertEqual((manifest['records'], manifest['errors'], manifest['identity']['fixed_iterations']), (0, 0, 1))
            error = KeyboardInterrupt('operator interruption')
            error.sampler_stdout, error.sampler_stderr = 'partial stdout\n', 'partial stderr\n'
            raise error
        for _ in range(2):
            with self.assertRaises(KeyboardInterrupt):
                self.invoke(out, interrupt, resume=(out / 'manifest.json').exists())
        self.assertEqual(self.rows(out), [])
        manifest_only = self.directory / 'manifest-only'
        manifest_only.mkdir()
        (manifest_only / 'manifest.json').write_bytes((out / 'manifest.json').read_bytes())
        self.assertEqual(self.invoke(manifest_only, lambda *a: observed(), resume=True), 0)
        old_files = {p: p.read_bytes() for p in (out / 'cases').rglob('*') if p.is_file()}
        self.assertEqual(len(list((out / 'cases').iterdir())), 2)
        self.assertEqual(self.invoke(out, lambda *a: observed(), resume=True), 0)
        rows = self.rows(out)
        self.assertEqual([r['status'] for r in rows], ['ok'] * 3)
        self.assertTrue(rows[0]['case_dir'].endswith('pulse-plain-chain-1-1-0-attempt-3'))
        self.assertEqual(old_files, {p: p.read_bytes() for p in old_files})
        self.assertEqual((out / 'cases' / 'pulse-plain-chain-1-1-0' / 'stdout.json').read_text(), 'partial stdout\n')

    def test_later_interruption_retains_history_and_rejects_changed_identity(self):
        out = self.directory / 'history'
        calls = []
        def interrupted(*args):
            calls.append(1)
            if len(calls) == 2:
                raise KeyboardInterrupt('second case interrupted')
            return observed(payload(status='error', error='real historical failure', run_ns=None, iterations=0), returncode=1)
        with self.assertRaises(KeyboardInterrupt):
            self.invoke(out, interrupted)
        old_raw = (out / 'raw.jsonl').read_bytes()
        old_dir = out / 'cases' / 'pulse-plain-chain-1-1-1'
        before = {p: p.read_bytes() for p in old_dir.iterdir()}
        self.assertEqual(self.invoke(out, lambda *a: observed(), resume=True), 1)
        self.assertEqual([r['status'] for r in self.rows(out)], ['error', 'ok', 'ok'])
        self.assertTrue((out / 'raw.jsonl').read_bytes().startswith(old_raw))
        self.assertEqual(before, {p: p.read_bytes() for p in before})
        self.assertTrue(self.rows(out)[1]['case_dir'].endswith('-attempt-2'))
        never_run = lambda *a: self.fail('a completed case was repeated')
        self.assertEqual(self.invoke(out, never_run, resume=True), 1)
        manifest_path = out / 'manifest.json'
        original = json.loads(manifest_path.read_text(encoding='utf-8'))
        self.assertEqual(original['errors'], 1)
        missing_raw = self.directory / 'missing-history'
        missing_raw.mkdir()
        (missing_raw / 'manifest.json').write_text(json.dumps(original), encoding='utf-8')
        with self.assertRaisesRegex(ValueError, 'missing recorded cases'):
            self.invoke(missing_raw, never_run, resume=True)
        self.assertEqual(json.loads((missing_raw / 'manifest.json').read_text())['errors'], 1)
        raw_before = (out / 'raw.jsonl').read_bytes()
        for field in ('binary_sha256', 'sampler_sha256', 'containment_sha256', 'fixed_iterations'):
            with self.subTest(field=field):
                changed = json.loads(json.dumps(original))
                changed['identity'][field] = 'changed'
                manifest_path.write_text(json.dumps(changed), encoding='utf-8')
                with self.assertRaisesRegex(ValueError, 'identity'):
                    self.invoke(out, never_run, resume=True)
                self.assertEqual((out / 'raw.jsonl').read_bytes(), raw_before)
        manifest_path.write_text(json.dumps(original), encoding='utf-8')

    def assert_resume_rejected_without_writes(self, out):
        before = {str(p.relative_to(out)): p.read_bytes() if p.is_file() else None for p in out.rglob('*')}
        with patch.object(production, 'sample_process', side_effect=AssertionError('corrupt history was sampled')) as sampler:
            with self.assertRaises(ValueError):
                self.invoke(out, sampler, resume=True, repeats=2)
            sampler.assert_not_called()
        self.assertEqual(before, {str(p.relative_to(out)): p.read_bytes() if p.is_file() else None
                                  for p in out.rglob('*')})

    def test_resume_rejects_completed_corruption_without_writes(self):
        for change in ('empty', 'modified', 'missing-record', 'missing-file', 'records', 'errors', 'statuses', 'missing-hash'):
            with self.subTest(change=change):
                out = self.directory / change
                answers = iter([observed(payload(status='error', error='historical failure', iterations=0,
                                                 run_ns=None), returncode=1), observed()])
                self.assertEqual(self.invoke(out, lambda *a: next(answers), repeats=2), 1)
                raw, path = out / 'raw.jsonl', out / 'manifest.json'
                metadata = json.loads(path.read_text(encoding='utf-8'))
                if change == 'empty':
                    raw.write_bytes(b'')
                elif change == 'modified':
                    raw.write_bytes(raw.read_bytes().replace(b'historical failure', b'fabricated failure'))
                elif change == 'missing-record':
                    raw.write_bytes(raw.read_bytes().splitlines(keepends=True)[1])
                elif change == 'missing-file':
                    raw.rename(out / 'retained-raw.jsonl')
                else:
                    if change == 'missing-hash':
                        metadata.pop('raw_sha256')
                    else:
                        metadata[change] = {'ok': 2} if change == 'statuses' else 0
                    path.write_text(json.dumps(metadata), encoding='utf-8')
                self.assert_resume_rejected_without_writes(out)

    def test_resume_rejects_partial_corruption_without_writes(self):
        for change in ('empty', 'modified', 'bad-line', 'blank-line', 'records', 'errors', 'statuses', 'missing-prefix'):
            with self.subTest(change=change):
                out = self.directory / change
                calls = []
                def interrupted(*args):
                    calls.append(1)
                    if len(calls) == 2:
                        raise KeyboardInterrupt('interrupted after one confirmed failure')
                    return observed(payload(status='error', error='historical failure', iterations=0, run_ns=None),
                                    returncode=1)
                with self.assertRaises(KeyboardInterrupt):
                    self.invoke(out, interrupted, repeats=2)
                raw, path = out / 'raw.jsonl', out / 'manifest.json'
                metadata = json.loads(path.read_text(encoding='utf-8'))
                if change == 'empty':
                    raw.write_bytes(b'')
                elif change == 'modified':
                    raw.write_bytes(raw.read_bytes().replace(b'historical failure', b'fabricated failure'))
                elif change in ('bad-line', 'blank-line'):
                    with raw.open('ab') as stream:
                        stream.write(b'{' if change == 'bad-line' else b'\n')
                else:
                    if change == 'missing-prefix':
                        metadata.pop('raw_prefix', None)
                    else:
                        metadata[change] = {'ok': 1} if change == 'statuses' else 0
                    path.write_text(json.dumps(metadata), encoding='utf-8')
                self.assert_resume_rejected_without_writes(out)

    def test_resume_accepts_one_flushed_record_and_preserves_error(self):
        out = self.directory / 'flushed-before-manifest'
        write_manifest = production.write_manifest
        def interrupt_manifest(path, manifest, rows):
            if len(rows) == 1:
                raise KeyboardInterrupt('raw flushed before manifest replacement')
            return write_manifest(path, manifest, rows)
        with patch.object(production, 'write_manifest', side_effect=interrupt_manifest):
            with self.assertRaises(KeyboardInterrupt):
                self.invoke(out, lambda *a: observed(payload(status='error', error='historical failure',
                                                             iterations=0, run_ns=None), returncode=1), repeats=2)
        original_raw = (out / 'raw.jsonl').read_bytes()
        original_manifest = json.loads((out / 'manifest.json').read_text(encoding='utf-8'))
        self.assertIn('raw_prefix', original_manifest)
        self.assertEqual(original_manifest['records'], 0)
        self.assertEqual(original_manifest['raw_prefix']['bytes'], 0)
        calls = []
        def finish(*args):
            calls.append(1)
            return observed()
        self.assertEqual(self.invoke(out, finish, resume=True, repeats=2), 1)
        self.assertEqual(calls, [1])
        self.assertEqual([row['status'] for row in self.rows(out)], ['error', 'ok'])
        self.assertTrue((out / 'raw.jsonl').read_bytes().startswith(original_raw))
        metadata = json.loads((out / 'manifest.json').read_text(encoding='utf-8'))
        self.assertEqual((metadata['records'], metadata['errors']), (2, 1))
        self.assertEqual(metadata['raw_prefix']['sha256'], production.digest(out / 'raw.jsonl'))
        self.assertEqual(metadata['raw_sha256'], metadata['raw_prefix']['sha256'])

    def test_cli_parallelism_is_verified_and_retained(self):
        missing = {k: v for k, v in CONFIG.items() if k != 'parallelism'}
        for index, config in enumerate([missing] + [dict(CONFIG, parallelism=v) for v in (1, None, True, 4.0, '4')]):
            with self.subTest(config=config):
                out = self.directory / str(index)
                answers = iter([observed(payload(config=config)), observed()])
                self.assertEqual(self.invoke(out, lambda *a: next(answers), repeats=2), 1)
                first, second = self.rows(out)
                self.assertEqual((first['status'], second['status']), ('error', 'ok'))
                self.assertIn('parallelism', first['error'])
                self.assertNotIn('config', first)
                self.assertEqual(second['config'], CONFIG)
                self.assertEqual(second['parallelism'], 4)
                metadata = json.loads((out / 'manifest.json').read_text(encoding='utf-8'))
                normalized = summary.normalize(second, 'resources', metadata['identity'])
                self.assertEqual(normalized['parallelism'], second['config']['parallelism'])
        with self.assertRaisesRegex(ValueError, 'parallelism'):
            production.validate(dict(CONFIG, parallelism=1), payload(config=dict(CONFIG, parallelism=True)), 0)
        verified = production.validate(dict(CONFIG), payload(), 0)
        self.assertEqual(verified['config'], CONFIG)
        self.assertNotIn('gomaxprocs', verified['config'])

    def test_invalid_matrices_are_rejected_before_any_output(self):
        invalid = [('--repeats', '0'), ('--repeats', '-1'), ('--nodes', '0'), ('--nodes', '-1'),
                   ('--rounds', '0'), ('--rounds', '-1'), ('--iterations', '0'), ('--iterations', '-1')]
        invalid += [(flag, value) for flag in ('--nodes', '--rounds', '--shapes', '--modes', '--engines')
                    for value in ('', ' ', ',', '[]')]
        invalid += [('--' + field, value) for field, value in
                    (('shapes', 'chain, chain'), ('engines', 'pulse, pulse'), ('modes', 'plain, plain'),
                     ('nodes', '1, 01'), ('rounds', '1, 01'))]
        for index, extra in enumerate(invalid):
            with self.subTest(extra=extra):
                out = self.directory / str(index)
                with patch.object(production, 'digest', side_effect=AssertionError('binary hashed before validation')), \
                        contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as caught:
                        self.invoke(out, lambda *a: self.fail('invalid matrix started a process'), extra=extra)
                self.assertEqual(caught.exception.code, 2)
                self.assertFalse(out.exists())

    def test_real_matrix_roundtrip_and_equivalent_resume(self):
        cli = self.directory / 'fixture_cli.py'
        cli.write_text('import json,sys\n'
                       f'sys.path.insert(0, {str(Path(production.__file__).parent)!r})\n'
                       'from run_production import oracle\n'
                       'config={k:sys.argv[sys.argv.index("-"+k)+1] for k in ("engine","mode","shape")}\n'
                       'config.update({k:int(sys.argv[sys.argv.index("-"+k)+1]) for k in ("nodes","rounds","parallelism")})\n'
                       'print(json.dumps(dict(config=config,status="ok",iterations=1,requested_iterations=1,\n'
                       'first_completed=True,first_ns=100,run_ns=100,checksum=oracle(config["shape"],config["nodes"],config["rounds"]))))\n',
                       encoding='utf-8', newline='\n')
        real_sample = production.sample_process
        def sample(command, cwd):
            return real_sample([sys.executable, '-B', str(cli), *command[1:]], cwd, timeout=5)
        for field, spaced, canonical in (('shapes', 'chain, wide', 'chain,wide'),
                                         ('engines', 'pulse, eino', 'pulse,eino'), ('modes', ' plain ', 'plain')):
            with self.subTest(field=field):
                out = self.directory / field
                self.assertEqual(self.invoke(out, sample, repeats=1,
                                             extra=('--' + field, spaced, '--nodes', ' 01 ', '--rounds', ' 01 ')), 0)
                records, identity, details = summary.load_dataset('resources', out)
                self.assertEqual(identity[field], canonical)
                self.assertEqual((identity['nodes'], identity['rounds']), ('1', '1'))
                self.assertEqual(details['matrix'][field], canonical)
                self.assertEqual(len(summary.summarize(records, {'resources': identity})), len(records))
                original = (out / 'raw.jsonl').read_bytes()
                self.assertEqual(self.invoke(out, lambda *a: self.fail('completed sample repeated'), resume=True,
                                             repeats=1, extra=('--' + field, canonical)), 0)
                self.assertEqual((out / 'raw.jsonl').read_bytes(), original)

    def test_resume_legacy_matrix_identity_and_rejects_changes(self):
        for kind in ('string', 'list', 'scalar'):
            with self.subTest(kind=kind):
                out = self.directory / kind
                self.assertEqual(self.invoke(out, lambda *a: observed(), repeats=1), 0)
                path = out / 'manifest.json'
                metadata = json.loads(path.read_text(encoding='utf-8'))
                for field in ('nodes', 'rounds', 'shapes', 'engines', 'modes'):
                    value = metadata['identity'][field]
                    metadata['identity'][field] = (' ' + value + ' ' if kind == 'string' else
                        [int(value) if field in ('nodes', 'rounds') else value] if kind == 'list' else
                        int(value) if field in ('nodes', 'rounds') else value)
                path.write_text(json.dumps(metadata), encoding='utf-8')
                self.assertEqual(self.invoke(out, lambda *a: self.fail('legacy sample repeated'), repeats=1, resume=True), 0)
                summary.load_dataset('resources', out)
                before = {p: p.read_bytes() for p in out.rglob('*') if p.is_file()}
                for extra in (('--shapes', 'wide'), ('--engines', 'eino'), ('--modes', 'observe'),
                              ('--nodes', '2'), ('--rounds', '2')):
                    with self.subTest(extra=extra), self.assertRaisesRegex(ValueError, 'identity'):
                        self.invoke(out, lambda *a: self.fail('changed matrix sampled'), repeats=1, resume=True, extra=extra)
                    self.assertEqual(before, {p: p.read_bytes() for p in before})

    def test_bad_cli_payload_is_one_error_and_next_case_still_runs(self):
        bad = [[], payload(config=None), payload(config=dict(CONFIG, nodes=True)),
               payload(run_ns='100'), payload(run_ns=True), payload(run_ns=-1), payload(run_ns=0),
               payload(run_ns=float('nan')), payload(run_ns=float('inf')), payload(run_ns=-float('inf')),
               payload(extra=float('nan')), payload(run_ns=10**400), payload(iterations=True),
               payload(iterations=1.5), payload(iterations=-1), payload(requested_iterations=False),
               payload(first_completed=1), payload(first_ns=False), payload(server_setup_ns=-1),
               payload(alloc_bytes_per_run='0'), payload(alloc_objects_per_run=-1),
               payload(store_after_batch_bytes=True), payload(observations=[]),
               payload(observations={'completed': True}), payload(observations={'completed': -1}),
               payload(checksum=True), payload(checksum=1.0), payload(checksum=-1),
               payload(checksum=1 << 64), payload(status=[]), payload(error={})]
        for index, value in enumerate(bad):
            with self.subTest(index=index, value=value):
                out = self.directory / str(index)
                answers = iter([observed(value), observed()])
                self.assertEqual(self.invoke(out, lambda *a: next(answers), repeats=2), 1)
                rows = self.rows(out)
                self.assertEqual([r['status'] for r in rows], ['error', 'ok'])
                self.assertEqual(rows[0]['engine'], 'pulse')
                self.assertEqual((out / rows[0]['case_dir'] / 'stdout.json').read_text(), json.dumps(value))
                json.dumps(rows, allow_nan=False)

    def test_whitelist_null_allocations_progress_and_serial_fixed_work(self):
        out = self.directory / 'fixed-work'
        order = []
        def sample(command, cwd):
            config = {k: command[command.index('-' + k) + 1] for k in ('engine', 'mode', 'shape')}
            config.update(nodes=1, rounds=1, parallelism=4)
            order.append(config['engine'])
            for flag, expected in (('-iterations', '1'), ('-parallelism', '4'), ('-gomaxprocs', '4'), ('-timeout', '60s')):
                self.assertEqual(command[command.index(flag) + 1], expected)
            return observed(payload(config=config, engine='injected', nodes=999, repeat=999,
                                    case_dir='injected', command=['injected'], observations={'completed': 1}))
        self.assertEqual(self.invoke(out, sample, extra=('--engines', 'pulse,eino,temporal')), 0)
        self.assertEqual(order, ['pulse', 'eino', 'temporal', 'eino', 'temporal', 'pulse', 'temporal', 'pulse', 'eino'])
        for row in self.rows(out):
            self.assertEqual(row['nodes'], 1)
            self.assertIn(row['repeat'], range(3))
            self.assertNotEqual(row['command'], ['injected'])
            self.assertNotEqual(row['case_dir'], 'injected')
            self.assertIsNone(row['alloc_bytes_per_run'])
            self.assertIsNone(row['alloc_objects_per_run'])
            self.assertEqual((row['iterations'], row['requested_iterations'], row['first_completed']), (1, 1, True))

    def test_limits_are_not_repeated_and_sampler_errors_remain_errors(self):
        for status in ('deadline', 'hard_timeout', 'not_equivalent'):
            with self.subTest(status=status):
                out = self.directory / status
                calls = []
                def limit(*args):
                    calls.append(1)
                    return observed(payload(status='deadline' if status == 'hard_timeout' else status,
                                            checksum=None, run_ns=None, iterations=0),
                                    returncode=1, hard_timeout=(status == 'hard_timeout'))
                self.assertEqual(self.invoke(out, limit), 0)
                self.assertEqual([r['status'] for r in self.rows(out)], [status, 'not_repeated', 'not_repeated'])
                self.assertEqual(len(calls), 1)
        errors = [OSError('Job query failed'), RuntimeError('reader failed'),
                  production.psutil.AccessDenied(123), subprocess.TimeoutExpired(['mock'], 1)]
        for index, error in enumerate(errors):
            with self.subTest(error=error):
                out = self.directory / ('error-' + str(index))
                calls = []
                error.sampler_stdout, error.sampler_stderr = 'retained partial stdout', 'retained partial stderr'
                def fail_once(*args):
                    calls.append(1)
                    if len(calls) == 1:
                        raise error
                    return observed()
                self.assertEqual(self.invoke(out, fail_once, repeats=2), 1)
                rows = self.rows(out)
                self.assertEqual([r['status'] for r in rows], ['error', 'ok'])
                case = out / rows[0]['case_dir']
                self.assertEqual((case / 'stdout.json').read_text(), error.sampler_stdout)
                self.assertEqual((case / 'stderr.txt').read_text(), error.sampler_stderr)
                self.assertIn(type(error).__name__, (case / 'sampler-error.txt').read_text())

    def test_exited_root_keeps_output_exit_and_job_cpu_with_null_rss(self):
        created = []
        real_popen, real_process = subprocess.Popen, production.psutil.Process
        def capture(*args, **kwargs):
            process = real_popen(*args, **kwargs)
            created.append(process)
            return process
        def after_exit(pid):
            created[0].wait(timeout=3)
            return real_process(pid)
        with patch.object(subprocess, 'Popen', side_effect=capture), \
                patch.object(production.psutil, 'Process', side_effect=after_exit):
            result = production.sample_process([sys.executable, '-B', '-c', 'print(123)'], self.directory, timeout=3)
        self.assertEqual((result['returncode'], result['stdout'].strip(), result['hard_timeout']), (0, '123', False))
        self.assertEqual((result['sampled_tree_peak_rss_bytes'], result['rss_polls']), (None, 0))
        if os.name == 'nt':
            self.assertEqual(result['job_total_processes'], 1)
            self.assertGreaterEqual(result['job_cpu_s'], 0)

    @unittest.skipUnless(os.name == 'nt', 'Windows Job containment')
    def test_job_rejection_kills_unassigned_root_and_preserves_original_error(self):
        created = []
        real_popen = subprocess.Popen
        rejection = OSError('AssignProcessToJobObject rejected this sample')
        class RejectJob(production.WindowsJob):
            def attach_and_resume(self, process):
                raise rejection
        def capture(*args, **kwargs):
            process = real_popen(*args, **kwargs)
            created.append(process)
            return process
        try:
            with patch.object(production, 'WindowsJob', RejectJob), patch.object(subprocess, 'Popen', side_effect=capture):
                with self.assertRaises(OSError) as caught:
                    production.sample_process([sys.executable, '-B', '-c', 'import time; time.sleep(30)'], self.directory, timeout=.3)
            self.assertIs(caught.exception, rejection)
            self.assertIsNotNone(created[0].poll())
            self.assertFalse(production.psutil.pid_exists(created[0].pid))
        finally:
            for process in created:
                if process.poll() is None:
                    process.kill()
                process.wait(timeout=3)

    @unittest.skipUnless(os.name == 'nt', 'Windows Job containment')
    def test_job_accounts_exited_child_cpu(self):
        child_code = ('import json,os,time\nstart=time.process_time()\n'
                      'while time.process_time()-start<.15: pass\n'
                      'print(json.dumps(dict(child_pid=os.getpid(),child_cpu_s=time.process_time()-start)))')
        parent_code = ('import json,os,subprocess,sys,time\n'
                       f'p=subprocess.Popen([sys.executable,"-B","-c",{child_code!r}],stdout=subprocess.PIPE,text=True)\n'
                       'out,_=p.communicate()\n'
                       'if p.returncode: sys.exit(p.returncode)\n'
                       'report=json.loads(out)\n'
                       'report.update(parent_pid=os.getpid(),parent_cpu_s=time.process_time())\n'
                       'print(json.dumps(report))')
        # 使用正常的安全看门狗；Python 启动所用的墙钟时间不属于计账 SLO。
        result = production.sample_process([sys.executable, '-B', '-c', parent_code], self.directory)
        self.assertFalse(result['hard_timeout'])
        self.assertEqual((result['returncode'], result['job_total_processes']), (0, 2))
        report = json.loads(result['stdout'])
        self.assertGreaterEqual(report['child_cpu_s'], .15)
        # 允许一个 Windows 计账时钟节拍的误差，同时排除只统计根进程 CPU 的情况。
        self.assertGreaterEqual(result['job_cpu_s'], report['child_cpu_s'] + report['parent_cpu_s'] - .02)
        self.assertGreater(result['rss_polls'], 0)
        self.assertGreater(result['sampled_tree_peak_rss_bytes'], 0)
        for pid in (report['parent_pid'], report['child_pid']):
            self.assertFalse(production.psutil.pid_exists(pid))

    @unittest.skipUnless(os.name == 'nt', 'Windows Job containment')
    def test_watchdog_kills_ready_tree_at_deadline(self):
        child_ready, parent_ready = self.directory / 'child-ready.json', self.directory / 'parent-ready.json'
        child_code = ('import json,os,sys,time\nfrom pathlib import Path\n'
                      'ready=Path(sys.argv[1])\npending=ready.with_suffix(".pending")\n'
                      'pending.write_text(json.dumps(dict(pid=os.getpid(),ppid=os.getppid())))\n'
                      'pending.replace(ready)\ntime.sleep(30)')
        parent_code = ('import json,os,subprocess,sys,time\nfrom pathlib import Path\n'
                       f'p=subprocess.Popen([sys.executable,"-B","-c",{child_code!r},sys.argv[1]])\n'
                       'while not Path(sys.argv[1]).exists():\n'
                       '    if p.poll() is not None: raise RuntimeError("child exited before readiness")\n'
                       '    time.sleep(.01)\n'
                       'ready=Path(sys.argv[2])\npending=ready.with_suffix(".pending")\n'
                       'pending.write_text(json.dumps(dict(pid=os.getpid(),child_pid=p.pid)))\n'
                       'pending.replace(ready)\np.wait(timeout=30)')
        processes, checkpoints, exited = {}, [], []
        before_deadline = math.nextafter(1.0, 0.0)
        setup_deadline = time.monotonic() + 30
        started = False
        def clock():
            nonlocal started
            if not started:
                started = True
                return 0.0
            if not processes:
                self.assertLess(time.monotonic(), setup_deadline, 'parent/child readiness barrier timed out')
                if not parent_ready.exists():
                    return 0.0
                parent, child = json.loads(parent_ready.read_text()), json.loads(child_ready.read_text())
                self.assertEqual((parent['child_pid'], parent['pid']), (child['pid'], child['ppid']))
                processes.update(parent=production.psutil.Process(parent['pid']),
                                 child=production.psutil.Process(child['pid']))
                self.assertEqual(processes['child'].ppid(), processes['parent'].pid)
            if len(checkpoints) < 2:
                for process in processes.values():
                    self.assertTrue(process.is_running(), 'tree exited before watchdog boundary')
                value = (before_deadline, 1.0)[len(checkpoints)]
                checkpoints.append(value)
                return value
            # 最后一次读取时钟发生在 Job.close 之前：不能让清理动作代替看门狗。
            for name, process in processes.items():
                try:
                    process.wait(timeout=2)
                except production.psutil.TimeoutExpired:
                    self.fail(f'watchdog left {name} alive at the exact deadline')
                self.assertFalse(process.is_running())
                exited.append(name)
            return 1.0
        # 只控制采样器时钟；真实的 Job 归属设置、RSS 和 CPU 采集以及进程终止仍照常执行。
        with patch.object(production, 'time', SimpleNamespace(perf_counter=clock)):
            result = production.sample_process([sys.executable, '-B', '-c', parent_code,
                                                str(child_ready), str(parent_ready)], self.directory, timeout=1)
        self.assertEqual(checkpoints, [before_deadline, 1.0])
        self.assertEqual(exited, ['parent', 'child'])
        self.assertTrue(result['hard_timeout'])
        self.assertEqual((result['returncode'], result['job_total_processes']), (124, 2))
        self.assertGreater(result['job_cpu_s'], 0)
        self.assertGreater(result['rss_polls'], 0)
        self.assertGreater(result['sampled_tree_peak_rss_bytes'], 0)


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        TEST_ROOT.mkdir(parents=True, exist_ok=True)
        self.directory = Path(tempfile.mkdtemp(prefix=self._testMethodName + '-', dir=TEST_ROOT))
        self.binary = self.directory / 'mock-cli.bin'
        self.binary.write_bytes(b'mock CLI; never executed\n')

    def test_invalid_matrix_is_rejected_before_any_output(self):
        invalid = [('--repeats', '0'), ('--repeats', '-1')]
        invalid += [('--nodes', value) for value in ('0', '-1', '1', '', ' ', ',', '[]')]
        for index, extra in enumerate(invalid):
            with self.subTest(extra=extra):
                out = self.directory / str(index)
                args = ['run_recovery', '--binary', str(self.binary), '--temporal-bin', str(self.binary),
                        '--out', str(out)] + list(extra)
                with patch.object(sys, 'argv', args), \
                        patch.object(recovery.hashlib, 'sha256', side_effect=AssertionError('binary hashed before validation')), \
                        patch.object(recovery, 'Case', side_effect=AssertionError('invalid matrix started a case')), \
                        contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as caught:
                        recovery.main()
                self.assertEqual(caught.exception.code, 2)
                self.assertFalse(out.exists())

    def test_real_cli_rejects_zero_negative_and_empty_matrix(self):
        for script in ('run_production.py', 'run_recovery.py'):
            for index, extra in enumerate((('--repeats', '0'), ('--repeats', '-1'), ('--nodes', ''))):
                with self.subTest(script=script, extra=extra):
                    out = self.directory / (script + str(index))
                    command = [sys.executable, '-B', str(Path(__file__).with_name(script)),
                               '--binary', str(self.binary), '--temporal-bin', str(self.binary), '--out', str(out), *extra]
                    result = subprocess.run(command, capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertIn(extra[0][2:], result.stderr)
                    self.assertEqual(result.stdout, '')
                    self.assertFalse(out.exists())

    @unittest.skipUnless(os.name == 'nt', 'Windows Job containment')
    def test_timeout_keeps_stdout_and_reaps_real_tree(self):
        root = self.directory / 'timeout'
        root.mkdir()
        ready = root / 'ready.json'
        child_code = 'import time; time.sleep(30)'
        parent_code = ('import json,os,subprocess,sys,time\nfrom pathlib import Path\n'
                       f'child=subprocess.Popen([sys.executable,"-B","-c",{child_code!r}])\n'
                       'print(123,flush=True)\n'
                       'Path(sys.argv[1]).write_text(json.dumps(dict(root=os.getpid(),child=child.pid)))\n'
                       'time.sleep(30)')
        case = recovery.Case(root, Path(sys.executable), Path(sys.executable), 10, 'pulse', 0)
        case.command = lambda operation: [sys.executable, '-B', '-c', parent_code, str(ready)]
        launch = case.launch
        created = []
        def ready_launch(*args):
            process = launch(*args)
            created.append(process)
            deadline = time.monotonic() + 5
            while not ready.exists():
                self.assertLess(time.monotonic(), deadline, 'fixture did not become ready')
                self.assertIsNone(process.poll())
                time.sleep(.01)
            # 先确认真实输出与子进程已产生，再缩短本次 communicate/wait 的预算。
            case.start = time.perf_counter() - 64.95
            return process
        case.launch = ready_launch
        try:
            with self.assertRaises(subprocess.TimeoutExpired):
                case.call('resume')
            stdout = root / '01-resume.json'
            self.assertTrue(stdout.is_file(), 'timeout discarded captured stdout')
            self.assertEqual(stdout.read_text(encoding='utf-8').strip(), '123')
            self.assertIsNotNone(created[0].poll())
            self.assertFalse(case.timer.is_alive())
            self.assertIsNone(case.job.handle)
            for pid in json.loads(ready.read_text()).values():
                try:
                    production.psutil.Process(pid).wait(timeout=3)
                except production.psutil.NoSuchProcess:
                    pass
                self.assertFalse(production.psutil.pid_exists(pid))
        finally:
            case.close()
            for process in created:
                if process.poll() is None:
                    process.kill()
                process.wait(timeout=3)

    def test_call_preserves_original_timeout_when_cleanup_fails(self):
        original = subprocess.TimeoutExpired(['fixture'], .1)
        cleanup_error = OSError('fixture cleanup error')
        case = recovery.Case.__new__(recovery.Case)
        case.root, case.counter, case.start = self.directory, 0, time.perf_counter()
        from unittest.mock import Mock
        process = Mock()
        process.wait.side_effect = [original, None]
        process.communicate.side_effect = original
        process.poll.return_value = None
        case.timer = Mock()
        case.job = Mock()
        case.job.close.side_effect = cleanup_error
        out, err = io.StringIO('123\n'), io.StringIO()
        case.processes = [(process, out, err)]
        case.command = lambda operation: ['fixture']
        case.launch = lambda *args: process
        (self.directory / '01-resume.json').write_text('123\n', encoding='utf-8')
        with self.assertRaises(subprocess.TimeoutExpired) as caught:
            case.call('resume')
        self.assertIs(caught.exception, original)
        self.assertTrue(any('fixture cleanup error' in note for note in getattr(original, '__notes__', ())))
        process.kill.assert_called_once()
        self.assertTrue(out.closed)
        self.assertTrue(err.closed)


if __name__ == '__main__':
    unittest.main()
