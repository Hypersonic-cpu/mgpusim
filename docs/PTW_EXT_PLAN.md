# Codex Implementation Plan: Detailed GMMU, PTW, PWC, and TLB Replay

> **Repository root:** current working directory  
> **Design document:** `docs/PTW_LATPC_EXT.md`  
> **Target scope:** single GPU only  
> **Baseline repository:** the currently checked-out MGPUSim tree  
> **Workloads:** ATAX, BICG, FDTD2D, MVT, NW, LUD, BFS, PageRank, and SpMV  
> **Excluded:** LATPC-specific mechanisms, multi-GPU execution, RDMA, migration, and recoverable page faults

---

## 1. Execution contract

Follow the phases in order. Do not start a later phase before its gate passes.

### Mandatory stop conditions

Stop immediately and report without modifying implementation code when any of the following occurs:

1. The current implementation materially disagrees with the “current model” described in `docs/PTW_LATPC_EXT.md`.
2. The repository has uncommitted changes that overlap files that must be modified.
3. The 12 GiB host-memory limit cannot be enforced on the current operating system.
4. More than four of the nine baseline functional workloads fail.
5. The debug trace exceeds 20 MiB during the required end-to-end trace run.
6. A correctness failure cannot be isolated without changing the agreed architecture.
7. The detailed GMMU can only be connected by bypassing the shared L2 cache.
8. A commit would include unrelated user changes.

For one to four failed baseline workloads, continue only with the passing workloads and report every failure.

### General rules

- Read `docs/PTW_LATPC_EXT.md` before editing code.
- Preserve the current functional mode.
- Use one GPU only. Do not pass multiple GPU IDs.
- Use 4 KiB base pages for the new detailed translation model.
- All caches at and below L1 must consume physical addresses.
- PTW requests must bypass private L1 caches and enter the shared L2.
- Keep the flat page table only as a debug/verification oracle.
- Do not let the detailed timing path call `PageTable.Find` to complete a translation.
- Do not implement the LATPC section: no Regularity Detector, LATC, LATP, grouped leaf-PTE prefetch, or compressed TLB MSHRs.
- Do not use `git add -A`.
- Do not amend, squash, rebase, reset, or force-push.
- Commit only after the relevant tests and workload checks pass.

---

## 2. Required output directories

Use an ignored working directory for generated binaries, logs, metrics, and reports:

```text
out/ptw-latpc/
├── preflight/
├── functional-baseline/
├── mapping-trace/
├── gmmu-tests/
├── replay-trace/
└── timing/
```

Do not commit generated binaries, SQLite databases, traces, or full logs.

Create a concise machine-readable status file for each run:

```json
{
  "workload": "atax",
  "command": "...",
  "git_commit": "...",
  "mode": "functional",
  "exit_code": 0,
  "verify": "pass",
  "wall_seconds": 0.0,
  "max_rss_bytes": 0,
  "log_bytes": 0
}
```

---

# Phase 0 — Repository and model-consistency gate

## 3. Protect the working tree

Run:

```bash
git status --short
git rev-parse --show-toplevel
git rev-parse HEAD
git branch --show-current
go version
go env GOMOD GOWORK
```

Rules:

- Record the output in `out/ptw-latpc/preflight/environment.txt`.
- Do not include unrelated dirty files in later commits.
- If dirty files overlap the MMU, GMMU, TLB, memory allocator, cache wiring, storage, runner, or test files, stop and report.
- An uncommitted `docs/PTW_LATPC_EXT.md` may remain untouched, but never include it accidentally in a commit.

## 4. Audit the current code against the document

Produce:

```text
out/ptw-latpc/preflight/current_model_audit.md
```

For each item, provide:

- result: `MATCH`, `MINOR DIFFERENCE`, or `MISMATCH`;
- exact file path;
- symbol/function name;
- line range;
- short explanation.

Audit all of the following.

### 4.1 Page-table representation

Verify whether the current page table:

- is keyed by PID and aligned virtual address;
- stores a flat `Page` record;
- has no simulated root physical address;
- has no memory-backed PDE/PTE hierarchy;
- uses sparse host-side data structures.

### 4.2 Mapping establishment

Verify:

- which Driver/allocator function assigns VA ranges;
- which function assigns physical pages;
- where mappings are inserted;
- whether mappings are established before kernel launch;
- whether unified and device allocations follow different paths.

### 4.3 Current MMU and GMMU

Verify both packages if both exist:

- accepted request and response message types;
- fixed walk latency or countdown;
- maximum in-flight requests;
- direct call to the flat page table;
- behavior on a missing page;
- ports and downstream connections;
- whether the MI300X builder uses `mmu` or `gmmu`.

### 4.4 Address Translator

Verify:

- where original virtual requests are retained;
- how the page offset is combined with the returned physical page;
- where translated requests are sent;
- whether replay already exists;
- whether vector, scalar, and instruction paths behave consistently.

### 4.5 TLB hierarchy

Verify:

