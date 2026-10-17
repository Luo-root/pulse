from __future__ import annotations

import datetime as dt
import hashlib
import importlib.util
import itertools
import json
import os
from pathlib import Path
import statistics
import subprocess
import time

ROOT = Path(__file__).resolve().parent
OUT = Path(os.environ.get('PULSE_OPTIMIZATION_OUT', str(ROOT / 'matrix')))
manifest = json.loads((OUT/'manifest.json').read_text(encoding='utf-8'))
BENCH = Path(manifest['benchmark_module'])
tools = BENCH.parent/'tools/run_matrix.py'
spec = importlib.util.spec_from_file_location('accepted_sampler', tools)
sampler = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sampler)
RAW = OUT/'samples.jsonl'
if RAW.exists():
    raise RuntimeError('Refusing to overwrite existing timed samples')
variants = manifest['variants']
for item in variants.values():
    assert hashlib.sha256(Path(item['binary']).read_bytes()).hexdigest() == item['sha256']

# Independent Python oracles and all diagnostic probes finish before timing.
oracles = {(s,n,r):sampler.oracle(s,n,r) for s,n,r in itertools.product(['chain','wide','diamonds'],[1,10,100,1000,10000],[1,256])}
verification = []
for label in ['baseline','index','validation','optimized','taskflow']:
    binary = variants['baseline' if label == 'taskflow' else label]['binary']
    engine = 'taskflow' if label == 'taskflow' else 'pulse'
    for shape, rounds in itertools.product(['chain','wide','diamonds'],[1,256]):
        command = [binary, '-engine',engine,'-shape',shape,'-nodes','10000','-rounds',str(rounds),'-mode','reuse','-parallelism','4','-procs','4','-iterations','1','-min-time','1ms','-timeout','60s','-verify=true']
        rc, stdout, stderr = sampler.sample_process(command, str(BENCH), 65)
        result = json.loads(stdout)
        case = dict(engine=engine,shape=shape,nodes=10000,rounds=rounds,mode='reuse',parallelism=4,procs=4)
        sampler.check_result(result, case, oracles[(shape,10000,rounds)])
        assert rc == 0 and result['status'] == 'ok', (command,stdout,stderr)
        verification.append(dict(variant=label,command=command,returncode=rc,stdout=stdout,stderr=stderr,result=result))
    print(f'CORRECTNESS {label}: three N10000 shapes, two workloads PASS', flush=True)
(OUT/'large-correctness.json').write_text(json.dumps(verification, indent=2)+'\n',encoding='utf-8',newline='\n')

plan = []
for si, shape in enumerate(['chain','wide','diamonds']):
    for ni, nodes in enumerate([1,10,100,1000,10000]):
        for ri, rounds in enumerate([1,256]):
            for mi, mode in enumerate(['build','reuse','e2e']):
                labels = ['baseline','optimized','taskflow']
                if shape == 'chain' and nodes == 10000 and rounds == 1:
                    labels = ['baseline','index','validation','optimized','taskflow']
                for repetition in range(1,6):
                    shift = (si+ni+ri+mi+repetition-1) % len(labels)
                    for label in labels[shift:]+labels[:shift]:
                        plan.append(dict(variant=label,engine='taskflow' if label=='taskflow' else 'pulse',shape=shape,nodes=nodes,rounds=rounds,mode=mode,sample=repetition,parallelism=4,procs=4))
