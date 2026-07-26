package latpc

import (
	"fmt"
	"log"
	"sync"

	"github.com/sarchlab/akita/v5/mem"
	"github.com/sarchlab/akita/v5/mem/memcontrolprotocol"
	"github.com/sarchlab/akita/v5/mem/vm/vmprotocol"
	"github.com/sarchlab/akita/v5/messaging"
	"github.com/sarchlab/akita/v5/modeling"
	"github.com/sarchlab/akita/v5/timing"
	"github.com/sarchlab/mgpusim/v5/amd/simdebug"
	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

const (
	// LATCTopPortName receives translation requests from an address translator.
	LATCTopPortName = "Top"
	// LATCBottomPortName sends requests to the L1 TLB.
	LATCBottomPortName = "Bottom"
	// LATCControlPortName participates in the translation lifecycle.
	LATCControlPortName = "Control"
)

// LATCSpec contains immutable compression timing and capacity parameters.
type LATCSpec struct {
	Freq           timing.Freq `json:"freq"`
	MSHRSize       int         `json:"mshr_size"`
	NumReqPerCycle int         `json:"num_req_per_cycle"`
}

// LATCState contains the externally checkpointable lifecycle state.
type LATCState struct {
	ControlState memcontrolprotocol.State `json:"control_state"`
}

// LATCResources contains the downstream L1-TLB route.
type LATCResources struct {
	TranslationProviderMapper mem.AddressToPortMapper `json:"-"`
	Format                    mgpuvm.PageTableFormat  `json:"-"`
}

// LATCStats reports compression, pressure, and completion behavior.
type LATCStats struct {
	AcceptedRequests        uint64
	DiscardedRequests       uint64
	DiscardedReservations   uint64
	GroupsPresented         uint64
	CompressedAllocations   uint64
	MSHRAllocations         uint64
	RepresentedTranslations uint64
	ReservationFailures     uint64
	PeakMSHROccupancy       int
	GroupedEntryLifetime    timing.VTimeInPicoSec
	MemberCompletionDelay   timing.VTimeInPicoSec
	CompletedGroups         uint64
	CompletedMembers        uint64
}

// CompressionRatio returns represented translations per logical allocation.
func (s LATCStats) CompressionRatio() float64 {
	if s.MSHRAllocations == 0 {
		return 1
	}
	return float64(s.RepresentedTranslations) / float64(s.MSHRAllocations)
}

// AverageGroupedEntryLifetime returns zero when no group completed.
func (s LATCStats) AverageGroupedEntryLifetime() timing.VTimeInPicoSec {
	if s.CompletedGroups == 0 {
		return 0
	}
	return s.GroupedEntryLifetime / timing.VTimeInPicoSec(s.CompletedGroups)
}

// AverageMemberCompletionDelay returns zero when no member completed.
func (s LATCStats) AverageMemberCompletionDelay() timing.VTimeInPicoSec {
	if s.CompletedMembers == 0 {
		return 0
	}
	return s.MemberCompletionDelay / timing.VTimeInPicoSec(s.CompletedMembers)
}

type latcRequest struct {
	original   vmprotocol.TranslationReq
	admittedAt timing.VTimeInPicoSec
}

type latcInstruction struct {
	members  map[uint16]GroupMember
	waiters  map[uint16][]uint64
	requests map[uint64]latcRequest
	count    uint16
	blocked  bool
}

type latcReservation struct {
	id          uint64
	regular     bool
	allocatedAt timing.VTimeInPicoSec
	remaining   int
}

type latcForward struct {
	request       vmprotocol.TranslationReq
	reservationID uint64
}

type latcMiddleware struct {
	comp     *LATCComp
	detector Detector

	collecting       map[uint64]*latcInstruction
	ready            []*latcInstruction
	reservations     map[uint64]*latcReservation
	requestToReserve map[uint64]uint64
	requests         map[uint64]latcRequest
	pendingForward   []latcForward
	nextReservation  uint64
	stats            LATCStats

	draining     bool
	pendingDrain *memcontrolprotocol.Req
}

// LATCComp is a detector/L1-MSHR compression component.
type LATCComp struct {
	*modeling.Component[LATCSpec, LATCState, LATCResources]
	middleware *latcMiddleware
}

var latcComponents sync.Map

// ResetLATCRegistry clears runtime component discovery before a platform build.
func ResetLATCRegistry() {
	latcComponents = sync.Map{}
}

// LATCComponents returns all currently registered LATC components.
func LATCComponents() []*LATCComp {
	var result []*LATCComp
	latcComponents.Range(func(_, value any) bool {
		result = append(result, value.(*LATCComp))
		return true
	})
	return result
}

// Stats returns a stable copy of the component counters.
func (c *LATCComp) Stats() LATCStats {
	return c.middleware.stats
}

// IsDrained reports whether LATC holds no request or logical reservation.
func (c *LATCComp) IsDrained() bool {
	return c.middleware.isIdle() &&
		len(c.middleware.requestToReserve) == 0 &&
		len(c.middleware.requests) == 0
}

// ValidateInvariants checks counter conservation and internal ownership.
func (c *LATCComp) ValidateInvariants() error {
	m := c.middleware
	if len(m.requestToReserve) != len(m.requests) {
		return fmt.Errorf(
			"LATC %s: request ownership mismatch: reservations=%d requests=%d",
			c.Name(), len(m.requestToReserve), len(m.requests))
	}
	remaining := 0
	for id, reservation := range m.reservations {
		if reservation.remaining <= 0 {
			return fmt.Errorf(
				"LATC %s: reservation %d has non-positive remaining count",
				c.Name(), id)
		}
		remaining += reservation.remaining
	}
	if remaining != len(m.requests) {
		return fmt.Errorf(
			"LATC %s: reservation members=%d live requests=%d",
			c.Name(), remaining, len(m.requests))
	}
	if m.stats.CompletedMembers+m.stats.DiscardedRequests+
		uint64(len(m.requests)) >
		m.stats.AcceptedRequests {
		return fmt.Errorf(
			"LATC %s: completed/live requests exceed accepted requests",
			c.Name())
	}
	if m.stats.CompletedGroups+m.stats.DiscardedReservations+
		uint64(len(m.reservations)) >
		m.stats.MSHRAllocations {
		return fmt.Errorf(
			"LATC %s: completed/live reservations exceed allocations",
			c.Name())
	}
	if m.stats.CompressedAllocations > m.stats.MSHRAllocations {
		return fmt.Errorf(
			"LATC %s: compressed allocations exceed all allocations",
			c.Name())
	}
	if c.IsDrained() &&
		(m.stats.CompletedMembers+m.stats.DiscardedRequests !=
			m.stats.AcceptedRequests ||
			m.stats.CompletedGroups+m.stats.DiscardedReservations !=
				m.stats.MSHRAllocations) {
		return fmt.Errorf(
			"LATC %s: drained counters do not conserve requests/reservations",
			c.Name())
	}
	return nil
}

// LATCBuilder constructs a LATC component.
type LATCBuilder struct {
	registrar modeling.Registrar
	spec      LATCSpec
	resources LATCResources
}

// MakeLATCBuilder creates a builder with paper-profile defaults.
func MakeLATCBuilder() LATCBuilder {
	return LATCBuilder{
		spec: LATCSpec{
			Freq:           1 * timing.GHz,
			MSHRSize:       16,
			NumReqPerCycle: 4,
		},
		resources: LATCResources{Format: mgpuvm.X86FourLevel4KFormat()},
	}
}

// WithRegistrar supplies the component registrar.
func (b LATCBuilder) WithRegistrar(registrar modeling.Registrar) LATCBuilder {
	b.registrar = registrar
	return b
}

// WithSpec replaces the LATC specification.
func (b LATCBuilder) WithSpec(spec LATCSpec) LATCBuilder {
	b.spec = spec
	return b
}

// WithResources supplies the downstream translation route.
func (b LATCBuilder) WithResources(resources LATCResources) LATCBuilder {
	b.resources = resources
	return b
}

// Build creates and registers one LATC component.
func (b LATCBuilder) Build(name string) *LATCComp {
	if b.registrar == nil {
		panic("latpc LATC: WithRegistrar is required")
	}
	if b.spec.MSHRSize <= 0 || b.spec.NumReqPerCycle <= 0 {
		panic("latpc LATC: capacities must be positive")
	}
	detector, err := NewDetector(b.resources.Format)
	if err != nil {
		panic(err)
	}

	modelComp := modeling.NewBuilder[LATCSpec, LATCState, LATCResources]().
		WithEngine(b.registrar.GetEngine()).
		WithFreq(b.spec.Freq).
		WithSpec(b.spec).
		WithResources(b.resources).
		Build(name)
	modelComp.State.ControlState = memcontrolprotocol.StateEnabled
	modelComp.DeclarePort(LATCTopPortName, vmprotocol.Responder)
	modelComp.DeclarePort(LATCBottomPortName, vmprotocol.Requester)
	modelComp.DeclarePort(LATCControlPortName, memcontrolprotocol.Responder)

	comp := &LATCComp{Component: modelComp}
	middleware := &latcMiddleware{
		comp:             comp,
		detector:         detector,
		collecting:       make(map[uint64]*latcInstruction),
		reservations:     make(map[uint64]*latcReservation),
		requestToReserve: make(map[uint64]uint64),
		requests:         make(map[uint64]latcRequest),
		nextReservation:  1,
	}
	comp.middleware = middleware
	modelComp.AddMiddleware(middleware)
	b.registrar.RegisterComponent(modelComp)
	latcComponents.Store(name, comp)
	return comp
}

func (m *latcMiddleware) Tick() bool {
	progress := m.handleControl()
	progress = m.sendForward() || progress
	progress = m.receiveResponse() || progress
	progress = m.allocateReady() || progress
	if m.comp.State.ControlState == memcontrolprotocol.StateEnabled &&
		!m.draining {
		progress = m.receiveRequest() || progress
	}
	progress = m.completeDrain() || progress
	return progress
}

func (m *latcMiddleware) topPort() messaging.Port {
	return m.comp.GetPortByName(LATCTopPortName)
}

func (m *latcMiddleware) bottomPort() messaging.Port {
	return m.comp.GetPortByName(LATCBottomPortName)
}

func (m *latcMiddleware) controlPort() messaging.Port {
	return m.comp.GetPortByName(LATCControlPortName)
}

func (m *latcMiddleware) receiveRequest() bool {
	progress := false
	for range m.comp.Spec().NumReqPerCycle {
		message := m.topPort().PeekIncoming()
		if message == nil {
			break
		}
		req, ok := message.(vmprotocol.TranslationReq)
		if !ok {
			log.Panicf("LATC: unexpected Top message %T", message)
		}
		member, found := RequestMetadata(req.ID)
		if !found {
			member = GroupMember{
				InstructionID: req.ID,
				RequestID:     req.ID,
				Count:         1,
				VAddr:         req.VAddr,
			}
		}
		instruction := m.collecting[member.InstructionID]
		if instruction == nil {
			instruction = &latcInstruction{
				members:  make(map[uint16]GroupMember),
				waiters:  make(map[uint16][]uint64),
				requests: make(map[uint64]latcRequest),
				count:    member.Count,
			}
			m.collecting[member.InstructionID] = instruction
		}
		if _, exists := instruction.members[member.Position]; !exists {
			instruction.members[member.Position] = member
		}
		instruction.waiters[member.Position] = append(
			instruction.waiters[member.Position], req.ID)
		instruction.requests[req.ID] = latcRequest{
			original: req, admittedAt: m.comp.CurrentTime(),
		}
		m.stats.AcceptedRequests++
		m.topPort().RetrieveIncoming()
		if len(instruction.members) == int(instruction.count) {
			delete(m.collecting, member.InstructionID)
			m.ready = append(m.ready, instruction)
		}
		progress = true
	}
	return progress
}

func (m *latcMiddleware) allocateReady() bool {
	if len(m.ready) == 0 {
		return false
	}
	instruction := m.ready[0]
	members := make([]GroupMember, 0, len(instruction.members))
	for _, member := range instruction.members {
		members = append(members, member)
	}
	result := m.detector.Detect(members)
	m.addInstructionWaiters(&result, instruction)
	needed := requiredLATCEntries(result)
	if len(m.reservations)+needed > m.comp.Spec().MSHRSize {
		if !instruction.blocked {
			instruction.blocked = true
			m.stats.ReservationFailures++
		}
		return false
	}

	m.ready = m.ready[1:]
	for _, group := range result.Groups {
		m.stats.GroupsPresented++
		if group.Regular {
			m.allocateGroup(group, instruction)
			continue
		}
		for _, member := range group.Members {
			m.allocateMembers([]TranslationMember{member}, false, instruction)
		}
	}
	if len(m.reservations) > m.stats.PeakMSHROccupancy {
		m.stats.PeakMSHROccupancy = len(m.reservations)
	}
	return true
}

func (m *latcMiddleware) addInstructionWaiters(
	result *DetectionResult,
	instruction *latcInstruction,
) {
	for groupIndex := range result.Groups {
		group := &result.Groups[groupIndex]
		for memberIndex := range group.Members {
			member := &group.Members[memberIndex]
			member.Waiters = nil
			for _, position := range member.Positions {
				member.Waiters = append(
					member.Waiters, instruction.waiters[position]...)
			}
		}
	}
}

func requiredLATCEntries(result DetectionResult) int {
	needed := 0
	for _, group := range result.Groups {
		if group.Regular {
			needed++
		} else {
			needed += len(group.Members)
		}
	}
	return needed
}

func (m *latcMiddleware) allocateGroup(
	group TranslationGroup,
	instruction *latcInstruction,
) {
	m.stats.CompressedAllocations++
	m.allocateMembers(group.Members, true, instruction)
}

func (m *latcMiddleware) allocateMembers(
	members []TranslationMember,
	regular bool,
	instruction *latcInstruction,
) {
	id := m.nextReservation
	m.nextReservation++
	reservation := &latcReservation{
		id:          id,
		regular:     regular,
		allocatedAt: m.comp.CurrentTime(),
	}
	m.reservations[id] = reservation
	m.stats.MSHRAllocations++
	m.stats.RepresentedTranslations += uint64(len(members))

	for _, member := range members {
		for _, waiterID := range member.Waiters {
			request, found := instruction.requests[waiterID]
			if !found {
				panic(fmt.Sprintf("LATC: missing waiter request %d", waiterID))
			}
			reservation.remaining++
			m.requests[waiterID] = request
			m.requestToReserve[waiterID] = id
			m.pendingForward = append(m.pendingForward, latcForward{
				request:       request.original,
				reservationID: id,
			})
		}
	}
	simdebug.DPrintf(
		simdebug.LATC,
		"allocate reservation=%d regular=%t translations=%d waiters=%d occupancy=%d",
		id, regular, len(members), reservation.remaining, len(m.reservations))
}

func (m *latcMiddleware) sendForward() bool {
	progress := false
	for range m.comp.Spec().NumReqPerCycle {
		if len(m.pendingForward) == 0 || !m.bottomPort().CanSend() {
			break
		}
		forward := m.pendingForward[0]
		req := forward.request
		req.Src = m.bottomPort().AsRemote()
		req.Dst = m.comp.Resources().TranslationProviderMapper.Find(req.VAddr)
		m.bottomPort().Send(req)
		m.pendingForward = m.pendingForward[1:]
		progress = true
	}
	return progress
}

func (m *latcMiddleware) receiveResponse() bool {
	progress := false
	for range m.comp.Spec().NumReqPerCycle {
		message := m.bottomPort().PeekIncoming()
		if message == nil || !m.topPort().CanSend() {
			break
		}
		rsp, ok := message.(vmprotocol.TranslationRsp)
		if !ok {
			log.Panicf("LATC: unexpected Bottom message %T", message)
		}
		request, found := m.requests[rsp.RspTo]
		if !found {
			m.bottomPort().RetrieveIncoming()
			progress = true
			continue
		}
		upstream := rsp
		upstream.ID = timing.GetIDGenerator().Generate()
		upstream.Src = m.topPort().AsRemote()
		upstream.Dst = request.original.Src
		upstream.RspTo = request.original.ID
		m.topPort().Send(upstream)
		m.bottomPort().RetrieveIncoming()
		m.completeMember(request.original.ID)
		progress = true
	}
	return progress
}

func (m *latcMiddleware) completeMember(requestID uint64) {
	reservationID := m.requestToReserve[requestID]
	reservation := m.reservations[reservationID]
	request := m.requests[requestID]
	delete(m.requestToReserve, requestID)
	delete(m.requests, requestID)
	reservation.remaining--
	m.stats.CompletedMembers++
	m.stats.MemberCompletionDelay += m.comp.CurrentTime() - request.admittedAt
	if reservation.remaining > 0 {
		return
	}
	delete(m.reservations, reservationID)
	m.stats.CompletedGroups++
	m.stats.GroupedEntryLifetime += m.comp.CurrentTime() - reservation.allocatedAt
	simdebug.DPrintf(
		simdebug.LATC,
		"release reservation=%d regular=%t occupancy=%d",
		reservationID, reservation.regular, len(m.reservations))
}

func (m *latcMiddleware) handleControl() bool {
	message := m.controlPort().PeekIncoming()
	if message == nil {
		return false
	}
	req, ok := message.(memcontrolprotocol.Req)
	if !ok {
		m.controlPort().RetrieveIncoming()
		return true
	}
	if m.pendingDrain != nil {
		return false
	}
	switch req.Command {
	case memcontrolprotocol.CmdEnable:
		return m.respondControl(req, true, "", memcontrolprotocol.StateEnabled)
	case memcontrolprotocol.CmdPause:
		return m.respondControl(req, true, "", memcontrolprotocol.StatePaused)
	case memcontrolprotocol.CmdDrain:
		m.draining = true
		m.pendingDrain = &req
		m.controlPort().RetrieveIncoming()
		return true
	case memcontrolprotocol.CmdReset:
		m.reset()
		return m.respondControl(req, true, "", memcontrolprotocol.StateEnabled)
	case memcontrolprotocol.CmdInvalidate:
		if m.comp.State.ControlState != memcontrolprotocol.StatePaused {
			return m.respondControl(
				req, false, memcontrolprotocol.ErrMustBePausedOrDrained,
				m.comp.State.ControlState)
		}
		return m.respondControl(req, true, "", memcontrolprotocol.StatePaused)
	default:
		return m.respondControl(
			req, false, memcontrolprotocol.ErrUnsupported,
			m.comp.State.ControlState)
	}
}

func (m *latcMiddleware) respondControl(
	req memcontrolprotocol.Req,
	success bool,
	errMessage string,
	state memcontrolprotocol.State,
) bool {
	if !m.controlPort().CanSend() {
		return false
	}
	rsp := memcontrolprotocol.Rsp{
		Command: req.Command,
		Success: success,
		Error:   errMessage,
	}
	rsp.ID = timing.GetIDGenerator().Generate()
	rsp.Src = m.controlPort().AsRemote()
	rsp.Dst = req.Src
	rsp.RspTo = req.ID
	rsp.TrafficClass = "memcontrolprotocol.Rsp"
	m.controlPort().Send(rsp)
	m.controlPort().RetrieveIncoming()
	m.comp.State.ControlState = state
	m.draining = false
	return true
}

func (m *latcMiddleware) completeDrain() bool {
	if m.pendingDrain == nil || !m.isIdle() || !m.controlPort().CanSend() {
		return false
	}
	req := *m.pendingDrain
	m.pendingDrain = nil
	m.comp.State.ControlState = memcontrolprotocol.StatePaused
	m.draining = false
	rsp := memcontrolprotocol.Rsp{Command: req.Command, Success: true}
	rsp.ID = timing.GetIDGenerator().Generate()
	rsp.Src = m.controlPort().AsRemote()
	rsp.Dst = req.Src
	rsp.RspTo = req.ID
	rsp.TrafficClass = "memcontrolprotocol.Rsp"
	m.controlPort().Send(rsp)
	return true
}

func (m *latcMiddleware) isIdle() bool {
	return len(m.collecting) == 0 &&
		len(m.ready) == 0 &&
		len(m.reservations) == 0 &&
		len(m.pendingForward) == 0 &&
		m.bottomPort().PeekIncoming() == nil
}

func (m *latcMiddleware) reset() {
	m.stats.DiscardedRequests +=
		m.stats.AcceptedRequests -
			m.stats.CompletedMembers -
			m.stats.DiscardedRequests
	m.stats.DiscardedReservations +=
		m.stats.MSHRAllocations -
			m.stats.CompletedGroups -
			m.stats.DiscardedReservations
	m.collecting = make(map[uint64]*latcInstruction)
	m.ready = nil
	m.reservations = make(map[uint64]*latcReservation)
	m.requestToReserve = make(map[uint64]uint64)
	m.requests = make(map[uint64]latcRequest)
	m.pendingForward = nil
	m.draining = false
	m.pendingDrain = nil
	for m.topPort().PeekIncoming() != nil {
		m.topPort().RetrieveIncoming()
	}
	for m.bottomPort().PeekIncoming() != nil {
		m.bottomPort().RetrieveIncoming()
	}
}
