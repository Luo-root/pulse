#!/usr/bin/env python3
"""固定三框架、串行轮换；raw/log/DB 只写 ignored results 或仓库外目录。"""
import argparse
import ctypes
import hashlib
import json
import math
import os
from pathlib import Path
import signal
import subprocess
import threading
import time
import traceback
from datetime import datetime, timezone

import psutil
from run_matrix import WindowsJob

FIELDS = ('engine', 'mode', 'shape', 'nodes', 'rounds', 'repeat')
METRICS = ('status', 'error', 'iterations', 'requested_iterations', 'first_completed',
           'server_setup_ns', 'adapter_setup_ns',
           'first_ns', 'run_ns', 'alloc_bytes_per_run', 'alloc_objects_per_run',
           'checksum', 'observations', 'store_before_bytes', 'store_after_first_bytes', 'store_after_batch_bytes')
LIMITS = {'deadline', 'hard_timeout', 'not_equivalent'}


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def oracle(shape, n, rounds):
    mask = (1 << 64) - 1
    preds = [[] for _ in range(n)]
    if shape == 'chain':
        for i in range(1, n): preds[i] = [i - 1]
    elif shape == 'diamonds':
        previous, first, width = [], 0, 1
        while first < n:
            layer = list(range(first, min(n, first + width)))
            for i in layer: preds[i] = list(previous)
            previous, first, width = layer, first + len(layer), 3 - width
    elif shape != 'wide':
        raise ValueError(shape)
    values, used = [], set()
    for i, p in enumerate(preds):
        used.update(p)
        x = ((1 + sum(values[j] for j in p)) & mask) ^ (((i + 1) * 0x9e3779b97f4a7c15) & mask)
        for _ in range(rounds):
            x ^= x >> 12
            x ^= (x << 25) & mask
            x ^= x >> 27
            x = x * 0x2545f4914f6cdd1d & mask
        values.append(x)
    return sum(v for i, v in enumerate(values) if i not in used) & mask


def job_cpu(job):
    """Windows Job accounting includes terminated descendants, unlike polling CPU."""
    from ctypes import wintypes as w
    class Accounting(ctypes.Structure):
        _fields_ = [('user', ctypes.c_int64), ('kernel', ctypes.c_int64),
                    ('period_user', ctypes.c_int64), ('period_kernel', ctypes.c_int64),
                    ('faults', w.DWORD), ('total', w.DWORD), ('active', w.DWORD), ('terminated', w.DWORD)]
    query = job.kernel.QueryInformationJobObject
    query.argtypes = [w.HANDLE, ctypes.c_int, ctypes.c_void_p, w.DWORD, ctypes.c_void_p]
    query.restype = w.BOOL
    info = Accounting()
    if not query(job.handle, 1, ctypes.byref(info), ctypes.sizeof(info), None):
        raise ctypes.WinError(ctypes.get_last_error())
    return (info.user + info.kernel) / 10_000_000, info.total


