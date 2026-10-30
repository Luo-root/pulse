#!/usr/bin/env python3
"""读取保留的四组生产评测实验，写出一个 CSV、身份 JSON 和不超过 4 张 PNG。

不启动引擎、服务器或采样器；拒绝使用已有的输出目录。
run_ns 已是复用批次均值：各次重复采样权重相同，不按迭代次数加权。
"""
import argparse
from collections import Counter, defaultdict
import csv
from functools import lru_cache
import hashlib
import json
import math
from pathlib import Path
import statistics


EXPERIMENTS = ('native', 'resources', 'checkpoint', 'recovery')
ENGINES = ('pulse', 'eino', 'temporal')
SHAPES = ('chain', 'wide', 'diamonds')
STATUSES = ('ok', 'deadline', 'hard_timeout', 'not_repeated', 'not_equivalent', 'error')
GROUP = ('experiment', 'engine', 'mode', 'shape', 'nodes', 'rounds', 'kind', 'prefix',
         'parallelism', 'gomaxprocs')
METRICS = ('server_setup_ns', 'adapter_setup_ns', 'first_ns', 'run_ns', 'wall_s',
           'job_cpu_s', 'sampled_tree_peak_rss_bytes', 'alloc_bytes_per_run',
           'alloc_objects_per_run', 'store_before_bytes', 'store_after_first_bytes',
           'store_after_batch_bytes', 'database_bytes', 'recovery_wall_s', 'operation_ns')
INTEGER_METRICS = set(METRICS) - {'run_ns', 'wall_s', 'job_cpu_s', 'recovery_wall_s'}
PRIMARY_MODULES = {'github.com/Luo-root/pulse': 'pulse',
                   'github.com/Luo-root/pulse-durable': 'pulse_durable',
                   'github.com/cloudwego/eino': 'eino', 'go.temporal.io/sdk': 'temporal_sdk',
                   'modernc.org/sqlite': 'sqlite'}
COLORS = {'pulse': '#147d64', 'eino': '#2468b4', 'temporal': '#b56118'}
LABELS = {'pulse': 'Pulse', 'eino': 'Eino', 'temporal': 'Temporal'}
SCOPES = {
    'run_ns': 'Mean of one completed reuse batch; median/min/max across ok repeats, without iteration weighting.',
    'resources': 'Fixed batch=1: whole case includes setup, common harness, first execution and one reuse; Temporal includes its Server.',
    'job_cpu_s': 'Windows Job user+kernel CPU, including exited descendants; native unequal batches are not CPU comparisons.',
    'sampled_tree_peak_rss_bytes': 'Maximum observed process-tree RSS sum at 25 ms polling; not an exact RSS peak.',
    'alloc_bytes_per_run': 'Go process-local reuse allocations; Temporal Server excluded. Not whole-engine memory.',
    'store_bytes': 'File-length snapshots include SQLite main/WAL/SHM, not bytes written or bytes per node.',
    'observations': 'Counts cover first plus completed reuse executions; Pulse lifecycle callbacks, Eino Graph/Lambda start/end/error, Temporal replay-filtered Workflow and Activity interceptors. Surfaces differ.',
    'recovery': 'Pulse confirmed-prefix hard kill; Eino active interrupt; Temporal worker kill with Server alive or restarted. Separate scenarios, no overall ranking.',
    'checkpoint': 'Only Pulse+Durable is sampled here. Eino ordinary success has no per-node checkpoint; Temporal native/plain already persists. Refer to native rows without copying them into checkpoint rows.',
}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def decode(text):
    def invalid(value):
        raise ValueError('non-finite JSON number: ' + value)
    return json.loads(text, parse_constant=invalid)


def integer(value, name, minimum=0):
    if type(value) is not int or value < minimum:
        raise ValueError(f'{name} must be an integer >= {minimum}')
    return value


def number(value, name):
    if type(value) not in (int, float) or value < 0:
        raise ValueError(f'{name} must be a finite nonnegative number')
    try:
        finite = math.isfinite(value)
    except OverflowError:
        finite = False
    if not finite:
        raise ValueError(f'{name} must be a finite nonnegative number')
    return value


