# Codex Execution Plan: LATPC Extension and Evaluation

> **Repository root:** current working directory
> **Single source of truth for architecture and mechanism:** `docs/PTW_LATPC_EXT.md`
> **Starting point:** the detailed memory-backed page-table walker is already implemented
> **Scope:** single-GPU MGPUSim only

---

# 0. Non-negotiable rule: read the design document first

Before changing any code, read the entire file:

```text
docs/PTW_LATPC_EXT.md
```

Treat that document as the **only authoritative specification** for:

- Regularity Detector behavior;
- LATC semantics;
- LATP semantics;
- LATPC integration;
- group formation;
- MSHR and PW-buffer behavior;
- PWC/PTW interaction;
- request completion and replay;
- invalidation, reset, drain, and fault behavior;
- required hardware structures and timing assumptions.

This plan does **not** redefine those mechanisms.

When this plan and `docs/PTW_LATPC_EXT.md` appear inconsistent:

1. stop;
2. quote the conflicting sections;
3. explain the code impact;
4. ask for a design decision;
5. do not silently choose one interpretation.

Do not introduce an alternative LATPC design because it looks simpler.

---

# 1. Execution discipline

## 1.1 Stop conditions

Stop and report when:

- the existing detailed PTW baseline is incorrect;
- the timing path still uses a flat host-side translation lookup;
- PTW traffic does not pass through shared L2;
- L1 caches receive virtual addresses;
- current code materially conflicts with `docs/PTW_LATPC_EXT.md`;
- more than four selected workloads fail after permitted OOM fallback;
- a debug trace exceeds 20 MiB;
- unrelated dirty files overlap the implementation;
- a requested commit would include unrelated changes.

## 1.2 Git policy

Create a git commit after every major validated step:

1. mode/profile infrastructure;
2. translation-group metadata;
3. Regularity Detector;
4. LATC;
5. LATP;
6. full LATPC integration;
7. statistics;
8. each validated diagnosis-driven fix;
9. experiment/plotting tooling.

Before every commit:

```bash
git status --short
git diff --check
go test <changed packages>
git diff --cached --stat
git diff --cached
```

For integration milestones also run:

```bash
go test ./...
```

Do not use:

```text
git add -A
git commit --amend
git rebase
git reset --hard
git push --force
```

Do not commit failed speculative changes.

---

# 2. Required experiment configuration

## 2.1 Translation modes

Use one MI300X-class builder and one implementation.

Do not copy the GPU model into a separate `MI300X-LATPC` implementation.

Provide these modes:

```text
baseline
latc
latp
latpc
ideal
```

Expected relationship:

| Mode | Detector | LATC | LATP | Detailed miss walk |
|---|---:|---:|---:|---:|
| baseline | Off | Off | Off | Yes |
| latc | On | On | Off | Yes |
| latp | On | Off | On | Yes |
| latpc | On | On | On | Yes |
| ideal | Bypassed | Bypassed | Bypassed | No |

The exact mechanism behavior comes from:

```text
docs/PTW_LATPC_EXT.md
```

Expose one selector:

```text
-translation-mode baseline|latc|latp|latpc|ideal
```

## 2.2 Translation-resource profiles

Implement two named presets.

### Paper profile

```text
page size     4 KiB
L1 TLB        32 entries
L2 TLB        1024 entries
PWQ           128 entries
walkers       16
PWC           16 / 16 / 16
```

### Large-resource sensitivity profile

```text
page size     4 KiB
L1 TLB        64 entries
L2 TLB        4096 entries
PWQ           256 entries
walkers       32
PWC           32 / 32 / 32
```

Expose:

```text
-translation-profile paper|large-resource
```

The large-resource profile is a sensitivity configuration, not a claim about undocumented physical MI300X TLB/PWC capacities.

Within one profile, all five translation modes must share identical non-LATPC parameters.

---

# 3. Workload inputs

Use the Large input first.

Use Small only when Large fails specifically because of memory exhaustion.

Do not fall back to Small for:

- weak LATPC speedup;
- long execution time;
- timeout;
- verification failure;
- simulator deadlock;
- implementation bug.

