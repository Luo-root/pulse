from __future__ import annotations

import csv
import hashlib
import itertools
import json
import os
from pathlib import Path
import statistics

ROOT = Path(__file__).resolve().parent
OUT = Path(os.environ.get('PULSE_OPTIMIZATION_OUT', str(ROOT/'matrix')))
manifest = json.loads((OUT/'manifest.json').read_text(encoding='utf-8'))
plan = json.loads((OUT/'plan.json').read_text(encoding='utf-8'))
records = [json.loads(line) for line in (OUT/'samples.jsonl').read_text(encoding='utf-8').splitlines()]
fields = ['variant','shape','nodes','rounds','mode','sample']
identity = lambda c:tuple(c[k] for k in fields)
assert len(records) == len(plan) == 1380
assert len({identity(r['case']) for r in records}) == 1380
assert {identity(r['case']) for r in records} == {identity(c) for c in plan}
summary = json.loads((OUT/'summary.json').read_text(encoding='utf-8'))
lookup = {(r['variant'],r['shape'],r['nodes'],r['rounds'],r['mode']):r for r in summary}
for row in summary:
    members = [r for r in records if identity(r['case'])[:5] == (row['variant'],row['shape'],row['nodes'],row['rounds'],row['mode'])]
    ok = [r['result'] for r in members if r['status']=='ok']
    assert len(members) == row['samples'] == 5
    assert len(ok) == row['ok']
    if ok:
        for metric in ['ns_per_op','bytes_per_op','allocs_per_op']:
            assert row[metric]['median'] == statistics.median(r[metric] for r in ok)
comparison = []
for shape,nodes,rounds,mode in itertools.product(['chain','wide','diamonds'],[1,10,100,1000,10000],[1,256],['build','reuse','e2e']):
    base = lookup[('baseline',shape,nodes,rounds,mode)]
    opt = lookup[('optimized',shape,nodes,rounds,mode)]
    task = lookup[('taskflow',shape,nodes,rounds,mode)]
    item = dict(shape=shape,nodes=nodes,rounds=rounds,mode=mode)
    for label,row in [('baseline',base),('optimized',opt),('taskflow',task)]:
        item[f'{label}_ok'] = row['ok']
        for metric,scale,name in [('ns_per_op',1e6,'ms'),('bytes_per_op',1048576,'mib'),('allocs_per_op',1,'allocs')]:
            item[f'{label}_{name}'] = row[metric]['median']/scale if row[metric] else None
    if base['ok']==5 and opt['ok']==5:
        item['latency_reduction_percent'] = (1-opt['ns_per_op']['median']/base['ns_per_op']['median'])*100
        item['allocation_bytes_reduction_percent'] = (1-opt['bytes_per_op']['median']/base['bytes_per_op']['median'])*100
        item['allocation_count_reduction_percent'] = (1-opt['allocs_per_op']['median']/base['allocs_per_op']['median'])*100
        item['optimized_range_above_baseline_range'] = opt['ns_per_op']['min'] > base['ns_per_op']['max']
    comparison.append(item)
(OUT/'comparison.json').write_text(json.dumps(comparison,indent=2)+'\n',encoding='utf-8',newline='\n')
with (OUT/'comparison.csv').open('w',encoding='utf-8',newline='') as stream:
    writer = csv.DictWriter(stream,fieldnames=list(comparison[0]),lineterminator='\n')
    writer.writeheader()
    writer.writerows(comparison)
slower = [r for r in comparison if r.get('latency_reduction_percent',0)<-10]
nonoverlap = [r for r in comparison if r.get('optimized_range_above_baseline_range')]
review = dict(points=len(comparison),over_ten_percent_slower_medians=slower,nonoverlapping_slower_ranges=nonoverlap,notes=['Five batch-average samples are not request latency percentiles or confidence intervals.', 'A slower min-max range here is an observation, not a statistical significance claim.', 'Allocation is cumulative Go TotalAlloc/Mallocs, not peak heap or RSS.'])
(OUT/'regression-observations.json').write_text(json.dumps(review,indent=2)+'\n',encoding='utf-8',newline='\n')

