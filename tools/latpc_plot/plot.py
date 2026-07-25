#!/usr/bin/env python3
"""Generate the required LATPC evaluation figures without seaborn."""

from __future__ import annotations

import argparse
import csv
import json
import math
import subprocess
from collections import defaultdict
from pathlib import Path
from typing import Any

import matplotlib.pyplot as plt
import numpy as np


ROOT = Path(__file__).resolve().parents[2]
DEFAULT_CSV = ROOT / "out" / "latpc" / "evaluation" / "results.csv"
DEFAULT_RESOLVED = (
    ROOT / "out" / "latpc" / "evaluation" / "resolved_inputs.json"
)
DEFAULT_OUTPUT = ROOT / "out" / "latpc" / "evaluation" / "figures"
MODES = ("latc", "latp", "latpc")
WORKLOAD_ORDER = (
    "atax",
    "bicg",
    "fdtd2d",
    "mvt",
    "lud",
    "nw",
    "bfs",
    "pagerank",
    "spmv",
)
MODE_LABELS = {"latc": "LATC", "latp": "LATP", "latpc": "LATPC", "ideal": "Ideal"}
COLORS = {
    "latc": "#4C78A8",
    "latp": "#F58518",
    "latpc": "#54A24B",
    "ideal": "#B279A2",
}


def number(value: str) -> float:
    try:
        return float(value)
    except (TypeError, ValueError):
        return math.nan


def load_rows(path: Path) -> list[dict[str, Any]]:
    with path.open(newline="") as source:
        rows = list(csv.DictReader(source))
    numeric = {
        "sim_cycles",
        "pwq_stall_cycles",
        "l1_mshr_failures",
        "l1_tlb_accesses",
        "prefetch_coverage",
        "prefetch_accuracy",
    }
    for row in rows:
        for field in numeric:
            row[field] = number(row[field])
    return rows


def passing(
    rows: list[dict[str, Any]],
    profile: str,
) -> dict[tuple[str, str], dict[str, Any]]:
    return {
        (row["workload"], row["mode"]): row
        for row in rows
        if row["translation_profile"] == profile
        and row["status"] == "PASS"
        and row["verify"] == "true"
    }


def selected_workloads(
    table: dict[tuple[str, str], dict[str, Any]],
) -> list[str]:
    names = {
        workload
        for workload, mode in table
        if mode == "baseline"
        and all((workload, candidate) in table for candidate in MODES)
    }
    return sorted(names)


def geometric_mean(values: list[float]) -> float:
    positive = [value for value in values if value > 0 and math.isfinite(value)]
    if not positive:
        return math.nan
    return math.exp(sum(math.log(value) for value in positive) / len(positive))


def save_figure(fig: plt.Figure, output: Path, stem: str) -> None:
    output.mkdir(parents=True, exist_ok=True)
    fig.savefig(output / f"{stem}.png", dpi=220, bbox_inches="tight")
    fig.savefig(output / f"{stem}.pdf", bbox_inches="tight")
    plt.close(fig)


