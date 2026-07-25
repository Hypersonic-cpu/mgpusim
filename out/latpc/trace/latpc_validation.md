# Full LATPC trace validation

Date: 2026-07-25
Source commit: `1aa8dd4d6758` plus the uncommitted Phase 6 integration audit

## Configuration

- Workload: MVT `-size 256`
- Platform: MI300X-class timing model, CDNA3, one GPU
- Translation mode/profile: `latpc` / `paper`
- Page size: 4096 bytes
- Verification: enabled, passed
- Debug categories:
  `LATPCDetector,LATC,LATP,GMMUWalk,PWC,TLBFill,TLBReplay`
- Debug limit: 20 MiB
- Actual trace size: 483,212 bytes
- Host memory control: `GOMEMLIMIT=10GiB`; no `ulimit` was used

## Causal-chain checks

| Check | Evidence | Result |
|---|---|---|
| Detector reaches LATC | 2,056 detector decisions and 2,056 LATC allocations | Pass |
| LATC releases exactly once | 2,056 allocations, 2,056 releases, final occupancy zero | Pass |
| LATP receives compressed regular groups | four 16-member batches, each saving 15 independent walks | Pass |
| PTW remains detailed | GMMU walk and PWC events appear; leaf reads use the normal shared-L2 path | Pass |
| TLB fill/replay is balanced | 72 TLB fills and 72 replay releases | Pass |
| No request is lost or replayed twice | application verification passed; fill/replay counts balance | Pass |
| Physical replay is correct | every replay event records a physical page base and MVT verification passes | Pass |
| Trace is bounded | 483,212 B is below 20 MiB | Pass |

An irregular BFS timing run (`-node 64 -degree 3 -depth 8`) also passed in
full `latpc/paper` mode.

## Post-integration smoke recheck

Source commit: `54ec822b65d2`

After starting the official Large-input evaluation, the current source was
rebuilt independently and rechecked in timing mode without stopping or
signalling that evaluation:

| Workload | Input | Modes | Verification | Page | Mechanism evidence |
|---|---|---|---|---:|---|
| ATAX | `-x=16 -y=16` | baseline, latc, latp, latpc, ideal | all pass | 4096 B | detailed modes start 8 walks; ideal starts 0 |
| MVT | `-size=256` | baseline, latc, latp, latpc, ideal | all pass | 4096 B | LATC compression 8.471x; LATP saves 22 walks; LATPC saves 60 |

For MVT, baseline starts 72 walks, LATP starts 50, full LATPC starts 12,
and ideal starts zero. The full LATPC run forms four groups containing 64
members. Raw smoke databases are intentionally temporary under
`/private/tmp/latpc-atax-smoke/`; the reproducible official matrix remains
under `out/latpc/evaluation/`.
