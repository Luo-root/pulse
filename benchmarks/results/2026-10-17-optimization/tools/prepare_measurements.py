from __future__ import annotations

import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import time

ROOT = Path(__file__).resolve().parent
PREVIOUS = ROOT.parent / 'performance-20261016'
BENCH = PREVIOUS / 'core/benchmarks/dag'
OUT = Path(os.environ.get('PULSE_OPTIMIZATION_OUT', str(ROOT / 'matrix')))
OUT.mkdir(parents=True, exist_ok=True)
ENV = dict(os.environ, GOWORK='off', GOFLAGS='-buildvcs=false')
commands = []


def run(command, label, *, cwd=BENCH, env=ENV, timeout=360):
    started = dt.datetime.now(dt.timezone.utc).isoformat()
    t = time.perf_counter()
    p = subprocess.run(command, cwd=cwd, env=env, capture_output=True, text=True, encoding='utf-8', errors='replace', timeout=timeout)
    (OUT / f'prepare-{label}.stdout.txt').write_text(p.stdout, encoding='utf-8', newline='\n')
    (OUT / f'prepare-{label}.stderr.txt').write_text(p.stderr, encoding='utf-8', newline='\n')
    commands.append(dict(label=label, command=command, cwd=str(cwd), seconds=time.perf_counter()-t, started_at=started, returncode=p.returncode))
    (OUT / 'prepare-commands.json').write_text(json.dumps(commands, indent=2)+'\n', encoding='utf-8', newline='\n')
    if p.returncode:
        raise RuntimeError(f'{label}: {p.stdout}\n{p.stderr}')
    return p.stdout


variants = {}
for label, path in [('baseline', None), ('index', ROOT/'core-index'), ('validation', ROOT/'core-validate'), ('optimized', ROOT/'core-runtime')]:
    args = ['go', 'build']
    if path is None:
        version = json.loads(run(['go', 'list', '-m', '-json', 'github.com/Luo-root/pulse'], 'baseline-module'))
        origin = dict(version=version['Version'], commit='7d70e1e1477192669d5008d47729e3c8c19a336f')
    else:
        assert not run(['git', 'status', '--porcelain=v1'], f'{label}-status', cwd=path).strip()
        sha = run(['git', 'rev-parse', 'HEAD'], f'{label}-head', cwd=path).strip()
        modfile = OUT / f'{label}.mod'
        modfile.write_bytes((BENCH/'go.mod').read_bytes())
        modfile.with_suffix('.sum').write_bytes((BENCH/'go.sum').read_bytes())
        run(['go', 'mod', 'edit', f'-modfile={modfile}', f'-replace=github.com/Luo-root/pulse={path.as_posix()}'], f'{label}-modfile')
        args.append(f'-modfile={modfile}')
        origin = dict(commit=sha, source_path=str(path), modfile=str(modfile), note='Only this generated diagnostic modfile has a local replacement; project go.mod/go.sum are unchanged.')
    binary = OUT / f'{label}.exe'
    run([*args, '-o', str(binary), '.'], f'{label}-compile')
    variants[label] = dict(binary=str(binary), sha256=hashlib.sha256(binary.read_bytes()).hexdigest(), **origin)

# Cross-package acceptance uses the final Core checkout; subprocess recovery
# helpers execute the same compiled test binary, so all use the candidate code.
durable = PREVIOUS / 'durable'
modfile = OUT / 'durable-check.mod'
modfile.write_bytes((durable/'go.mod').read_bytes())
modfile.with_suffix('.sum').write_bytes((durable/'go.sum').read_bytes())
run(['go', 'mod', 'edit', f'-modfile={modfile}', f'-replace=github.com/Luo-root/pulse={(ROOT/"core-runtime").as_posix()}'], 'durable-modfile', cwd=durable)
durable_env = dict(ENV, GOFLAGS=f'-buildvcs=false -modfile={modfile.as_posix()}')
resolved = json.loads(run(['go', 'list', '-m', '-json', 'github.com/Luo-root/pulse'], 'durable-candidate', cwd=durable, env=durable_env))
assert Path(resolved['Replace']['Dir']).resolve() == (ROOT/'core-runtime').resolve()
print('RUNNING DURABLE FULL RACE AGAINST FINAL CORE', flush=True)
stdout = run(['go', 'test', '-race', '-count=1', '-timeout=180s', './...'], 'durable-full-race', cwd=durable, env=durable_env)
print(stdout.strip(), flush=True)
assert not run(['git', 'status', '--porcelain=v1'], 'durable-status', cwd=durable).strip()
manifest = dict(created_at=dt.datetime.now(dt.timezone.utc).isoformat(), benchmark_source_commit=run(['git','rev-parse','HEAD'],'benchmark-head',cwd=PREVIOUS/'core').strip(), benchmark_module=str(BENCH), benchmark_go_mod_sha256=hashlib.sha256((BENCH/'go.mod').read_bytes()).hexdigest(), benchmark_go_sum_sha256=hashlib.sha256((BENCH/'go.sum').read_bytes()).hexdigest(), go_version=run(['go','version'],'go-version').strip(), platform=platform.platform(), logical_cpus=os.cpu_count(), variants=variants, scope=dict(full_variants=['baseline','optimized','taskflow'], intermediate_variants=['index','validation'], intermediate_shape='chain', intermediate_nodes=10000, intermediate_rounds=1, shapes=['chain','wide','diamonds'], nodes=[1,10,100,1000,10000], rounds=[1,256], modes=['build','reuse','e2e'], repetitions=5, min_time='100ms', parallelism=4, procs=4), notes=['All builds and tests precede timed sampling.', 'Baseline and Taskflow use the identical original benchmark binary; Taskflow engine is selected by CLI flag.', 'Original raw baseline samples are not overwritten or mixed with this new same-machine comparison.', 'No production or shipped benchmark go.mod/go.sum is changed.'])
(OUT/'manifest.json').write_text(json.dumps(manifest, indent=2)+'\n', encoding='utf-8', newline='\n')
print('PREPARATION COMPLETE; BINARIES FROZEN', flush=True)