# Render only after all timed sampling has finished.
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
FIGURES = OUT/'figures'
FIGURES.mkdir(exist_ok=True)
colors = {'baseline':'#526071','optimized':'#167b66','taskflow':'#c47427'}
labels = {'baseline':'Pulse baseline','optimized':'Pulse optimized','taskflow':'Taskflow 1.2.0'}
plt.rcParams.update({'font.family':'DejaVu Sans','font.size':9,'svg.hashsalt':'pulse-422-20261017','axes.spines.top':False,'axes.spines.right':False})
for shape,rounds in itertools.product(['chain','wide','diamonds'],[1,256]):
    for metric,unit,scale,title in [('ns_per_op','ms',1e6,'Time per graph'),('bytes_per_op','MiB',1048576,'Cumulative Go allocation per graph')]:
        fig,axes=plt.subplots(1,3,figsize=(13.8,4.1),layout='constrained')
        for ax,mode in zip(axes,['build','reuse','e2e']):
            all_values=[]
            for variant in ['baseline','optimized','taskflow']:
                xs=[]; med=[]; low=[]; high=[]
                for nodes in [1,10,100,1000,10000]:
                    row=lookup[(variant,shape,nodes,rounds,mode)]
                    if row['ok'] != 5 or row[metric] is None:
                        continue
                    xs.append(nodes)
                    med.append(row[metric]['median']/scale)
                    low.append(row[metric]['min']/scale)
                    high.append(row[metric]['max']/scale)
                if xs:
                    ax.plot(xs,med,marker='o',markersize=4,linewidth=1.7,color=colors[variant],label=labels[variant],clip_on=False)
                    ax.fill_between(xs,low,high,color=colors[variant],alpha=.12)
                    all_values.extend(low+high)
            ax.set_xscale('log')
            if all_values:
                positive=[v for v in all_values if v>0]
                if positive and len(positive)==len(all_values):
                    ax.set_yscale('log')
                    ax.set_ylim(min(positive)/1.7,max(positive)*1.8)
                elif positive:
                    ax.set_yscale('symlog',linthresh=min(positive)/10)
                    ax.set_ylim(0,max(positive)*1.8)
            ax.set_xticks([1,10,100,1000,10000],['1','10','100','1k','10k'])
            ax.set_title(mode)
            ax.set_xlabel('Business nodes')
            ax.set_ylabel(unit)
            ax.grid(True,which='major',alpha=.22)
        axes[0].legend(loc='upper left',fontsize=8)
        fig.suptitle(f'{title} | {shape} | rounds={rounds}',fontsize=13)
        fig.supxlabel('Same-machine serial comparison; 5 batch means per point. Bands: min-max, not confidence intervals.',fontsize=8)
        name=f'{shape}-R{rounds}-'+('time' if metric=='ns_per_op' else 'allocation')
        for extension in ['svg','png']:
            fig.savefig(FIGURES/f'{name}.{extension}',dpi=165)
        plt.close(fig)

key_points=[r for r in comparison if r['nodes']==10000 and r['rounds']==1]
stages=[lookup[(v,'chain',10000,1,m)] for m in ['build','reuse','e2e'] for v in ['baseline','index','validation','optimized','taskflow']]
(OUT/'delivery-summary.json').write_text(json.dumps(dict(key_points=key_points,chain_intermediate_stages=stages,raw_sha256=hashlib.sha256((OUT/'samples.jsonl').read_bytes()).hexdigest(),figure_pairs=12),indent=2)+'\n',encoding='utf-8',newline='\n')
print(json.dumps(dict(key_points=key_points,slower_over_ten_percent=len(slower),nonoverlap_slower=len(nonoverlap),figures=12),indent=2),flush=True)