## 3.1 Large and Small configurations

| Workload | Large | Estimated device allocation | Small OOM fallback | Estimated device allocation |
|---|---|---:|---|---:|
| ATAX | `-x 8192 -y 8192` | 268,533,760 B | `-x 1618 -y 1618` | 10,491,112 B |
| BICG | `-x 8192 -y 8192` | 268,566,528 B | `-x 1618 -y 1618` | 10,497,584 B |
| FDTD2D | `-size 4736 -tmax 10` | 269,156,352 B | `-size 936 -tmax 10` | 10,513,152 B |
| MVT | `-size 8192` | 268,566,528 B | `-size 1618` | 10,497,584 B |
| LUD | `-size 5792` | 134,189,056 B | `-size 1616` | 10,445,824 B |
| NW | `-length 4096` | 201,424,908 B | `-length 960` | 11,082,252 B |
| BFS | `-node 4194304 -degree 30` | 536,870,916 B | `-node 65536 -degree 38` | 10,485,768 B |
| PageRank | `-node 2097152 -sparsity 0.00001430511474609375 -iterations 16` | 528,482,308 B | `-node 65536 -sparsity 0.0002899169921875 -iterations 16` | 10,747,908 B |
| SpMV | `-dim 2097152 -sparsity 0.00001430511474609375` | 528,482,308 B | `-dim 65536 -sparsity 0.0002899169921875` | 10,747,908 B |

## 3.2 macOS memory control

Use:

```text
GOMEMLIMIT             = 10 GiB
RSS watchdog threshold = 11.5 GiB
sampling interval      = 200 ms
```

Terminate the entire process group and classify the result as:

```text
MEMORY_LIMIT_EXCEEDED
```

Record resolved inputs in:

```text
out/latpc/evaluation/resolved_inputs.json
```

Once a workload resolves to Large or Small, every mode for that workload must use the same input.

Never compare: large baseline against Small LATPC.

## 3.3 FDTD2D restriction

The current large and small FDTD2D inputs exceed one work-group.

Do not include FDTD2D in timing GMean until the known cross-kernel cache-coherence issue is fixed and all modes verify.

It may remain in functional testing and be reported as excluded from timing.

---

# Phase 0 — Audit the existing PTW baseline

Read:

```text
docs/PTW_LATPC_EXT.md
```

Then audit the current implementation.

Confirm:

- page tables live in simulated physical memory;
- each PID has a physical root;
- the walker is level-generic;
- PTW reads use physical addresses;
- PTW requests use shared L2;
- PWC behavior matches the design document;
- L2 TLB miss handling and MSHR coalescing are correct;
- completed translations fill the correct TLB levels;
- replay uses the correct physical address;
- vector, scalar, and instruction L1 accesses are translated;
- reset, drain, invalidation, and faults work.

Write:

```text
out/latpc/preflight/ptw_baseline_audit.md
```

Run:

```bash
go test ./...
```

Run one small end-to-end timing workload with debug disabled.

### Gate

Do not implement LATPC until the baseline passes.

---

# Phase 1 — Add modes and resource profiles

Implement:

```text
-translation-mode
-translation-profile
```

Do not implement LATPC mechanisms yet.

Add tests that prove:

- each mode selects the intended mechanism flags;
- each resource profile selects the intended capacities;
- all five modes have identical non-LATPC parameters within one profile;
- invalid names fail clearly;
- ideal mode preserves correctness.

Run the nine workloads in:

```text
profile = large-resource
mode    = baseline
input   = Large first, Small only on OOM
```

Generate:

```text
out/latpc/baseline/summary.md
out/latpc/evaluation/resolved_inputs.json
```

### Commit

```text
config: add latpc modes and translation resource profiles
```

---

# Phase 2 — Preserve required translation-group metadata

Follow the translation-group definition in:

```text
docs/PTW_LATPC_EXT.md
```

Trace metadata through:

```text
wavefront instruction
-> coalescer
-> L1 TLB
-> L2 TLB
-> GMMU
```

Requirements:

- preserve wavefront instruction identity;
- preserve lane/thread ordering required by the design;
- merge duplicate VPN requests correctly;
- preserve all replay waiters;
- baseline behavior remains unchanged;
- baseline adds no detector timing.

Collect opportunity metrics:

- unique VPNs per wavefront memory instruction;
- page-divergence distribution;
- regular groups;
- same-leaf-page groups;
- stride distribution.

Write:

```text
out/latpc/opportunity/page_divergence.md
```

### Gate

At least one selected workload must exhibit a valid LATPC opportunity.

### Commit

```text
vm: preserve latpc translation group metadata
```

---

# Phase 3 — Implement the Regularity Detector

Implement the detector exactly as specified in:

```text
docs/PTW_LATPC_EXT.md
```

Do not copy an alternative detector from this plan.

Required validation:

- regular ascending pattern;
- regular descending pattern;
- non-unit stride;
- irregular pattern;
- multiple groups in one instruction;
- duplicate VPN handling;
- group boundary at final-level PT page;
- wave64 behavior;
- four-level and five-level page-table formats;
- detector bypass in baseline and ideal modes.

Add a bounded debug category:

```text
LATPCDetector
```

### Commit

```text
latpc: implement the regularity detector
```

---

# Phase 4 — Implement LATC

Implement LATC according to:

```text
docs/PTW_LATPC_EXT.md
```

Do not infer LATC behavior from global TLB arrival order.

Required validation:

- one regular group reduces L1 TLB MSHR entry usage;
- all original waiters remain replayable;
- mixed L1 hit/miss behavior;
- mixed L2 hit/miss behavior;
- same-VPN merging remains correct;
- partial and out-of-order member completion;
- backpressure;
- invalidation;
- reset;
- drain;
- fault handling;
- LATC-disabled behavior matches baseline.

Required statistics:

- groups presented to LATC;
- compressed MSHR allocations;
- represented translations;
- compression ratio;
- MSHR occupancy;
- reservation failures;
- grouped-entry lifetime;
- per-member completion delay.

Run:

```text
baseline
latc
```

on one regular and one irregular workload using the same resolved input.

### Commit

```text
latpc: implement l1 tlb mshr compression
```

---

# Phase 5 — Implement LATP

Implement LATP according to:

```text
docs/PTW_LATPC_EXT.md
```

Preserve per-VPN L2 TLB/MSHR correctness.

Required validation:

- grouped PW-buffer behavior;
- one shared upper traversal;
- per-member final-level PTE reads;
- shared-L2 PTW traffic;
- merge while queued;
- merge during upper walk;
- merge during leaf walk;
- out-of-order responses;
- individual TLB fills;
- exact L2 MSHR release;
- PWQ/walker backpressure;
- invalidation;
- reset;
- drain;
- faults;
- LATP-disabled behavior matches baseline.

Required statistics:

- LATP groups;
- members per group;
- independent walks avoided;
- upper-level reads avoided;
- leaf PTE reads;
- PWQ occupancy;
- walker occupancy;
- queue delay;
- useful/late/unused prefetched translations;
- prefetch coverage;
- prefetch accuracy.

Run:

```text
baseline
latp
```

on one regular and one irregular workload using the same resolved input.

### Commit

```text
latpc: implement grouped page table walks
```

---

# Phase 6 — Integrate full LATPC

Compose Detector + LATC + LATP exactly as specified in:

```text
docs/PTW_LATPC_EXT.md
```

Validate the complete path:

```text
coalescer
-> detector
-> LATC
-> L2 TLB/MSHR
-> LATP
-> PTW/PWC/L2
-> TLB fills
-> member completion
-> replay
```

Run one bounded trace with:

```text
LATPCDetector,LATC,LATP,GMMUWalk,PWC,TLBFill,TLBReplay
```

Requirements:

- log stays below 20 MiB;
- no request is lost;
- no request is replayed twice;
- each MSHR releases exactly once;
- each PW-buffer entry releases exactly once;
- physical replay addresses are correct.

Write:

```text
out/latpc/trace/latpc_validation.md
```

### Commit

```text
latpc: integrate compression and grouped walks
```