def sample_process(command, cwd, timeout=65):
    job = WindowsJob() if os.name == 'nt' else None
    process = reader = None
    result = {}
    failure = None
    start = time.perf_counter()
    peak_rss = None
    polls = 0
    hard_timeout = False
    try:
        process = subprocess.Popen(command, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   env=dict(os.environ, GOMAXPROCS='4'),
                                   text=True, encoding='utf-8', errors='replace',
                                   creationflags=0x4 if job else 0, start_new_session=not job)
        if job: job.attach_and_resume(process)
        def communicate():
            try:
                result['stdout'], result['stderr'] = process.communicate()
            except Exception as error:
                result['reader_error'] = error
        reader = threading.Thread(target=communicate, daemon=True)
        reader.start()
        try:
            root = psutil.Process(process.pid)
        except psutil.NoSuchProcess:
            root = None  # A short CLI may exit before the first RSS handle is opened.
        while reader.is_alive():
            rss, sampled = 0, False
            tree = [root] if root is not None else []
            if root is not None:
                try: tree += root.children(recursive=True)
                except psutil.Error: pass
            for p in tree:
                try:
                    rss += p.memory_info().rss
                    sampled = True
                except psutil.Error:
                    pass
            if sampled:
                peak_rss = max(peak_rss or 0, rss)
                polls += 1
            if time.perf_counter() - start >= timeout:
                hard_timeout = True
                if job:
                    if not job.kernel.TerminateJobObject(job.handle, 124):
                        raise ctypes.WinError(ctypes.get_last_error())
                else: os.killpg(process.pid, signal.SIGKILL)
                break
            reader.join(0.025)
        reader.join(10)
        if reader.is_alive(): raise RuntimeError('contained process failed to exit')
        if 'reader_error' in result: raise result['reader_error']
        cpu, total = job_cpu(job) if job else (None, None)
        return dict(returncode=process.returncode, stdout=result.get('stdout', ''),
                    stderr=result.get('stderr', ''), hard_timeout=hard_timeout,
                    wall_s=time.perf_counter() - start, job_cpu_s=cpu,
                    sampled_tree_peak_rss_bytes=peak_rss, rss_polls=polls,
                    job_total_processes=total)
    except BaseException as error:
        failure = error
        raise
    finally:
        cleanup_errors = []
        def cleanup(action):
            try: action()
            except Exception as error: cleanup_errors.append(error)
        if job: cleanup(job.close)
        if process is not None:
            if not job:
                try: os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError: pass
                except OSError as error: cleanup_errors.append(error)
            # Assignment can fail before the suspended root belongs to the Job.
            if process.poll() is None: cleanup(process.kill)
            cleanup(lambda: process.wait(timeout=10))
            if reader is not None:
                reader.join(10)
                if reader.is_alive(): cleanup_errors.append(RuntimeError('output reader failed to exit'))
            else:
                cleanup(lambda: result.update(zip(('stdout', 'stderr'), process.communicate(timeout=10))))
        if failure is not None or cleanup_errors:
            error = failure if failure is not None else cleanup_errors[0]
            error.sampler_stdout = result.get('stdout', '')
            error.sampler_stderr = result.get('stderr', '')
            for extra in cleanup_errors:
                error.add_note(f'Process cleanup: {type(extra).__name__}: {extra}')
            if failure is None: raise error


def validate(row, payload, returncode):
    if not isinstance(payload, dict): raise ValueError('CLI output must be a JSON object')
    config = payload.get('config')
    if not isinstance(config, dict): raise ValueError('CLI config must be a JSON object')
    # Reject nonstandard JSON numbers even in fields outside the metrics whitelist.
    json.dumps(payload, allow_nan=False)
    for k in ('engine', 'mode', 'shape', 'nodes', 'rounds'):
        if type(config.get(k)) is not type(row[k]) or config.get(k) != row[k]:
            raise ValueError('CLI identity mismatch: ' + k)
    status = payload.get('status')
    if status not in ('ok', 'deadline', 'error', 'not_equivalent'): raise ValueError('unknown CLI status')
    if 'error' in payload and not isinstance(payload['error'], str): raise ValueError('CLI error must be a string')
    if returncode != 0 and status == 'ok': raise ValueError('nonzero exit with ok JSON')
    nullable = {'first_ns', 'alloc_bytes_per_run', 'alloc_objects_per_run', 'checksum',
                'store_before_bytes', 'store_after_first_bytes', 'store_after_batch_bytes'}
    for k in ('iterations', 'requested_iterations', 'server_setup_ns', 'adapter_setup_ns', *nullable):
        if k not in payload or (payload[k] is None and k in nullable): continue
        if type(payload[k]) is not int or payload[k] < 0:
            raise ValueError('CLI metric must be a nonnegative integer: ' + k)
    if payload.get('checksum') is not None and payload['checksum'] > (1 << 64) - 1:
        raise ValueError('CLI checksum must be a uint64')
    run_ns = payload.get('run_ns')
    if run_ns is not None:
        if type(run_ns) not in (int, float) or not math.isfinite(run_ns) or run_ns < 0:
            raise ValueError('CLI run_ns must be finite and nonnegative')
    if 'first_completed' in payload and type(payload['first_completed']) is not bool:
        raise ValueError('CLI first_completed must be a boolean')
    if 'observations' in payload:
        observations = payload['observations']
        if not isinstance(observations, dict) or any(type(v) is not int or v < 0 for v in observations.values()):
            raise ValueError('CLI observations must contain nonnegative integer counts')
    if status == 'ok':
        if payload.get('checksum') != oracle(row['shape'], row['nodes'], row['rounds']):
            raise ValueError('independent Python checksum mismatch')
        if not payload.get('iterations') or run_ns is None or run_ns <= 0:
            raise ValueError('missing completed timing')
    return {k: payload[k] for k in METRICS if k in payload}