- private L1 TLB placement;
- shared L2 TLB placement;
- L1 and L2 MSHR behavior;
- same-translation request merging;
- fill behavior;
- response fan-out and replay;
- current sizes, associativities, latencies, and page size.

### 4.6 Cache address domains

For L1V, L1S, L1I, L2, MALL, and DRAM, determine whether tags and routing use:

- virtual address;
- physical address;
- or a mixture.

This check is especially important for L1I. Do not assume the instruction path is physically addressed merely because the vector path is.

### 4.7 Physical storage

Verify:

- storage capacity handling;
- sparse backing unit size;
- whether untouched address ranges consume host memory;
- whether `Read` allocates a previously absent storage unit;
- whether a storage unit can currently be discarded.

### Gate 0

If any material statement in `docs/PTW_LATPC_EXT.md` about the current code is wrong, stop here.

Report:

```text
Expected by document
Actual code behavior
Affected paths and symbols
Why implementation must not proceed
Suggested document correction
```

Do not edit implementation code.

---

# Phase 1 — Baseline functional workload boot

## 5. Add no implementation changes before this phase

Run the nine workloads using the original checked-out code.

Workload directories:

| Workload | Expected directory |
|---|---|
| ATAX | `amd/samples/atax` |
| BICG | `amd/samples/bicg` |
| FDTD2D | `amd/samples/polybench_fdtd2d` |
| MVT | `amd/samples/polybench_mvt` |
| NW | `amd/samples/nw` |
| LUD | `amd/samples/rodinia_lud` |
| BFS | `amd/samples/bfs` |
| PageRank | `amd/samples/pagerank` |
| SpMV | `amd/samples/spmv` |

If a path differs, locate the actual benchmark by package name and record the difference. A missing benchmark counts as a failure.

## 6. Discover, do not guess, benchmark flags

For each workload:

1. Inspect its `main.go`, runner setup, and README.
2. Build it with `go build`.
3. Run its help output.
4. Select the smallest nontrivial input that:
   - launches at least one GPU kernel;
   - exercises global memory;
   - supports verification where available;
   - completes reasonably on the development machine.

Functional mode means:

- do **not** pass `-timing`;
- use CDNA3/GFX942 binaries where required;
- select one MI300X GPU if the runner requires a GPU model;
- enable `-verify` where supported;
- do not silently remove verification after a failure.

A likely common prefix is:

```bash
-arch cdna3 -gpu mi300x -verify -disable-rtm
```

Use it only after confirming that the benchmark accepts those flags.

Record the exact final command for every workload in:

```text
out/ptw-latpc/functional-baseline/commands.tsv
```

## 7. Enforce the 12 GiB host-memory limit

Create an uncommitted run helper under `out/ptw-latpc/tools/`, or a committed reusable helper later in Phase 2.

Required behavior:

- hard address-space limit: 12 GiB;
- launch the benchmark in its own process group;
- capture stdout and stderr;
- record wall time and maximum RSS when available;
- preserve the benchmark exit code;
- terminate the process group on timeout;
- clearly distinguish timeout, memory-limit failure, verification failure, and ordinary nonzero exit.

Preferred implementation:

1. Use Python `resource.setrlimit(resource.RLIMIT_AS, ...)` on Unix.
2. Use `prlimit --as` where available as an alternative.
3. Set `GOMEMLIMIT=10GiB` as a cooperative Go-runtime limit, but never treat it as a substitute for the hard 12 GiB limit.
4. If a hard limit cannot be applied on the host, stop and report.

Use a per-workload timeout appropriate for functional mode, initially 10 minutes.

## 8. Baseline result gate

Produce:

```text
out/ptw-latpc/functional-baseline/summary.md
```

Table columns:

| Workload | Build | Launch | Verify | Exit code | Wall time | Max RSS | Failure reason |
|---|---:|---:|---:|---:|---:|---:|---|

Rules:

- Zero failures: continue.
- One to four failures: report them and continue with the passing set.
- More than four failures: stop before implementation.

Do not commit baseline output artifacts.

---

# Phase 2 — Flag-controlled debug logging

## 9. Follow project conventions first

Search for existing logging, tracing, and debug-flag facilities.

If a project facility already provides:

- named categories;
- cheap disabled checks;
- controlled output destination;
- deterministic formatting;

extend it rather than creating a competing logger.

Otherwise implement a small package named something that does not conflict with Go’s `runtime/debug`, for example:

```text
simdebug
```

## 10. Required debug API

Provide programmatic configuration and environment-based control.

Suggested interface:

```go
package simdebug

type Flag string

const (
    VMMap      Flag = "VMMap"
    PageTable  Flag = "PageTable"
    GMMUWalk   Flag = "GMMUWalk"
    PWC        Flag = "PWC"
    PTWMem     Flag = "PTWMem"
    TLBReplay  Flag = "TLBReplay"
    TLBFill    Flag = "TLBFill"
    Fault      Flag = "Fault"
)

func Enabled(flag Flag) bool
func DPrintf(flag Flag, format string, args ...any)
func Configure(cfg Config) error
func Close() error
```

