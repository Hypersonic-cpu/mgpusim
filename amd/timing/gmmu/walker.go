// Package gmmu implements a memory-backed GPU page-table walker.
package gmmu

import (
	"encoding/binary"
	"errors"
	"fmt"

	akitavm "github.com/sarchlab/akita/v5/mem/vm"
	"github.com/sarchlab/mgpusim/v5/amd/simdebug"
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
	RequestsAccepted uint64
	RequestsRejected uint64
	WalksCompleted   uint64
	Faults           uint64
	MemoryReads      uint64
	PWCHits          [mgpuvm.MaxPageTableLevels]uint64
	PWCMisses        [mgpuvm.MaxPageTableLevels]uint64
	PeakPWQOccupancy int
	PeakWalkersBusy  int
}

// Config controls the detailed page-walk resources.
type Config struct {
	Format     mgpuvm.PageTableFormat
	PWQEntries int
	NumWalkers int
	PWCEntries int
}

// DefaultConfig returns the LATPC-baseline translation resources.
func DefaultConfig() Config {
	return Config{
		Format:     mgpuvm.X86FourLevel4KFormat(),
		PWQEntries: DefaultPWQEntries,
		NumWalkers: DefaultNumWalkers,
		PWCEntries: DefaultPWCEntries,
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
	inheritedRW      bool
	inheritedUser    bool
	inheritedNX      bool
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
	reqToWalker  map[uint64]int
	nextMemoryID uint64
	paused       bool
	draining     bool
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
		reqToWalker:  make(map[uint64]int),
		nextMemoryID: 1,
	}
	for level := range core.pwcs {
		core.pwcs[level] = newPageWalkCache(config.PWCEntries)
	}
	return core, nil
}

// Submit admits one L2-TLB miss to the FCFS PWQ.
func (c *Core) Submit(req WalkRequest) error {
	if !c.CanSubmit() {
		c.Stats.RequestsRejected++
		return ErrPWQFull
	}
	c.pwq = append(c.pwq, req)
	c.Stats.RequestsAccepted++
	if len(c.pwq) > c.Stats.PeakPWQOccupancy {
		c.Stats.PeakPWQOccupancy = len(c.pwq)
	}
	c.dispatch()
	return nil
}

// CanSubmit reports whether Top may retrieve another translation request.
func (c *Core) CanSubmit() bool {
	return !c.paused && !c.draining && len(c.pwq) < c.Config.PWQEntries
}

// CompleteMemoryRead correlates an out-of-order memory response to its walker.
func (c *Core) CompleteMemoryRead(requestID uint64, data []byte) error {
	walkerIndex, ok := c.reqToWalker[requestID]
	if !ok {
		return fmt.Errorf("%w: %d", ErrUnknownMemoryResponse, requestID)
	}
	delete(c.reqToWalker, requestID)

	walker := &c.Walkers[walkerIndex]
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
	entry, err := decodePTEData(data)
	if err != nil {
		c.failWalker(walkerIndex, err.Error())
		return nil
	}

	level := walker.CurrentLevel
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
		c.pwcs[level].insert(c.pwcKey(walker.Req, level), pwcValue{
			ChildTablePAddr: entry.PhysicalBase,
			ReadWrite:       walker.inheritedRW,
			User:            walker.inheritedUser,
			NoExecute:       walker.inheritedNX,
		})
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
	for index := range c.Walkers {
		c.Walkers[index] = WalkerState{}
	}
	for level := range c.pwcs {
		c.pwcs[level].reset()
	}
	c.paused = false
	c.draining = false
}

// InvalidatePID removes cached intermediate entries for one process.
func (c *Core) InvalidatePID(pid akitavm.PID) {
	for level := range c.pwcs {
		c.pwcs[level].invalidatePID(pid)
	}
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
		req := c.pwq[0]
		c.pwq = c.pwq[1:]
		c.startWalker(walkerIndex, req)
	}
	busy := c.busyWalkers()
	if busy > c.Stats.PeakWalkersBusy {
		c.Stats.PeakWalkersBusy = busy
	}
}

func (c *Core) startWalker(walkerIndex int, req WalkRequest) {
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
		inheritedRW:   true,
		inheritedUser: true,
	}
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
	c.reqToWalker[requestID] = walkerIndex
	c.outgoing = append(c.outgoing, MemoryRead{
		ID:           requestID,
		Walker:       walkerIndex,
		PID:          walker.Req.PID,
		PAddr:        entryPAddr,
		ByteSize:     uint64(c.Config.Format.EntryBytes),
		TrafficClass: PTETrafficClass,
	})
	c.Stats.MemoryReads++
}

func (c *Core) completeWalker(walkerIndex int, physicalBase uint64) {
	walker := &c.Walkers[walkerIndex]
	pageSize := uint64(1) << c.Config.Format.PageOffsetBits
	page, ok := c.Table.MappingMetadata(walker.Req.PID, walker.Req.VAddr)
	if !ok {
		page = akitavm.Page{
			PID:      walker.Req.PID,
			VAddr:    walker.Req.VAddr &^ (pageSize - 1),
			PageSize: pageSize,
			DeviceID: walker.Req.DeviceID,
			Valid:    true,
		}
	}
	page.PAddr = physicalBase
	c.completed = append(c.completed, Translation{
		RequestID: walker.Req.ID,
		Page:      page,
		Reads:     walker.Reads,
	})
	c.Stats.WalksCompleted++
	c.Walkers[walkerIndex] = WalkerState{}
	c.dispatch()
}

func (c *Core) failWalker(walkerIndex int, reason string) {
	walker := &c.Walkers[walkerIndex]
	c.faults = append(c.faults, TranslationFaultError{
		RequestID: walker.Req.ID,
		PID:       walker.Req.PID,
		VAddr:     walker.Req.VAddr,
		Level:     walker.CurrentLevel,
		Reason:    reason,
	})
	c.Stats.Faults++
	c.Walkers[walkerIndex] = WalkerState{}
	c.dispatch()
}

func (c *Core) permissionsAllow(walker *WalkerState, entry mgpuvm.PTE) bool {
	if walker.Req.Access == AccessWrite &&
		(!walker.inheritedRW || !entry.ReadWrite) {
		return false
	}
	if walker.Req.Access == AccessExecute &&
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
