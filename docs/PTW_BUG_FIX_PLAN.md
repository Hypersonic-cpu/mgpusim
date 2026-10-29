# PTW Bug Fix Plan

Scope: `research/latpc-v5-beta1`, including the MGPUSim PTW/LATPC path and the Akita v5.0.0-beta.8 VM components it depends on.

## Correctness rules

- Invalid or unsupported virtual addresses must raise an explicit translation fault. Never truncate or alias them into a valid address.
- Broken simulator-internal invariants must fail immediately with a panic/assertion containing request ID, PID, VA, and relevant component state.
- A control command must not acknowledge quiescence while accepted translation work can still produce a response.

## P0: Fix before evaluation

### 1. GPU physical ranges overlap the reserved page-table region

**Problem.** `RegisterGPU` subtracts `ReservedPageTableBytes` from every GPU's device span, while GPU builders still route memory with the full `gpuMemSize` stride. GPU2+ allocator bases can therefore shift downward and overlap GPU1's reserved page-table region.

**Fix.** Give every GPU an explicit physical base and full hardware span. Reserve the page-table region only from GPU1's allocatable subrange; do not shrink the physical span used to place the next GPU. Register allocator ranges from the builder's `memAddrOffset` rather than cumulative reduced sizes.

**Checks.** Assert non-overlapping GPU spans, allocator ranges contained in the owning GPU span, and zero overlap between normal allocations and the page-table region. Add 2-GPU and 4-GPU boundary-allocation tests.

### 2. Shootdown can allow an old PTW to refill a new TLB state

**Problem.** The current shootdown can pause/invalidate TLB/PWC state while accepted translation work still exists. An old PTW response may arrive after remapping and refill stale translation state.

**Fix.** Stop new translation producers first, then drain accepted work before invalidation. Drain in dependency order: AT/ROB producers -> L1 TLBs -> L2 TLB -> LATP -> GMMU. Invalidate/remap only after all drain acknowledgements. Keep the hierarchy paused until restart.

**Checks.** Add a remap test with a deliberately delayed PTE response. After restart, the first translation must resolve only to the new PA. Assert no live MSHRs, walkers, pending responses, or tracked requests when shootdown completes.

### 3. Akita TLB Drain ignores lookup-pipeline state

**Problem.** Akita v5.0.0-beta.8 TLB drain completion checks MSHRs/responding state/bottom input but not the TLB pipeline and post-pipeline buffer. It can enter `pause` with accepted requests still frozen internally.

**Fix.** Require both `Pipeline` and `BufferItems` to be empty in addition to the existing drain conditions. Add one internal-idle helper and use it for Drain completion/assertions. Update the Akita dependency to the fixed revision/version.

**Checks.** Unit-test Drain with requests in each pipeline stage and with a final response waiting for the top port. Drain must acknowledge only after all accepted requests leave the TLB.

### 4. GMMU early faults can orphan a LATP group or stop redispatch

**Problem.** `takeNextWalkGroup` removes requests from the PWQ before `startWalker` validates the VA and PID address space. Early failure can leave group members without terminal faults, stale `submittedAt` entries, and remaining PWQ work without a redispatch trigger.

**Fix.** Validate the full selected group before committing it to a walker. On failure, emit one terminal `TranslationFaultError` per request, clear every corresponding timestamp, keep the walker idle, and continue dispatching. Assert that every request removed from the PWQ immediately becomes walker-owned or terminally faulted.

**Checks.** Test missing PID, invalid VA, and a grouped request containing one invalid member. All PWQ/walker/request/timestamp state must drain to zero.

### 5. Four-level translation silently ignores unsupported VA high bits

**Problem.** `ExtractIndices` consumes only the configured page-table bits. A malformed high-bit VA can alias a valid low VA.

**Fix.** Explicitly support only the lower canonical range `[0, 2^47)`. Validate before index extraction, mapping, and walking. Any address outside this range must return `ErrUnsupportedVirtualAddress` and terminate as a translation fault. Do not mask or canonicalize it.