Environment configuration:

```text
MGPUSIM_DEBUG=VMMap,GMMUWalk,PWC
MGPUSIM_DEBUG_FILE=out/ptw-latpc/replay-trace/trace.log
MGPUSIM_DEBUG_MAX_BYTES=20971520
```

Requirements:

- disabled by default;
- unknown flag names are rejected;
- thread-safe writes;
- each line contains category and, when available, simulation tick/component;
- output is deterministic enough for tests;
- disabled flags do not format large payloads;
- heavy call sites use `Enabled` before constructing expensive strings;
- logger closes and flushes cleanly;
- byte count is tracked;
- exceeding the internal byte limit returns a controlled error or triggers the run helper to terminate the process.

Add unit tests for:

- enable/disable behavior;
- multiple categories;
- unknown category handling;
- output redirection;
- concurrent writes;
- maximum-byte behavior.

### Commit checkpoint A

After logging tests pass:

```text
infra: add flag-controlled simulation debug logging
```

Commit only the logger, its tests, and any reusable limit-run helper.

---

# Phase 3 — Configurable page format and physical allocation policies

## 11. Configurable page-table format

Do not scatter hard-coded shifts such as 12, 21, 30, or 39 through the code.

Use a maximum of five levels and an array-based representation.

Preferred Go shape:

```go
const MaxPageTableLevels = 5

type PageTableFormat struct {
    NumLevels     uint8
    PageOffsetBits uint8
    EntryBytes    uint8
    IndexBits     [MaxPageTableLevels]uint8
}
```

Provide constructors returning values, not mutable global slices:

```go
func X86FourLevel4KFormat() PageTableFormat
func X86FiveLevel4KFormat() PageTableFormat
```

Four-level baseline:

```text
NumLevels      = 4
PageOffsetBits = 12
EntryBytes     = 8
IndexBits      = [9, 9, 9, 9, 0]
```

All index extraction, masks, shifts, table sizes, and walker loops must derive from the format.

Add tests for:

- four-level indices;
- five-level indices;
- page offset;
- invalid formats;
- table-page size;
- entry-address calculation;
- canonical-address validation if implemented.

## 12. Physical page allocation policies

Introduce a policy boundary independent of sparse storage backing:

```go
type PhysicalPageAllocationPolicy interface {
    AllocatePage(deviceID int) (uint64, error)
    FreePage(deviceID int, pAddr uint64) error
}
```

Implement:

### 12.1 Linear policy

- deterministic;
- near-contiguous PFNs;
- 4 KiB aligned;
- in the selected GPU’s physical range.

### 12.2 Pseudo-randomized policy

Requirements:

- deterministic for a fixed seed;
- one-to-one within a device frame range;
- no full-frame-array shuffle;
- O(1), or small bounded, policy state per allocation;
- no duplicate PFN before exhaustion;
- no cross-device allocation;
- repeatable expected addresses in tests.

Use a documented keyed permutation over:

```text
0 <= logical frame < NumFrames
```

A suitable implementation is a small-domain bijection with cycle walking over:

```text
2^ceil(log2(NumFrames))
```

Do not use repeated `rand.Intn` collision retries and do not allocate an array covering all frames.

Keep data-page and page-table-page allocators separate.

## 13. Intermediate compatibility

At this phase, retain the legacy flat lookup for timing translation so that the tree remains runnable.

Change only what is needed to:

- use 4 KiB pages in the new translation configuration;
- establish deterministic linear or pseudo-randomized VA-to-PA mappings;
- store the chosen PAddr in the existing shadow mapping;
- preserve functional-mode correctness.

## 14. Mapping debug trace

At every mapping establishment, add a `VMMap` debug line with at least:

```text
PID
device ID
allocation policy
VA page base
PA page base
page size
allocation sequence number
seed or policy identifier
```

Example shape:

```text
[VMMap] pid=1 dev=1 policy=linear seq=3 va=0x... pa=0x... size=4096
```

Do not print mappings unless `VMMap` is enabled.

## 15. Mapping tests

Follow existing project test style where available.

Required tests:

- linear first-N expected pages;
- randomized first-N expected pages for a fixed seed;
- randomized determinism;
- randomized uniqueness;
- randomized in-range property;
- different seeds produce different sequences;
- free and safe reuse;
- per-device range isolation;
- mapping insertion uses the selected PAddr;
- shadow lookup returns the expected mapping;
- sparse storage does not require dense backing up to the largest PAddr.

## 16. Real-workload mapping validation

Choose one passing regular workload, preferably ATAX or BICG.

Run it in functional mode twice under the 12 GiB limit:

1. linear mapping with `VMMap`;
2. pseudo-randomized mapping with `VMMap` and a fixed seed.

Keep each log below 20 MiB.

Validate:

- all addresses are 4 KiB aligned;
- linear PFNs follow the expected sequence;
- pseudo-randomized PFNs match the unit-test golden sequence;
- no duplicate physical page is assigned;
- all pages remain inside the selected GPU’s physical range;
- verification passes.