def plot_speedup(
    rows: list[dict[str, Any]],
    resolved: dict[str, Any],
    output: Path,
) -> tuple[list[str], list[str]]:
    table = passing(rows, "large-resource")
    selected = selected_workloads(table)
    workloads = [
        name for name in WORKLOAD_ORDER if name in resolved["workloads"]
    ]
    labels = [
        f"{name.upper()}\n(excluded)"
        if resolved["workloads"][name].get("temporarily_excluded")
        else name.upper()
        for name in workloads
    ] + ["GMean"]
    x = np.arange(len(labels))
    width = 0.22
    fig, ax = plt.subplots(figsize=(max(8.0, len(labels) * 1.1), 4.8))
    for index, mode in enumerate(MODES):
        speedups = [
            table[(name, "baseline")]["sim_cycles"]
            / table[(name, mode)]["sim_cycles"]
            for name in selected
        ]
        values = [
            table[(name, "baseline")]["sim_cycles"]
            / table[(name, mode)]["sim_cycles"]
            if name in selected
            else math.nan
            for name in workloads
        ] + [geometric_mean(speedups)]
        bars = ax.bar(
            x + (index - (len(MODES) - 1) / 2) * width,
            values,
            width,
            label=MODE_LABELS[mode],
            color=COLORS[mode],
        )
        for workload_index, workload in enumerate(workloads):
            tier = resolved["workloads"][workload]["input_tier"]
            if workload in selected and tier == "Small":
                bars[workload_index].set_hatch("///")
                bars[workload_index].set_edgecolor("black")
    ax.axhline(1.0, color="black", linewidth=1, linestyle="--")
    ax.set_ylabel("Speedup over baseline (cycles)")
    ax.set_xticks(x, labels, rotation=25, ha="right")
    ax.legend(ncol=4, frameon=False)
    ax.grid(axis="y", alpha=0.25)
    ax.set_title("LATPC application performance — large-resource profile")
    fig.tight_layout()
    save_figure(fig, output, "latpc_speedup_by_workload")
    return workloads, selected


def safe_rate(numerator: float, denominator: float) -> float:
    if not math.isfinite(numerator) or not math.isfinite(denominator):
        return math.nan
    return numerator / denominator if denominator > 0 else 0.0


def plot_bottlenecks(
    rows: list[dict[str, Any]],
    output: Path,
    workloads: list[str],
    selected: list[str],
) -> None:
    table = passing(rows, "large-resource")
    modes = ("baseline", "latc", "latp", "latpc")
    labels = [name.upper() for name in workloads]
    x = np.arange(len(labels))
    width = 0.2
    fig, axes = plt.subplots(2, 1, figsize=(max(8.0, len(labels) * 1.1), 7.0))
    colors = {
        "baseline": "#9D9D9D",
        "latc": COLORS["latc"],
        "latp": COLORS["latp"],
        "latpc": COLORS["latpc"],
    }
    for index, mode in enumerate(modes):
        pwq = [
            safe_rate(
                table[(workload, mode)]["pwq_stall_cycles"],
                table[(workload, mode)]["sim_cycles"],
            )
            if workload in selected and (workload, mode) in table
            else math.nan
            for workload in workloads
        ]
        mshr = [
            safe_rate(
                table[(workload, mode)]["l1_mshr_failures"],
                table[(workload, mode)]["l1_tlb_accesses"],
            )
            if workload in selected and (workload, mode) in table
            else math.nan
            for workload in workloads
        ]
        offset = (index - 1.5) * width
        axes[0].bar(
            x + offset,
            pwq,
            width,
            label=mode.upper(),
            color=colors[mode],
        )
        axes[1].bar(x + offset, mshr, width, color=colors[mode])
    axes[0].set_ylabel("PWQ full stalls / cycle")
    axes[1].set_ylabel("L1 reservation failures / access")
    axes[1].set_xticks(x, labels, rotation=25, ha="right")
    axes[0].set_xticks(x, [])
    axes[0].legend(ncol=4, frameon=False)
    for ax in axes:
        ax.grid(axis="y", alpha=0.25)
    axes[0].set_title("Translation-resource pressure — large-resource profile")
    fig.tight_layout()
    save_figure(fig, output, "latpc_translation_bottlenecks")


