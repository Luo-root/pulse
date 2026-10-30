"""对隔离的 DAG/工作流评测 CLI 串行采样，并记录版本信息。"""
import argparse
import hashlib
import itertools
import json
import math
import os
from pathlib import Path
import platform
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone

MASK = (1 << 64) - 1
LIMITS = {"timeout", "resource_limit", "unsupported"}
FAILURES = {"error", "invalid_result"}


def graph(shape, nodes):
    parents = [[] for _ in range(nodes)]
    if shape == "chain":
        for i in range(1, nodes):
            parents[i] = [i - 1]
    elif shape == "diamonds":
        previous, at, width = [], 0, 1
        while at < nodes:
            layer = list(range(at, min(nodes, at + width)))
            for i in layer:
                parents[i] = previous
            previous, at, width = layer, at + len(layer), 3 - width
    elif shape != "wide":
        raise ValueError(f"unknown shape: {shape}")
    return parents


def oracle(shape, nodes, rounds):
    parents = graph(shape, nodes)
    values, depth, nonterminals = [], [], set()
    for i, incoming in enumerate(parents):
        value = (1 + sum(values[j] for j in incoming)) & MASK
        value ^= ((i + 1) * 0x9E3779B97F4A7C15) & MASK
        for _ in range(rounds):
            value ^= value >> 12
            value ^= (value << 25) & MASK
            value ^= value >> 27
            value = (value * 0x2545F4914F6CDD1D) & MASK
        values.append(value)
        depth.append(1 + max((depth[j] for j in incoming), default=0))
        nonterminals.update(incoming)
    return {"checksum": sum(v for i, v in enumerate(values) if i not in nonterminals) & MASK,
            "edges": sum(map(len, parents)), "critical_path": max(depth)}


def capture(command, cwd, timeout=180):
    result = subprocess.run(command, cwd=cwd, capture_output=True, text=True,
                            encoding="utf-8", errors="replace", timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"{command} failed: {result.stderr[-4000:]}")
    return result.stdout


class WindowsJob:
    """在挂起的采样进程能够创建任何后代进程前，将其纳入隔离控制。"""
    def __init__(self):
        import ctypes
        from ctypes import wintypes as w
        self.ctypes = ctypes
        self.kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        self.ntdll = ctypes.WinDLL("ntdll")
        size = ctypes.c_size_t

        class Basic(ctypes.Structure):
            _fields_ = [("process_time", ctypes.c_int64), ("job_time", ctypes.c_int64),
                        ("flags", w.DWORD), ("min_working_set", size), ("max_working_set", size),
                        ("active_processes", w.DWORD), ("affinity", size),
                        ("priority", w.DWORD), ("scheduling", w.DWORD)]

        class Extended(ctypes.Structure):
            _fields_ = [("basic", Basic), ("io", ctypes.c_uint64 * 6),
                        ("process_memory", size), ("job_memory", size),
                        ("peak_process", size), ("peak_job", size)]

        self.kernel.CreateJobObjectW.argtypes = [ctypes.c_void_p, w.LPCWSTR]
        self.kernel.CreateJobObjectW.restype = w.HANDLE
        self.kernel.SetInformationJobObject.argtypes = [w.HANDLE, ctypes.c_int, ctypes.c_void_p, w.DWORD]
        self.kernel.AssignProcessToJobObject.argtypes = [w.HANDLE, w.HANDLE]
        self.kernel.TerminateJobObject.argtypes = [w.HANDLE, w.UINT]
        self.kernel.CloseHandle.argtypes = [w.HANDLE]
        self.ntdll.NtResumeProcess.argtypes = [w.HANDLE]
        self.ntdll.NtResumeProcess.restype = ctypes.c_long
        self.handle = self.kernel.CreateJobObjectW(None, None)
        if not self.handle:
            raise ctypes.WinError(ctypes.get_last_error())
        limits = Extended()
        limits.basic.flags = 0x2000  # JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
        if not self.kernel.SetInformationJobObject(self.handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)):
            error = ctypes.WinError(ctypes.get_last_error())
            self.kernel.CloseHandle(self.handle)
            raise error

    def attach_and_resume(self, process):
        handle = int(process._handle)  # CPython 保留 CreateProcess 的进程句柄。
        if not self.kernel.AssignProcessToJobObject(self.handle, handle):
            raise self.ctypes.WinError(self.ctypes.get_last_error())
        status = self.ntdll.NtResumeProcess(handle)
        if status < 0:
            raise OSError(f"NtResumeProcess failed: {status}")

    def close(self):
        if self.handle:
            handle, self.handle = self.handle, None
            self.kernel.CloseHandle(handle)  # 即使父进程已退出，仍会终止其后代进程。