Write:

```text
out/ptw-latpc/mapping-trace/validation.md
```

### Commit checkpoint B

After tests and both workload runs pass:

```text
vm: add configurable page formats and physical page policies
```

Include the mapping debug hooks in this commit if they are tightly coupled to allocation.

---

# Phase 4 — Memory-backed radix page table

## 17. Build the physical hierarchy

Implement a generic radix page table driven by `PageTableFormat`.

Required structures:

```go
type AddressSpace struct {
    PID       vm.PID
    RootPAddr uint64
}

type RadixPageTable struct {
    Format         PageTableFormat
    RootTable      ...
    TableAllocator ...
    Storage        *mem.Storage
    Shadow         vm.PageTable
}
```

Requirements:

- one root physical address per PID;
- page-table pages allocated from a separate physical region;
- each table page aligned to its configured table-page size;
- new table pages zero-filled in simulated storage;
- intermediate entries point to child table physical addresses;
- leaf entries point to physical data pages;
- page-table construction occurs before kernel access;
- initialization writes are functional and untimed;
- the shadow flat map is updated only for validation and existing compatibility.

## 18. PTE codec

Implement one x86-style 64-bit entry codec.

Required initial fields:

- Present;
- Read/Write;
- User;
- Page Size;
- physical base;
- No Execute.

Add encode/decode tests, including malformed and out-of-range physical addresses.

Do not encode GPU ownership into x86 PTE bits. Determine device ownership from the physical-address map or separate metadata.

## 19. Generic mapping walk

Use loops and level-indexed arrays.

Do not add fields named only for a fixed level, such as:

```text
PML4Addr
PDPTAddr
PDAddr
PTAddr
```

Use:

```go
[MaxPageTableLevels]uint64
```

with `NumLevels` controlling valid entries.

Required tests:

- first mapping creates every required level;
- adjacent VPN reuses upper levels;
- mapping across a PT boundary creates the expected new leaf table;
- mapping across higher-level boundaries creates the expected hierarchy;
- two PIDs can map the same VA differently;
- linear and randomized leaf PPNs are encoded correctly;
- four-level and five-level formats both traverse correctly;
- the physical entries in `mem.Storage` decode to the expected tree;
- shadow mapping and radix mapping agree.

### Commit checkpoint C

After all page-table tests pass:

```text
vm: add configurable memory-backed radix page tables
```

Do not integrate the timing GMMU in this commit unless the page-table implementation cannot be tested independently.

---

# Phase 5 — Detailed GMMU, PTW, PWQ, and PWC

## 20. Extend the existing GMMU when practical

Inspect the existing `gmmu` package and project component conventions.

Prefer extending and replacing its fixed-latency path over introducing a second competing GMMU package, provided this can be done cleanly.

The detailed path must not use the flat page table to obtain the translation result.

## 21. GMMU ports

Required ports:

| Port | Messages | Connection |
|---|---|---|
| `Top` | Translation request/response | shared L2 TLB |
| `Memory` | physical read request/data response | shared L2 cache interconnect |
| `Control` | enable/pause/drain/reset | existing control infrastructure |
| `Fault` | structured page fault | fatal handler initially |

The `Memory` port must use the same physical-address mapper and shared-L2 path used by other physical clients.

A direct GMMU-to-storage or GMMU-to-DRAM shortcut is not acceptable.

## 22. Page-walk queue

Baseline constants:

```text
PWQ entries = 128
walkers     = 16
policy      = FCFS
```

Required behavior:

- L2 TLB miss submits one request after L2 MSHR allocation;
- queue-full state creates backpressure;
- queue time is tracked;
- a request leaves PWQ only when a walker accepts it;
- drain waits for queued walks.

## 23. Array-based walkers

Use fixed-capacity per-level arrays:

```go
type WalkerState struct {
    Busy             bool
    Req              WalkRequest
    CurrentLevel     uint8
    Indices          [MaxPageTableLevels]uint64
    TablePAddrs      [MaxPageTableLevels]uint64
    EntryPAddrs      [MaxPageTableLevels]uint64
    DecodedEntries   [MaxPageTableLevels]uint64
    LevelCompleted   [MaxPageTableLevels]bool
    WaitingMemory    bool
    OutstandingReqID uint64
}
```

Use an array or slice of walkers:

```go
Walkers []WalkerState
```

All loops must use `Format.NumLevels`.

The simple baseline allows one outstanding PDE/PTE memory request per walker.

## 24. Per-level PWCs

For an N-level page table, instantiate N-1 intermediate PWCs.

Four-level baseline:

```text
PWC[0] = PML4 result cache, 16 entries
PWC[1] = PDPT result cache, 16 entries
PWC[2] = PD result cache, 16 entries
```

Required behavior:

- level-specific key derived from PID and consumed VA prefix;
- cached value contains child-table PAddr and inherited permissions;
- hit skips that level’s memory read;
- miss generates the level’s physical memory read;
- leaf PTE is not stored in an intermediate PWC;
- replacement is deterministic, initially LRU;
- reset and invalidation are implemented.

