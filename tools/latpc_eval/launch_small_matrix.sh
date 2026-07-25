#!/bin/sh
# Launch the resumable LATPC Small-input timing matrix.
set -eu

latpc_jobs=${1:-2}
case "$latpc_jobs" in
    ''|*[!0-9]*)
        echo "worker count must be a positive integer" >&2
        exit 2
        ;;
esac
if [ "$latpc_jobs" -lt 1 ]; then
    echo "worker count must be positive" >&2
    exit 2
fi

exec env GOMEMLIMIT=10GiB python3 tools/latpc_eval/run.py all \
    --input-tier Small --jobs "$latpc_jobs"