---

# Phase 7 — Complete and validate statistics

Use the statistics required by:

```text
docs/PTW_LATPC_EXT.md
```

At minimum validate:

## Opportunity

- page-divergence distribution;
- regularity coverage;
- same-leaf-page grouping rate.

## LATC

- compression ratio;
- MSHR occupancy;
- reservation failures;
- grouped-entry lifetime.

## LATP

- walks saved;
- upper-level reads saved;
- prefetch coverage;
- prefetch accuracy;
- useful/late/unused prefetches;
- PWQ/walker pressure.

## End-to-end

- L1/L2 TLB hit/miss;
- PTW count;
- translation latency;
- replay delay;
- kernel cycles/time;
- ideal-translation gap.

Add counter invariants and drain-time zero-leak assertions.

### Commit

```text
metrics: add and validate latpc statistics
```

---

# Phase 8 — Functional regression

For every baseline-passing workload, run:

```text
paper profile:
    baseline latc latp latpc ideal

large-resource profile:
    baseline latc latp latpc ideal
```

Use the same resolved input within each comparison.

All modes must produce identical application results.

Generate:

```text
out/latpc/evaluation/functional_matrix.md
```

Do not mix paper-profile and large-resource-profile results.

---

# Phase 9 — Timing experiment

## 9.1 Primary result

Use:

```text
translation-profile = large-resource
```

For each workload, use the resolved Large or Small tier from:

```text
out/latpc/evaluation/resolved_inputs.json
```

Run:

```text
baseline
latc
latp
latpc
ideal
```

## 9.2 Mechanism-fidelity result

Run the same five modes with:

```text
translation-profile = paper
```

Report this separately.

## 9.3 Raw results

Write:

```text
out/latpc/evaluation/results.csv
```

Required columns:

```text
workload
translation_profile
mode
input_tier
input
estimated_device_bytes
verify
status
sim_cycles
sim_kernel_time
ipc
host_wall_seconds
max_rss_bytes
l1_tlb_accesses
l1_tlb_misses
l1_mshr_failures
l2_tlb_misses
ptw_invocations
pwq_stall_cycles
avg_walk_latency
latc_compression_ratio
latp_walks_saved
prefetch_coverage
prefetch_accuracy
```

---

# Phase 10 — Required figures

Use reproducible Python scripts with `matplotlib`.

Do not use seaborn.

Store scripts in:

```text
tools/latpc_plot/
```

Store figures in:

```text
out/latpc/evaluation/figures/
```

## 10.1 Primary application comparison

Generate:

```text
latpc_speedup_by_workload.png
latpc_speedup_by_workload.pdf
```

Use only:

```text
translation-profile = large-resource
```

Show:

- every selected passing workload;
- GMean;
- LATC;
- LATP;
- LATPC;
- Ideal;
- baseline line at 1.0;
- visible marker for Small/OOM fallback workloads.

Speedup:

```text
baseline cycles / mode cycles
```

Use geometric mean.

## 10.2 Translation bottleneck figure

Generate:

```text
latpc_translation_bottlenecks.png
latpc_translation_bottlenecks.pdf
```

Compare:

- normalized PWQ/PTW stall;
- L1 TLB MSHR reservation-failure rate.

## 10.3 Mechanism quality figure

Generate:

```text
latpc_prefetch_quality.png
latpc_prefetch_quality.pdf
```

Show:

- prefetch coverage;
- prefetch accuracy;
- optionally LATC compression in a separate figure.

## 10.4 Manifest

Write:

```text
out/latpc/evaluation/figures/manifest.md
```

Record:

- git commit;
- source CSV;
- translation profile;
- resolved input tier per workload;
- OOM fallback reason;
- excluded workloads;
- normalization formula;
- generated figure paths.

---

# Phase 11 — Diagnose weak results and iterate

Do not stop merely because speedup is small or negative.

The first ablation is a diagnostic checkpoint.

Investigate in this order:

1. correctness and counter accounting;
2. workload opportunity;
3. agreement with `docs/PTW_LATPC_EXT.md`;
4. expected LATC pressure reduction;
5. expected LATP pressure reduction;
6. bottleneck migration;
7. paper/profile differences;
8. controlled one-factor sensitivity.