Clarify in code comments and tests:

> An L2 TLB miss triggers a PTW. PWC misses determine how many page-table memory reads the PTW performs; they do not determine whether the PTW exists.

## 25. PTW memory requests

For each required level:

1. Compute the physical entry address from the current table base, index, and entry size.
2. Emit an 8-byte physical read.
3. Route it through shared L2.
4. Wait for the data response.
5. Decode the entry.
6. On an intermediate entry, update the next-level table base and fill the corresponding PWC.
7. On a valid leaf, complete translation.
8. On invalid or denied access, emit a structured fault.

Use a distinct traffic class:

```text
gmmu.pte-read
```

Do not bypass cache-line behavior. The GMMU requests the entry bytes; the cache hierarchy decides line fetch and fill behavior.

## 26. Control behavior

Implement and test:

- enable;
- pause;
- drain;
- reset;
- response backpressure;
- no stale response fill after reset;
- no orphaned memory request IDs.

### Commit checkpoint D

After standalone GMMU/PWQ/PWC tests pass:

```text
gmmu: add detailed page walkers and per-level walk caches
```

At this point the component may still be tested with a memory stub, but its production `Memory` port must be designed for shared-L2 integration.

---

# Phase 6 — GMMU unit and component tests

## 27. Follow existing component-test patterns

Search nearby Akita/MGPUSim packages for:

- Ginkgo/Gomega tests;
- standard `_test.go` tests;
- mocked ports;
- cycle stepping;
- message builders;
- component-control tests.

Use the dominant local style. Do not introduce a second testing framework solely for this work.

## 28. Required PWC/PTW tests

At minimum test:

### 28.1 Cold four-level walk

Expected:

```text
PML4E read
PDPTE read
PDE read
PTE read
TranslationRsp
```

Exactly four page-table memory reads.

### 28.2 Repeated same-prefix walk

After the first walk, issue a different VPN sharing the first three prefixes.

Expected:

- all three intermediate PWCs hit;
- exactly one leaf PTE read;
- correct final PAddr.

### 28.3 L3 PWC miss

Preload PWC[0] and PWC[1], leave PWC[2] absent.

Expected:

- one PD-level entry read;
- one leaf PTE read;
- total two reads;
- PWC[2] filled.

### 28.4 Individual PWC replacement

Fill beyond 16 entries and confirm deterministic LRU replacement independently at each level.

### 28.5 PWQ and walker pressure

- fill all 16 walkers;
- enqueue additional requests;
- fill the 128-entry PWQ;
- verify backpressure;
- verify FCFS dispatch.

### 28.6 Faults

- missing root;
- non-present intermediate entry;
- non-present leaf;
- write permission denial;
- execute permission denial when enabled;
- malformed entry.

### 28.7 Four-level and five-level operation

Use the same walker implementation with both formats. No hard-coded four-level state may appear.

### 28.8 Memory-response correlation

Return responses out of order across walkers and verify that every response advances only its owning walker.

## 29. Gate 6

Do not integrate with the real cache hierarchy until:

- all targeted tests pass;
- no test uses the flat table to provide the result;
- memory-request count matches expected PWC behavior;
- race detector passes on the new packages where practical:

```bash
go test -race ./path/to/changed/packages/...
```

Write the results to:

```text
out/ptw-latpc/gmmu-tests/summary.md
```

---

# Phase 7 — Shared-L2 integration and end-to-end replay

## 30. Connect GMMU memory traffic to L2

Modify the single-GPU MI300X timing builder so that:

```text
GMMU.Memory -> physical shared-L2 interconnect -> L2 -> MALL -> HBM
```

Requirements:

- use physical PTE addresses;
- reuse the production physical-address mapper;
- preserve traffic class;
- no private-L1 probe or fill;
- no direct storage access;
- no multi-GPU routing;
- no RDMA path.

## 31. Enforce translation before every L1

Verify and correct the paths:

```text
Vector ROB -> Vector Address Translator -> L1V
Scalar ROB -> Scalar Address Translator -> L1S
Instruction ROB -> Instruction Address Translator -> L1I
```

Every request entering an L1 must contain the translated physical address.

If instruction translation cannot be made consistent without a major unrelated redesign, stop and report before weakening the physical-address invariant.

## 32. Preserve and verify TLB replay

Required end-to-end sequence:

```text
original memory request
  -> Address Translator retains request
  -> L1 TLB miss and MSHR
  -> L2 TLB miss and MSHR
  -> PWQ and walker
  -> PWC and physical page-table reads
  -> GMMU TranslationRsp
  -> L2 TLB fill and MSHR fan-out
  -> L1 TLB fill and MSHR fan-out
  -> physical address construction
  -> original request replay to physical L1
  -> normal memory response
```

Do not create a second replay mechanism if the existing TLB and Address Translator behavior can be reused.

## 33. End-to-end debug flags

Add debug points:

### `GMMUWalk`

