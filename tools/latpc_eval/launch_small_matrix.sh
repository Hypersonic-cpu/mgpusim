#!/bin/sh
# Launch the staged five-workload LATPC Small-input timing matrix.
set -eu

latpc_baseline_jobs=${1:-6}
latpc_mechanism_jobs=${2:-5}
case "$latpc_baseline_jobs" in
    ''|*[!0-9]*)
        echo "baseline worker count must be a positive integer" >&2
        exit 2
        ;;
esac
if [ "$latpc_baseline_jobs" -lt 1 ]; then
    echo "baseline worker count must be positive" >&2
    exit 2
fi
case "$latpc_mechanism_jobs" in
    ''|*[!0-9]*)
        echo "mechanism worker count must be a positive integer" >&2
        exit 2
        ;;
esac
if [ "$latpc_mechanism_jobs" -lt 1 ]; then
    echo "mechanism worker count must be positive" >&2
    exit 2
fi

latpc_log_dir=out/latpc/evaluation/controller
mkdir -p "$latpc_log_dir"

run_stage() {
    latpc_stage=$1
    latpc_initial_wait=$2
    latpc_periodic_wait=$3
    shift 3

    "$@" >"$latpc_log_dir/$latpc_stage.log" 2>&1 &
    latpc_pid=$!
    sleep "$latpc_initial_wait"
    while kill -0 "$latpc_pid" 2>/dev/null; do
        date '+%Y-%m-%dT%H:%M:%S%z %Z' >>"$latpc_log_dir/$latpc_stage.log"
        sleep "$latpc_periodic_wait"
    done
    wait "$latpc_pid"
}

check_baselines() {
    python3 - <<'PY'
import json
from pathlib import Path

root = Path("out/latpc/evaluation/runs")
workloads = ("atax", "bicg", "mvt", "nw", "bfs")
profiles = ("large-resource", "paper")
failures = []
for profile in profiles:
    for workload in workloads:
        path = root / profile / "baseline" / workload / "status.json"
        if not path.exists():
            failures.append(f"{profile}/{workload}: missing status")
            continue
        status = json.loads(path.read_text())
        if status.get("status") != "PASS" or not status.get("verify"):
            failures.append(
                f"{profile}/{workload}: {status.get('status')} "
                f"verify={status.get('verify')}"
            )
if failures:
    raise SystemExit("baseline failure; mechanisms were not launched:\n" + "\n".join(failures))
PY
}

run_stage resolve_baseline 900 3600 \
    env GOMEMLIMIT=10GiB python3 tools/latpc_eval/run.py resolve \
    --force --input-tier Small --jobs "$latpc_baseline_jobs"
run_stage baseline_paper 900 3600 \
    env GOMEMLIMIT=10GiB python3 tools/latpc_eval/run.py matrix \
    --force --profile paper --mode baseline --jobs "$latpc_baseline_jobs"
check_baselines
run_stage mechanisms 900 3600 \
    env GOMEMLIMIT=10GiB python3 tools/latpc_eval/run.py matrix \
    --force --mode latc --mode latp --mode latpc --mode ideal \
    --jobs "$latpc_mechanism_jobs"
env GOMEMLIMIT=10GiB python3 tools/latpc_eval/run.py collect
env GOMEMLIMIT=10GiB python3 tools/latpc_plot/plot.py
env GOMEMLIMIT=10GiB python3 tools/latpc_eval/report.py
