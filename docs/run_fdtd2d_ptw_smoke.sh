#!/usr/bin/env bash
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

OUT="${ROOT}/out/ptw-fdtd2d-smoke"
BIN="${OUT}/fdtd2d"
mkdir -p "$OUT"

echo "[build] amd/samples/polybench_fdtd2d"
go build -o "$BIN" ./amd/samples/polybench_fdtd2d

run_case() {
  local size="$1"
  local dir="${OUT}/size-${size}"
  mkdir -p "$dir"

  echo "[run] FDTD2D size=${size}, tmax=10"
  /usr/bin/time -p \
    "$BIN" \
      -timing \
      -arch=cdna3 \
      -gpu=mi300x \
      -translation-mode=baseline \
      -translation-profile=paper \
      -verify \
      -disable-rtm \
      -size="$size" \
      -tmax=10 \
      >"${dir}/stdout.log" \
      2>"${dir}/stderr.log"

  echo "PASS size=${size}" | tee "${dir}/status.txt"
}

run_case 16
run_case 32
run_case 64

echo
echo "All FDTD2D smoke cases passed."
echo "Output: ${OUT}"