- walk allocation;
- root PAddr;
- current level;
- index;
- entry PAddr;
- decoded entry;
- completion or fault.

### `PWC`

- level;
- key/prefix;
- hit/miss;
- fill;
- eviction.

### `PTWMem`

- memory request ID;
- walker ID;
- level;
- PAddr;
- send and receive tick;
- L2 destination.

### `TLBFill`

- PID/VPN;
- level of TLB;
- PPN;
- MSHR identifier;
- number of waiting requests.

### `TLBReplay`

- original request ID;
- VA;
- PA;
- requesting CU/translator;
- replay send;
- final completion.

Avoid printing full cache lines.

## 34. Bounded trace runner

Use the run helper with:

```text
hard host-memory limit = 12 GiB
hard debug-log limit   = 20 MiB
```

The helper must:

- monitor the trace file while the workload runs;
- terminate the entire process group if the file exceeds 20 MiB;
- mark the run failed as `LOG_LIMIT_EXCEEDED`;
- preserve the last complete lines where possible.

Use a small passing workload, preferably ATAX, BICG, or MVT.

Start with only:

```text
VMMap,GMMUWalk,PWC,TLBFill,TLBReplay
```

Enable `PTWMem` only if the first trace remains well below the cap.

## 35. Trace validation

The trace must demonstrate at least one complete cold translation:

1. mapping insertion;
2. L1 TLB miss;
3. L2 TLB miss;
4. PWQ admission;
5. walker allocation;
6. three intermediate PWC misses or hits as applicable;
7. physical PTE/PDE traffic through L2;
8. valid leaf decode;
9. L2 TLB fill;
10. L1 TLB fill;
11. replay with the expected physical byte address;
12. final request completion.

Cross-check the walked PAddr against the shadow map.

Write a concise annotated excerpt, not the entire log, to:

```text
out/ptw-latpc/replay-trace/validation.md
```

### Commit checkpoint E

After verification and bounded trace pass:

```text
timing: integrate detailed gmmu with l2 and tlb replay
```

---

# Phase 8 — Complete non-LATPC design requirements and statistics

## 36. Implement all applicable pre-LATPC requirements

Implement the requirements in `docs/PTW_LATPC_EXT.md` up to, but not including, the LATPC extension section.

Also implement the document’s statistics section even if its numerical section label differs from this plan.

Required remaining behavior includes:

- page-table format validation;
- structured fatal faults;
- permissions;
- per-PID roots;
- TLB invalidation;
- per-level PWC invalidation;
- process reset/teardown behavior;
- clean drain;
- storage-unit discard support if needed by page release;
- deterministic checkpointable allocator and GMMU state if the surrounding components support checkpointing.

Do not implement recoverable OS service, migration, remote page tables, RDMA, or multi-GPU behavior.

## 37. Required statistics

Use the project’s existing metrics/statistics framework. Do not substitute debug prints.

### 37.1 L1 and L2 TLB

- accesses;
- hits;
- misses;
- MSHR hits;
- MSHR allocations;
- MSHR-full stalls;
- fills;
- evictions;
- average miss latency.

### 37.2 GMMU

- translation requests;
- completed walks;
- faults;
- PWQ occupancy;
- maximum PWQ occupancy;
- PWQ-full stalls;
- average queue delay;
- walker busy cycles;
- walker utilization;
- average walk latency;
- walk-latency distribution or buckets;
- walks with one, two, three, four, and five memory reads;
- translation-response backpressure.

### 37.3 Per-level PWC

- probes;
- hits;
- misses;
- fills;
- evictions;
- invalidations.

Names must include the level index and remain valid for four- and five-level formats.

### 37.4 PTW memory traffic

- L2 hits and misses for PTW traffic;
- MALL hits and misses where available;
- HBM bytes generated by PTW;
- average PTW memory latency;
- application traffic delayed by PTW where the framework can measure it;
- PTW traffic delayed by application traffic where the framework can measure it.

### 37.5 Replay

- waiting requests per translation;
- requests awakened per TLB fill;
- translation-to-replay delay;
- original-request-to-L1-admission latency.

## 38. Statistics tests

Use controlled synthetic tests to establish exact expected counts.

Examples:

- cold four-level walk: four PTW reads;
- upper PWC hits: one PTW read;
- two requests merged in one L2 MSHR: one walk and two awakened requests;
- PWQ-full test: exact stall count;
- invalidation test: next access walks again;
- reset test: counters and state follow existing project semantics.

### Commit checkpoint F

After non-LATPC features and statistics pass:

```text
vm: add translation invalidation and detailed gmmu statistics
```

If this is too large, split into:

```text
vm: add translation invalidation and fault handling
metrics: add detailed translation and ptw statistics
```

---

# Phase 9 — Regression and timing report

## 39. Full regression

Run:

```bash
go test ./...
```

Also run targeted race tests where affordable.

Rebuild all nine workloads.

Run all baseline-passing workloads in functional mode again with debug disabled and the same inputs.

Report any regression. Do not hide workloads that already failed before modification; distinguish baseline failure from new regression.