def plot_prefetch_quality(
    rows: list[dict[str, Any]],
    output: Path,
    workloads: list[str],
    selected: list[str],
) -> None:
    table = passing(rows, "large-resource")
    modes = ("latp", "latpc")
    labels = [name.upper() for name in workloads]
    x = np.arange(len(labels))
    width = 0.2
    fig, axes = plt.subplots(2, 1, figsize=(max(8.0, len(labels) * 1.1), 7.0))
    for index, mode in enumerate(modes):
        offset = (index - 0.5) * width
        coverage = [
            table[(workload, mode)]["prefetch_coverage"]
            if workload in selected and (workload, mode) in table
            else math.nan
            for workload in workloads
        ]
        accuracy = [
            table[(workload, mode)]["prefetch_accuracy"]
            if workload in selected and (workload, mode) in table
            else math.nan
            for workload in workloads
        ]
        axes[0].bar(
            x + offset,
            coverage,
            width,
            label=MODE_LABELS[mode],
            color=COLORS[mode],
        )
        axes[1].bar(
            x + offset,
            accuracy,
            width,
            color=COLORS[mode],
        )
    axes[0].set_ylabel("Prefetch coverage")
    axes[1].set_ylabel("Prefetch accuracy")
    axes[1].set_xticks(x, labels, rotation=25, ha="right")
    axes[0].set_xticks(x, [])
    axes[0].legend(ncol=2, frameon=False)
    for ax in axes:
        ax.set_ylim(0, 1.05)
        ax.grid(axis="y", alpha=0.25)
    axes[0].set_title("LATP grouped-leaf request quality — large-resource profile")
    fig.tight_layout()
    save_figure(fig, output, "latpc_prefetch_quality")


def current_commit() -> str:
    result = subprocess.run(
        ["git", "rev-parse", "HEAD"],
        cwd=ROOT,
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout.strip()


def write_manifest(
    rows: list[dict[str, Any]],
    resolved: dict[str, Any],
    output: Path,
    workloads: list[str],
    source_csv: Path,
) -> None:
    status_by_workload: dict[str, set[str]] = defaultdict(set)
    for row in rows:
        if row["status"] != "PASS":
            status_by_workload[row["workload"]].add(row["status"])
    excluded = [
        name
        for name, config in resolved["workloads"].items()
        if name not in workloads or not config.get("selected_for_matrix")
    ]
    lines = [
        "# LATPC figure manifest",
        "",
        f"- Git commit: `{current_commit()}`",
        f"- Source CSV: `{source_csv.relative_to(ROOT)}`",
        "- Primary translation profile: `large-resource`",
        "- Speedup normalization: baseline simulated cycles / mode simulated cycles",
        "- GMean: geometric mean across selected passing workloads",
        "- Hatched speedup bars: Small input selected only after the RSS OOM gate",
        f"- Excluded workloads: {', '.join(name.upper() for name in excluded) or 'none'}",
        "",
        "## Resolved inputs",
        "",
        "| Workload | Tier | Input | OOM fallback reason |",
        "|---|---|---|---|",
    ]
    for name, config in resolved["workloads"].items():
        reason = config.get("resolution_reason", "")
        lines.append(
            f"| {name.upper()} | {config['input_tier']} | `{config['input']}` | "
            f"{reason} |"
        )
    if status_by_workload:
        lines.extend(["", "## Non-passing results", ""])
        for name, statuses in sorted(status_by_workload.items()):
            lines.append(f"- {name.upper()}: {', '.join(sorted(statuses))}")
    lines.extend(
        [
            "",
            "## Generated files",
            "",
            "- `latpc_speedup_by_workload.png`",
            "- `latpc_speedup_by_workload.pdf`",
            "- `latpc_translation_bottlenecks.png`",
            "- `latpc_translation_bottlenecks.pdf`",
            "- `latpc_prefetch_quality.png`",
            "- `latpc_prefetch_quality.pdf`",
        ]
    )
    (output / "manifest.md").write_text("\n".join(lines) + "\n")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--csv", type=Path, default=DEFAULT_CSV)
    parser.add_argument("--resolved", type=Path, default=DEFAULT_RESOLVED)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    args = parser.parse_args()
    rows = load_rows(args.csv)
    resolved = json.loads(args.resolved.read_text())
    workloads, selected = plot_speedup(rows, resolved, args.output)
    if not selected:
        raise SystemExit("no complete passing large-resource workload matrix")
    plot_bottlenecks(rows, args.output, workloads, selected)
    plot_prefetch_quality(rows, args.output, workloads, selected)
    write_manifest(rows, resolved, args.output, selected, args.csv)


if __name__ == "__main__":
    main()