@lru_cache(maxsize=None)
def oracle(shape, nodes, rounds):
    """为 production/model.go 提供独立的 uint64 参考实现，始终使用整数运算。"""
    mask = (1 << 64) - 1
    values, used = [], set()
    previous, first, width = [], 0, 1
    for i in range(nodes):
        if shape == 'chain':
            predecessors = [i - 1] if i else []
        elif shape == 'wide':
            predecessors = []
        elif shape == 'diamonds':
            if i == first + width:
                previous = list(range(first, i))
                first, width = i, 3 - width
            predecessors = previous
        else:
            raise ValueError('unknown shape: ' + shape)
        used.update(predecessors)
        x = ((1 + sum(values[p] for p in predecessors)) & mask) ^ (((i + 1) * 0x9e3779b97f4a7c15) & mask)
        for _ in range(rounds):
            x ^= x >> 12
            x ^= (x << 25) & mask
            x ^= x >> 27
            x = x * 0x2545f4914f6cdd1d & mask
        values.append(x)
    return sum(v for i, v in enumerate(values) if i not in used) & mask


def matrix_values(identity, name):
    value = identity.get(name)
    if value is None:
        return None
    entries = value.split(',') if isinstance(value, str) else value if isinstance(value, list) else [value]
    values = [str(item).strip() for item in entries]
    if not values or any(not item for item in values):
        raise ValueError('manifest matrix must be nonempty: ' + name)
    if name in ('nodes', 'rounds'):
        values = [str(integer(int(item), 'manifest ' + name, 1)) for item in values]
    if len(set(values)) != len(values):
        raise ValueError('duplicate manifest matrix values: ' + name)
    return values