## 40. Timing-mode workloads

Run at least four representative passing workloads with debug fully disabled:

| Class | Preferred workload |
|---|---|
| Regular/vector-like | ATAX or BICG |
| Regular stencil/dense | FDTD2D or MVT |
| Irregular graph | BFS or PageRank |
| Indirect sparse | SpMV |

Add LUD or NW when runtime permits.

Requirements:

- single GPU;
- MI300X timing model;
- CDNA3;
- verification enabled;
- 12 GiB hard host-memory limit;
- same small, nontrivial inputs documented in the report;
- `-report-all`;
- unique metric file per run;
- no debug environment variables;
- timeout documented.

## 41. Timing report

Create:

```text
out/ptw-latpc/timing/report.md
```

For every run include:

| Workload | Input | Verify | Simulated kernel time | Simulated cycles if available | Host wall time | Max RSS | L1 TLB misses | L2 TLB misses | Walks | Reads/walk | PWC hit rates | Avg walk latency | PTW L2 hit rate |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|

Also include:

- exact command;
- git commit;
- page-table format;
- physical allocation policy and seed;
- TLB/PWQ/walker/PWC configuration;
- failures and timeouts;
- interpretation of any unexpectedly high page-walk count or replay delay.

Do not claim MI300X hardware fidelity. Call the configuration a:

```text
MI300X-class MGPUSim timing model with LATPC-baseline translation resources
```

---

# 10. Commit and validation policy

## 42. Commit sequence

Expected sequence:

1. `infra: add flag-controlled simulation debug logging`
2. `vm: add configurable page formats and physical page policies`
3. `vm: add configurable memory-backed radix page tables`
4. `gmmu: add detailed page walkers and per-level walk caches`
5. `timing: integrate detailed gmmu with l2 and tlb replay`
6. `vm: add translation invalidation and detailed gmmu statistics`

A commit may be split further when a coherent independently tested unit exists.

Do not create a commit when:

- tests fail;
- the selected real workload does not verify;
- the trace is incomplete;
- the 20 MiB limit was exceeded;
- unrelated changes would be included.

## 43. Before every commit

Run:

```bash
git status --short
git diff --check
go test <targeted packages>
```

Before integration commits also run:

```bash
go test ./...
```

Use explicit staging:

```bash
git add path/to/file1 path/to/file2
git diff --cached --stat
git diff --cached
```

Record the resulting commit SHA in the phase report.

---

# 11. Explicit scope boundary

The following items are forbidden in this task:

- Regularity Detector;
- stride detection;
- LATC compressed MSHRs;
- wave64 valid-mask encoding for LATC;
- LATP grouped leaf-PTE prefetch;
- multiple TLB fills from one LATP group;
- multi-GPU execution;
- cluster topology;
- RDMA page-table requests;
- page migration;
- XNACK-style recoverable faults;
- CPU timing model;
- OS page-fault handler;
- application-specific scheduling policies.

The detailed baseline must be correct and measurable before any LATPC mechanism is added.

---

# 12. Final Codex report

At completion, report in this order:

1. **Preflight consistency result**
2. **Baseline functional workload table**
3. **Architecture implemented**
4. **Files and major symbols changed**
5. **Unit/component test results**
6. **Mapping validation for linear and pseudo-randomized policies**
7. **PWC/PTW validation**
8. **End-to-end TLB replay trace conclusion**
9. **Statistics implemented**
10. **Final functional regression**
11. **Timing report**
12. **Commit list with SHA and subject**
13. **Known limitations**
14. **Confirmation that LATPC extensions were not implemented**

When a mandatory stop condition is reached, produce the same report only through the completed phase and state the exact stop reason.

---

# 13. Live task checklist

Codex must keep this checklist current throughout the work.

Update rules:

- Change `[ ]` to `[x]` only after the task and its validation gate have passed.
- Use `[-]` for a task currently in progress.
- Use `[!]` for a blocked or failed task.
- After `[!]`, add a short reason and the relevant log or report path.
- Never mark a task complete merely because code was written; required tests, workload runs, trace checks, and commits must also pass.
- Update this section immediately whenever a task changes state.
- Keep the checklist in the repository plan file used during execution, not only in the final response.
- Add the corresponding commit SHA after tasks that require a commit.
- If a mandatory stop condition is reached, update the current item to `[!]`, leave later items unchecked, and stop.

## User-requested task status

- [x] **1. Audit the current code against `docs/PTW_LATPC_EXT.md`.**
  - Confirm current MMU/GMMU behavior.
  - Confirm address translation and replay behavior.
  - Confirm virtual versus physical address usage throughout the memory hierarchy.
  - Confirm mapping insertion and sparse physical storage.
  - Stop if the document materially disagrees with the code.
  - Evidence: `out/ptw-latpc/preflight/current_model_audit.md`

- [x] **2. Boot the nine workloads in original functional mode under a 12 GiB host-memory limit.**
  - ATAX
  - BICG
  - FDTD2D
  - MVT
  - NW
  - LUD
  - BFS
  - PageRank
  - SpMV
  - Stop if more than four fail.
  - Evidence: `out/ptw-latpc/functional-baseline/summary.md`

