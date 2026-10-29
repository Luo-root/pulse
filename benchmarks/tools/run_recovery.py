#!/usr/bin/env python3
"""相同三框架：先独立确认前缀，再跨 OS 进程恢复；Eino 限主动检查点。"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import socket
import sqlite3
import subprocess
import threading
import time

from run_matrix import WindowsJob
from run_production import oracle, parse_matrix


class Case:
    def __init__(self, root, binary, temporal, n, engine, repeat, restart_server=False):
        self.root, self.binary, self.temporal = root, binary, temporal
        self.n, self.engine, self.repeat = n, engine, repeat
        self.prefix = max(1, min(10, n // 2))
        self.run_id = f'recovery-{engine}-{n}-{repeat}'
        self.queue = self.run_id
        self.store = root/'state.sqlite'
        self.address = ''
        self.processes = []
        self.job = WindowsJob()
        self.start = time.perf_counter()
        self.expired = threading.Event()
        def hard_timeout():
            self.expired.set()
            if self.job.handle:
                self.job.kernel.TerminateJobObject(self.job.handle, 124)
        self.timer = threading.Timer(65, hard_timeout)
        self.timer.start()
        self.restart_server = restart_server
        self.counter = 0

    def launch(self, args, label, capture=False):
        if self.expired.is_set() or time.perf_counter() - self.start >= 60:
            raise TimeoutError('whole recovery case budget exhausted')
        stdout = stderr = p = None
        try:
            # 让子进程直接写入原有命名的证据文件，超时或解码失败也保留部分输出。
            stdout = (self.root/(label+('.json' if capture else '.stdout.txt'))).open('w', encoding='utf-8', newline='\n')
            stderr = (self.root/(label+'.stderr.txt')).open('w', encoding='utf-8', newline='\n')
            p = subprocess.Popen(args, stdout=stdout, stderr=stderr, env=dict(os.environ, GOMAXPROCS='4'), text=True, encoding='utf-8',
                                 errors='replace', creationflags=0x4)
            self.processes.append((p, stdout, stderr))
            self.job.attach_and_resume(p)
            if self.expired.is_set():
                raise TimeoutError('whole recovery case watchdog expired')
            return p
        except BaseException as error:
            self.close(error)
            if p is None:
                for stream in (stdout, stderr):
                    if stream is not None:
                        try: stream.close()
                        except Exception as extra: error.add_note(f'进程输出清理：{type(extra).__name__}: {extra}')
            raise

    def command(self, operation):
        return [str(self.binary), '-engine', self.engine, '-operation', operation, '-shape', 'chain',
                '-nodes', str(self.n), '-rounds', '256', '-parallelism', '4', '-gomaxprocs', '4',
                '-mode', 'plain', '-store', str(self.store), '-run-id', self.run_id,
                '-checkpoint-at', str(self.prefix), '-queue', self.queue, '-address', self.address,
                '-timeout', str(max(0.001, 60 - (time.perf_counter() - self.start)))+'s']

    def call(self, operation):
        self.counter += 1
        label = f'{self.counter:02d}-{operation}'
        try:
            p = self.launch(self.command(operation), label, True)
            p.wait(timeout=max(0.1, 65 - (time.perf_counter() - self.start)))
            stdout = (self.root/(label+'.json')).read_text(encoding='utf-8', errors='replace')
            payload = json.loads(stdout)
            if p.returncode or payload['status'] != 'ok': raise RuntimeError(label+': '+stdout)
            return payload
        except BaseException as error:
            self.close(error)
            raise

    def server(self):
        if not self.address:
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0))
                port = sock.getsockname()[1]
            self.address = f'127.0.0.1:{port}'
        port = self.address.rsplit(':', 1)[1]
        p = self.launch([str(self.temporal), 'server', 'start-dev', '--headless', '--ip', '127.0.0.1',
                         '--port', port, '--db-filename', str(self.store), '--sqlite-pragma', 'journal_mode=WAL',
                         '--sqlite-pragma', 'synchronous=FULL'], 'server-'+str(len(self.processes)))
        while True:
            try:
                with socket.create_connection(('127.0.0.1', int(port)), 0.1): return p
            except OSError:
                if p.poll() is not None: raise RuntimeError('server exited before ready')
                if time.perf_counter()-self.start >= 60: raise TimeoutError('server startup budget')
                time.sleep(0.025)

    def kill(self, p):
        if p.poll() is not None: raise RuntimeError('process exited before intended hard kill')
        p.kill()
        p.wait(timeout=5)

    def pulse_prefix(self):
        while True:
            try:
                with sqlite3.connect(self.store, timeout=0.05) as db:
                    row = db.execute('SELECT record FROM durable_runs WHERE run_id=?', (self.run_id,)).fetchone()
                if row:
                    record = json.loads(row[0])
                    nodes = record['scopes'][record['root_scope']]['nodes']
                    audit = [json.loads(s)['node'] for s in Path(str(self.store)+'.calls.jsonl').read_text(encoding='utf-8').splitlines()]
                    if set(nodes) == {f'n{i}' for i in range(self.prefix)} and audit == list(range(self.prefix)):
                        (self.root/'confirmed-prefix.json').write_text(json.dumps(record, indent=2), encoding='utf-8')
                        return
            except (sqlite3.Error, OSError):
                pass
            if time.perf_counter()-self.start >= 60: raise TimeoutError('prefix confirmation budget')
            time.sleep(0.01)

    def run(self):
        if self.engine == 'pulse':
            worker = self.launch(self.command('prepare'), 'prepare')
            self.pulse_prefix()
            start = time.perf_counter()
            self.kill(worker)
            result = self.call('resume')
            resumed = result['details']
            audit = [json.loads(s)['node'] for s in Path(str(self.store)+'.calls.jsonl').read_text(encoding='utf-8').splitlines()]
            if audit != list(range(self.n)) or resumed['confirmed_nodes'] != self.n:
                raise ValueError('Pulse prefix reran or final record incomplete')
        elif self.engine == 'eino':
            prepared = self.call('prepare')
            if prepared['details']['executed'] != list(range(self.prefix)): raise ValueError('Eino incorrect prepared prefix')
            with sqlite3.connect(self.store) as db:
                blob = db.execute('SELECT value FROM eino_checkpoints WHERE key=?', (self.run_id,)).fetchone()
            if not blob or not blob[0]: raise ValueError('independent Eino blob missing')
            (self.root/'checkpoint.sha256').write_text(hashlib.sha256(blob[0]).hexdigest()+'\n', encoding='utf-8')
            start = time.perf_counter()
            result = self.call('resume')
            resumed = result['details']
            if resumed['executed'] != list(range(self.prefix, self.n)): raise ValueError('Eino prefix reran or suffix missing')
        else:
            server = self.server()
            worker = self.launch(self.command('worker'), 'worker-before')
            submitted = self.call('submit')
            self.run_id = submitted['details']['workflow_id']
            evidence = self.call('history')['details']
            if evidence['scheduled'] != self.prefix or evidence['completed'] != self.prefix or evidence['pending']:
                raise ValueError('Temporal prefix not independently confirmed')
            start = time.perf_counter()
            self.kill(worker)
            if self.restart_server:
                self.kill(server)
                self.server()
                self.call('history')
            self.launch(self.command('worker'), 'worker-after')
            self.call('signal')
            result = self.call('result')
            resumed = result['details']
            if resumed['scheduled'] != self.n or resumed['completed'] != self.n or resumed['pending']:
                raise ValueError('Temporal unique Activity history incomplete')
        elapsed = time.perf_counter()-start
        if resumed['checksum'] != oracle('chain', self.n, 256): raise ValueError('independent recovery checksum mismatch')
        return dict(engine=self.engine, nodes=self.n, prefix=self.prefix, repeat=self.repeat,
                    kind='server_restart' if self.restart_server else ('active_interrupt' if self.engine=='eino' else 'runtime_kill'),
                    status='ok', recovery_wall_s=elapsed, operation_ns=result['operation_ns'], checksum=resumed['checksum'],
                    database_bytes=sum(p.stat().st_size for p in self.root.glob('state.sqlite*') if p.is_file() and not p.name.endswith('.jsonl')))

    def close(self, failure=None):
        errors = []
        def cleanup(action):
            try: action()
            except Exception as error: errors.append(error)
        cleanup(self.timer.cancel)
        cleanup(lambda: self.timer.join(timeout=5))
        cleanup(self.job.close)
        for p, out, err in self.processes:
            # Job 归属设置失败时，挂起的根进程仍须单独终止并等待回收。
            cleanup(lambda: p.kill() if p.poll() is None else None)
            cleanup(lambda: p.wait(timeout=10))
            cleanup(out.close)
            cleanup(err.close)
        if errors:
            original = failure if failure is not None else errors[0]
            for error in errors:
                original.add_note(f'进程清理：{type(error).__name__}: {error}')
            if failure is None: raise original


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',type=Path,required=True)
    parser.add_argument('--temporal-bin',type=Path,required=True)
    parser.add_argument('--out',type=Path,required=True)
    parser.add_argument('--nodes',default='10,100,1000')
    parser.add_argument('--repeats',type=int,default=3)
    args=parser.parse_args()
    try:
        if type(args.repeats) is not int or args.repeats < 1:
            raise ValueError('repeats must be a positive integer')
        # 恢复必须同时有已确认前缀和待执行后缀。
        nodes = parse_matrix(args.nodes, 'nodes', minimum=2)
    except ValueError as error:
        parser.error(str(error))
    args.out.mkdir(parents=True,exist_ok=False)
    manifest=dict(binary_sha256=hashlib.sha256(args.binary.read_bytes()).hexdigest(),
                  temporal_sha256=hashlib.sha256(args.temporal_bin.read_bytes()).hexdigest(),
                  controller_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                  nodes=args.nodes,repeats=args.repeats,budget_s=60,watchdog_s=65,rounds=256,parallelism=4,gomaxprocs=4)
    (args.out/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n',encoding='utf-8')
    rows=[]
    for repeat in range(args.repeats):
        for n in nodes:
            for engine,restart in [('pulse',False),('eino',False),('temporal',False),('temporal',True)]:
                root=args.out/f'{engine}-{n}-{repeat}-{int(restart)}';root.mkdir()
                case=Case(root,args.binary.resolve(),args.temporal_bin.resolve(),n,engine,repeat,restart)
                failure = None
                try: row=case.run()
                except BaseException as error:
                    failure = error
                    if not isinstance(error, Exception): raise
                    row=dict(engine=engine,nodes=n,repeat=repeat,status='error',error=str(error),server_restart=restart)
                finally: case.close(failure)
                rows.append(row)
                with (args.out/'raw.jsonl').open('a',encoding='utf-8') as stream:stream.write(json.dumps(row)+'\n')
                print(json.dumps(row),flush=True)
    return int(any(r['status']!='ok' for r in rows))

if __name__=='__main__':raise SystemExit(main())