Do not begin with arbitrary parameter sweeping.

## 11.1 Required iteration log

Maintain:

```text
out/latpc/diagnosis/iterations.md
```

For each iteration record:

| Field | Content |
|---|---|
| Iteration | ID |
| Observation | Weak or unexpected behavior |
| Hypothesis | Specific cause |
| Evidence | Counters, trace, or test |
| Change | Exact code/config/input change |
| Expected metric | What should move |
| Result | New outcome |
| Decision | Keep, revert, or continue |
| Commit | SHA or none |

Preserve:

```text
results_iter00.csv
results_iter01.csv
...
```

## 11.2 Commit during diagnosis

Commit every validated major fix separately.

Examples:

```text
fix(latc): correct grouped member completion
fix(latp): merge members arriving during leaf phase
timing: remove unintended group completion serialization
metrics: correct prefetch usefulness accounting
```

Revert disproven experimental code before continuing.

## 11.3 Allowed stopping condition

Stop only when:

- functional correctness passes;
- counter invariants pass;
- selected workloads have measured translation opportunity, or its absence is demonstrated;
- LATC changes its intended MSHR-pressure metrics when opportunity exists;
- LATP changes its intended PTW/PWQ metrics when opportunity exists;
- differences from `docs/PTW_LATPC_EXT.md` are resolved;
- no uninvestigated simulator bottleneck masks the mechanism;
- controlled sensitivity does not reveal a correctable problem;
- remaining weak performance is explained by measured behavior.

Only then may the report conclude that LATPC has limited benefit under the validated configuration.

After every accepted performance-affecting change:

1. rerun targeted tests;
2. rerun relevant workload gates;
3. rerun the full ablation matrix;
4. regenerate `results.csv`;
5. regenerate all figures;
6. update the report;
7. commit.

---

# Phase 12 — Final report

Write:

```text
out/latpc/evaluation/report.md
```

Include:

1. exact commit;
2. statement that `docs/PTW_LATPC_EXT.md` is the design source;
3. implementation summary;
4. paper and large-resource profiles;
5. resolved workload inputs;
6. functional correctness matrix;
7. performance figures;
8. LATC contribution;
9. LATP contribution;
10. LATPC contribution;
11. ideal-translation gap;
12. mechanism statistics;
13. diagnosis iterations;
14. excluded/failed workloads;
15. limitations;
16. commit list.

Use this description:

```text
MI300X-class MGPUSim model with a detailed memory-backed PTW
and LATPC-inspired translation extensions
```

Do not claim exact MI300X translation-resource fidelity.

---

# Live checklist

State notation:

- `[ ]` not started;
- `[-]` in progress;
- `[x]` complete and validated;
- `[!]` blocked or failed.

- [ ] Read all of `docs/PTW_LATPC_EXT.md`.
- [ ] Audit and validate the existing detailed PTW.
- [ ] Add translation modes and resource profiles.
- [ ] Resolve Large/Small workload inputs.
- [ ] Commit mode/profile infrastructure.
- [ ] Preserve translation-group metadata.
- [ ] Commit translation-group metadata.
- [ ] Implement Detector from `docs/PTW_LATPC_EXT.md`.
- [ ] Commit Detector.
- [ ] Implement LATC from `docs/PTW_LATPC_EXT.md`.
- [ ] Commit LATC.
- [ ] Implement LATP from `docs/PTW_LATPC_EXT.md`.
- [ ] Commit LATP.
- [ ] Integrate full LATPC.
- [ ] Commit full integration.
- [ ] Complete and validate statistics.
- [ ] Commit statistics.
- [ ] Run functional matrix.
- [ ] Run paper-profile timing matrix.
- [ ] Run large-resource timing matrix.
- [ ] Generate all comparison figures.
- [ ] Diagnose and iterate if results are weak.
- [ ] Commit every accepted major fix.
- [ ] Confirm allowed stopping condition.
- [ ] Produce final report.
- [ ] Run `go test ./...`.
