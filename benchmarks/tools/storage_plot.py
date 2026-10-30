"""为 shape/rounds/parallelism/procs 固定的单一工作负载绘制存储增长图。

在写入任何输出前，拒绝混合或缺失工作负载身份信息的输入。
部分完成的运行不会被当作已完成的数据点。
"""
import argparse
from collections import Counter, defaultdict
import csv
import json
from pathlib import Path
import statistics


WORKLOAD_FIELDS = ("shape", "rounds", "parallelism", "procs")


def workload_identity(row, line_number):
    identity = tuple(row.get(field) for field in WORKLOAD_FIELDS)
    for field, value in zip(WORKLOAD_FIELDS, identity):
        valid = isinstance(value, str) and bool(value.strip()) if field == "shape" else type(value) is int and value > 0
        if not valid:
            expected = "a nonempty string" if field == "shape" else "a positive integer"
            raise ValueError(f"storage plot input line {line_number}: {field} must be {expected}; got {value!r}")
    return identity


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    groups = defaultdict(list)
    workload = None
    for line_number, line in enumerate(args.input.read_text(encoding="utf-8").splitlines(), 1):
        row = json.loads(line)
        if row["mode"] != "growth":
            raise ValueError("storage plot accepts growth measurements only")
        current = workload_identity(row, line_number)
        if workload is None:
            workload = current
        elif current != workload:
            differences = ", ".join(f"{field}={value!r} (expected {expected!r})"
                                    for field, value, expected in zip(WORKLOAD_FIELDS, current, workload)
                                    if value != expected)
            raise ValueError(f"storage plot input line {line_number}: mixed workloads: {differences}; "
                             "split input by shape/rounds/parallelism/procs before plotting")
        groups[(row["engine"], row["backend"], row["nodes"])].append(row)
    fields = ("confirmed_steps", "save_calls", "successful_saves", "logical_json_bytes",
              "final_record_json_bytes", "node_records", "history_events", "db_bytes", "wal_bytes", "db_wal_bytes")
    summaries = []
    for (engine, backend, nodes), group in sorted(groups.items()):
        attempted = [r for r in group if r["status"] != "not_repeated"]
        complete = [r for r in attempted if r["status"] == "ok"]
        # 完整运行的值与部分前缀的值不能合并统计。
        for state, records in (("complete", complete), ("partial", [r for r in attempted if r["status"] != "ok"])):
            if not records:
                continue
            summary = dict(engine=engine, backend=backend, requested_nodes=nodes, state=state,
                           samples=len(records), statuses=json.dumps(dict(Counter(r["status"] for r in records))))
            for field in fields:
                if field == "db_wal_bytes":
                    # 先对每个样本求和，再取中位数；各项中位数相加会得到不同结果。
                    values = [r["storage"]["db_bytes"] + r["storage"]["wal_bytes"] for r in records
                              if all(type(r.get("storage", {}).get(k)) in (int, float) for k in ("db_bytes", "wal_bytes"))]
                else:
                    values = [r.get(field) if field == "confirmed_steps" else r.get("storage", {}).get(field) for r in records]
                    values = [v for v in values if type(v) in (int, float)]
                summary[field] = statistics.median(values) if values else ""
            summaries.append(summary)
    args.out.mkdir(parents=True, exist_ok=True)
    with (args.out / "storage-summary.csv").open("w", encoding="utf-8", newline="") as output:
        writer = csv.DictWriter(output, fieldnames=["engine", "backend", "requested_nodes", "state", "samples", "statuses", *fields], lineterminator="\n")
        writer.writeheader(); writer.writerows(summaries)
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    from matplotlib.ticker import ScalarFormatter
    plt.rcParams.update({"svg.fonttype": "none", "font.size": 10, "axes.spines.top": False, "axes.spines.right": False})
    figure, axes = plt.subplots(1, 3, figsize=(15, 4.4))
    panels = [("durable", "logical_json_bytes", "Cumulative successful-Save JSON", "MiB, logical bytes"),
              ("durable", "final_record_json_bytes", "Final retained JSON record", "MiB, encoded record"),
              ("go-workflows", "db_wal_bytes", "SQLite file + WAL before close", "MiB, current file lengths")]
    limits = []
    for axis, (engine, metric, title, unit) in zip(axes, panels):
        for backend, color in (("memory", "#13795b"), ("file", "#7446ad")):
            rows = [r for r in summaries if r["engine"] == engine and r["backend"] == backend]
            full = sorted([r for r in rows if r["state"] == "complete" and r.get(metric, "") != ""], key=lambda r: r["requested_nodes"])
            if full:
                observed = {r["requested_nodes"]: r[metric] / 1048576 for r in full}
                requested = sorted({r["requested_nodes"] for r in rows})
                axis.plot(requested, [observed.get(n, float("nan")) for n in requested],
                          marker="o", label=backend, color=color)
            partial = [r for r in rows if r["state"] == "partial" and r.get(metric, "") != ""]
            if partial:
                axis.scatter([r["requested_nodes"] for r in partial], [r[metric] / 1048576 for r in partial],
                             facecolors="none", edgecolors=color, marker="s", s=65, label=backend + " partial prefix")
                if metric == "logical_json_bytes":
                    limits.extend(f"{backend} N={r['requested_nodes']}: confirmed prefix {r['confirmed_steps']}, {r['statuses']}" for r in partial)
        axis.set_xscale("log")
        measured_values = [r[metric] for r in summaries if r["engine"] == engine and r.get(metric, "") != ""]
        if measured_values and min(measured_values) > 0:
            axis.set_yscale("log")
        else:
            axis.set_yscale("symlog", linthresh=0.0001)
            axis.set_ylim(bottom=0)
        sizes = sorted({r["requested_nodes"] for r in summaries})
        axis.set_xticks(sizes); axis.xaxis.set_major_formatter(ScalarFormatter())
        axis.set_xlabel("Requested business steps"); axis.set_ylabel(unit); axis.set_title(title)
        axis.grid(alpha=0.2); axis.legend(fontsize=8)
    figure.suptitle("Instrumented growth: distinct metric scopes, not physical disk writes", fontsize=13)
    figure.tight_layout(rect=(0, 0.12 + 0.025 * len(limits), 1, 0.96))
    figure.text(0.02, 0.02, "Filled circles: completed tasks. Hollow squares: actual partial prefixes; never extrapolated.\n" + "\n".join(limits), fontsize=8)
    svg_path = args.out / "storage-growth.svg"
    figure.savefig(svg_path, bbox_inches="tight", metadata={"Date": None})
    svg_text = svg_path.read_text(encoding="utf-8")
    svg_path.write_text("\n".join(line.rstrip() for line in svg_text.splitlines()) + "\n", encoding="utf-8", newline="\n")
    figure.savefig(args.out / "storage-growth.png", bbox_inches="tight", dpi=150)
    plt.close(figure)
    print(json.dumps({"storage_groups": len(summaries)}))


if __name__ == "__main__":
    main()