- [x] **3. Implement the configurable address-translation foundation.**
  - Configurable page-table depth and per-level index bits.
  - Four-level x86-64 baseline with support for up to five levels.
  - 4 KiB pages and 8-byte entries.
  - Linear physical-page allocation.
  - Deterministic pseudo-randomized physical-page allocation.
  - Sparse host backing remains independent of simulated physical-address spread.
  - Required unit tests pass.
  - Commit: `97baffca`

- [x] **3-b. Implement flag-controlled DPRINTF-style debug logging.**
  - Named debug categories.
  - Disabled by default.
  - Environment and programmatic control.
  - Output-file support.
  - Thread safety.
  - 20 MiB bounded-log support.
  - Logging unit tests pass.
  - Commit: `15e0e235`

- [x] **4. Add tests for page-table and VA-to-PA mapping establishment.**
  - Follow existing project test conventions.
  - Test format validation and index extraction.
  - Test linear mapping.
  - Test pseudo-randomized mapping.
  - Test deterministic seed behavior, uniqueness, range isolation, and free/reuse.
  - Test shadow mapping agreement.
  - Test sparse storage behavior.
  - Evidence: targeted `go test` output.

- [x] **4-b. Validate mapping establishment with a real workload and debug output.**
  - Run one passing workload with linear mapping and `VMMap`.
  - Run the same workload with pseudo-randomized mapping and a fixed seed.
  - Confirm printed insertion addresses match expected sequences.
  - Keep each log below 20 MiB.
  - Verify application correctness in both runs.
  - Evidence: `out/ptw-latpc/mapping-trace/validation.md`
  - Commit: `<pending>`

- [-] **5. Implement the detailed memory-backed page table, GMMU, PTW, PWQ, and PWC.**
  - Physical root address per PID.
  - Memory-backed multi-level PDE/PTE hierarchy.
  - Array-based per-level walker state.
  - 128-entry PWQ.
  - 16 walkers.
  - Three intermediate PWCs for the four-level baseline.
  - Physical PTW reads routed through shared L2, then MALL/HBM.
  - Structured fatal fault handling.
  - No timing translation through flat `PageTable.Find`.
  - Commit(s): `<pending>`

- [x] **6. Test GMMU, three-level PWC behavior, and PTW triggering.**
  - Cold four-level walk performs four memory reads.
  - Intermediate PWC hits reduce remaining reads correctly.
  - Final-level PTE access occurs after intermediate traversal.
  - L2 TLB miss triggers one PTW after MSHR merging.
  - PWQ capacity, walker pressure, LRU replacement, faults, and out-of-order responses are tested.
  - Four-level and five-level formats use the same generic walker.
  - Evidence: `out/ptw-latpc/gmmu-tests/summary.md`

- [ ] **7. Validate complete translation, TLB fill, and replay with a real workload.**
  - Connect `GMMU.Memory` to shared L2.
  - Ensure L1V, L1S, and L1I receive physical addresses.
  - Trace mapping, L1/L2 TLB misses, PWQ admission, PWC behavior, PTW memory traffic, leaf decode, TLB fills, and replay.
  - Enforce 12 GiB host-memory limit.
  - Kill the process if the debug log exceeds 20 MiB.
  - Cross-check walked PAddr against the shadow mapping.
  - Evidence: `out/ptw-latpc/replay-trace/validation.md`
  - Commit: `<pending>`

- [ ] **8. Complete all non-LATPC functions through the agreed design boundary and implement statistics.**
  - Permissions.
  - Per-PID roots.
  - TLB invalidation.
  - Per-level PWC invalidation.
  - Reset, drain, and process teardown.
  - Storage discard where needed.
  - TLB metrics.
  - PWQ and walker metrics.
  - Per-level PWC metrics.
  - PTW L2/MALL/HBM traffic metrics.
  - Replay metrics.
  - Controlled statistics tests.
  - Do not implement Regularity Detector, LATC, or LATP.
  - Commit(s): `<pending>`

- [ ] **9. Run selected applications without debug flags and produce the timing report.**
  - Run at least four representative timing workloads.
  - Use one GPU, CDNA3, MI300X timing mode, verification, and a 12 GiB limit.
  - Record simulated kernel time, cycles where available, host wall time, RSS, TLB metrics, walk metrics, PWC rates, and PTW memory behavior.
  - Evidence: `out/ptw-latpc/timing/report.md`

## Final completion status

- [ ] All mandatory stop conditions were respected.
- [ ] `go test ./...` passes, or every pre-existing/unrelated failure is documented.
- [ ] All baseline-passing functional workloads were rerun after implementation.
- [ ] Debug logging is disabled for timing measurements.
- [ ] No generated logs, binaries, SQLite files, or unrelated changes were committed.
- [ ] Commit list and SHAs are recorded in the final Codex report.
- [ ] LATPC-specific extensions were not implemented.
