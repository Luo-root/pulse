#!/usr/bin/env python3
"""固定三框架、串行轮换；raw/log/DB 只写被忽略的 results 目录或仓库外目录。"""
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


def parse_matrix(value, name, minimum=None, allowed=None):
    if not isinstance(value, str) or not value.strip():
        raise ValueError(name + ' must be a nonempty comma-separated matrix')
    values = [item.strip() for item in value.split(',')]
    if any(not item for item in values):
        raise ValueError(name + ' contains an empty matrix entry')
    if minimum is not None:
        try:
            values = [int(item) for item in values]
        except ValueError as error:
            raise ValueError(name + ' must contain integers') from error
        if any(item < minimum for item in values):
            raise ValueError(f'{name} must contain integers >= {minimum}')
    if allowed is not None and any(item not in allowed for item in values):
        raise ValueError(name + ' contains an unsupported matrix entry')
    if len(set(values)) != len(values):
        raise ValueError(name + ' contains duplicate matrix entries')
    return values


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
    """Windows Job 计账包含已终止后代进程的 CPU，用轮询获取 CPU 则无法覆盖这些进程。"""
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
            root = None  # 短时 CLI 可能在首次打开 RSS 句柄前就已退出。
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
            # 将挂起的根进程加入 Job 的操作可能失败，此时该进程尚不属于 Job。
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
    # 即使字段不在指标白名单中，也要拒绝其中的非标准 JSON 数字。
    json.dumps(payload, allow_nan=False)
    config_fields = ('engine', 'mode', 'shape', 'nodes', 'rounds', 'parallelism')
    for k in config_fields:
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
    return dict({k: payload[k] for k in METRICS if k in payload},
                config={k: config[k] for k in config_fields}, parallelism=config['parallelism'])


