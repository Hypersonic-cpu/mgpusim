// Package gmmu implements a memory-backed GPU page-table walker.
package gmmu

import (
	"encoding/binary"
	"errors"
	"fmt"

	akitavm "github.com/sarchlab/akita/v5/mem/vm"
	"github.com/sarchlab/akita/v5/timing"
	"github.com/sarchlab/mgpusim/v5/amd/simdebug"
	"github.com/sarchlab/mgpusim/v5/amd/timing/latpc"
	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

const (
	// DefaultPWQEntries is the baseline FCFS page-walk queue capacity.
	DefaultPWQEntries = 128
	// DefaultNumWalkers is the baseline number of concurrent walkers.
	DefaultNumWalkers = 16
	// DefaultPWCEntries is the capacity of every intermediate-level PWC.
	DefaultPWCEntries = 16
	// PTETrafficClass distinguishes page-table reads in the memory hierarchy.
	PTETrafficClass = "gmmu.pte-read"
)

var (
	// ErrPWQFull indicates translation-request backpressure.
	ErrPWQFull = errors.New("GMMU page-walk queue is full")
	// ErrUnknownMemoryResponse indicates a stale or orphan response.
	ErrUnknownMemoryResponse = errors.New("unknown GMMU memory response")
)

// AccessType describes the permission required by a translation.
type AccessType uint8

const (
	// AccessRead requests a readable translation.
	AccessRead AccessType = iota
	// AccessWrite requests a writable translation.
	AccessWrite
	// AccessExecute requests an executable translation.
	AccessExecute
)

// WalkRequest is one L2-TLB miss admitted to the page-walk queue.
type WalkRequest struct {
	ID       uint64
	PID      akitavm.PID
	VAddr    uint64
	DeviceID uint64
	Access   AccessType
	Group    latpc.GroupMember
	HasGroup bool
}

// MemoryRead is an 8-byte physical PTE/PDE read emitted by a walker.
type MemoryRead struct {
	ID           uint64
	Walker       int
	PID          akitavm.PID
	PAddr        uint64
	ByteSize     uint64
	TrafficClass string
}

// Translation is a completed authoritative radix translation.
type Translation struct {
	RequestID uint64
	Page      akitavm.Page
	Reads     uint64
}

// TranslationFaultError is a structured, fatal translation fault.
type TranslationFaultError struct {
	RequestID uint64
	PID       akitavm.PID
	VAddr     uint64
	Level     uint8
	Reason    string
}

func (f TranslationFaultError) Error() string {
	return fmt.Sprintf(
		"gmmu fault: request=%d pid=%d va=0x%x level=%d reason=%s",
		f.RequestID, f.PID, f.VAddr, f.Level, f.Reason)
}

// Stats reports deterministic walker and PWC activity.
type Stats struct {
	// TranslationRequests is the number of L2-TLB misses accepted by the GMMU.
	TranslationRequests     uint64
	WalksStarted            uint64
	RequestsAccepted        uint64
	RequestsRejected        uint64
	PWQFullStalls           uint64
	WalksCompleted          uint64
	Faults                  uint64
	MemoryReads             uint64
	PTWBytes                uint64
	MemoryResponses         uint64
	PTECoherenceRepairs     uint64
	LATPGroups              uint64
	LATPMembers             uint64
	IndependentWalksAvoided uint64
	UpperReadsAvoided       uint64
	LeafPTEReads            uint64

	PWQQueueDelay        timing.VTimeInPicoSec
	WalkerBusyTime       timing.VTimeInPicoSec
	WalkLatency          timing.VTimeInPicoSec
	PTWMemoryLatency     timing.VTimeInPicoSec
	ResponseBackpressure uint64
	WalksByMemoryReads   [mgpuvm.MaxPageTableLevels + 1]uint64
	WalkLatencyBuckets   [5]uint64

	PWCProbes        [mgpuvm.MaxPageTableLevels]uint64
	PWCHits          [mgpuvm.MaxPageTableLevels]uint64
	PWCMisses        [mgpuvm.MaxPageTableLevels]uint64
	PWCFills         [mgpuvm.MaxPageTableLevels]uint64
	PWCEvictions     [mgpuvm.MaxPageTableLevels]uint64
	PWCInvalidations [mgpuvm.MaxPageTableLevels]uint64
	PeakPWQOccupancy int
	PeakWalkersBusy  int
}

// AverageWalkLatency returns zero when no walk has completed.
func (s Stats) AverageWalkLatency() timing.VTimeInPicoSec {
	if s.WalksCompleted == 0 {
		return 0
	}
	return s.WalkLatency / timing.VTimeInPicoSec(s.WalksCompleted)
}

// AverageQueueDelay returns zero when no request has started walking.
func (s Stats) AverageQueueDelay() timing.VTimeInPicoSec {
	if s.WalksStarted == 0 {
		return 0
	}
	return s.PWQQueueDelay / timing.VTimeInPicoSec(s.WalksStarted)
}

// AveragePTWMemoryLatency returns zero when no PTE response has arrived.
func (s Stats) AveragePTWMemoryLatency() timing.VTimeInPicoSec {
	if s.MemoryResponses == 0 {
		return 0
	}
	return s.PTWMemoryLatency / timing.VTimeInPicoSec(s.MemoryResponses)
}

// Config controls the detailed page-walk resources.
type Config struct {
	Format          mgpuvm.PageTableFormat
	PWQEntries      int
	NumWalkers      int
	PWCEntries      int
	TranslationMode latpc.Mode
}

// DefaultConfig returns the LATPC-baseline translation resources.
func DefaultConfig() Config {
	return Config{
		Format:          mgpuvm.X86FourLevel4KFormat(),
		PWQEntries:      DefaultPWQEntries,
		NumWalkers:      DefaultNumWalkers,
		PWCEntries:      DefaultPWCEntries,
		TranslationMode: latpc.ModeBaseline,
	}
}

// WalkerState contains only level-indexed fixed-capacity walk state.
type WalkerState struct {
	Busy             bool
	Req              WalkRequest
	CurrentLevel     uint8
	Indices          [mgpuvm.MaxPageTableLevels]uint64
	TablePAddrs      [mgpuvm.MaxPageTableLevels]uint64
	EntryPAddrs      [mgpuvm.MaxPageTableLevels]uint64
	DecodedEntries   [mgpuvm.MaxPageTableLevels]uint64
	LevelCompleted   [mgpuvm.MaxPageTableLevels]bool
	WaitingMemory    bool
	OutstandingReqID uint64
	Reads            uint64
	SubmittedAt      timing.VTimeInPicoSec
	StartedAt        timing.VTimeInPicoSec
	inheritedRW      bool
	inheritedUser    bool
	inheritedNX      bool
	GroupReqs        []WalkRequest
	GroupIndex       int
	GroupOutstanding int
}

// Core is a cycle-independent detailed walker engine. A component wrapper
// sends MemoryReads through its physical Memory port and feeds responses back.
type Core struct {
	Config  Config
	Table   *mgpuvm.RadixPageTable
	Walkers []WalkerState
	Stats   Stats

	pwq          []WalkRequest
	pwcs         []pageWalkCache
	outgoing     []MemoryRead
	completed    []Translation
	faults       []TranslationFaultError
	reqToWalker  map[uint64]memoryTarget
	submittedAt  map[uint64]timing.VTimeInPicoSec
	memoryIssued map[uint64]timing.VTimeInPicoSec
	nextMemoryID uint64
	currentTime  timing.VTimeInPicoSec
	paused       bool
	draining     bool
}

type memoryTarget struct {
	WalkerIndex int
	GroupMember int
}

// NewCore creates a detailed walker with N-1 intermediate PWCs.
func NewCore(config Config, table *mgpuvm.RadixPageTable) (*Core, error) {
	if err := config.Format.Validate(); err != nil {
		return nil, err
	}
	if table == nil {
		return nil, fmt.Errorf("gmmu: nil radix page table")
	}
	if config.PWQEntries <= 0 || config.NumWalkers <= 0 || config.PWCEntries <= 0 {
		return nil, fmt.Errorf("gmmu: capacities must be positive")
	}
	if table.Format != config.Format {
		return nil, fmt.Errorf("gmmu: page-table format mismatch")
	}

	core := &Core{
		Config:       config,
		Table:        table,
		Walkers:      make([]WalkerState, config.NumWalkers),
		pwcs:         make([]pageWalkCache, int(config.Format.NumLevels)-1),
		reqToWalker:  make(map[uint64]memoryTarget),
		submittedAt:  make(map[uint64]timing.VTimeInPicoSec),
		memoryIssued: make(map[uint64]timing.VTimeInPicoSec),
		nextMemoryID: 1,
	}
	for level := range core.pwcs {
		core.pwcs[level] = newPageWalkCache(config.PWCEntries)
	}
	return core, nil
}

// Submit admits one L2-TLB miss to the FCFS PWQ.
func (c *Core) Submit(req WalkRequest) error {
	return c.SubmitAt(req, c.currentTime)
}

// SubmitAt admits a request and records its simulated arrival time.
func (c *Core) SubmitAt(req WalkRequest, now timing.VTimeInPicoSec) error {
	c.AdvanceTime(now)
	now = c.currentTime
	if !c.CanSubmit() {
		c.Stats.RequestsRejected++
		c.Stats.PWQFullStalls++
		return ErrPWQFull
	}
	if c.Config.TranslationMode == latpc.ModeIdeal {
		c.Stats.RequestsAccepted++
		c.Stats.TranslationRequests++
		page, ok := c.Table.Find(req.PID, req.VAddr)
		if !ok {
			c.Stats.Faults++
			c.faults = append(c.faults, TranslationFaultError{
				RequestID: req.ID,
				PID:       req.PID,
				VAddr:     req.VAddr,
				Reason:    "ideal translation mapping not found",
			})
			return nil
		}
		c.Stats.WalksCompleted++
		c.completed = append(c.completed, Translation{
			RequestID: req.ID,
			Page:      page,
		})
		return nil
	}
	c.pwq = append(c.pwq, req)
	c.submittedAt[req.ID] = now
	c.Stats.RequestsAccepted++
	c.Stats.TranslationRequests++
	if len(c.pwq) > c.Stats.PeakPWQOccupancy {
		c.Stats.PeakPWQOccupancy = len(c.pwq)
	}
	c.dispatch()
	return nil
}

// CanSubmit reports whether Top may retrieve another translation request.
func (c *Core) CanSubmit() bool {
	if c.Config.TranslationMode == latpc.ModeIdeal {
		return !c.paused && !c.draining
	}
	return !c.paused && !c.draining && len(c.pwq) < c.Config.PWQEntries
}

// CompleteMemoryRead correlates an out-of-order memory response to its walker.
func (c *Core) CompleteMemoryRead(requestID uint64, data []byte) error {
	return c.CompleteMemoryReadAt(requestID, data, c.currentTime)
}

// CompleteMemoryReadAt correlates a response and accounts for PTW latency.
//
//nolint:funlen // A response advances the complete, level-dependent walk state.
func (c *Core) CompleteMemoryReadAt(
	requestID uint64,
	data []byte,
	now timing.VTimeInPicoSec,
) error {
	c.AdvanceTime(now)
	now = c.currentTime
	target, ok := c.reqToWalker[requestID]
	if !ok {
		return fmt.Errorf("%w: %d", ErrUnknownMemoryResponse, requestID)
	}
	delete(c.reqToWalker, requestID)
	issuedAt := c.memoryIssued[requestID]
	delete(c.memoryIssued, requestID)
	c.Stats.MemoryResponses++
	c.Stats.PTWMemoryLatency += now - issuedAt

	walkerIndex := target.WalkerIndex
	walker := &c.Walkers[walkerIndex]
	if target.GroupMember >= 0 {
		return c.completeGroupLeafRead(
			walkerIndex, target.GroupMember, requestID, data)
	}
	if !walker.Busy || !walker.WaitingMemory ||
		walker.OutstandingReqID != requestID {
		return fmt.Errorf("%w: %d", ErrUnknownMemoryResponse, requestID)
	}
	walker.WaitingMemory = false
	walker.OutstandingReqID = 0

	if len(data) != int(c.Config.Format.EntryBytes) {
		c.failWalker(walkerIndex, "memory response has invalid entry size")
		return nil
	}
	level := walker.CurrentLevel
	if coherentData, repaired := c.Table.ResolveCoherentEntryData(
		walker.EntryPAddrs[level], data); repaired {
		data = coherentData
		c.Stats.PTECoherenceRepairs++
		simdebug.DPrintf(
			simdebug.PageTable,
			"coherence-repair req=%d pid=%d level=%d entry-pa=0x%x",
			walker.Req.ID,
			walker.Req.PID,
			level,
			walker.EntryPAddrs[level],
		)
	}
	entry, err := decodePTEData(data)
	if err != nil {
		c.failWalker(walkerIndex, err.Error())
		return nil
	}

	walker.DecodedEntries[level] = binary.LittleEndian.Uint64(data)
	walker.LevelCompleted[level] = true
	if !entry.Present {
		c.failWalker(walkerIndex, "non-present entry")
		return nil
	}
	if !c.permissionsAllow(walker, entry) {
		c.failWalker(walkerIndex, "permission denied")
		return nil
	}
	c.mergePermissions(walker, entry)

	if level < c.Config.Format.NumLevels-1 {
		if entry.PageSize {
			c.failWalker(walkerIndex, "unexpected intermediate page-size entry")
			return nil
		}
		if c.pwcs[level].insert(c.pwcKey(walker.Req, level), pwcValue{
			ChildTablePAddr: entry.PhysicalBase,
			ReadWrite:       walker.inheritedRW,
			User:            walker.inheritedUser,
			NoExecute:       walker.inheritedNX,
		}) {
			c.Stats.PWCEvictions[level]++
		}
		c.Stats.PWCFills[level]++
		simdebug.DPrintf(
			simdebug.PWC,
			"fill req=%d pid=%d level=%d child=0x%x",
			walker.Req.ID, walker.Req.PID, level, entry.PhysicalBase)
		walker.CurrentLevel++
		walker.TablePAddrs[walker.CurrentLevel] = entry.PhysicalBase
		c.advanceWalker(walkerIndex)
		return nil
	}

	c.completeWalker(walkerIndex, entry.PhysicalBase)
	return nil
}

// DrainMemoryReads returns newly issued physical reads in issue order.
func (c *Core) DrainMemoryReads() []MemoryRead {
	reads := c.outgoing
	c.outgoing = nil
	return reads
}

// DrainTranslations returns completed translations in completion order.
func (c *Core) DrainTranslations() []Translation {
	translations := c.completed
	c.completed = nil
	return translations
}

// DrainFaults returns structured faults in detection order.
func (c *Core) DrainFaults() []TranslationFaultError {
	faults := c.faults
	c.faults = nil
	return faults
}

// Pause prevents admission and walker dispatch.
func (c *Core) Pause() {
	c.paused = true
}

// Enable resumes admission and dispatch.
func (c *Core) Enable() {
	c.paused = false
	c.draining = false
	c.dispatch()
}

// Drain stops admission and reports completion via IsDrained.
func (c *Core) Drain() {
	c.draining = true
}

// IsDrained reports whether all accepted work has completed.
func (c *Core) IsDrained() bool {
	return len(c.pwq) == 0 && c.busyWalkers() == 0 && len(c.reqToWalker) == 0
}

// Reset discards all runtime work and invalidates every PWC.
func (c *Core) Reset() {
	c.pwq = nil
	c.outgoing = nil
	c.completed = nil
	c.faults = nil
	clear(c.reqToWalker)
	clear(c.submittedAt)
	clear(c.memoryIssued)
	for index := range c.Walkers {
		c.Walkers[index] = WalkerState{}
	}
	for level := range c.pwcs {
		c.Stats.PWCInvalidations[level] += c.pwcs[level].reset()
	}
	c.paused = false
	c.draining = false
}

// InvalidatePID removes cached intermediate entries for one process.
func (c *Core) InvalidatePID(pid akitavm.PID) {
	for level := range c.pwcs {
		c.Stats.PWCInvalidations[level] += c.pwcs[level].invalidatePID(pid)
	}
}

// InvalidateAll removes all intermediate entries, including every PID.
func (c *Core) InvalidateAll() {
	for level := range c.pwcs {
		c.Stats.PWCInvalidations[level] += c.pwcs[level].reset()
	}
}

// AdvanceTime accounts for busy walkers between two component ticks.
func (c *Core) AdvanceTime(now timing.VTimeInPicoSec) {
	if now < c.currentTime {
		return
	}
	c.Stats.WalkerBusyTime += (now - c.currentTime) *
		timing.VTimeInPicoSec(c.busyWalkers())
	c.currentTime = now
}

// RecordResponseBackpressure records a cycle in which a completed
// translation could not be returned to the L2 TLB.
func (c *Core) RecordResponseBackpressure() {
	c.Stats.ResponseBackpressure++
}

// PWCOccupancy returns one intermediate cache's current occupancy.
func (c *Core) PWCOccupancy(level uint8) int {
	return c.pwcs[level].lru.Len()
}

func (c *Core) dispatch() {
	if c.paused {
		return
	}
	for walkerIndex := range c.Walkers {
		if len(c.pwq) == 0 {
			break
		}
		if c.Walkers[walkerIndex].Busy {
			continue
		}
		requests, ready := c.takeNextWalkGroup()
		if !ready {
			break
		}
		c.startWalker(walkerIndex, requests)
	}
	busy := c.busyWalkers()
	if busy > c.Stats.PeakWalkersBusy {
		c.Stats.PeakWalkersBusy = busy
	}
}

func (c *Core) takeNextWalkGroup() ([]WalkRequest, bool) {
	head := c.pwq[0]
	count := int(head.Group.LATPBatchCount)
	if !head.HasGroup || count <= 1 || head.Group.LATPBatchID == 0 {
		c.pwq = c.pwq[1:]
		return []WalkRequest{head}, true
	}
	requests := make([]WalkRequest, 0, count)
	for _, req := range c.pwq {
		if req.HasGroup &&
			req.Group.LATPBatchID == head.Group.LATPBatchID {
			requests = append(requests, req)
		}
	}
	if len(requests) < count {
		return nil, false
	}
	remaining := c.pwq[:0]
	for _, req := range c.pwq {
		if req.HasGroup &&
			req.Group.LATPBatchID == head.Group.LATPBatchID {
			continue
		}
		remaining = append(remaining, req)
	}
	c.pwq = remaining
	return requests, true
}

func (c *Core) startWalker(walkerIndex int, requests []WalkRequest) {
	req := requests[0]
	indices, err := c.Config.Format.ExtractIndices(req.VAddr)
	if err != nil {
		c.faults = append(c.faults, TranslationFaultError{
			RequestID: req.ID, PID: req.PID, VAddr: req.VAddr, Reason: err.Error(),
		})
		c.Stats.Faults++
		return
	}
	addressSpace, ok := c.Table.AddressSpace(req.PID)
	if !ok {
		c.faults = append(c.faults, TranslationFaultError{
			RequestID: req.ID,
			PID:       req.PID,
			VAddr:     req.VAddr,
			Reason:    mgpuvm.ErrAddressSpaceNotFound.Error(),
		})
		c.Stats.Faults++
		return
	}

	c.Walkers[walkerIndex] = WalkerState{
		Busy:          true,
		Req:           req,
		Indices:       indices,
		SubmittedAt:   c.submittedAt[req.ID],
		StartedAt:     c.currentTime,
		inheritedRW:   true,
		inheritedUser: true,
		GroupReqs:     requests,
	}
	for _, memberReq := range requests {
		delete(c.submittedAt, memberReq.ID)
	}
	c.Stats.WalksStarted++
	if len(requests) > 1 {
		saved := uint64(len(requests) - 1)
		c.Stats.LATPGroups++
		c.Stats.LATPMembers += uint64(len(requests))
		c.Stats.IndependentWalksAvoided += saved
		c.Stats.UpperReadsAvoided +=
			saved * uint64(c.Config.Format.NumLevels-1)
	}
	c.Stats.PWQQueueDelay += c.currentTime - c.Walkers[walkerIndex].SubmittedAt
	c.Walkers[walkerIndex].TablePAddrs[0] = addressSpace.RootPAddr
	simdebug.DPrintf(
		simdebug.GMMUWalk,
		"start req=%d walker=%d pid=%d va=0x%x root=0x%x",
		req.ID, walkerIndex, req.PID, req.VAddr, addressSpace.RootPAddr)
	c.advanceWalker(walkerIndex)
}

func (c *Core) advanceWalker(walkerIndex int) {
	walker := &c.Walkers[walkerIndex]
	for walker.CurrentLevel < c.Config.Format.NumLevels-1 {
		level := walker.CurrentLevel
		c.Stats.PWCProbes[level]++
		value, hit := c.pwcs[level].lookup(c.pwcKey(walker.Req, level))
		if !hit {
			c.Stats.PWCMisses[level]++
			simdebug.DPrintf(
				simdebug.PWC,
				"miss req=%d pid=%d level=%d prefix=0x%x",
				walker.Req.ID,
				walker.Req.PID,
				level,
				c.pwcKey(walker.Req, level).Prefix,
			)
			c.issueRead(walkerIndex)
			return
		}
		c.Stats.PWCHits[level]++
		simdebug.DPrintf(
			simdebug.PWC,
			"hit req=%d pid=%d level=%d prefix=0x%x child=0x%x",
			walker.Req.ID,
			walker.Req.PID,
			level,
			c.pwcKey(walker.Req, level).Prefix,
			value.ChildTablePAddr,
		)
		walker.inheritedRW = walker.inheritedRW && value.ReadWrite
		walker.inheritedUser = walker.inheritedUser && value.User
		walker.inheritedNX = walker.inheritedNX || value.NoExecute
		walker.LevelCompleted[level] = true
		walker.CurrentLevel++
		walker.TablePAddrs[walker.CurrentLevel] = value.ChildTablePAddr
	}
	if len(walker.GroupReqs) > 1 {
		c.issueGroupLeafReads(walkerIndex)
		return
	}
	c.issueRead(walkerIndex)
}

func (c *Core) issueRead(walkerIndex int) {
	walker := &c.Walkers[walkerIndex]
	level := walker.CurrentLevel
	entryPAddr, err := c.Config.Format.EntryAddress(
		walker.TablePAddrs[level], level, walker.Indices[level])
	if err != nil {
		c.failWalker(walkerIndex, err.Error())
		return
	}
	requestID := c.nextMemoryID
	c.nextMemoryID++
	walker.EntryPAddrs[level] = entryPAddr
	walker.WaitingMemory = true
	walker.OutstandingReqID = requestID
	walker.Reads++
	c.reqToWalker[requestID] = memoryTarget{
		WalkerIndex: walkerIndex,
		GroupMember: -1,
	}
	c.memoryIssued[requestID] = c.currentTime
	c.outgoing = append(c.outgoing, MemoryRead{
		ID:           requestID,
		Walker:       walkerIndex,
		PID:          walker.Req.PID,
		PAddr:        entryPAddr,
		ByteSize:     uint64(c.Config.Format.EntryBytes),
		TrafficClass: PTETrafficClass,
	})
	c.Stats.MemoryReads++
	c.Stats.PTWBytes += uint64(c.Config.Format.EntryBytes)
	if len(walker.GroupReqs) > 1 &&
		level == c.Config.Format.NumLevels-1 {
		c.Stats.LeafPTEReads++
	}
}

func (c *Core) issueGroupLeafReads(walkerIndex int) {
	walker := &c.Walkers[walkerIndex]
	level := c.Config.Format.NumLevels - 1
	walker.WaitingMemory = true
	for memberIndex, req := range walker.GroupReqs {
		indices, err := c.Config.Format.ExtractIndices(req.VAddr)
		if err != nil {
			c.appendFault(req, level, err.Error())
			continue
		}
		entryPAddr, err := c.Config.Format.EntryAddress(
			walker.TablePAddrs[level], level, indices[level])
		if err != nil {
			c.appendFault(req, level, err.Error())
			continue
		}
		requestID := c.nextMemoryID
		c.nextMemoryID++
		walker.GroupOutstanding++
		walker.Reads++
		c.reqToWalker[requestID] = memoryTarget{
			WalkerIndex: walkerIndex,
			GroupMember: memberIndex,
		}
		c.memoryIssued[requestID] = c.currentTime
		c.outgoing = append(c.outgoing, MemoryRead{
			ID:           requestID,
			Walker:       walkerIndex,
			PID:          req.PID,
			PAddr:        entryPAddr,
			ByteSize:     uint64(c.Config.Format.EntryBytes),
			TrafficClass: PTETrafficClass,
		})
		c.Stats.MemoryReads++
		c.Stats.PTWBytes += uint64(c.Config.Format.EntryBytes)
		c.Stats.LeafPTEReads++
	}
	if walker.GroupOutstanding == 0 {
		c.finishWalker(walkerIndex)
	}
}

func (c *Core) completeGroupLeafRead(
	walkerIndex, memberIndex int,
	requestID uint64,
	data []byte,
) error {
	walker := &c.Walkers[walkerIndex]
	if !walker.Busy || !walker.WaitingMemory ||
		memberIndex < 0 || memberIndex >= len(walker.GroupReqs) {
		return fmt.Errorf("%w: %d", ErrUnknownMemoryResponse, requestID)
	}
	req := walker.GroupReqs[memberIndex]
	level := c.Config.Format.NumLevels - 1
	indices, err := c.Config.Format.ExtractIndices(req.VAddr)
	if err != nil {
		c.appendFault(req, level, err.Error())
		return c.finishGroupLeafResponse(walkerIndex)
	}
	entryPAddr, err := c.Config.Format.EntryAddress(
		walker.TablePAddrs[level], level, indices[level])
	if err != nil {
		c.appendFault(req, level, err.Error())
		return c.finishGroupLeafResponse(walkerIndex)
	}
	if len(data) != int(c.Config.Format.EntryBytes) {
		c.appendFault(req, level, "memory response has invalid entry size")
		return c.finishGroupLeafResponse(walkerIndex)
	}
	if coherentData, repaired := c.Table.ResolveCoherentEntryData(
		entryPAddr, data); repaired {
		data = coherentData
		c.Stats.PTECoherenceRepairs++
	}
	entry, err := decodePTEData(data)
	if err != nil {
		c.appendFault(req, level, err.Error())
		return c.finishGroupLeafResponse(walkerIndex)
	}
	if !entry.Present {
		c.appendFault(req, level, "non-present entry")
		return c.finishGroupLeafResponse(walkerIndex)
	}
	if !c.permissionsAllowAccess(walker, entry, req.Access) {
		c.appendFault(req, level, "permission denied")
		return c.finishGroupLeafResponse(walkerIndex)
	}
	c.appendTranslation(req, entry.PhysicalBase, 1)
	return c.finishGroupLeafResponse(walkerIndex)
}

func (c *Core) finishGroupLeafResponse(walkerIndex int) error {
	walker := &c.Walkers[walkerIndex]
	walker.GroupOutstanding--
	if walker.GroupOutstanding == 0 {
		walker.WaitingMemory = false
		c.finishWalker(walkerIndex)
	}
	return nil
}

func (c *Core) completeWalker(walkerIndex int, physicalBase uint64) {
	walker := &c.Walkers[walkerIndex]
	c.appendTranslation(walker.Req, physicalBase, walker.Reads)
	c.finishWalker(walkerIndex)
}

func (c *Core) appendTranslation(
	req WalkRequest,
	physicalBase uint64,
	reads uint64,
) {
	pageSize := uint64(1) << c.Config.Format.PageOffsetBits
	page, ok := c.Table.MappingMetadata(req.PID, req.VAddr)
	if !ok {
		page = akitavm.Page{
			PID:      req.PID,
			VAddr:    req.VAddr &^ (pageSize - 1),
			PageSize: pageSize,
			DeviceID: req.DeviceID,
			Valid:    true,
		}
	}
	page.PAddr = physicalBase
	c.completed = append(c.completed, Translation{
		RequestID: req.ID,
		Page:      page,
		Reads:     reads,
	})
}

func (c *Core) finishWalker(walkerIndex int) {
	walker := &c.Walkers[walkerIndex]
	c.Stats.WalksCompleted++
	c.Stats.WalkLatency += c.currentTime - walker.StartedAt
	if walker.Reads <= mgpuvm.MaxPageTableLevels {
		c.Stats.WalksByMemoryReads[walker.Reads]++
	}
	c.Stats.WalkLatencyBuckets[walkLatencyBucket(c.currentTime-walker.StartedAt)]++
	c.Walkers[walkerIndex] = WalkerState{}
	c.dispatch()
}

func walkLatencyBucket(latency timing.VTimeInPicoSec) int {
	switch {
	case latency == 0:
		return 0
	case latency <= 10_000:
		return 1
	case latency <= 100_000:
		return 2
	case latency <= 1_000_000:
		return 3
	default:
		return 4
	}
}

func (c *Core) failWalker(walkerIndex int, reason string) {
	walker := &c.Walkers[walkerIndex]
	if len(walker.GroupReqs) > 1 &&
		walker.CurrentLevel == c.Config.Format.NumLevels-1 {
		c.appendFault(walker.Req, walker.CurrentLevel, reason)
		if walker.GroupIndex+1 < len(walker.GroupReqs) {
			walker.GroupIndex++
			walker.Req = walker.GroupReqs[walker.GroupIndex]
			indices, err := c.Config.Format.ExtractIndices(walker.Req.VAddr)
			if err != nil {
				c.appendFault(walker.Req, walker.CurrentLevel, err.Error())
				c.finishWalker(walkerIndex)
				return
			}
			walker.Indices = indices
			c.issueRead(walkerIndex)
			return
		}
		c.finishWalker(walkerIndex)
		return
	}
	if len(walker.GroupReqs) > 1 {
		for _, req := range walker.GroupReqs {
			c.appendFault(req, walker.CurrentLevel, reason)
		}
		c.finishWalker(walkerIndex)
		return
	}
	c.appendFault(walker.Req, walker.CurrentLevel, reason)
	c.Walkers[walkerIndex] = WalkerState{}
	c.dispatch()
}

func (c *Core) appendFault(
	req WalkRequest,
	level uint8,
	reason string,
) {
	c.faults = append(c.faults, TranslationFaultError{
		RequestID: req.ID,
		PID:       req.PID,
		VAddr:     req.VAddr,
		Level:     level,
		Reason:    reason,
	})
	c.Stats.Faults++
}

func (c *Core) permissionsAllow(walker *WalkerState, entry mgpuvm.PTE) bool {
	return c.permissionsAllowAccess(walker, entry, walker.Req.Access)
}

func (c *Core) permissionsAllowAccess(
	walker *WalkerState,
	entry mgpuvm.PTE,
	access AccessType,
) bool {
	if access == AccessWrite &&
		(!walker.inheritedRW || !entry.ReadWrite) {
		return false
	}
	if access == AccessExecute &&
		(walker.inheritedNX || entry.NoExecute) {
		return false
	}
	return true
}

func (c *Core) mergePermissions(walker *WalkerState, entry mgpuvm.PTE) {
	walker.inheritedRW = walker.inheritedRW && entry.ReadWrite
	walker.inheritedUser = walker.inheritedUser && entry.User
	walker.inheritedNX = walker.inheritedNX || entry.NoExecute
}

func (c *Core) pwcKey(req WalkRequest, level uint8) pwcKey {
	shift := uint64(c.Config.Format.PageOffsetBits)
	for lower := int(c.Config.Format.NumLevels) - 1; lower > int(level); lower-- {
		shift += uint64(c.Config.Format.IndexBits[lower])
	}
	return pwcKey{PID: req.PID, Prefix: req.VAddr >> shift}
}

func (c *Core) busyWalkers() int {
	busy := 0
	for index := range c.Walkers {
		if c.Walkers[index].Busy {
			busy++
		}
	}
	return busy
}

func decodePTEData(data []byte) (mgpuvm.PTE, error) {
	return mgpuvm.DecodePTE(binary.LittleEndian.Uint64(data))
}
