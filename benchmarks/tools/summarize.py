"""Summarize retained samples and generate scientific plots (requires matplotlib)."""
import argparse
from collections import Counter, defaultdict
import csv
import json
from pathlib import Path
import statistics

GROUP = ("suite", "engine", "backend", "shape", "nodes", "rounds", "mode", "parallelism", "procs")
METRICS = ("ns_per_op", "bytes_per_op", "allocs_per_op")
COLORS = {"direct": "#6b7280", "pulse": "#13795b", "azure": "#146ebe", "taskflow": "#af5c12",
          "durable": "#13795b", "go-workflows": "#7446ad"}


def summarize(records):
    groups = defaultdict(list)
    for record in records:
        groups[tuple(record.get(field, "") for field in GROUP)].append(record)
    rows = []
    for identity, samples in sorted(groups.items(), key=lambda pair: tuple(str(v) for v in pair[0])):
        row = dict(zip(GROUP, identity))
        good = [item for item in samples if item["status"] == "ok"]
        row.update(samples_planned=len(samples), samples_ok=len(good),
                   attempts=sum(item["status"] != "not_repeated" for item in samples),
                   statuses=json.dumps(dict(Counter(item["status"] for item in samples)), sort_keys=True))
        for metric in METRICS:
            numbers = [item[metric] for item in good if isinstance(item.get(metric), (int, float))]
            for suffix, value in (("median", statistics.median(numbers) if numbers else ""),
                                  ("min", min(numbers) if numbers else ""),
                                  ("max", max(numbers) if numbers else "")):
                row[f"{metric}_{suffix}"] = value
        if good:
            row["ns_per_node"] = row["ns_per_op_median"] / row["nodes"]
            row["nodes_per_second"] = row["nodes"] * 1e9 / row["ns_per_op_median"]
            row["allocated_bytes_per_node"] = row["bytes_per_op_median"] / row["nodes"] if row["bytes_per_op_median"] != "" else ""
        rows.append(row)
    return rows