def resume_rows(raw, manifest, expected_keys):
    """一次核验已确认前缀；最多接纳一条先 flush、后落清单的完整记录。"""
    recorded = manifest.get('records')
    if type(recorded) is not int or recorded < 0:
        raise ValueError('resume manifest records must be a nonnegative integer')
    if not raw.exists() and recorded:
        raise ValueError('resume raw.jsonl missing recorded cases')
    data = raw.read_bytes() if raw.exists() else b''
    final_hash = manifest.get('raw_sha256')
    if 'ended_at' in manifest and final_hash is None:
        raise ValueError('resume completed manifest missing raw_sha256')
    prefix = manifest.get('raw_prefix')
    if prefix is None:
        # 旧版完整报告可用最终摘要核验；无摘要的非空中断记录不能安全恢复。
        if recorded and final_hash is None:
            raise ValueError('resume missing confirmed raw prefix')
        prefix_size = len(data) if final_hash is not None else 0
    else:
        if not isinstance(prefix, dict) or type(prefix.get('bytes')) is not int or prefix['bytes'] < 0:
            raise ValueError('resume invalid raw prefix')
        prefix_size = prefix['bytes']
        if prefix_size > len(data):
            raise ValueError('resume raw.jsonl missing confirmed prefix bytes')
    raw_hash = hashlib.sha256(data[:prefix_size])
    if prefix is not None and prefix.get('sha256') != raw_hash.hexdigest():
        raise ValueError('resume confirmed raw prefix sha256 mismatch')
    raw_hash.update(data[prefix_size:])
    if final_hash is not None and final_hash != raw_hash.hexdigest():
        raise ValueError('resume raw_sha256 mismatch')
    rows, seen, offset = [], set(), 0
    confirmed = 0 if prefix_size == 0 else None
    for line in data.splitlines(keepends=True):
        if not line.endswith(b'\n') or not line.strip():
            raise ValueError('resume raw.jsonl contains an incomplete or empty record')
        row = json.loads(line)
        if not isinstance(row, dict):
            raise ValueError('resume raw record must be an object')
        key = tuple(row.get(k) for k in FIELDS)
        if any(type(value) is not kind for value, kind in zip(key, (str, str, str, int, int, int))):
            raise ValueError('resume raw record identity has an invalid type')
        if key not in expected_keys or key in seen:
            raise ValueError('resume raw record outside matrix or duplicated')
        if row.get('status') not in ('ok', 'error', 'deadline', 'hard_timeout', 'not_equivalent', 'not_repeated'):
            raise ValueError('resume raw record has an invalid status')
        seen.add(key)
        rows.append(row)
        offset += len(line)
        if offset == prefix_size:
            confirmed = len(rows)
    if confirmed is None or confirmed != recorded:
        raise ValueError('resume confirmed record count mismatch')
    confirmed_rows = rows[:confirmed]
    errors = sum(row['status'] == 'error' for row in confirmed_rows)
    statuses = {s: sum(row['status'] == s for row in confirmed_rows)
                for s in sorted({row['status'] for row in confirmed_rows})}
    if type(manifest.get('errors')) is not int or manifest['errors'] != errors:
        raise ValueError('resume confirmed error count mismatch')
    if (not isinstance(manifest.get('statuses'), dict) or manifest['statuses'] != statuses
            or any(type(v) is not int for v in manifest['statuses'].values())):
        raise ValueError('resume confirmed status counts mismatch')
    if final_hash is not None:
        if len(rows) != recorded or len(rows) != len(expected_keys):
            raise ValueError('resume completed record count mismatch')
    elif len(rows) > recorded + 1:
        raise ValueError('resume raw.jsonl exceeds one unconfirmed append')
    return rows, raw_hash, len(data)


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
    try:
        if type(args.repeats) is not int or args.repeats < 1:
            raise ValueError('repeats must be a positive integer')
        if args.iterations is not None and (type(args.iterations) is not int or args.iterations < 1):
            raise ValueError('iterations must be a positive integer')
        nodes = parse_matrix(args.nodes, 'nodes', minimum=1)
        rounds_values = parse_matrix(args.rounds, 'rounds', minimum=1)
        shapes = parse_matrix(args.shapes, 'shapes', allowed=('chain', 'wide', 'diamonds'))
        modes = parse_matrix(args.modes, 'modes', allowed=('plain', 'observe', 'checkpoint'))
        engines = parse_matrix(args.engines, 'engines', allowed=('pulse', 'eino', 'temporal'))
    except ValueError as error:
        parser.error(str(error))
    cases = [(mode, shape, n, rounds) for mode in modes for shape in shapes for n in nodes for rounds in rounds_values]
    expected_keys = {(engine, mode, shape, n, rounds, repeat) for repeat in range(args.repeats)
                     for mode, shape, n, rounds in cases for engine in engines}
    binary, temporal = args.binary.resolve(), args.temporal_bin.resolve()
    out = args.out.resolve()
    identity = dict(repeats=args.repeats, nodes=','.join(map(str, nodes)), rounds=','.join(map(str, rounds_values)),
                    shapes=','.join(shapes), modes=','.join(modes), engines=','.join(engines), binary_sha256=digest(binary),
                    temporal_sha256=digest(temporal), tool='run_production.py',
                    sampler_sha256=digest(__file__), containment_sha256=digest(Path(__file__).with_name('run_matrix.py')),
                    budget_s=60, watchdog_s=65,
                    parallelism=4, gomaxprocs=4, fixed_iterations=args.iterations)
    manifest_path, raw = out/'manifest.json', out/'raw.jsonl'
    if args.resume:
        manifest = json.loads(manifest_path.read_text(encoding='utf-8'))
        if not isinstance(manifest, dict) or not isinstance(manifest.get('identity'), dict):
            raise ValueError('resume identity/binary changed')
        previous = dict(manifest['identity'])
        # 只规范矩阵的历史表示；二进制、采样器和其余身份仍须逐项相同。
        for field in ('nodes', 'rounds', 'shapes', 'modes', 'engines'):
            value = previous.get(field)
            text = ','.join(map(str, value)) if isinstance(value, list) else str(value)
            previous[field] = ','.join(map(str, parse_matrix(text, field, minimum=1 if field in ('nodes', 'rounds') else None)))
        if previous != identity:
            raise ValueError('resume identity/binary changed')
        rows, raw_hash, raw_size = resume_rows(raw, manifest, expected_keys)
    else:
        if manifest_path.exists() or raw.exists(): raise ValueError('existing run: use --resume or a fresh output')
        manifest = dict(identity=identity, started_at=datetime.now(timezone.utc).isoformat())
        rows, raw_hash, raw_size = [], hashlib.sha256(), 0
    # 历史核验全部通过后才能修改清单、raw 或案例目录。
    out.mkdir(parents=True, exist_ok=True)
    manifest['identity'] = identity
    manifest.pop('ended_at', None)
    manifest.pop('raw_sha256', None)
    manifest['raw_prefix'] = dict(bytes=raw_size, sha256=raw_hash.hexdigest())
    write_manifest(manifest_path, manifest, rows)  # 在首个案例可能被中断前持久化身份信息。
    seen = {tuple(r[k] for k in FIELDS) for r in rows}
    limited = {tuple(r[k] for k in FIELDS[:-1]) for r in rows if r['status'] in LIMITS}
    with raw.open('ab') as stream:
        for repeat in range(args.repeats):
            for case_index, (mode, shape, n, rounds) in enumerate(cases):
                offset = (repeat + case_index) % len(engines)
                for engine in engines[offset:] + engines[:offset]:
                    row = dict(engine=engine, mode=mode, shape=shape, nodes=n, rounds=rounds, repeat=repeat,
                               parallelism=identity['parallelism'])
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
                            if not isinstance(error, Exception): raise  # 中断后仍可恢复，且不记录为采样结果。
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
                    encoded = (json.dumps(row, ensure_ascii=False, allow_nan=False) + '\n').encode('utf-8')
                    stream.write(encoded)
                    stream.flush()
                    # 每次只散列新追加字节，不重复读取整个 raw。
                    raw_hash.update(encoded)
                    raw_size += len(encoded)
                    manifest['raw_prefix'] = dict(bytes=raw_size, sha256=raw_hash.hexdigest())
                    seen.add(key)
                    write_manifest(manifest_path, manifest, rows)
                    if row['status'] != 'not_repeated': print(f"{len(rows)} {label} {row['status']} {row.get('run_ns')}", flush=True)
    manifest['ended_at'] = datetime.now(timezone.utc).isoformat()
    manifest['raw_sha256'] = raw_hash.hexdigest()
    write_manifest(manifest_path, manifest, rows)
    print(json.dumps(manifest, indent=2), flush=True)
    return int(any(r['status'] == 'error' for r in rows))


if __name__ == '__main__':
    raise SystemExit(main())