def write_manifest(path, manifest, rows):
    manifest['records'] = len(rows)
    manifest['errors'] = sum(r['status'] == 'error' for r in rows)
    manifest['statuses'] = {s: sum(r['status'] == s for r in rows) for s in sorted({r['status'] for r in rows})}
    pending = path.with_suffix('.json.tmp')
    pending.write_text(json.dumps(manifest, indent=2)+'\n', encoding='utf-8')
    pending.replace(path)


def new_case_directory(out, label):
    attempt = 1
    while True:
        case = out/'cases'/(label if attempt == 1 else f'{label}-attempt-{attempt}')
        try:
            case.mkdir(parents=True, exist_ok=False)
            return case
        except FileExistsError:
            attempt += 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', required=True, type=Path)
    parser.add_argument('--temporal-bin', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    parser.add_argument('--repeats', type=int, default=5)
    parser.add_argument('--nodes', default='1,10,100,1000,10000')
    parser.add_argument('--rounds', default='1,256')
    parser.add_argument('--shapes', default='chain,wide,diamonds')
    parser.add_argument('--modes', default='plain,observe')
    parser.add_argument('--engines', default='pulse,eino,temporal')
    parser.add_argument('--iterations', type=int, default=None, help='fixed reuse batch; resource comparisons use 1 for every engine')
    parser.add_argument('--resume', action='store_true')
    args = parser.parse_args()
    binary, temporal = args.binary.resolve(), args.temporal_bin.resolve()
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=True)
    identity = dict(repeats=args.repeats, nodes=args.nodes, rounds=args.rounds, shapes=args.shapes,
                    modes=args.modes, engines=args.engines, binary_sha256=digest(binary),
                    temporal_sha256=digest(temporal), tool='run_production.py',
                    sampler_sha256=digest(__file__), containment_sha256=digest(Path(__file__).with_name('run_matrix.py')),
                    budget_s=60, watchdog_s=65,
                    parallelism=4, gomaxprocs=4, fixed_iterations=args.iterations)
    manifest_path, raw = out/'manifest.json', out/'raw.jsonl'
    if args.resume:
        manifest = json.loads(manifest_path.read_text(encoding='utf-8'))
        if not isinstance(manifest, dict) or manifest.get('identity') != identity:
            raise ValueError('resume identity/binary changed')
        if raw.exists():
            rows = [json.loads(line) for line in raw.read_text(encoding='utf-8').splitlines()]
        elif manifest.get('records', 0):
            raise ValueError('resume raw.jsonl missing recorded cases')
        else:
            rows = []
    else:
        if manifest_path.exists() or raw.exists(): raise ValueError('existing run: use --resume or a fresh output')
        manifest = dict(identity=identity, started_at=datetime.now(timezone.utc).isoformat())
        rows = []
    manifest.pop('ended_at', None)
    manifest.pop('raw_sha256', None)
    write_manifest(manifest_path, manifest, rows)  # Persist identity before the first case can be interrupted.
    seen = {tuple(r[k] for k in FIELDS) for r in rows}
    limited = {tuple(r[k] for k in FIELDS[:-1]) for r in rows if r['status'] in LIMITS}
    engines = args.engines.split(',')
    cases = [(mode, shape, int(n), int(rounds)) for mode in args.modes.split(',')
             for shape in args.shapes.split(',') for n in args.nodes.split(',') for rounds in args.rounds.split(',')]
    with raw.open('a', encoding='utf-8', newline='\n') as stream:
        for repeat in range(args.repeats):
            for case_index, (mode, shape, n, rounds) in enumerate(cases):
                offset = (repeat + case_index) % len(engines)
                for engine in engines[offset:] + engines[:offset]:
                    row = dict(engine=engine, mode=mode, shape=shape, nodes=n, rounds=rounds, repeat=repeat)
                    key = tuple(row[k] for k in FIELDS)
                    if key in seen: continue
                    group = key[:-1]
                    if group in limited:
                        row['status'] = 'not_repeated'
                        row['reason'] = 'Earlier whole-case limit or non-equivalent mode; no value fabricated.'
                    else:
                        label = '-'.join(map(str, key))
                        case = new_case_directory(out, label)
                        row['case_dir'] = str(case.relative_to(out))
                        iterations = args.iterations if args.iterations is not None else (1 if engine == 'temporal' or mode == 'checkpoint' else max(1, min(1000, 100000//n)))
                        command = [str(binary), '-engine', engine, '-mode', mode, '-shape', shape,
                                   '-nodes', str(n), '-rounds', str(rounds), '-parallelism', '4',
                                   '-gomaxprocs', '4', '-iterations', str(iterations), '-timeout', '60s',
                                   '-store', str(case/'state.sqlite'), '-temporal-bin', str(temporal)]
                        row['command'] = command
                        try:
                            observed = sample_process(command, binary.parent)
                        except BaseException as error:
                            (case/'stdout.json').write_text(getattr(error, 'sampler_stdout', ''), encoding='utf-8')
                            (case/'stderr.txt').write_text(getattr(error, 'sampler_stderr', ''), encoding='utf-8')
                            (case/'sampler-error.txt').write_text(
                                ''.join(traceback.format_exception(type(error), error, error.__traceback__)), encoding='utf-8')
                            if not isinstance(error, Exception): raise  # Keep interruptions resumable and unrecorded.
                            row.update(status='error', error=f'{type(error).__name__}: {error}')
                        else:
                            stdout = observed.pop('stdout')
                            (case/'stdout.json').write_text(stdout, encoding='utf-8')
                            (case/'stderr.txt').write_text(observed.pop('stderr'), encoding='utf-8')
                            row.update(observed)
                            try:
                                if row.pop('hard_timeout'):
                                    row.update(status='hard_timeout', error='Parent watchdog killed contained process tree')
                                else:
                                    row.update(validate(row, json.loads(stdout), row['returncode']))
                            except (ValueError, KeyError, TypeError, OverflowError) as error:
                                row.update(status='error', error=f'{type(error).__name__}: {error}')
                        row['database_bytes'] = sum(p.stat().st_size for p in case.glob('state.sqlite*') if p.is_file() and not p.name.endswith('.log'))
                        if row['status'] in LIMITS: limited.add(group)
                    rows.append(row)
                    stream.write(json.dumps(row, ensure_ascii=False, allow_nan=False) + '\n')
                    stream.flush()
                    seen.add(key)
                    write_manifest(manifest_path, manifest, rows)
                    if row['status'] != 'not_repeated': print(f"{len(rows)} {label} {row['status']} {row.get('run_ns')}", flush=True)
    manifest['ended_at'] = datetime.now(timezone.utc).isoformat()
    manifest['raw_sha256'] = digest(raw)
    write_manifest(manifest_path, manifest, rows)
    print(json.dumps(manifest, indent=2), flush=True)
    return int(any(r['status'] == 'error' for r in rows))


if __name__ == '__main__':
    raise SystemExit(main())