def sample_process(command, cwd, timeout):
    job = WindowsJob() if os.name == "nt" else None
    process = None
    try:
        process = subprocess.Popen(command, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   text=True, encoding="utf-8", errors="replace",
                                   start_new_session=os.name != "nt", creationflags=0x4 if job else 0)
        if job:
            job.attach_and_resume(process)
        try:
            stdout, stderr = process.communicate(timeout=timeout)
            return process.returncode, stdout, stderr
        except subprocess.TimeoutExpired:
            if job:
                job.close()
            else:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            stdout, stderr = process.communicate(timeout=10)
            raise subprocess.TimeoutExpired(command, timeout, output=stdout, stderr=stderr)
    finally:
        if job:
            job.close()
        if process is not None:
            if process.poll() is None:
                process.kill()
            if os.name != "nt":
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            process.wait(timeout=10)


def key(record):
    return tuple(record.get(k, "") for k in
                 ("engine", "backend", "shape", "nodes", "rounds", "mode", "sample"))


def check_result(result, case, expected):
    if not isinstance(result, dict):
        raise ValueError("CLI output must be a JSON object")
    reserved = {"sample", "suite", "command", "expected", "wall_seconds", "stdout", "stderr", "returncode", "result"}
    if "backend" not in case:
        reserved.add("backend")
    if reserved.intersection(result):
        raise ValueError("CLI output contains sampler-owned fields")
    for field in ("engine", "shape", "nodes", "rounds", "mode", "parallelism", "procs"):
        if result.get(field) != case[field] or type(result.get(field)) is not type(case[field]):
            raise ValueError(f"CLI {field} disagrees with requested case")
    if "backend" in case and result.get("backend") != case["backend"]:
        raise ValueError("CLI backend disagrees with requested case")
    if result.get("status") not in ("ok", "error", "timeout", "resource_limit", "unsupported"):
        raise ValueError("missing or unknown sample status")
    # 即使在可选字段中也拒绝 NaN/Infinity；原始 stdout 单独保留。
    json.dumps(result, allow_nan=False)
    if result["status"] != "ok":
        return
    for field in ("edges", "critical_path"):
        if type(result.get(field)) is not int or result[field] != expected[field]:
            raise ValueError(f"incorrect {field}: {result.get(field)} != {expected[field]}")
    for field in ("ns_per_op", "bytes_per_op", "allocs_per_op"):
        value = result.get(field)
        if value is None and field != "ns_per_op" and case["mode"] in ("resume", "reopen"):
            continue  # 单独审计的子进程场景不提供分配量测量结果。
        if isinstance(value, bool) or not isinstance(value, (float, int)) or not math.isfinite(value):
            raise ValueError(f"missing/non-finite {field}")
        if value < 0 or (field == "ns_per_op" and value == 0):
            raise ValueError(f"invalid {field}: {value}")
    count = result.get("iterations")
    if type(count) is not int or not 1 <= count <= (1 << 63) - 1:
        raise ValueError("successful sample needs a positive integer iteration count")
    if case["mode"] != "build":
        checksum = result.get("checksum")
        if not (type(checksum) is int or (isinstance(checksum, str) and checksum.isascii() and checksum.isdecimal())):
            raise ValueError("checksum must be a uint64 or its decimal string")
        if not 0 <= int(checksum) <= MASK or int(checksum) != expected["checksum"]:
            raise ValueError(f"incorrect checksum: {checksum} != {expected['checksum']}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--suite", choices=["dag", "workflow"], required=True)
    parser.add_argument("--module", type=Path, required=True)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--sizes", default="1,10,100,1000,10000")
    parser.add_argument("--rounds", default="1,256")
    parser.add_argument("--shapes")
    parser.add_argument("--modes")
    parser.add_argument("--backends", default="memory,file")
    parser.add_argument("--engines")
    parser.add_argument("--repeats", type=int, default=5)
    parser.add_argument("--procs", type=int, default=4)
    parser.add_argument("--parallelism", type=int, default=4)
    parser.add_argument("--timeout", type=float, default=65)
    parser.add_argument("--case-timeout", default="60s")
    parser.add_argument("--min-time", default="100ms")
    parser.add_argument("--no-warmup", action="store_true", help="workflow suite only, e.g. independent growth accounting")
    parser.add_argument("--resume", action="store_true")
    args = parser.parse_args()
    if args.no_warmup and args.suite != "workflow":
        parser.error("--no-warmup is supported only by the workflow suite")
    sizes, rounds = [int(n) for n in args.sizes.split(",")], [int(n) for n in args.rounds.split(",")]
    if min(sizes + rounds + [args.repeats, args.procs, args.parallelism]) <= 0 or not math.isfinite(args.timeout) or args.timeout <= 0:
        parser.error("sizes, rounds, repeats, concurrency and timeout must be positive")
    module, binary, out = args.module.resolve(), args.binary.resolve(), args.out.resolve()
    shapes = (args.shapes or ("chain,wide,diamonds" if args.suite == "dag" else "chain")).split(",")
    modes = (args.modes or ("e2e,build,reuse" if args.suite == "dag" else "run")).split(",")
    engines = (args.engines or ("direct,pulse,azure,taskflow" if args.suite == "dag"
                               else "durable,go-workflows")).split(",")
    backends = [""] if args.suite == "dag" else args.backends.split(",")
    for values in (sizes, rounds, shapes, modes, engines, backends):
        if len(values) != len(set(values)) or (values != [""] and "" in values):
            parser.error("matrix dimensions must contain unique, nonempty values")
    out.mkdir(parents=True, exist_ok=True)
    raw_path = out / "samples.jsonl"
    config = {"suite": args.suite, "sizes": sizes, "rounds": rounds, "shapes": shapes,
              "modes": modes, "engines": engines, "backends": backends, "repeats": args.repeats,
              "procs": args.procs, "parallelism": args.parallelism, "timeout": args.timeout,
              "case_timeout": args.case_timeout, "min_time": args.min_time, "warmup": not args.no_warmup}
    completed, limited, failures = set(), set(), 0
    if raw_path.exists():
        if not args.resume:
            parser.error("output already contains samples; use another directory or --resume")
        manifest = json.loads((out / "manifest.json").read_text(encoding="utf-8"))
        if manifest["config"] != config or manifest["binary_sha256"] != hashlib.sha256(binary.read_bytes()).hexdigest():
            parser.error("resume requires identical configuration and binary")
        for line in raw_path.read_text(encoding="utf-8").splitlines():
            item = json.loads(line)
            if key(item) in completed:
                parser.error("existing data contains duplicate sample identities")
            completed.add(key(item))
            failures += item["status"] in FAILURES
            if item["status"] in LIMITS:
                limited.add(key(item)[:-1])
        if any(json.loads(line)["status"] == "invalid_result" for line in raw_path.read_text(encoding="utf-8").splitlines()):
            parser.error("existing correctness/output failure; fix it and use a fresh output directory")
    else:
        manifest = {"schema": 1, "started_at_utc": datetime.now(timezone.utc).isoformat(),
                    "config": config, "system": {k: v for k, v in platform.uname()._asdict().items() if k != "node"},
                    "logical_cpus": os.cpu_count(), "python": sys.version,
                    "go_version": capture(["go", "version"], module).strip(), "module": str(module),
                    "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                    "environment": {k: os.environ.get(k) for k in ("GOWORK", "GOFLAGS", "GOTOOLCHAIN")},
                    "git_head": capture(["git", "rev-parse", "HEAD"], module).strip(),
                    "git_status": capture(["git", "status", "--short"], module),
                    "go_modules_json": capture(["go", "list", "-m", "-json", "all"], module),
                    "go_download_json": capture(["go", "mod", "download", "-json"], module, 300),
                    "notes": ["Samples execute serially; engines rotate within each repeated case.",
                              "Process launch is excluded from CLI ns_per_op; wall_seconds includes it.",
                              "Go allocation counters are cumulative allocated bytes, not peak memory."]}

    def save_manifest():
        manifest["failed_samples"] = failures
        manifest["last_updated_at_utc"] = datetime.now(timezone.utc).isoformat()
        (out / "manifest.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2, allow_nan=False) + "\n", encoding="utf-8")

    save_manifest()
    with raw_path.open("a", encoding="utf-8", newline="\n") as log:
        for case_no, (shape, nodes, work, mode, backend) in enumerate(itertools.product(shapes, sizes, rounds, modes, backends)):
            expected = oracle(shape, nodes, work)
            for sample in range(1, args.repeats + 1):
                pivot = (sample - 1 + case_no) % len(engines)
                for engine in engines[pivot:] + engines[:pivot]:
                    case = {"engine": engine, "shape": shape, "nodes": nodes, "rounds": work,
                            "mode": mode, "sample": sample, "suite": args.suite,
                            "parallelism": args.parallelism, "procs": args.procs}
                    if backend:
                        case["backend"] = backend
                    if key(case) in completed:
                        continue
                    command = [str(binary), "-engine", engine, "-shape", shape, "-nodes", str(nodes),
                               "-rounds", str(work), "-mode", mode, "-parallelism", str(args.parallelism),
                               "-procs", str(args.procs), "-timeout", args.case_timeout]
                    if args.suite == "dag":
                        command += ["-min-time", args.min_time]
                    else:
                        case_dir = out / "work" / ("-".join(str(v) for v in key(case)))
                        command += ["-backend", backend, "-iterations", "1", "-dir", str(case_dir)]
                        if args.no_warmup:
                            command += ["-warmup=false"]
                    record = {**case, "command": command, "expected": expected}
                    if key(case)[:-1] in limited:
                        record.update(status="not_repeated", notes="An earlier attempt reached a limit or was unsupported; no measurement fabricated.")
                    else:
                        start = time.perf_counter()
                        try:
                            returncode, stdout, stderr = sample_process(command, module, args.timeout)
                            record.update(stdout=stdout, stderr=stderr,
                                          returncode=returncode, wall_seconds=time.perf_counter() - start)
                            try:
                                measured = json.loads(stdout)
                                check_result(measured, case, expected)
                                if returncode and measured["status"] == "ok":
                                    raise ValueError("nonzero process exit with successful JSON")
                                record.update(measured)
                            except (ValueError, TypeError, KeyError, OverflowError) as exc:
                                record.update(status="invalid_result", error=str(exc))
                        except subprocess.TimeoutExpired as exc:
                            record.update(status="timeout", wall_seconds=time.perf_counter() - start,
                                          error="Parent hard timeout; contained process tree terminated.",
                                          stdout=exc.stdout, stderr=exc.stderr)
                        except OSError as exc:
                            record.update(status="error", error=str(exc), wall_seconds=time.perf_counter() - start)
                        if record["status"] in LIMITS:
                            limited.add(key(case)[:-1])
                        elif record["status"] != "ok":
                            failures += 1
                    log.write(json.dumps(record, ensure_ascii=False, separators=(",", ":"), allow_nan=False) + "\n")
                    log.flush()
                    os.fsync(log.fileno())
                    completed.add(key(case))
                    save_manifest()
                    value = f"{record['ns_per_op'] / 1e6:.4f} ms" if record["status"] == "ok" else record["status"]
                    print(f"{shape} N={nodes} rounds={work} {mode} {backend} {engine} sample={sample}: {value}", flush=True)
                    if record["status"] == "invalid_result":
                        raise RuntimeError("Correctness/output gate failed; raw sample retained; matrix stopped.")
    manifest["ended_at_utc"] = datetime.now(timezone.utc).isoformat()
    save_manifest()
    return int(failures != 0)


if __name__ == "__main__":
    raise SystemExit(main())