def draw(rows, destination, selected_modes, selected_rounds):
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    from matplotlib.ticker import ScalarFormatter
    plt.rcParams.update({"font.size": 10, "axes.spines.top": False, "axes.spines.right": False,
                         "svg.fonttype": "none", "savefig.bbox": "tight"})
    for suite in sorted({row["suite"] for row in rows}):
        suite_rows = [r for r in rows if r["suite"] == suite]
        for mode in sorted({row["mode"] for row in suite_rows}):
            if selected_modes and mode not in selected_modes:
                continue
            for rounds in sorted({row["rounds"] for row in suite_rows}):
                if selected_rounds and str(rounds) not in selected_rounds:
                    continue
                subset = [row for row in suite_rows if row["mode"] == mode and row["rounds"] == rounds]
                shapes = sorted({row["shape"] for row in subset})
                backends = sorted({row["backend"] for row in subset})
                for metric, scale, title, unit in [
                    ("ns_per_op", 1e-6, "Whole task time", "ms / task"),
                    ("ns_per_op", None, "Time per business node", "microseconds / node"),
                    ("bytes_per_op", 1 / (1024 ** 2), "Cumulative Go allocations", "MiB allocated / task"),
                ]:
                    normalized = scale is None
                    figure, axes = plt.subplots(len(shapes), len(backends), squeeze=False,
                                                figsize=(6.4 * len(backends), 3.9 * len(shapes)))
                    for si, shape in enumerate(shapes):
                        for bi, backend in enumerate(backends):
                            axis = axes[si][bi]
                            panel = [r for r in subset if r["shape"] == shape and r["backend"] == backend]
                            sizes = sorted({r["nodes"] for r in panel})
                            limits, positive = [], False
                            configurations = sorted({(r["engine"], r["parallelism"], r["procs"]) for r in panel})
                            multiple_configs = len({(p, g) for _, p, g in configurations}) > 1
                            for engine, parallelism, procs in configurations:
                                label = f"{engine} / parallel={parallelism}, procs={procs}" if multiple_configs else engine
                                series = {r["nodes"]: r for r in panel if
                                          (r["engine"], r["parallelism"], r["procs"]) == (engine, parallelism, procs)}
                                ys, low, high = [], [], []
                                incomplete_x, incomplete_y = [], []
                                for nodes in sizes:
                                    row = series.get(nodes)
                                    if not row or not row["samples_ok"] or row[f"{metric}_median"] == "":
                                        ys.append(float("nan")); low.append(0); high.append(0)
                                        if row:
                                            statuses = "/".join(k for k in json.loads(row["statuses"]) if k != "not_repeated")
                                            limits.append(f"{label} N={nodes}: {statuses}")
                                        continue
                                    factor = 1e-3 / nodes if normalized else scale
                                    mid = row[f"{metric}_median"] * factor
                                    positive = positive or mid > 0
                                    ys.append(mid)
                                    low.append(mid - row[f"{metric}_min"] * factor)
                                    high.append(row[f"{metric}_max"] * factor - mid)
                                    if row["samples_ok"] < (5 if suite == "dag" else 3):
                                        incomplete_x.append(nodes); incomplete_y.append(mid)
                                axis.errorbar(sizes, ys, yerr=[low, high], label=label, marker="o", markersize=4,
                                              capsize=3, linewidth=1.5, color=COLORS.get(engine))
                                if incomplete_x:
                                    axis.scatter(incomplete_x, incomplete_y, s=70, facecolors="none", edgecolors="red",
                                                 label=f"{label}: few samples")
                            axis.set_xscale("log")
                            axis.set_xticks(sizes)
                            axis.xaxis.set_major_formatter(ScalarFormatter())
                            if metric == "bytes_per_op":
                                axis.set_yscale("symlog", linthresh=0.001)
                            elif positive:
                                axis.set_yscale("log")
                            else:
                                axis.set_ylim(0, 1)
                                axis.text(0.5, 0.5, "No valid measurements", transform=axis.transAxes,
                                          ha="center", va="center", color="#9b1c20")
                            axis.set_xlabel("Business nodes / steps")
                            axis.set_ylabel(unit)
                            axis.set_title(shape + (f" / {backend}" if backend else ""))
                            axis.grid(True, which="major", alpha=0.22)
                            axis.legend(fontsize=8, loc="best")
                            if limits:
                                axis.text(0, -0.29, "\n".join(limits), transform=axis.transAxes, fontsize=8, color="#9b1c20")
                    figure.suptitle(f"{suite} / {mode} / rounds={rounds}: {title}", fontsize=13)
                    figure.tight_layout(rect=(0, 0.04, 1, 0.97), h_pad=4)
                    figure.text(0.02, 0.01, "Median of repeated batch means; bars: observed min–max. Limits have no fabricated value.", fontsize=9)
                    name = "per-node" if normalized else ("time" if metric == "ns_per_op" else "allocations")
                    figure.savefig(destination / f"{suite}-{mode}-r{rounds}-{name}.svg", metadata={"Date": None})
                    plt.close(figure)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inputs", type=Path, nargs="+")
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--modes", default="")
    parser.add_argument("--rounds", default="")
    parser.add_argument("--no-plots", action="store_true")
    args = parser.parse_args()
    records = [json.loads(line) for path in args.inputs for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]
    if len({tuple(r.get(k, "") for k in GROUP + ("sample",)) for r in records}) != len(records):
        raise ValueError("duplicate sample identity; do not combine overlapping matrices")
    rows = summarize(records)
    args.out.mkdir(parents=True, exist_ok=True)
    fieldnames = list(GROUP) + ["samples_planned", "samples_ok", "attempts", "statuses"]
    fieldnames += [f"{metric}_{suffix}" for metric in METRICS for suffix in ("median", "min", "max")]
    fieldnames += ["ns_per_node", "nodes_per_second", "allocated_bytes_per_node"]
    with (args.out / "summary.csv").open("w", encoding="utf-8", newline="") as output:
        writer = csv.DictWriter(output, fieldnames=fieldnames)
        writer.writeheader(); writer.writerows(rows)
    (args.out / "summary.json").write_text(json.dumps(rows, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    if not args.no_plots:
        draw(rows, args.out, set(filter(None, args.modes.split(","))), set(filter(None, args.rounds.split(","))))
    print(json.dumps({"samples": len(records), "groups": len(rows), "statuses": dict(Counter(r["status"] for r in records))}))


if __name__ == "__main__":
    main()
