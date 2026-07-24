#!/usr/bin/env python3
"""Generate the Phase 12 LATPC report from resolved inputs and results.csv."""

from __future__ import annotations

import csv
import json
import math
import subprocess
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[2]
EVALUATION = ROOT / "out" / "latpc" / "evaluation"
RESULTS = EVALUATION / "results.csv"
RESOLVED = EVALUATION / "resolved_inputs.json"
REPORT = EVALUATION / "report.md"
MODES = ("baseline", "latc", "latp", "latpc", "ideal")
PROFILES = ("large-resource", "paper")


def numeric(value: str) -> float:
    try:
        return float(value)
    except (TypeError, ValueError):
        return math.nan


def load_rows() -> list[dict[str, Any]]:
    with RESULTS.open(newline="") as source:
        rows = list(csv.DictReader(source))
    numeric_fields = {
        "sim_cycles",
        "sim_kernel_time",
        "ipc",
        "ptw_invocations",
        "pwq_stall_cycles",
        "avg_walk_latency",
        "latc_compression_ratio",
        "latp_walks_saved",
        "prefetch_coverage",
        "prefetch_accuracy",
    }
    for row in rows:
        for field in numeric_fields:
            row[field] = numeric(row[field])
    return rows


def command_output(command: list[str]) -> str:
    return subprocess.run(
        command,
        cwd=ROOT,
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()


def geometric_mean(values: list[float]) -> float:
    usable = [value for value in values if value > 0 and math.isfinite(value)]
    if not usable:
        return math.nan
    return math.exp(sum(math.log(value) for value in usable) / len(usable))


def fmt(value: float, digits: int = 3) -> str:
    if not math.isfinite(value):
        return "n/a"
    return f"{value:.{digits}f}"


def passing_table(
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


def complete_workloads(
    table: dict[tuple[str, str], dict[str, Any]],
) -> list[str]:
    return sorted(
        workload
        for workload, mode in table
        if mode == "baseline"
        and all((workload, candidate) in table for candidate in MODES)
    )


def speedup(
    table: dict[tuple[str, str], dict[str, Any]],
    workload: str,
    mode: str,
) -> float:
    return (
        table[(workload, "baseline")]["sim_cycles"]
        / table[(workload, mode)]["sim_cycles"]
    )


def profile_performance(
    lines: list[str],
    rows: list[dict[str, Any]],
    profile: str,
) -> list[str]:
    table = passing_table(rows, profile)
    workloads = complete_workloads(table)
    lines.extend(
        [
            f"### {profile} profile",
            "",
            "| Workload | Baseline cycles | LATC | LATP | LATPC | Ideal | "
            "LATPC-to-Ideal remaining gap |",
            "|---|---:|---:|---:|---:|---:|---:|",
        ]
    )
    mode_values: dict[str, list[float]] = {
        mode: [] for mode in MODES if mode != "baseline"
    }
    remaining_gaps: list[float] = []
    for workload in workloads:
        values = {
            mode: speedup(table, workload, mode)
            for mode in MODES
            if mode != "baseline"
        }
        for mode, value in values.items():
            mode_values[mode].append(value)
        remaining = (
            table[(workload, "latpc")]["sim_cycles"]
            / table[(workload, "ideal")]["sim_cycles"]
            - 1
        )
        remaining_gaps.append(remaining)
        lines.append(
            f"| {workload.upper()} | "
            f"{table[(workload, 'baseline')]['sim_cycles']:.0f} | "
            f"{values['latc']:.3f}× | {values['latp']:.3f}× | "
            f"{values['latpc']:.3f}× | {values['ideal']:.3f}× | "
            f"{remaining * 100:.2f}% |"
        )
    if workloads:
        lines.append(
            "| **GMean** | — | "
            f"**{geometric_mean(mode_values['latc']):.3f}×** | "
            f"**{geometric_mean(mode_values['latp']):.3f}×** | "
            f"**{geometric_mean(mode_values['latpc']):.3f}×** | "
            f"**{geometric_mean(mode_values['ideal']):.3f}×** | "
            f"**{sum(remaining_gaps) / len(remaining_gaps) * 100:.2f}%** |"
        )
    else:
        lines.append("| No complete passing matrix | — | — | — | — | — | — |")
    lines.append("")
    return workloads


def append_mechanism_statistics(
    lines: list[str],
    rows: list[dict[str, Any]],
    workloads: list[str],
) -> None:
    table = passing_table(rows, "large-resource")
    lines.extend(
        [
            "## Mechanism statistics",
            "",
            "| Workload | LATC compression | LATPC compression | "
            "LATP walks saved | LATPC walks saved | LATP coverage/accuracy | "
            "LATPC coverage/accuracy | Baseline PTWs |",
            "|---|---:|---:|---:|---:|---:|---:|---:|",
        ]
    )
    for workload in workloads:
        latc = table[(workload, "latc")]
        latp = table[(workload, "latp")]
        latpc = table[(workload, "latpc")]
        baseline = table[(workload, "baseline")]
        lines.append(
            f"| {workload.upper()} | {fmt(latc['latc_compression_ratio'])}× | "
            f"{fmt(latpc['latc_compression_ratio'])}× | "
            f"{fmt(latp['latp_walks_saved'], 0)} | "
            f"{fmt(latpc['latp_walks_saved'], 0)} | "
            f"{fmt(latp['prefetch_coverage'])}/"
            f"{fmt(latp['prefetch_accuracy'])} | "
            f"{fmt(latpc['prefetch_coverage'])}/"
            f"{fmt(latpc['prefetch_accuracy'])} | "
            f"{fmt(baseline['ptw_invocations'], 0)} |"
        )
    lines.extend(
        [
            "",
            "LATC contribution is isolated by `latc` versus baseline. LATP "
            "contribution is isolated by `latp` versus baseline. The combined "
            "`latpc` row measures composition rather than adding the two "
            "individual speedups.",
            "",
            "LATP's extra grouped leaf reads are demand-backed: members already "
            "have L2 demand MSHRs when grouped. They are therefore classified as "
            "late and accurate, not as speculative useful prefetches.",
            "",
        ]
    )


def append_functional_matrix(
    lines: list[str],
    rows: list[dict[str, Any]],
    resolved: dict[str, Any],
) -> None:
    by_key = {
        (row["workload"], row["translation_profile"], row["mode"]): row
        for row in rows
    }
    lines.extend(
        [
            "## Functional correctness matrix",
            "",
            "Every cell is a timing-mode application-verification result using "
            "the same resolved input within a workload comparison.",
            "",
            "| Workload | Profile | Baseline | LATC | LATP | LATPC | Ideal |",
            "|---|---|---|---|---|---|---|",
        ]
    )
    for workload, config in resolved["workloads"].items():
        if not config.get("selected_for_matrix"):
            continue
        for profile in PROFILES:
            statuses = []
            for mode in MODES:
                row = by_key.get((workload, profile, mode))
                statuses.append(row["status"] if row else "NOT_RUN")
            lines.append(
                f"| {workload.upper()} | {profile} | "
                + " | ".join(statuses)
                + " |"
            )
    lines.extend(["", "Raw matrix: [`functional_matrix.md`](functional_matrix.md).", ""])


def append_inputs(lines: list[str], resolved: dict[str, Any]) -> None:
    lines.extend(
        [
            "## Resolved workload inputs",
            "",
            "| Workload | Tier | Input | Estimated device bytes | "
            "Resolution |",
            "|---|---|---|---:|---|",
        ]
    )
    for workload, config in resolved["workloads"].items():
        lines.append(
            f"| {workload.upper()} | {config['input_tier']} | "
            f"`{config['input']}` | {config['estimated_device_bytes']} | "
            f"{config['resolution_reason']} |"
        )
    lines.append("")


def append_failures(
    lines: list[str],
    rows: list[dict[str, Any]],
    resolved: dict[str, Any],
    selected: list[str],
) -> None:
    failures = [
        row
        for row in rows
        if row["status"] != "PASS"
    ]
    excluded = [
        workload
        for workload, config in resolved["workloads"].items()
        if not config.get("selected_for_matrix") or workload not in selected
    ]
    lines.extend(
        [
            "## Excluded or failed workloads",
            "",
            f"Excluded from the primary GMean: "
            f"{', '.join(name.upper() for name in excluded) or 'none'}.",
            "",
        ]
    )
    if failures:
        lines.extend(
            [
                "| Workload | Profile | Mode | Status |",
                "|---|---|---|---|",
            ]
        )
        for row in failures:
            lines.append(
                f"| {row['workload'].upper()} | "
                f"{row['translation_profile']} | {row['mode']} | "
                f"{row['status']} |"
            )
        lines.append("")
    else:
        lines.extend(["All executed matrix rows passed application verification.", ""])
    if "fdtd2d" in selected:
        lines.extend(
            [
                "FDTD2D is included only because the cross-kernel page-table/cache "
                "coherence repair is present and all five modes verified in both "
                "profiles. Otherwise it remains excluded as required by the plan.",
                "",
            ]
        )


def commit_list() -> list[str]:
    output = command_output(
        [
            "git",
            "log",
            "--reverse",
            "--format=%h %s",
            "4ad517e0^..HEAD",
        ]
    )
    return output.splitlines()


def main() -> None:
    rows = load_rows()
    resolved = json.loads(RESOLVED.read_text())
    evaluated_commits = sorted(
        {
            row["git_commit"]
            for row in rows
            if row["status"] == "PASS" and row["git_commit"]
        }
    )
    if len(evaluated_commits) == 1:
        commit_text = f"`{evaluated_commits[0]}`"
    else:
        commit_text = ", ".join(f"`{commit}`" for commit in evaluated_commits)
    lines = [
        "# LATPC evaluation report",
        "",
        f"Exact evaluated implementation commit: {commit_text}.",
        "",
        "This evaluates an **MI300X-class MGPUSim model with a detailed "
        "memory-backed PTW and LATPC-inspired translation extensions**. "
        "`docs/PTW_LATPC_EXT.md` is the authoritative design source. This "
        "report does not claim exact MI300X translation-resource fidelity.",
        "",
        "## Implementation summary",
        "",
        "- Added selectable baseline, LATC, LATP, LATPC, and ideal translation "
        "modes plus paper and large-resource profiles.",
        "- Preserved wavefront instruction/lane metadata from coalescing through "
        "both TLB levels and the GMMU.",
        "- Added a format-derived regularity detector, logical L1 MSHR "
        "compression (LATC), grouped page walks (LATP), active upper/leaf-phase "
        "merging, and individual replay/fill correctness.",
        "- Kept all page tables in simulated physical memory and all PTW traffic "
        "on the shared L2 path. Every profile and MI300X run uses 4 KiB pages.",
        "- Added metrics, drain-time conservation assertions, a resumable RSS "
        "watchdog runner, plots, and report generation.",
        "",
        "## Translation profiles",
        "",
        "| Profile | Page | L1 TLB entries/MSHRs/ports/latency | "
        "L2 TLB entries/MSHRs/ports/latency | PWQ/walkers/PWC per level |",
        "|---|---:|---|---|---|",
        "| paper | 4096 B | 32 / 16 / 4 / 20 cycles | "
        "1024 / 128 / 16 / 80 cycles | 128 / 16 / 16 |",
        "| large-resource | 4096 B | 64 / 16 / 4 / 20 cycles | "
        "4096 / 128 / 16 / 80 cycles | 256 / 32 / 32 |",
        "",
        "The paper profile is the mechanism-fidelity result. The "
        "large-resource profile is the primary sensitivity result; mode "
        "comparisons never mix profiles.",
        "",
    ]
    append_inputs(lines, resolved)
    append_functional_matrix(lines, rows, resolved)
    lines.extend(["## Performance", ""])
    large_workloads = profile_performance(lines, rows, "large-resource")
    profile_performance(lines, rows, "paper")
    lines.extend(
        [
            "### Figures",
            "",
            "- [Application speedup (PNG)](figures/latpc_speedup_by_workload.png) "
            "([PDF](figures/latpc_speedup_by_workload.pdf))",
            "- [Translation bottlenecks "
            "(PNG)](figures/latpc_translation_bottlenecks.png) "
            "([PDF](figures/latpc_translation_bottlenecks.pdf))",
            "- [Prefetch quality (PNG)](figures/latpc_prefetch_quality.png) "
            "([PDF](figures/latpc_prefetch_quality.pdf))",
            "- [Figure manifest](figures/manifest.md)",
            "",
        ]
    )
    append_mechanism_statistics(lines, rows, large_workloads)
    lines.extend(
        [
            "## Diagnosis iterations",
            "",
            "The preserved iteration log records the no-op opportunity check and "
            "the accepted active-walk merge fidelity fix: "
            "[`iterations.md`](../diagnosis/iterations.md).",
            "",
        ]
    )
    append_failures(lines, rows, resolved, large_workloads)
    lines.extend(
        [
            "## Limitations",
            "",
            "- The resource profiles are explicit research configurations, not "
            "claims about undocumented MI300X translation structures.",
            "- `sim_cycles` is derived from Driver kernel busy time at the modeled "
            "2.1 GHz GPU clock; `ipc` is retired CU instructions divided by that "
            "cycle count.",
            "- The Akita TLB does not export a native full-MSHR rejection counter. "
            "`l1_mshr_failures` therefore reports LATC logical reservation "
            "failures; modes without LATC report zero.",
            "- Results are one deterministic simulation per configuration and do "
            "not quantify run-to-run variance.",
            "- LATP groups observed demand misses; it does not predict untouched "
            "VPNs, so speculative useful/unused prefetch counts remain zero.",
            "",
            "## Commit list",
            "",
        ]
    )
    lines.extend(f"- `{entry}`" for entry in commit_list())
    lines.extend(
        [
            "",
            "## Reproducibility",
            "",
            "- Raw results: [`results.csv`](results.csv)",
            "- Resolved inputs: "
            "[`resolved_inputs.json`](resolved_inputs.json)",
            "- Opportunity report: "
            "[`../opportunity/page_divergence.md`](../opportunity/page_divergence.md)",
            "- Host policy: `GOMEMLIMIT=10GiB`, 11.5 GiB process-group RSS "
            "watchdog, 200 ms sampling; no `ulimit`.",
            "- Simulation mode: detailed timing mode, `cdna3`, MI300X model, "
            "4 KiB page size.",
            "",
        ]
    )
    EVALUATION.mkdir(parents=True, exist_ok=True)
    REPORT.write_text("\n".join(lines))


if __name__ == "__main__":
    main()