def normalize(raw, experiment, identity):
    if not isinstance(raw, dict):
        raise ValueError('raw record must be an object')
    row = dict(raw, experiment=experiment)
    if row.get('engine') not in ENGINES or row.get('status') not in STATUSES:
        raise ValueError('unknown engine or status')
    integer(row.get('nodes'), 'nodes', 1)
    integer(row.get('repeat'), 'repeat')
    if experiment == 'recovery':
        row.setdefault('mode', 'plain')
        row.setdefault('shape', 'chain')
        row.setdefault('rounds', identity.get('rounds', 256))
        if 'server_restart' in row and type(row['server_restart']) is not bool:
            raise ValueError('server_restart must be a boolean')
        row.setdefault('kind', 'active_interrupt' if row['engine'] == 'eino' else
                       'server_restart' if row.get('server_restart') else 'runtime_kill')
        row.setdefault('prefix', max(1, min(10, row['nodes'] // 2)))
        expected_kinds = {'pulse': ('runtime_kill',), 'eino': ('active_interrupt',),
                          'temporal': ('runtime_kill', 'server_restart')}
        if row['kind'] not in expected_kinds[row['engine']]:
            raise ValueError('recovery kind/engine mismatch')
        integer(row['prefix'], 'prefix')
        if row['prefix'] >= row['nodes']:
            raise ValueError('recovery prefix must precede the suffix')
    else:
        row['kind'], row['prefix'] = '', ''
    integer(row.get('rounds'), 'rounds', 1)
    if row.get('shape') not in SHAPES:
        raise ValueError('unknown shape')
    allowed_modes = {'native': ('plain', 'observe'), 'resources': ('plain',),
                     'checkpoint': ('checkpoint',), 'recovery': ('plain',)}
    if row.get('mode') not in allowed_modes[experiment]:
        raise ValueError('mode does not belong to experiment')
    if experiment == 'checkpoint' and row['engine'] != 'pulse':
        raise ValueError('checkpoint input must contain only Pulse+Durable')
    if 'config' in row:
        config = row['config']
        if not isinstance(config, dict):
            raise ValueError('CLI config must be an object')
        for field in ('engine', 'mode', 'shape', 'nodes', 'rounds'):
            if type(config.get(field)) is not type(row[field]) or config.get(field) != row[field]:
                raise ValueError('CLI config identity mismatch: ' + field)
        actual = integer(config.get('parallelism'), 'config.parallelism', 1)
        if identity.get('parallelism') is not None and actual != identity['parallelism']:
            raise ValueError('CLI config/manifest parallelism mismatch')
        if 'parallelism' in row:
            integer(row['parallelism'], 'parallelism', 1)
            if row['parallelism'] != actual:
                raise ValueError('CLI config/record parallelism mismatch')
        row['parallelism'] = actual
    # 旧报告没有 config 时继续使用其清单身份；不补造 CLI 未提供的 gomaxprocs。
    for field in ('parallelism', 'gomaxprocs'):
        value = row.get(field, identity.get(field))
        if value is not None:
            integer(value, field, 1)
            if identity.get(field) is not None and value != identity[field]:
                raise ValueError('manifest identity mismatch: ' + field)
        row[field] = value if value is not None else ''
    for plural, singular in (('engines', 'engine'), ('modes', 'mode'), ('shapes', 'shape'),
                             ('nodes', 'nodes'), ('rounds', 'rounds')):
        allowed = matrix_values(identity, plural)
        if allowed is not None and str(row[singular]) not in allowed:
            raise ValueError('record outside manifest matrix: ' + singular)
    if identity.get('repeats') is not None:
        integer(identity['repeats'], 'manifest repeats', 1)
        if row['repeat'] >= identity['repeats']:
            raise ValueError('repeat outside manifest matrix')
    for metric in METRICS:
        value = row.get(metric)
        if value is not None:
            (integer if metric in INTEGER_METRICS else number)(value, metric)
    for field in ('iterations', 'requested_iterations'):
        if row.get(field) is not None:
            integer(row[field], field)
    if 'first_completed' in row and type(row['first_completed']) is not bool:
        raise ValueError('first_completed must be a boolean')
    if 'error' in row and not isinstance(row['error'], str):
        raise ValueError('error must be a string')
    if row.get('checksum') is not None:
        integer(row['checksum'], 'checksum')
        if row['checksum'] >= 1 << 64:
            raise ValueError('checksum must be a uint64')
    observations = row.get('observations')
    if observations is not None:
        if not isinstance(observations, dict):
            raise ValueError('observations must be an object')
        for key, value in observations.items():
            integer(value, 'observations.' + key)
    if experiment in ('resources', 'checkpoint'):
        fixed = identity.get('fixed_iterations')
        if fixed is not None and type(fixed) is not int:
            raise ValueError(experiment + ' fixed_iterations must be an integer')
        if (experiment == 'resources' and fixed != 1) or (experiment == 'checkpoint' and fixed not in (None, 1)):
            raise ValueError(experiment + ' requires manifest fixed_iterations=1 (checkpoint may use its automatic batch=1)')
        if row.get('requested_iterations') is not None and row['requested_iterations'] != 1:
            raise ValueError(experiment + ' requires requested_iterations=1')
    if row['status'] == 'ok':
        if row.get('checksum') != oracle(row['shape'], row['nodes'], row['rounds']):
            raise ValueError('independent checksum mismatch')
        if row.get('error'):
            raise ValueError('ok record carries an error')
        if experiment == 'recovery':
            if row.get('recovery_wall_s') is None or row.get('operation_ns') is None:
                raise ValueError('ok recovery missing completed timing')
        else:
            if not row.get('first_completed') or not row.get('iterations') or not row.get('run_ns'):
                raise ValueError('ok record missing completed progress/timing')
            if row.get('requested_iterations') != row['iterations']:
                raise ValueError('ok record has incomplete reuse batch')
            if row.get('first_ns') is None:
                raise ValueError('ok record missing first execution timing')
            if experiment in ('resources', 'checkpoint') and row['iterations'] != 1:
                raise ValueError(experiment + ' requires exactly one reuse')
    return row


def version_evidence(directory, manifest, identity, records):
    """只使用已记录的元数据和 CLI stdout，不读取 go.mod，也不重新执行二进制。"""
    found = {}
    def take(payload, source):
        if not isinstance(payload, dict):
            return
        modules = payload.get('modules', {})
        if isinstance(modules, dict):
            for module, name in PRIMARY_MODULES.items():
                value = modules.get(module)
                if isinstance(value, str) and value:
                    evidence = {'value': value, 'source': source + '#modules.' + module}
                    if name in found and found[name]['value'] != value:
                        raise ValueError('conflicting recorded versions: ' + name)
                    found.setdefault(name, evidence)
        for field in ('toolchain', 'temporal_version', 'temporal_cli_version', 'temporal_server_version'):
            value = payload.get(field)
            name = 'toolchain' if field == 'toolchain' else field
            if isinstance(value, str) and value:
                evidence = {'value': value, 'source': source + '#' + field}
                if name in found and found[name]['value'] != value:
                    raise ValueError('conflicting recorded versions: ' + name)
                found.setdefault(name, evidence)
    take(manifest, 'manifest.json')
    take(identity, 'manifest.json#identity' if 'identity' in manifest else 'manifest.json')
    for index, row in enumerate(records, 1):
        take(row, f'raw.jsonl:record={index}')
    # 清单中的二进制哈希用于标识共用的二进制文件。每份输入有一份带元数据的 stdout
    # 即已足够；失败或截断的 stdout 会被跳过，不会编造补齐。
    for row in records:
        if all(name in found for name in (*PRIMARY_MODULES.values(), 'toolchain')):
            break
        if row.get('case_dir'):
            case = (directory / row['case_dir']).resolve()
            if not case.is_relative_to(directory):
                raise ValueError('case_dir escapes input directory')
            candidates = [case / 'stdout.json']
        elif row['experiment'] == 'recovery':
            case = directory / f"{row['engine']}-{row['nodes']}-{row['repeat']}-{int(row['kind'] == 'server_restart')}"
            candidates = sorted(case.glob('[0-9][0-9]-*.json'))
        else:
            candidates = []
        for path in candidates:
            if not path.is_file():
                continue
            data = path.read_bytes()
            try:
                payload = decode(data.decode('utf-8-sig'))
            except (ValueError, UnicodeError):
                continue
            take(payload, str(path.relative_to(directory)).replace('\\', '/') + '@sha256=' + digest(data))
            if all(name in found for name in (*PRIMARY_MODULES.values(), 'toolchain')):
                break
    return {name: found.get(name) for name in (*PRIMARY_MODULES.values(), 'toolchain',
            'temporal_version', 'temporal_cli_version', 'temporal_server_version')}


def load_dataset(experiment, directory):
    directory = Path(directory).resolve()
    raw_bytes = (directory / 'raw.jsonl').read_bytes()
    manifest_bytes = (directory / 'manifest.json').read_bytes()
    manifest = decode(manifest_bytes.decode('utf-8-sig'))
    if not isinstance(manifest, dict) or not isinstance(manifest.get('identity', manifest), dict):
        raise ValueError(experiment + ': manifest must be an object')
    identity = manifest.get('identity', manifest)
    raw_hash = digest(raw_bytes)
    if manifest.get('raw_sha256') is not None and manifest['raw_sha256'] != raw_hash:
        raise ValueError(experiment + ': manifest raw_sha256 mismatch')
    records, seen = [], set()
    for line_number, line in enumerate(raw_bytes.decode('utf-8-sig').splitlines(), 1):
        if not line.strip():
            continue
        try:
            row = normalize(decode(line), experiment, identity)
            key = tuple(row[k] for k in GROUP) + (row['repeat'],)
            if key in seen:
                raise ValueError('duplicate sample identity; do not combine sampling runs')
            seen.add(key)
            records.append(row)
        except (ValueError, TypeError, OverflowError) as error:
            raise ValueError(f'{experiment}/raw.jsonl:{line_number}: {error}') from error
    if manifest.get('records') is not None and manifest['records'] != len(records):
        raise ValueError(experiment + ': manifest record count mismatch')
    statuses = dict(sorted(Counter(r['status'] for r in records).items()))
    if manifest.get('statuses') is not None and manifest['statuses'] != statuses:
        raise ValueError(experiment + ': manifest status counts mismatch')
    details = {'path': str(directory), 'raw_sha256': raw_hash,
               'manifest_sha256': digest(manifest_bytes), 'records': len(records),
               'groups': len({tuple(r[k] for k in GROUP) for r in records}), 'statuses': statuses,
               'matrix': {k: identity[k] for k in ('engines', 'modes', 'shapes', 'nodes', 'rounds',
                          'repeats', 'parallelism', 'gomaxprocs', 'fixed_iterations') if k in identity},
               'artifacts': {k: v for k, v in identity.items() if k.endswith('_sha256')},
               'versions': version_evidence(directory, manifest, identity, records)}
    return records, identity, details


def describe(numbers):
    return {'n': len(numbers), 'median': statistics.median(numbers) if numbers else '',
            'min': min(numbers) if numbers else '', 'max': max(numbers) if numbers else ''}


def compact(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=True, allow_nan=False)


def summarize(records, identities):
    groups = defaultdict(list)
    for record in records:
        groups[tuple(record[k] for k in GROUP)].append(record)
    rows = []
    for key, samples in sorted(groups.items(), key=lambda item: tuple(str(v) for v in item[0])):
        row = dict(zip(GROUP, key))
        good = [s for s in samples if s['status'] == 'ok']
        statuses = Counter(s['status'] for s in samples)
        planned = identities[row['experiment']].get('repeats', '')
        row.update(samples_planned=planned, samples_recorded=len(samples),
                   samples_missing=planned - len(samples) if planned != '' else '',
                   attempts=sum(s['status'] != 'not_repeated' for s in samples),
                   **{'status_' + status: statuses[status] for status in STATUSES})
        for source, target in (('requested_iterations', 'planned_iterations'),
                               ('iterations', 'completed_iterations'), ('first_completed', 'first_completed')):
            values = [s[source] for s in samples if s.get(source) is not None]
            row[target + '_known'] = len(values)
            row[target + '_total'] = sum(values) if values else ''
        row['checksum'] = good[0]['checksum'] if good else ''  # CSV 将 uint64 精确写成十进制。
        errors = Counter(s['error'] for s in samples if s.get('error'))
        row['checksum_errors'] = sum('checksum' in s.get('error', '').lower() for s in samples)
        row['errors'] = compact(errors)
        for metric in METRICS:
            for suffix, value in describe([s[metric] for s in good if s.get(metric) is not None]).items():
                row[metric + '_' + suffix] = value
        observation_keys = sorted({k for s in good for k in (s.get('observations') or {})})
        row['observations'] = compact({k: describe([s['observations'][k] for s in good
                                        if k in (s.get('observations') or {})]) for k in observation_keys})
        rows.append(row)
    return rows


def series(rows, engine, metric, factor=1):
    """用 NaN 将缺失或失败的点保留为空缺；实测的零值仍保留为零。"""
    selected = sorted((r for r in rows if r['engine'] == engine), key=lambda r: r['nodes'])
    if len({r['nodes'] for r in selected}) != len(selected):
        raise ValueError('plot series contains multiple identities at the same node count')
    xs, ys, low, high = [], [], [], []
    for row in selected:
        xs.append(row['nodes'])
        mid = row[metric + '_median']
        if mid == '':
            ys.append(float('nan')); low.append(0); high.append(0)
        else:
            ys.append(mid * factor)
            low.append((mid - row[metric + '_min']) * factor)
            high.append((row[metric + '_max'] - mid) * factor)
    return xs, ys, low, high


def draw(rows, out):
    import matplotlib
    matplotlib.use('Agg')
    import matplotlib.pyplot as plt
    from matplotlib.ticker import ScalarFormatter
    plt.rcParams.update({'font.size': 9, 'axes.spines.top': False, 'axes.spines.right': False})
    output = []

    def panel(axis, selected, metric, factor, unit, configurations):
        missing = sum(r[metric + '_median'] == '' for r in selected)
        values = []
        for engine, subset, label, style in configurations:
            xs, ys, low, high = series(subset, engine, metric, factor)
            if not xs:
                continue
            values.extend(r[metric + '_' + stat] * factor for r in subset if r['engine'] == engine
                          for stat in ('median', 'min', 'max') if r[metric + '_' + stat] != '')
            work = sorted({r['rounds'] for r in selected})
            work_index = work.index(subset[0]['rounds']) if subset and subset[0]['rounds'] in work else 0
            marker = ('o', 's', '^', 'D')[work_index % 4]
            axis.errorbar(xs, ys, yerr=[low, high], label=label, color=COLORS[engine],
                          marker=marker, markersize=3, capsize=2, linewidth=1.3, linestyle=style)
        axis.set_xscale('log')
        sizes = sorted({r['nodes'] for r in selected})
        if sizes:
            axis.set_xticks(sizes)
            axis.xaxis.set_major_formatter(ScalarFormatter())
        if values:
            axis.set_yscale('log' if min(values) > 0 else 'symlog',
                            **({} if min(values) > 0 else {'linthresh': max(min((v for v in values if v > 0), default=1) / 2, 1e-6)}))
            if min(values) == 0:
                axis.set_ylim(bottom=0)
        else:
            axis.text(.5, .5, 'No completed cost', transform=axis.transAxes, ha='center')
        if missing:
            axis.text(.02, .02, f'{missing} point(s) without cost; see CSV statuses',
                      transform=axis.transAxes, fontsize=7, color='#9a2626')
        axis.set_xlabel('Business nodes')
        axis.set_ylabel(unit)
        axis.grid(True, which='major', alpha=.22)
        if axis.get_legend_handles_labels()[0]:
            axis.legend(fontsize=7)

    def save(figure, name, title, footer):
        figure.suptitle(title, fontsize=13)
        figure.tight_layout(rect=(0, .13, 1, .95))
        figure.text(.02, .015, footer, fontsize=8, va='bottom')
        figure.savefig(out / name, dpi=160, facecolor='white')
        plt.close(figure)
        output.append(name)

    native = [r for r in rows if r['experiment'] == 'native']
    rounds = sorted({r['rounds'] for r in native})
    if rounds:
        figure, axes = plt.subplots(len(rounds), 3, squeeze=False, figsize=(14, 3.5 * len(rounds) + 1))
        for ri, work in enumerate(rounds):
            for si, shape in enumerate(SHAPES):
                selected = [r for r in native if r['mode'] == 'plain' and r['shape'] == shape and r['rounds'] == work]
                configs = [(e, selected, LABELS[e], '-') for e in ENGINES]
                panel(axes[ri][si], selected, 'run_ns', 1e-6, 'Reuse batch mean (ms/run)', configs)
                axes[ri][si].set_title(f'{shape} / rounds={work}')
        save(figure, 'native_reuse.png', 'Native plain execution: completed reuse costs',
             'Median of repeated batch means; bars = observed min-max. Missing costs remain gaps.\n'
             'Native reuse batch lengths differ across frameworks; CPU totals are not compared here.')

    resources = [r for r in rows if r['experiment'] == 'resources']
    if resources:
        figure, axes = plt.subplots(2, 3, squeeze=False, figsize=(14, 8))
        for ri, (metric, factor, unit) in enumerate((('job_cpu_s', 1, 'Job CPU (s/case)'),
                ('sampled_tree_peak_rss_bytes', 1 / 1024**2, 'Sampled tree peak RSS (MiB)'))):
            for si, shape in enumerate(SHAPES):
                selected = [r for r in resources if r['shape'] == shape]
                if len({r['rounds'] for r in selected}) > 1:
                    raise ValueError('resources plot requires one business-round setting')
                panel(axes[ri][si], selected, metric, factor, unit, [(e, selected, LABELS[e], '-') for e in ENGINES])
                axes[ri][si].set_title(shape + ' / first + one reuse')
        save(figure, 'resources.png', 'Resources: identical first + one reuse per case',
             'Median of ok cases; bars = observed min-max. Includes setup and common harness; Temporal includes Server.\n'
             'Job CPU includes exited descendants. RSS = maximum observed tree sum at 25 ms polling, not an exact peak.\n'
             'Go allocation counters are process-local, exclude Temporal Server, and are not whole-engine memory.')

    if native:
        figure, axes = plt.subplots(3, 3, squeeze=False, figsize=(14, 11))
        for ei, engine in enumerate(ENGINES):
            for si, shape in enumerate(SHAPES):
                selected = [r for r in native if r['engine'] == engine and r['shape'] == shape]
                configs = [(engine, [r for r in selected if r['mode'] == mode and r['rounds'] == work],
                            f'{mode} / r={work}', '-' if mode == 'plain' else '--')
                           for work in rounds for mode in ('plain', 'observe')]
                panel(axes[ei][si], selected, 'run_ns', 1e-6, 'Reuse batch mean (ms/run)', configs)
                axes[ei][si].set_title(f'{LABELS[engine]} / {shape}')
        save(figure, 'observability.png', 'Plain and observe: actual reuse costs by framework',
             'Median of repeated batch means; bars = observed min-max. Solid = plain; dashed = observe.\n'
             'Pulse: graph lifecycle + waiting/running/finished. Eino: Graph/Lambda start/end/error callbacks.\n'
             'Temporal: replay-filtered Workflow + Activity interceptors. Event coverage differs; counts remain in CSV.')

    recovery = [r for r in rows if r['experiment'] == 'recovery']
    if recovery:
        figure, axes = plt.subplots(1, 2, squeeze=False, figsize=(13, 5))
        configs = []
        for engine, kind, label, style in (('pulse', 'runtime_kill', 'Pulse: confirmed prefix + kill', '-'),
                ('eino', 'active_interrupt', 'Eino: active interrupt', '-'),
                ('temporal', 'runtime_kill', 'Temporal: worker kill', '-'),
                ('temporal', 'server_restart', 'Temporal: worker kill + Server restart', '--')):
            selected = [r for r in recovery if r['engine'] == engine and r['kind'] == kind]
            if len({(r['shape'], r['rounds'], r['mode'], r['parallelism'], r['gomaxprocs']) for r in selected}) > 1:
                raise ValueError('recovery plot contains multiple scenario configurations')
            configs.append((engine, selected, label, style))
        for ci, (metric, factor, unit) in enumerate((('recovery_wall_s', 1, 'Controller recovery wall (s)'),
                                                   ('operation_ns', 1e-9, 'Final CLI operation (s)'))):
            panel(axes[0][ci], recovery, metric, factor, unit, configs)
        save(figure, 'recovery.png', 'Confirmed-prefix recovery by scenario',
             'Median of ok repeats; bars = observed min-max. Controller wall includes relaunch/restart after confirmed prefix.\n'
             'Eino uses an active interrupt checkpoint. Temporal scenarios keep Server alive or restart it; compare each scenario separately.\n'
             'Final CLI operation excludes controller restart/launch work. Exact prefix, workload and statuses are in CSV.')
    return output


def validate_joint_identity(identities):
    for field in ('binary_sha256', 'temporal_sha256'):
        values = [identity.get(field) for identity in identities.values()]
        if any(type(value) is not str or len(value) != 64 or any(c not in '0123456789abcdef' for c in value)
               for value in values) or len(set(values)) != 1:
            raise ValueError('joint sampling identity mismatch: ' + field)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    for experiment in EXPERIMENTS:
        parser.add_argument('--' + experiment, type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--no-plots', action='store_true', help='CSV and identity only; useful for small fixture validation')
    args = parser.parse_args(argv)
    out = args.out.resolve()
    if out.exists():
        raise ValueError('output already exists; choose a new --out directory')
    records, identities, inputs = [], {}, {}
    for experiment in EXPERIMENTS:
        directory = getattr(args, experiment).resolve()
        if out == directory or out.is_relative_to(directory) or directory.is_relative_to(out):
            raise ValueError('output must be separate from input directories')
        samples, identities[experiment], inputs[experiment] = load_dataset(experiment, directory)
        records.extend(samples)
    validate_joint_identity(identities)
    rows = summarize(records, identities)
    out.mkdir(parents=True, exist_ok=False)
    fields = list(GROUP) + ['samples_planned', 'samples_recorded', 'samples_missing', 'attempts']
    fields += ['status_' + s for s in STATUSES]
    fields += [p + suffix for p in ('planned_iterations', 'completed_iterations', 'first_completed') for suffix in ('_known', '_total')]
    fields += ['checksum', 'checksum_errors', 'errors']
    fields += [metric + '_' + suffix for metric in METRICS for suffix in ('n', 'median', 'min', 'max')]
    fields += ['observations']
    with (out / 'summary.csv').open('x', encoding='utf-8', newline='') as stream:
        writer = csv.DictWriter(stream, fieldnames=fields, lineterminator='\n')
        writer.writeheader()
        writer.writerows(rows)
    plots = [] if args.no_plots else draw(rows, out)
    identity = {'schema': 1, 'summary_tool_sha256': digest(Path(__file__).read_bytes()),
                'records': len(records), 'groups': len(rows), 'inputs': inputs,
                'scopes': SCOPES, 'outputs': ['summary.csv', 'identity.json'] + plots}
    (out / 'identity.json').write_text(json.dumps(identity, indent=2, ensure_ascii=True, allow_nan=False) + '\n', encoding='utf-8')
    checksum_errors = sum(row['checksum_errors'] for row in rows)
    print(compact({'out': str(out), 'records': len(records), 'groups': len(rows), 'plots': plots,
                   'statuses': dict(Counter(r['status'] for r in records)), 'checksum_errors': checksum_errors}))
    # 保留可供审阅的部分报告，同时让调用方获知实际失败。
    return int(checksum_errors > 0 or any(r['status'] == 'error' for r in records))


if __name__ == '__main__':
    raise SystemExit(main())