**Checks.** Test `2^47-1`, `2^47`, and addresses with arbitrary bits 48-63 set. Unsupported addresses must never emit a PTE memory read.

## P1: Correctness hardening

### 6. GMMU Drain can pause with a pending fault

**Problem.** `completeDrain` does not include `pendingFaults` and does not explicitly require `topRequests` to be empty. A backpressured fault can be stranded after Drain acknowledges.

**Fix.** Require core idle plus empty pending-memory, memory-request map, pending-response, pending-fault, and top-request state before Drain completes. Strengthen `Core.IsDrained` to include its outgoing/completed/fault queues. Assert all are empty when entering Paused.

**Checks.** Backpressure fault/response ports during Drain and verify the control response is withheld until delivery completes.

### 7. LATP/GMMU grouping trusts metadata too much

**Problem.** Corrupt grouping metadata can cause requests with different upper-level page-table paths to share one walker.

**Fix.** Before starting or merging a grouped walk, assert same PID, same non-zero batch ID, consistent batch count, and identical indices for levels `0..NumLevels-2`. Reject duplicate group positions. Panic with member IDs/VAs on violation.

**Checks.** Inject inconsistent metadata and require an immediate assertion before any grouped leaf read is issued.

### 8. Translation access type is lost before the GMMU

**Problem.** Integrated GMMU requests leave `WalkRequest.Access` at the Go zero value (`AccessRead`), so write and execute permission checks are ineffective.

**Fix.** Carry access type explicitly through `TranslationReq`. Data ATs derive read/write from the original memory operation; instruction translation marks execute. Preserve the field through every TLB child request. Do not rely on a zero-value default.

**Checks.** Add read-only and NX PTE tests for data writes and instruction fetches. Forbidden access must fault before issuing the translated memory request.

### 9. Driver VA metadata is keyed only by virtual address

**Problem.** `vAddrToPageMapping` uses only `VAddr` although each PID starts at the same low VA range. Multiple contexts can overwrite each other's allocator metadata.

**Fix.** Key allocator metadata by `{PID, page-aligned VAddr}`. Pass PID through `Free`/`RemovePage`/remap helpers. Panic if the exact PID+VA mapping does not exist.

**Checks.** Allocate the same VA in two contexts, free/remap one, and verify the other mapping is unchanged.

### 10. Per-level PWC configuration is collapsed to one scalar

**Problem.** Profiles expose `PWCEntries []int`, while the builder passes only `PWCEntries[0]` and the GMMU uses one capacity for every level.

**Fix.** Store one PWC capacity per intermediate level. Require exactly `NumLevels-1` positive entries and instantiate each PWC with its own configured size.

**Checks.** Build a profile with distinct capacities and verify per-level occupancy/eviction behavior.

## PTW correctness oracle

Add a debug-only assertion mode, for example `MGPUSIM_PTW_ASSERT=1`.

For every completed detailed walk, compare the walker's final PA with `RadixPageTable.Walk(PID, VA)`. Check every member of a grouped walk. On mismatch, panic immediately with request ID, PID, VA, walker level state, expected PA, and observed PA.

This is a correctness oracle only and must be disabled for final performance measurements.

## Regression order

1. VM format/radix and allocator unit tests.
2. GMMU walker, drain, grouping, and remap/shootdown tests.
3. Akita TLB drain regression and dependency update.
4. Run the standalone FDTD2D smoke script in order: size 16 -> 32 -> 64.
5. Only after the smoke test passes, rerun the LATPC evaluation workloads.

FDTD2D size 32/64 also exercises the existing multi-workgroup cache-coherence limitation. A verification failure there is not by itself evidence of a PTW fault; a PTW assertion/fault must be diagnosed separately.

## Out of scope

DBTracer milestone cost and linear MSHR lookup are host-performance issues. Optimize them separately after PTW correctness is established.
