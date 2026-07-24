# LATPC diagnosis iterations

| Field | Content |
|---|---|
| Iteration | 00 |
| Observation | The size-64 MVT smoke test exposed regular LATC groups but no LATP batch because only 12 L2 misses remained. |
| Hypothesis | A slightly larger regular workload would create simultaneous same-leaf L2 misses without changing the mechanism. |
| Evidence | MVT size 256 produced four 16-member LATP groups; each group avoided 15 independent walks. The bounded combined trace stayed below 20 MiB. |
| Change | No mechanism or evaluation input change. Size 256 was used only for the Phase 6 bounded trace. |
| Expected metric | Nonzero `LATP.groups`, `walks_saved`, and `upper_reads_saved`. |
| Result | 4 groups, 64 members, 60 walks saved; application verification passed. |
| Decision | Keep the mechanism and retain the trace as validation evidence. |
| Commit | `c20532e7` |

| Field | Content |
|---|---|
| Iteration | 01 |
| Observation | A requirement audit found that the initial GMMU waited for a complete batch before starting, so members could not merge after the upper or leaf phase began. |
| Hypothesis | Starting the first batch member immediately and merging later members into the active walker preserves one shared upper traversal while allowing leaf-phase arrivals. |
| Evidence | New deterministic tests cover merge while queued, during an outstanding upper read, and during outstanding leaf reads; all GMMU/LATPC/platform tests and `go test ./...` pass. |
| Change | Added request-aware GMMU admission, active batch lookup, dynamic grouped-leaf issue, and globally unique LATP batch IDs. |
| Expected metric | One walk start per batch, one final-level read per member, and `(members - 1) × upper-levels` upper reads avoided regardless of arrival phase. |
| Result | Two-member four-level tests issue five reads (three shared upper plus two leaf), start one walk, and return both translations, including reversed leaf completion. |
| Decision | Keep. This resolves the explicit Phase 5 queued/upper/leaf merge requirement. |
| Commit | `3b566882` |
