#!/usr/bin/env python3
"""Run and collect the LATPC timing evaluation.

The runner intentionally uses GOMEMLIMIT plus a process-group RSS watchdog.
It never changes host ulimit settings.
"""

from __future__ import annotations

import argparse
import csv
from concurrent.futures import ThreadPoolExecutor, as_completed
import json
import math
import os
import signal
import sqlite3
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / "out" / "latpc" / "evaluation"
RUNS = OUT / "runs"
BIN = OUT / "bin"
RESOLVED = OUT / "resolved_inputs.json"
RESULTS = OUT / "results.csv"
GOMEMLIMIT = "10GiB"
RSS_LIMIT_BYTES = int(11.5 * 1024**3)
RSS_INTERVAL_SECONDS = 0.2
RUN_STATE_CHECKPOINT_SECONDS = 30.0
GPU_CLOCK_HZ = 2_100_000_000
PROFILES = ("large-resource", "paper")
MODES = ("baseline", "latc", "latp", "latpc", "ideal")
# FDTD2D has one already-running baseline. Keep that process untouched while
# temporarily removing the workload from new scheduling and result selection.
TEMPORARILY_EXCLUDED_WORKLOADS = frozenset({"fdtd2d"})


@dataclass(frozen=True)
class Workload:
    name: str
    package: str
    large: tuple[str, ...]
    large_bytes: int
    small: tuple[str, ...]
    small_bytes: int

    def input_text(self, tier: str) -> str:
        return " ".join(self.args(tier))

    def args(self, tier: str) -> tuple[str, ...]:
        if tier == "Large":
            return self.large
        if tier == "Small":
            return self.small
        raise ValueError(f"unknown input tier {tier!r}")

    def estimated_bytes(self, tier: str) -> int:
        return self.large_bytes if tier == "Large" else self.small_bytes


WORKLOADS = (
    Workload(
        "atax",
        "./amd/samples/atax",
        ("-x=8192", "-y=8192"),
        268_533_760,
        ("-x=1144", "-y=1144"),
        5_234_688,
    ),
    Workload(
        "bicg",
        "./amd/samples/bicg",
        ("-x=8192", "-y=8192"),
        268_566_528,
        ("-x=1144", "-y=1144"),
        5_237_264,
    ),
    Workload(
        "fdtd2d",
        "./amd/samples/polybench_fdtd2d",
        ("-size=4736", "-tmax=10"),
        269_156_352,
        ("-size=936", "-tmax=10"),
        10_513_152,
    ),
    Workload(
        "mvt",
        "./amd/samples/polybench_mvt",
        ("-size=8192",),
        268_566_528,
        ("-size=1144",),
        5_237_264,
    ),
    Workload(
        "lud",
        "./amd/samples/rodinia_lud",
        ("-size=5792",),
        134_189_056,
        ("-size=1024",),
        4_194_304,
        # ("-size=256",),
        # 262_144,
    ),
    Workload(
        "nw",
        "./amd/samples/nw",
        ("-length=4096",),
        201_424_908,
        ("-length=704",),
        5_947_392,
    ),
    Workload(
        "bfs",
        "./amd/samples/bfs",
        ("-node=4194304", "-degree=30"),
        536_870_916,
        ("-node=32768", "-degree=38"),
        5_242_884,
    ),
    Workload(
        "pagerank",
        "./amd/samples/pagerank",
        (
            "-node=2097152",
            "-sparsity=0.00001430511474609375",
            "-iterations=16",
        ),
        528_482_308,
        (
            "-node=32768",
            "-sparsity=0.000579833984375",
            "-iterations=16",
        ),
        5_373_954,
    ),
    Workload(
        "spmv",
        "./amd/samples/spmv",
        ("-dim=2097152", "-sparsity=0.00001430511474609375"),
        528_482_308,
        ("-dim=32768", "-sparsity=0.000579833984375"),
        5_373_954,
    ),
)
WORKLOAD_BY_NAME = {workload.name: workload for workload in WORKLOADS}


def atomic_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    temporary.replace(path)


