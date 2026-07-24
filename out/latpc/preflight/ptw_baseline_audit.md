# Detailed PTW baseline audit

Date: 2026-07-25
Baseline source commit: `76ffbfe72506`

`docs/PTW_LATPC_EXT.md` was read in full and used as the architecture source.

## Audit result

| Requirement | Evidence | Result |
|---|---|---|
| Page tables live in simulated physical memory | `amd/vm/radix.go` stores radix entries in shared `mem.Storage`; the driver receives the radix table | Pass |
| Each PID has a physical root | `RadixPageTable.addressSpaces` and `AddressSpace.RootPAddr` | Pass |
| Walker is level-generic | `amd/timing/gmmu/walker.go` sizes fixed arrays to `MaxPageTableLevels` and iterates `Format.NumLevels` | Pass |
| PTW reads use physical addresses | `MemoryRead.PAddr` is calculated from the current table base and level index | Pass |
| PTW traffic uses shared L2 | the GMMU Memory port is plugged into the physical L1-to-L2 connection and uses the L2-bank mapper | Pass |
| PWC behavior follows the format | the core builds `NumLevels-1` PWCs, keys by PID and consumed VA prefix, and never creates a leaf PWC | Pass |
| L2 miss handling and MSHR coalescing | Akita TLB holds one downstream request per `(PID, VPN)` MSHR and wakes every waiter | Pass |
| Completed translations fill both TLB levels | the GMMU responds to L2; L2 fills and responds to L1; L1 fills and responds to the translator | Pass |
| Replay uses physical addresses | Akita address translators add the original page offset to `Page.PAddr` before forwarding to physical L1 | Pass |
| Vector, scalar, and instruction paths translate | shader-array wiring places an address translator and L1 TLB before L1V, L1S, and L1I | Pass |
| Reset, drain, invalidation, and faults | GMMU component/core tests cover lifecycle, PWC PID invalidation, structured faults, response draining, and stale-response rejection | Pass |
| Timed lookup is not a flat page-table lookup | baseline requests enter the finite PWQ and walkers and issue physical PTE reads; direct lookup is reserved for explicit `ideal` mode | Pass |

## Validation

- `GOMEMLIMIT=10GiB go test ./...`: pass.
- MI300X-class timing smoke test: ATAX `-x 16 -y 16`, `baseline/paper`,
  CDNA3, verification enabled: pass.
- Page size: 4096 bytes (`log2=12`) at driver, address translators, both TLB
  levels, and the four-level radix format.
- Debug logging: disabled.
- Host limit: `GOMEMLIMIT=10GiB`; no `ulimit` was used.

The Phase 0 gate passes.
