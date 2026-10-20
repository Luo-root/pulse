"""Bounded sampler regressions: mock CLI payloads and short contained processes only."""
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


TEST_ROOT = Path(os.environ.get('PULSE_PRODUCTION_TEST_TMP',
                 Path(__file__).resolve().parents[1] / 'results' / 'test-production-tools'))
CONFIG = dict(engine='pulse', mode='plain', shape='chain', nodes=1, rounds=1)


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
        # Keep validation evidence in the selected directory; never use the system temp directory.
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
            config.update(nodes=1, rounds=1)
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
        # Use the normal safety watchdog; Python startup wall time is not an accounting SLO.
        result = production.sample_process([sys.executable, '-B', '-c', parent_code], self.directory)
        self.assertFalse(result['hard_timeout'])
        self.assertEqual((result['returncode'], result['job_total_processes']), (0, 2))
        report = json.loads(result['stdout'])
        self.assertGreaterEqual(report['child_cpu_s'], .15)
        # Allow one Windows accounting tick, while excluding root-only CPU accounting.
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
            # This final clock read is before Job.close: cleanup cannot stand in for the watchdog.
            for name, process in processes.items():
                try:
                    process.wait(timeout=2)
                except production.psutil.TimeoutExpired:
                    self.fail(f'watchdog left {name} alive at the exact deadline')
                self.assertFalse(process.is_running())
                exited.append(name)
            return 1.0
        # Only the sampler clock is controlled; real Job assignment, RSS, CPU and kills still run.
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


if __name__ == '__main__':
    unittest.main()