def git_commit() -> str:
    result = subprocess.run(
        ["git", "rev-parse", "HEAD"],
        cwd=ROOT,
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout.strip()


def build_binaries(selected: set[str] | None = None) -> None:
    BIN.mkdir(parents=True, exist_ok=True)
    environment = os.environ.copy()
    environment["GOMEMLIMIT"] = GOMEMLIMIT
    environment.setdefault("GOCACHE", "/private/tmp/mgpusim-go-cache")
    for workload in WORKLOADS:
        if workload.name in TEMPORARILY_EXCLUDED_WORKLOADS:
            continue
        if selected and workload.name not in selected:
            continue
        command = [
            "go",
            "build",
            "-o",
            str(BIN / workload.name),
            workload.package,
        ]
        print("+", " ".join(command), flush=True)
        subprocess.run(command, cwd=ROOT, env=environment, check=True)
    manifest_path = BIN / "build_manifest.json"
    manifest: dict[str, Any] = {
        "gOMEMLIMIT": GOMEMLIMIT,
        "binaries": {},
    }
    if manifest_path.exists():
        manifest = json.loads(manifest_path.read_text())
        manifest["gOMEMLIMIT"] = GOMEMLIMIT
    for workload in WORKLOADS:
        if workload.name in TEMPORARILY_EXCLUDED_WORKLOADS:
            continue
        if selected and workload.name not in selected:
            continue
        manifest["binaries"][workload.name] = {
            "package": workload.package,
            "git_commit": git_commit(),
        }
    atomic_json(manifest_path, manifest)


def binary_commit(workload: str) -> str:
    manifest_path = BIN / "build_manifest.json"
    if not manifest_path.exists():
        return git_commit()
    manifest = json.loads(manifest_path.read_text())
    binary = manifest["binaries"].get(workload)
    if not binary:
        return git_commit()
    return str(binary["git_commit"])


def process_group_rss_bytes(pgid: int) -> int:
    result = subprocess.run(
        ["ps", "-axo", "pgid=,rss="],
        check=False,
        capture_output=True,
        text=True,
    )
    total_kib = 0
    for line in result.stdout.splitlines():
        fields = line.split()
        if len(fields) != 2:
            continue
        try:
            row_pgid, rss_kib = int(fields[0]), int(fields[1])
        except ValueError:
            continue
        if row_pgid == pgid:
            total_kib += rss_kib
    return total_kib * 1024


def stop_process_group_pid(pid: int) -> None:
    try:
        os.killpg(pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    deadline = time.monotonic() + 5
    while process_is_alive(pid) and time.monotonic() < deadline:
        time.sleep(0.05)
    if not process_is_alive(pid):
        return
    try:
        os.killpg(pid, signal.SIGKILL)
    except ProcessLookupError:
        return


def process_is_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def process_command(pid: int) -> str:
    result = subprocess.run(
        ["ps", "-p", str(pid), "-o", "command="],
        check=False,
        capture_output=True,
        text=True,
    )
    return result.stdout.strip()


def process_elapsed_seconds(pid: int) -> float:
    result = subprocess.run(
        ["ps", "-p", str(pid), "-o", "etime="],
        check=False,
        capture_output=True,
        text=True,
    )
    value = result.stdout.strip()
    if not value:
        raise RuntimeError(f"cannot determine elapsed time for PID {pid}")
    days = 0
    if "-" in value:
        day_text, value = value.split("-", 1)
        days = int(day_text)
    fields = [int(part) for part in value.split(":")]
    if len(fields) == 3:
        hours, minutes, seconds = fields
    elif len(fields) == 2:
        hours = 0
        minutes, seconds = fields
    else:
        hours = 0
        minutes = 0
        seconds = fields[0]
    return float((((days * 24) + hours) * 60 + minutes) * 60 + seconds)


def classify_failure(stdout: Path, stderr: Path) -> str:
    text = ""
    for path in (stdout, stderr):
        if path.exists():
            text += path.read_text(errors="replace").lower()
    if not text:
        return "RUNTIME_FAILURE"
    memory_markers = (
        "out of memory",
        "cannot allocate memory",
        "memory allocation failed",
        "runtime: out of memory",
    )
    if any(marker in text for marker in memory_markers):
        return "MEMORY_LIMIT_EXCEEDED"
    if "verification failed" in text or "mismatch" in text:
        return "VERIFY_FAILED"
    if "panic:" in text:
        return "RUNTIME_FAILURE"
    return "NONZERO_EXIT"


def metrics_are_complete(path: Path) -> bool:
    if not path.exists():
        return False
    try:
        with sqlite3.connect(path) as database:
            count = database.execute(
                "SELECT COUNT(*) FROM mgpusim_metrics"
            ).fetchone()
            kernel_time = database.execute(
                "SELECT COUNT(*) FROM mgpusim_metrics "
                "WHERE Location = 'Driver' AND What = 'kernel_time'"
            ).fetchone()
            page_size = database.execute(
                "SELECT Value FROM mgpusim_metrics "
                "WHERE Location = 'GMMU' AND What = 'page_size_bytes'"
            ).fetchone()
    except sqlite3.Error:
        return False
    return bool(
        count
        and count[0] > 0
        and kernel_time
        and kernel_time[0] == 1
        and page_size
        and float(page_size[0]) == 4096
    )


def run_directory(profile: str, mode: str, workload: str) -> Path:
    return RUNS / profile / mode / workload


def metric_database(directory: Path) -> Path:
    return directory / "metrics.sqlite3"


def run_one(
    workload: Workload,
    tier: str,
    profile: str,
    mode: str,
    *,
    directory: Path | None = None,
    force: bool = False,
) -> dict[str, Any]:
    directory = directory or run_directory(profile, mode, workload.name)
    directory.mkdir(parents=True, exist_ok=True)
    status_path = directory / "status.json"
    running_path = directory / "running.json"
    metrics_base = directory / "metrics"
    metrics_path = metric_database(directory)
    stdout_path = directory / "stdout.log"
    stderr_path = directory / "stderr.log"
    built_from = binary_commit(workload.name)

    if running_path.exists():
        running = json.loads(running_path.read_text())
        expected = str((BIN / workload.name).resolve())
        pid = int(running["pid"])
        if running.get("git_commit") != built_from:
            raise RuntimeError(
                f"{running_path}: active run uses commit "
                f"{running.get('git_commit')}, binary uses {built_from}"
            )
        if process_is_alive(pid):
            command_text = process_command(pid)
            if expected not in command_text:
                raise RuntimeError(
                    f"{running_path}: PID {pid} does not run {expected}"
                )
        print(
            f"reattach {workload.name} {profile} {mode} {tier}: pid={pid}",
            flush=True,
        )
        return monitor_run(
            workload=workload,
            tier=tier,
            profile=profile,
            mode=mode,
            directory=directory,
            command=list(running["command"]),
            built_from=built_from,
            pid=pid,
            started_unix=float(running["started_unix"]),
            initial_max_rss=int(running.get("max_rss_bytes", 0)),
            proc=None,
        )

    if not force and status_path.exists():
        status = json.loads(status_path.read_text())
        reusable = (
            status.get("git_commit") == built_from
            and status.get("input_tier") == tier
        )
        complete = status.get("status") != "PASS" or metrics_path.exists()
        if reusable and complete:
            print(
                f"resume {workload.name} {profile} {mode} {tier}: "
                f"{status.get('status')}",
                flush=True,
            )
            return status

    for path in (
        metrics_path,
        stdout_path,
        stderr_path,
        status_path,
        running_path,
    ):
        if path.exists():
            path.unlink()

    command = [
        str(BIN / workload.name),
        "-timing",
        "-arch=cdna3",
        "-gpu=mi300x",
        f"-translation-mode={mode}",
        f"-translation-profile={profile}",
        "-verify",
        "-report-all",
        "-disable-rtm",
        f"-metric-file-name={metrics_base}",
        *workload.args(tier),
    ]
    environment = os.environ.copy()
    environment["GOMEMLIMIT"] = GOMEMLIMIT
    print("+", " ".join(command), flush=True)
    stdout = stdout_path.open("wb")
    stderr = stderr_path.open("wb")
    try:
        proc = subprocess.Popen(
            command,
            cwd=ROOT,
            env=environment,
            stdout=stdout,
            stderr=stderr,
            start_new_session=True,
        )
    finally:
        stdout.close()
        stderr.close()
    started_unix = time.time()
    atomic_json(
        running_path,
        {
            "pid": proc.pid,
            "started_unix": started_unix,
            "max_rss_bytes": 0,
            "command": command,
            "git_commit": built_from,
        },
    )
    return monitor_run(
        workload=workload,
        tier=tier,
        profile=profile,
        mode=mode,
        directory=directory,
        command=command,
        built_from=built_from,
        pid=proc.pid,
        started_unix=started_unix,
        initial_max_rss=0,
        proc=proc,
    )


def monitor_run(  # noqa: PLR0913
    *,
    workload: Workload,
    tier: str,
    profile: str,
    mode: str,
    directory: Path,
    command: list[str],
    built_from: str,
    pid: int,
    started_unix: float,
    initial_max_rss: int,
    proc: subprocess.Popen[bytes] | None,
) -> dict[str, Any]:
    running_path = directory / "running.json"
    metrics_path = metric_database(directory)
    stdout_path = directory / "stdout.log"
    stderr_path = directory / "stderr.log"
    max_rss = initial_max_rss
    killed_for_memory = False
    last_checkpoint = 0.0

    try:
        while proc.poll() is None if proc is not None else process_is_alive(pid):
            rss = process_group_rss_bytes(pid)
            max_rss = max(max_rss, rss)
            if rss > RSS_LIMIT_BYTES:
                killed_for_memory = True
                stop_process_group_pid(pid)
                break
            now = time.monotonic()
            if now-last_checkpoint >= RUN_STATE_CHECKPOINT_SECONDS:
                atomic_json(
                    running_path,
                    {
                        "pid": pid,
                        "started_unix": started_unix,
                        "max_rss_bytes": max_rss,
                        "command": command,
                        "git_commit": built_from,
                    },
                )
                last_checkpoint = now
            time.sleep(RSS_INTERVAL_SECONDS)
    except BaseException:
        if process_is_alive(pid):
            stop_process_group_pid(pid)
        raise

    return_code: int | None
    if proc is not None:
        return_code = proc.wait()
    else:
        return_code = None
    max_rss = max(max_rss, process_group_rss_bytes(pid))
    wall = max(0.0, time.time() - started_unix)
    if killed_for_memory:
        outcome = "MEMORY_LIMIT_EXCEEDED"
    elif return_code == 0 and metrics_are_complete(metrics_path):
        outcome = "PASS"
    elif return_code is None and metrics_are_complete(metrics_path):
        outcome = "PASS"
    else:
        outcome = classify_failure(stdout_path, stderr_path)
    status = {
        "workload": workload.name,
        "translation_profile": profile,
        "mode": mode,
        "input_tier": tier,
        "input": workload.input_text(tier),
        "estimated_device_bytes": workload.estimated_bytes(tier),
        "status": outcome,
        "verify": outcome == "PASS",
        "return_code": return_code,
        "host_wall_seconds": wall,
        "max_rss_bytes": max_rss,
        "gOMEMLIMIT": GOMEMLIMIT,
        "rss_watchdog_bytes": RSS_LIMIT_BYTES,
        "rss_sampling_seconds": RSS_INTERVAL_SECONDS,
        "command": command,
        "git_commit": built_from,
        "metrics": str(metrics_path.relative_to(ROOT)),
        "stdout": str(stdout_path.relative_to(ROOT)),
        "stderr": str(stderr_path.relative_to(ROOT)),
    }
    atomic_json(directory / "status.json", status)
    if running_path.exists():
        running_path.unlink()
    print(
        f"{workload.name} {profile} {mode} {tier}: {outcome} "
        f"wall={wall:.1f}s rss={max_rss / 1024**3:.2f}GiB",
        flush=True,
    )
    return status


def adopt_run(
    workload: Workload,
    pid: int,
    tier: str,
    profile: str,
    mode: str,
) -> dict[str, Any]:
    directory = run_directory(profile, mode, workload.name)
    directory.mkdir(parents=True, exist_ok=True)
    expected = str((BIN / workload.name).resolve())
    command_text = process_command(pid)
    if not process_is_alive(pid) or expected not in command_text:
        raise SystemExit(f"PID {pid} is not an active {workload.name} binary")
    required = (
        "-timing",
        f"-translation-mode={mode}",
        f"-translation-profile={profile}",
        *workload.args(tier),
    )
    missing = [argument for argument in required if argument not in command_text]
    if missing:
        raise SystemExit(
            f"PID {pid} command does not match requested run; missing {missing}"
        )
    built_from = binary_commit(workload.name)
    started_unix = time.time() - process_elapsed_seconds(pid)
    command = command_text.split()
    atomic_json(
        directory / "running.json",
        {
            "pid": pid,
            "started_unix": started_unix,
            "max_rss_bytes": process_group_rss_bytes(pid),
            "command": command,
            "git_commit": built_from,
        },
    )
    return monitor_run(
        workload=workload,
        tier=tier,
        profile=profile,
        mode=mode,
        directory=directory,
        command=command,
        built_from=built_from,
        pid=pid,
        started_unix=started_unix,
        initial_max_rss=process_group_rss_bytes(pid),
        proc=None,
    )


def load_resolved() -> dict[str, Any]:
    if not RESOLVED.exists():
        raise SystemExit(f"{RESOLVED} does not exist; run resolve first")
    return json.loads(RESOLVED.read_text())


def write_baseline_summary(resolved: dict[str, Any]) -> None:
    path = ROOT / "out" / "latpc" / "baseline" / "summary.md"
    path.parent.mkdir(parents=True, exist_ok=True)
    lines = [
        "# LATPC baseline input resolution",
        "",
        "Mode: timing; GPU model: MI300X-class; page size: 4 KiB.",
        "",
        "Profile: `large-resource`; mode: `baseline`.",
        "",
        "| Workload | Tier | Input | Status | Wall (s) | Max RSS (GiB) | Reason |",
        "|---|---|---|---|---:|---:|---|",
    ]
    for name in WORKLOAD_BY_NAME:
        if name not in resolved["workloads"]:
            continue
        row = resolved["workloads"][name]
        lines.append(
            "| {name} | {tier} | `{input}` | {status} | {wall:.3f} | "
            "{rss:.3f} | {reason} |".format(
                name=name.upper(),
                tier=row["input_tier"],
                input=row["input"],
                status=row["status"],
                wall=row["host_wall_seconds"],
                rss=row["max_rss_bytes"] / 1024**3,
                reason=row.get("resolution_reason", ""),
            )
        )
    path.write_text("\n".join(lines) + "\n")


def resolve_inputs(
    selected: set[str] | None,
    force: bool,
    input_tier: str,
    jobs: int,
) -> None:
    if not BIN.exists():
        build_binaries(selected)
    resolved = {
        "schema_version": 1,
        "policy": (
            "Small selected by explicit user request"
            if input_tier == "Small"
            else "Large first; Small only on MEMORY_LIMIT_EXCEEDED"
        ),
        "gOMEMLIMIT": GOMEMLIMIT,
        "rss_watchdog_bytes": RSS_LIMIT_BYTES,
        "rss_sampling_seconds": RSS_INTERVAL_SECONDS,
        "gpu": "mi300x",
        "architecture": "cdna3",
        "page_size_bytes": 4096,
        "git_commit": git_commit(),
        "workloads": {},
    }
    if RESOLVED.exists() and not force:
        resolved = load_resolved()
    resolved["policy"] = (
        "Small selected by explicit user request"
        if input_tier == "Small"
        else "Large first; Small only on MEMORY_LIMIT_EXCEEDED"
    )
    resolved["git_commit"] = git_commit()

    to_resolve: list[Workload] = []
    for workload in WORKLOADS:
        if selected and workload.name not in selected:
            continue
        existing = resolved["workloads"].get(workload.name)
        if workload.name in TEMPORARILY_EXCLUDED_WORKLOADS:
            resolved["workloads"][workload.name] = {
                "input_tier": "Small",
                "input": workload.input_text("Small"),
                "estimated_device_bytes": workload.estimated_bytes("Small"),
                "status": "EXCLUDED",
                "verify": False,
                "host_wall_seconds": 0,
                "max_rss_bytes": 0,
                "selected_for_matrix": False,
                "temporarily_excluded": True,
                "resolution_reason": (
                    "Temporarily excluded by user request; an already-running "
                    "FDTD2D process was left untouched"
                ),
            }
            continue
        # Do not trust the resolution record as a cache. It deliberately does
        # not contain the binary provenance, whereas run_one's status record
        # does. In particular, a repaired binary must re-run a previously
        # failing baseline rather than merely reusing its old failure.
        # run_one is inexpensive when its provenance is current, because it
        # performs that precise reuse check itself.
        to_resolve.append(workload)

    def resolve_one(workload: Workload) -> tuple[str, dict[str, Any]]:
        if input_tier == "Small":
            chosen = run_one(
                workload,
                "Small",
                "large-resource",
                "baseline",
                force=force,
            )
            reason = "Small selected by explicit user request"
        else:
            large = run_one(
                workload,
                "Large",
                "large-resource",
                "baseline",
                force=force,
            )
            chosen = large
            reason = "Large completed without host memory exhaustion"
            if large["status"] == "MEMORY_LIMIT_EXCEEDED":
                oom_directory = RUNS / "resolution-large-oom" / workload.name
                oom_directory.mkdir(parents=True, exist_ok=True)
                source_directory = run_directory(
                    "large-resource", "baseline", workload.name
                )
                for filename in ("status.json", "stdout.log", "stderr.log"):
                    source = source_directory / filename
                    if source.exists():
                        source.replace(oom_directory / filename)
                metrics = metric_database(source_directory)
                if metrics.exists():
                    metrics.replace(oom_directory / metrics.name)
                chosen = run_one(
                    workload,
                    "Small",
                    "large-resource",
                    "baseline",
                    force=True,
                )
                reason = (
                    "Small selected because Large exceeded the 11.5 GiB RSS "
                    "watchdog"
                )
        return workload.name, {
            "input_tier": chosen["input_tier"],
            "input": chosen["input"],
            "estimated_device_bytes": chosen["estimated_device_bytes"],
            "status": chosen["status"],
            "verify": chosen["verify"],
            "host_wall_seconds": chosen["host_wall_seconds"],
            "max_rss_bytes": chosen["max_rss_bytes"],
            "selected_for_matrix": chosen["status"] == "PASS",
            "resolution_reason": reason,
        }

    with ThreadPoolExecutor(max_workers=jobs) as executor:
        futures = [executor.submit(resolve_one, workload) for workload in to_resolve]
        for future in as_completed(futures):
            name, resolution = future.result()
            resolved["workloads"][name] = resolution
            atomic_json(RESOLVED, resolved)
    write_baseline_summary(resolved)


def run_matrix(selected: set[str] | None, force: bool, jobs: int) -> None:
    resolved = load_resolved()
    if not BIN.exists():
        build_binaries(selected)
    tasks: list[tuple[Workload, str, str, str]] = []
    for workload in WORKLOADS:
        if selected and workload.name not in selected:
            continue
        resolution = resolved["workloads"].get(workload.name)
        if workload.name in TEMPORARILY_EXCLUDED_WORKLOADS:
            print(f"skip {workload.name}: temporarily excluded", flush=True)
            continue
        if not resolution or not resolution.get("selected_for_matrix"):
            print(f"skip {workload.name}: baseline did not pass", flush=True)
            continue
        tier = resolution["input_tier"]
        for profile in PROFILES:
            for mode in MODES:
                tasks.append((workload, tier, profile, mode))
    with ThreadPoolExecutor(max_workers=jobs) as executor:
        futures = [
            executor.submit(run_one, workload, tier, profile, mode, force=force)
            for workload, tier, profile, mode in tasks
        ]
        for future in as_completed(futures):
            future.result()


def read_metrics(path: Path) -> list[tuple[str, str, float, str]]:
    if not path.exists():
        return []
    with sqlite3.connect(path) as database:
        return [
            (str(location), str(what), float(value), str(unit))
            for location, what, value, unit in database.execute(
                "SELECT Location, What, Value, Unit FROM mgpusim_metrics"
            )
        ]


def metric_sum(
    metrics: list[tuple[str, str, float, str]],
    what: str,
    location_contains: str | None = None,
    location_equals: str | None = None,
) -> float:
    return sum(
        value
        for location, metric, value, _ in metrics
        if metric == what
        and (location_contains is None or location_contains in location)
        and (location_equals is None or location == location_equals)
    )


def metric_first(
    metrics: list[tuple[str, str, float, str]],
    what: str,
    location_equals: str,
) -> float:
    values = [
        value
        for location, metric, value, _ in metrics
        if metric == what and location == location_equals
    ]
    return values[0] if values else math.nan


CSV_COLUMNS = (
    "workload",
    "git_commit",
    "translation_profile",
    "mode",
    "input_tier",
    "input",
    "estimated_device_bytes",
    "verify",
    "status",
    "sim_cycles",
    "sim_kernel_time",
    "ipc",
    "host_wall_seconds",
    "max_rss_bytes",
    "l1_tlb_accesses",
    "l1_tlb_misses",
    "l1_mshr_failures",
    "l2_tlb_misses",
    "ptw_invocations",
    "pwq_stall_cycles",
    "avg_walk_latency",
    "latc_compression_ratio",
    "latp_walks_saved",
    "prefetch_coverage",
    "prefetch_accuracy",
)


def result_row(status: dict[str, Any], directory: Path) -> dict[str, Any]:
    metrics = read_metrics(metric_database(directory))
    kernel_time = metric_first(metrics, "kernel_time", "Driver")
    sim_cycles = (
        round(kernel_time * GPU_CLOCK_HZ) if not math.isnan(kernel_time) else ""
    )
    instructions = metric_sum(metrics, "cu_inst_count")
    ipc = (
        instructions / sim_cycles
        if isinstance(sim_cycles, int) and sim_cycles > 0
        else math.nan
    )
    page_size = metric_first(metrics, "page_size_bytes", "GMMU")
    if metrics and page_size != 4096:
        raise RuntimeError(
            f"{status['workload']}: GMMU reported page size {page_size}, want 4096"
        )
    return {
        "workload": status["workload"],
        "git_commit": status["git_commit"],
        "translation_profile": status["translation_profile"],
        "mode": status["mode"],
        "input_tier": status["input_tier"],
        "input": status["input"],
        "estimated_device_bytes": status["estimated_device_bytes"],
        "verify": str(bool(status["verify"])).lower(),
        "status": status["status"],
        "sim_cycles": sim_cycles,
        "sim_kernel_time": "" if math.isnan(kernel_time) else kernel_time,
        "ipc": "" if math.isnan(ipc) else ipc,
        "host_wall_seconds": status["host_wall_seconds"],
        "max_rss_bytes": status["max_rss_bytes"],
        "l1_tlb_accesses": metric_sum(metrics, "accesses", "L1"),
        "l1_tlb_misses": metric_sum(metrics, "miss", "L1"),
        "l1_mshr_failures": metric_first(
            metrics, "reservation_failures", "LATPC.LATC"
        ),
        "l2_tlb_misses": metric_sum(metrics, "miss", "L2TLB"),
        "ptw_invocations": metric_first(metrics, "walks_started", "GMMU"),
        "pwq_stall_cycles": metric_first(metrics, "pwq_full_stalls", "GMMU"),
        "avg_walk_latency": metric_first(
            metrics, "walk_average_latency", "GMMU"
        ),
        "latc_compression_ratio": metric_first(
            metrics, "compression_ratio", "LATPC.LATC"
        ),
        "latp_walks_saved": metric_first(metrics, "walks_saved", "LATPC.LATP"),
        "prefetch_coverage": metric_first(
            metrics, "prefetch_coverage", "LATPC.LATP"
        ),
        "prefetch_accuracy": metric_first(
            metrics, "prefetch_accuracy", "LATPC.LATP"
        ),
    }


def collect_results() -> None:
    resolved = load_resolved()
    rows: list[dict[str, Any]] = []
    for workload in WORKLOADS:
        resolution = resolved["workloads"].get(workload.name)
        if not resolution:
            continue
        if not resolution.get("selected_for_matrix"):
            rows.append(
                {
                    column: ""
                    for column in CSV_COLUMNS
                }
                | {
                    "workload": workload.name,
                    "git_commit": "",
                    "translation_profile": "large-resource",
                    "mode": "baseline",
                    "input_tier": resolution["input_tier"],
                    "input": resolution["input"],
                    "estimated_device_bytes": resolution[
                        "estimated_device_bytes"
                    ],
                    "verify": str(bool(resolution["verify"])).lower(),
                    "status": resolution["status"],
                    "host_wall_seconds": resolution["host_wall_seconds"],
                    "max_rss_bytes": resolution["max_rss_bytes"],
                }
            )
            continue
        for profile in PROFILES:
            for mode in MODES:
                directory = run_directory(profile, mode, workload.name)
                status_path = directory / "status.json"
                if not status_path.exists():
                    continue
                status = json.loads(status_path.read_text())
                rows.append(result_row(status, directory))
    OUT.mkdir(parents=True, exist_ok=True)
    with RESULTS.open("w", newline="") as output:
        writer = csv.DictWriter(output, fieldnames=CSV_COLUMNS)
        writer.writeheader()
        writer.writerows(rows)
    write_functional_matrix(rows)
    write_opportunity_report(rows)


def write_functional_matrix(rows: list[dict[str, Any]]) -> None:
    by_key = {
        (row["workload"], row["translation_profile"], row["mode"]): row
        for row in rows
    }
    lines = [
        "# LATPC functional correctness matrix",
        "",
        "Every entry is a timing-mode run with application verification enabled.",
        "All runs use the workload's resolved input and a 4 KiB page.",
        "",
        "| Workload | Profile | Baseline | LATC | LATP | LATPC | Ideal |",
        "|---|---|---|---|---|---|---|",
    ]
    resolved = load_resolved()
    for workload in WORKLOADS:
        resolution = resolved["workloads"].get(workload.name)
        if not resolution or not resolution.get("selected_for_matrix"):
            continue
        for profile in PROFILES:
            statuses = []
            for mode in MODES:
                row = by_key.get((workload.name, profile, mode))
                statuses.append(row["status"] if row else "NOT_RUN")
            lines.append(
                f"| {workload.name.upper()} | {profile} | "
                + " | ".join(statuses)
                + " |"
            )
    (OUT / "functional_matrix.md").write_text("\n".join(lines) + "\n")


def write_opportunity_report(rows: list[dict[str, Any]]) -> None:
    del rows
    lines = [
        "# LATPC translation opportunity",
        "",
        "Counters are from each passing `latpc` timing run. Page-divergence "
        "buckets report unique 4 KiB VPNs per vector-memory instruction.",
        "",
        "| Workload | Profile | Instructions | Regularity coverage | "
        "Same-leaf regular rate | Page-divergence distribution |",
        "|---|---|---:|---:|---:|---|",
    ]
    for workload in WORKLOADS:
        for profile in PROFILES:
            directory = run_directory(profile, "latpc", workload.name)
            status_path = directory / "status.json"
            if not status_path.exists():
                continue
            status = json.loads(status_path.read_text())
            if status["status"] != "PASS":
                continue
            metrics = read_metrics(metric_database(directory))
            instructions = metric_first(
                metrics, "instructions", "LATPC.Detector"
            )
            coverage = metric_first(
                metrics, "regularity_coverage", "LATPC.Detector"
            )
            same_leaf = metric_first(
                metrics,
                "same_leaf_regular_grouping_rate",
                "LATPC.Detector",
            )
            divergence = []
            for location, what, value, _ in metrics:
                if (
                    location == "LATPC.Detector"
                    and what.startswith("page_divergence_")
                ):
                    divergence.append((int(what.rsplit("_", 1)[1]), int(value)))
            divergence.sort()
            distribution = ", ".join(
                f"{pages}:{count}" for pages, count in divergence
            )
            lines.append(
                f"| {workload.name.upper()} | {profile} | "
                f"{instructions:.0f} | {coverage:.4f} | {same_leaf:.4f} | "
                f"`{distribution}` |"
            )
    path = ROOT / "out" / "latpc" / "opportunity" / "page_divergence.md"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(lines) + "\n")


def selected_names(values: list[str]) -> set[str] | None:
    if not values:
        return None
    unknown = set(values) - set(WORKLOAD_BY_NAME)
    if unknown:
        raise SystemExit("unknown workloads: " + ", ".join(sorted(unknown)))
    return set(values)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "command",
        choices=("build", "resolve", "matrix", "collect", "all", "adopt"),
    )
    parser.add_argument(
        "--workload",
        action="append",
        default=[],
        help="limit work to one workload; may be repeated",
    )
    parser.add_argument("--force", action="store_true")
    parser.add_argument(
        "--input-tier",
        choices=("auto", "Small"),
        default="auto",
        help="resolve Large-first automatically or force the documented Small input",
    )
    parser.add_argument(
        "--jobs",
        type=int,
        default=1,
        help="maximum concurrently supervised timing simulations",
    )
    parser.add_argument("--pid", type=int)
    parser.add_argument("--tier", choices=("Large", "Small"), default="Large")
    parser.add_argument("--profile", choices=PROFILES, default="large-resource")
    parser.add_argument("--mode", choices=MODES, default="baseline")
    args = parser.parse_args()
    if args.jobs <= 0:
        raise SystemExit("--jobs must be positive")
    selected = selected_names(args.workload)
    if args.command == "adopt":
        if args.pid is None or selected is None or len(selected) != 1:
            raise SystemExit(
                "adopt requires --pid and exactly one --workload"
            )
        adopt_run(
            WORKLOAD_BY_NAME[next(iter(selected))],
            args.pid,
            args.tier,
            args.profile,
            args.mode,
        )
        return
    if args.command in ("build", "all"):
        build_binaries(selected)
    if args.command in ("resolve", "all"):
        resolve_inputs(selected, args.force, args.input_tier, args.jobs)
    if args.command in ("matrix", "all"):
        run_matrix(selected, args.force, args.jobs)
    if args.command in ("collect", "all"):
        collect_results()


if __name__ == "__main__":
    main()