assert len(plan) == 1380
(OUT/'plan.json').write_text(json.dumps(plan,indent=2)+'\n',encoding='utf-8',newline='\n')
started = dt.datetime.now(dt.timezone.utc).isoformat()
records = []
limits = {}
with RAW.open('x',encoding='utf-8',newline='\n') as stream:
    for index, case in enumerate(plan,1):
        label = case['variant']
        binary = variants['baseline' if label=='taskflow' else label]['binary']
        command = [binary,'-engine',case['engine'],'-shape',case['shape'],'-nodes',str(case['nodes']),'-rounds',str(case['rounds']),'-mode',case['mode'],'-parallelism','4','-procs','4','-iterations','0','-min-time','100ms','-timeout','60s','-verify=false']
        identity = tuple(case[k] for k in ['variant','shape','nodes','rounds','mode'])
        record = dict(case=case,command=command,expected=oracles[(case['shape'],case['nodes'],case['rounds'])],started_at=dt.datetime.now(dt.timezone.utc).isoformat())
        if identity in limits:
            record.update(status='not_repeated',reason=limits[identity],returncode=None,stdout='',stderr='',result=None)
        else:
            t = time.perf_counter()
            try:
                rc, stdout, stderr = sampler.sample_process(command,str(BENCH),65)
                result = json.loads(stdout)
                sampler.check_result(result,{k:v for k,v in case.items() if k not in ['variant','sample']},record['expected'])
                status = result['status']
                if (status == 'ok' and rc != 0) or (status != 'ok' and rc != 1):
                    raise RuntimeError(f'CLI status/exit mismatch: {status}/{rc}')
                record.update(status=status,returncode=rc,stdout=stdout,stderr=stderr,result=result)
            except subprocess.TimeoutExpired as exc:
                def text(value):
                    return value.decode('utf-8',errors='replace') if isinstance(value,bytes) else (value or '')
                record.update(status='timeout',returncode=None,stdout=text(exc.output),stderr=text(exc.stderr),result=None,reason='65s parent process limit')
            record['wall_seconds'] = time.perf_counter()-t
            if record['status'] in sampler.LIMITS:
                limits[identity] = dict(sample=case['sample'],status=record['status'])
            if record['status'] in sampler.FAILURES:
                raise RuntimeError(f'Invalid candidate sample: {record}')
        stream.write(json.dumps(record,allow_nan=False)+'\n')
        stream.flush()
        records.append(record)
        if index % 30 == 0 or index == len(plan):
            progress = dict(completed=index,planned=len(plan),last_case=case,started_at=started,updated_at=dt.datetime.now(dt.timezone.utc).isoformat())
            (OUT/'progress.json').write_text(json.dumps(progress,indent=2)+'\n',encoding='utf-8',newline='\n')
            print(f'{index}/{len(plan)} {label} {case["shape"]} N{case["nodes"]} R{case["rounds"]} {case["mode"]}: {record["status"]}',flush=True)

summaries = []
group_fields = ['variant','shape','nodes','rounds','mode']
for identity, members in itertools.groupby(sorted(records,key=lambda r:tuple(r['case'][k] for k in group_fields)),key=lambda r:tuple(r['case'][k] for k in group_fields)):
    rows = list(members)
    ok = [r['result'] for r in rows if r['status']=='ok']
    item = dict(zip(group_fields,identity),samples=len(rows),ok=len(ok),statuses={status:sum(r['status']==status for r in rows) for status in sorted({r['status'] for r in rows})})
    for metric in ['ns_per_op','bytes_per_op','allocs_per_op']:
        values = [r[metric] for r in ok]
        item[metric] = dict(median=statistics.median(values),min=min(values),max=max(values)) if values else None
    summaries.append(item)
assert len(summaries) == 276
(OUT/'summary.json').write_text(json.dumps(summaries,indent=2)+'\n',encoding='utf-8',newline='\n')
post = {label:hashlib.sha256(Path(item['binary']).read_bytes()).hexdigest() for label,item in variants.items()}
assert all(post[label] == item['sha256'] for label,item in variants.items())
acceptance = dict(started_at=started,finished_at=dt.datetime.now(dt.timezone.utc).isoformat(),planned=len(plan),retained=len(records),summary_groups=len(summaries),statuses={status:sum(r['status']==status for r in records) for status in sorted({r['status'] for r in records})},binary_sha256_after=post,notes=['Serial sampling only, after all local compilation/testing/profiling stopped.', 'Each result validated against the independent Python oracle and CLI identity/exit contracts.', 'Pre/post binary hashes match at those two moments; no claim of continuous monitoring.'])
(OUT/'acceptance.json').write_text(json.dumps(acceptance,indent=2)+'\n',encoding='utf-8',newline='\n')
print(json.dumps(acceptance,indent=2),flush=True)
