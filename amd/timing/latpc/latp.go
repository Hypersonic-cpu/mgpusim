package latpc

import (
	"log"
	"sort"
	"sync"

	"github.com/sarchlab/akita/v5/mem"
	"github.com/sarchlab/akita/v5/mem/memcontrolprotocol"
	"github.com/sarchlab/akita/v5/mem/vm/vmprotocol"
	"github.com/sarchlab/akita/v5/messaging"
	"github.com/sarchlab/akita/v5/modeling"
	"github.com/sarchlab/akita/v5/timing"
	"github.com/sarchlab/mgpusim/v5/amd/simdebug"
)

const (
	// LATPTopPortName receives individual L2-TLB misses.
	LATPTopPortName = "Top"
	// LATPBottomPortName sends batches to the detailed GMMU.
	LATPBottomPortName = "Bottom"
	// LATPControlPortName participates in translation lifecycle control.
	LATPControlPortName = "Control"
)

// LATPSpec configures the grouped page-walk buffer.
type LATPSpec struct {
	Freq           timing.Freq `json:"freq"`
	Entries        int         `json:"entries"`
	NumReqPerCycle int         `json:"num_req_per_cycle"`
	BatchWindow    uint64      `json:"batch_window"`
	NumUpperLevels int         `json:"num_upper_levels"`
}

// LATPState stores lifecycle state.
type LATPState struct {
	ControlState memcontrolprotocol.State `json:"control_state"`
}

// LATPResources contains the GMMU route.
type LATPResources struct {
	TranslationProviderMapper mem.AddressToPortMapper `json:"-"`
}

// LATPStats reports grouped-walk pressure and savings.
type LATPStats struct {
	Groups                  uint64
	Members                 uint64
	IndependentWalksAvoided uint64
	UpperReadsAvoided       uint64
	PWBufferFullStalls      uint64
	PeakPWBufferOccupancy   int
	QueueDelay              timing.VTimeInPicoSec
	CompletedMembers        uint64
	RegularMisses           uint64
	Prefetches              uint64
	UsefulPrefetches        uint64
	LatePrefetches          uint64
	UnusedPrefetches        uint64
}

// PrefetchCoverage returns the fraction of regular misses whose independent
// walk was replaced by a grouped leaf access.
func (s LATPStats) PrefetchCoverage() float64 {
	if s.RegularMisses == 0 {
		return 0
	}
	return float64(s.Prefetches) / float64(s.RegularMisses)
}

// PrefetchAccuracy returns the fraction of grouped leaf fetches that serviced
// demand. Late requests count as accurate but are reported separately.
func (s LATPStats) PrefetchAccuracy() float64 {
	if s.Prefetches == 0 {
		return 0
	}
	return float64(s.UsefulPrefetches+s.LatePrefetches) /
		float64(s.Prefetches)
}

type latpGroupKey struct {
	PID           uint32
	InstructionID uint64
	GroupIndex    uint16
	LeafPage      uint64
}

type latpPendingGroup struct {
	requests   map[uint64]vmprotocol.TranslationReq
	positions  map[uint16]struct{}
	expected   uint16
	firstTick  uint64
	lastTick   uint64
	admittedAt timing.VTimeInPicoSec
}

type latpBatch struct {
	id       uint64
	requests []vmprotocol.TranslationReq
	next     int
}

type latpMiddleware struct {
	comp *LATPComp

	tick          uint64
	pendingGroups map[latpGroupKey]*latpPendingGroup
	ready         []*latpBatch
	requests      map[uint64]vmprotocol.TranslationReq
	admittedAt    map[uint64]timing.VTimeInPicoSec
	nextBatchID   uint64
	stats         LATPStats
	draining      bool
	pendingDrain  *memcontrolprotocol.Req
}

// LATPComp is the grouped page-walk buffer component.
type LATPComp struct {
	*modeling.Component[LATPSpec, LATPState, LATPResources]
	middleware *latpMiddleware
}

var latpComponents sync.Map

// ResetLATPRegistry clears runtime LATP discovery before a platform build.
func ResetLATPRegistry() {
	latpComponents = sync.Map{}
}

// LATPComponents returns all currently registered LATP components.
func LATPComponents() []*LATPComp {
	var result []*LATPComp
	latpComponents.Range(func(_, value any) bool {
		result = append(result, value.(*LATPComp))
		return true
	})
	return result
}

// Stats returns a stable counter copy.
func (c *LATPComp) Stats() LATPStats {
	return c.middleware.stats
}

// LATPBuilder constructs a grouped page-walk buffer.
type LATPBuilder struct {
	registrar modeling.Registrar
	spec      LATPSpec
	resources LATPResources
}

// MakeLATPBuilder creates a builder with paper-profile defaults.
func MakeLATPBuilder() LATPBuilder {
	return LATPBuilder{
		spec: LATPSpec{
			Freq:           1 * timing.GHz,
			Entries:        128,
			NumReqPerCycle: 16,
			BatchWindow:    4,
			NumUpperLevels: 3,
		},
	}
}

// WithRegistrar supplies the component registrar.
func (b LATPBuilder) WithRegistrar(registrar modeling.Registrar) LATPBuilder {
	b.registrar = registrar
	return b
}

// WithSpec replaces the component specification.
func (b LATPBuilder) WithSpec(spec LATPSpec) LATPBuilder {
	b.spec = spec
	return b
}

// WithResources supplies the downstream GMMU route.
func (b LATPBuilder) WithResources(resources LATPResources) LATPBuilder {
	b.resources = resources
	return b
}

// Build creates and registers one LATP component.
func (b LATPBuilder) Build(name string) *LATPComp {
	if b.registrar == nil {
		panic("latpc LATP: WithRegistrar is required")
	}
	if b.spec.Entries <= 0 || b.spec.NumReqPerCycle <= 0 ||
		b.spec.BatchWindow == 0 {
		panic("latpc LATP: capacities must be positive")
	}
	modelComp := modeling.NewBuilder[LATPSpec, LATPState, LATPResources]().
		WithEngine(b.registrar.GetEngine()).
		WithFreq(b.spec.Freq).
		WithSpec(b.spec).
		WithResources(b.resources).
		Build(name)
	modelComp.State.ControlState = memcontrolprotocol.StateEnabled
	modelComp.DeclarePort(LATPTopPortName, vmprotocol.Responder)
	modelComp.DeclarePort(LATPBottomPortName, vmprotocol.Requester)
	modelComp.DeclarePort(LATPControlPortName, memcontrolprotocol.Responder)

	comp := &LATPComp{Component: modelComp}
	middleware := &latpMiddleware{
		comp:          comp,
		pendingGroups: make(map[latpGroupKey]*latpPendingGroup),
		requests:      make(map[uint64]vmprotocol.TranslationReq),
		admittedAt:    make(map[uint64]timing.VTimeInPicoSec),
		nextBatchID:   1,
	}
	comp.middleware = middleware
	modelComp.AddMiddleware(middleware)
	b.registrar.RegisterComponent(modelComp)
	latpComponents.Store(name, comp)
	return comp
}

func (m *latpMiddleware) Tick() bool {
	m.tick++
	progress := m.handleControl()
	progress = m.receiveResponse() || progress
	progress = m.sendReady() || progress
	progress = m.closeAgedGroups() || progress
	if m.comp.State.ControlState == memcontrolprotocol.StateEnabled &&
		!m.draining {
		progress = m.receiveRequest() || progress
	}
	progress = m.completeDrain() || progress
	if len(m.pendingGroups) > 0 {
		// The finite collection window is active hardware work.
		progress = true
	}
	return progress
}

func (m *latpMiddleware) topPort() messaging.Port {
	return m.comp.GetPortByName(LATPTopPortName)
}

func (m *latpMiddleware) bottomPort() messaging.Port {
	return m.comp.GetPortByName(LATPBottomPortName)
}

func (m *latpMiddleware) controlPort() messaging.Port {
	return m.comp.GetPortByName(LATPControlPortName)
}

func (m *latpMiddleware) receiveRequest() bool {
	progress := false
	for range m.comp.Spec().NumReqPerCycle {
		message := m.topPort().PeekIncoming()
		if message == nil {
			break
		}
		if m.occupancy() >= m.comp.Spec().Entries {
			m.stats.PWBufferFullStalls++
			break
		}
		req, ok := message.(vmprotocol.TranslationReq)
		if !ok {
			log.Panicf("LATP: unexpected Top message %T", message)
		}
		member, found := RequestMetadata(req.ID)
		m.topPort().RetrieveIncoming()
		if !found || !member.Regular || member.GroupCount < 2 {
			m.enqueueBatch([]vmprotocol.TranslationReq{req})
			progress = true
			continue
		}
		m.stats.RegularMisses++
		key := latpGroupKey{
			PID:           uint32(req.PID),
			InstructionID: member.InstructionID,
			GroupIndex:    member.GroupIndex,
			LeafPage:      member.LeafPage,
		}
		group := m.pendingGroups[key]
		if group == nil {
			group = &latpPendingGroup{
				requests:   make(map[uint64]vmprotocol.TranslationReq),
				positions:  make(map[uint16]struct{}),
				expected:   member.GroupCount,
				firstTick:  m.tick,
				admittedAt: m.comp.CurrentTime(),
			}
			m.pendingGroups[key] = group
		}
		group.requests[req.ID] = req
		group.positions[member.GroupPosition] = struct{}{}
		group.lastTick = m.tick
		if len(group.positions) == int(group.expected) {
			m.closeGroup(key, group)
		}
		progress = true
	}
	if occupancy := m.occupancy(); occupancy > m.stats.PeakPWBufferOccupancy {
		m.stats.PeakPWBufferOccupancy = occupancy
	}
	return progress
}

func (m *latpMiddleware) closeAgedGroups() bool {
	for key, group := range m.pendingGroups {
		if m.tick-group.lastTick >= m.comp.Spec().BatchWindow {
			m.closeGroup(key, group)
			return true
		}
	}
	return false
}

func (m *latpMiddleware) closeGroup(
	key latpGroupKey,
	group *latpPendingGroup,
) {
	requests := make([]vmprotocol.TranslationReq, 0, len(group.requests))
	for _, request := range group.requests {
		requests = append(requests, request)
	}
	sort.Slice(requests, func(i, j int) bool {
		left, _ := RequestMetadata(requests[i].ID)
		right, _ := RequestMetadata(requests[j].ID)
		return left.GroupPosition < right.GroupPosition
	})
	delete(m.pendingGroups, key)
	m.enqueueBatch(requests)
	m.stats.QueueDelay += m.comp.CurrentTime() - group.admittedAt
}

func (m *latpMiddleware) enqueueBatch(
	requests []vmprotocol.TranslationReq,
) {
	batch := &latpBatch{id: m.nextBatchID, requests: requests}
	m.nextBatchID++
	ids := make([]uint64, len(requests))
	for i, request := range requests {
		ids[i] = request.ID
		m.requests[request.ID] = request
		m.admittedAt[request.ID] = m.comp.CurrentTime()
	}
	MarkLATPBatch(ids, batch.id)
	m.ready = append(m.ready, batch)
	if len(requests) > 1 {
		m.stats.Groups++
		m.stats.Members += uint64(len(requests))
		saved := uint64(len(requests) - 1)
		m.stats.IndependentWalksAvoided += saved
		m.stats.UpperReadsAvoided +=
			saved * uint64(m.comp.Spec().NumUpperLevels)
		m.stats.Prefetches += saved
		// Every additional member already has an L2 demand MSHR when the
		// group closes, so its grouped leaf access is a late prefetch.
		m.stats.LatePrefetches += saved
		simdebug.DPrintf(
			simdebug.LATP,
			"batch=%d members=%d walks-saved=%d",
			batch.id, len(requests), saved)
	}
}

func (m *latpMiddleware) sendReady() bool {
	progress := false
	for range m.comp.Spec().NumReqPerCycle {
		if len(m.ready) == 0 || !m.bottomPort().CanSend() {
			break
		}
		batch := m.ready[0]
		req := batch.requests[batch.next]
		req.Src = m.bottomPort().AsRemote()
		req.Dst = m.comp.Resources().TranslationProviderMapper.Find(req.VAddr)
		m.bottomPort().Send(req)
		batch.next++
		if batch.next == len(batch.requests) {
			m.ready = m.ready[1:]
		}
		progress = true
	}
	return progress
}

func (m *latpMiddleware) receiveResponse() bool {
	progress := false
	for range m.comp.Spec().NumReqPerCycle {
		message := m.bottomPort().PeekIncoming()
		if message == nil || !m.topPort().CanSend() {
			break
		}
		rsp, ok := message.(vmprotocol.TranslationRsp)
		if !ok {
			log.Panicf("LATP: unexpected Bottom message %T", message)
		}
		request, found := m.requests[rsp.RspTo]
		m.bottomPort().RetrieveIncoming()
		if !found {
			progress = true
			continue
		}
		upstream := rsp
		upstream.ID = timing.GetIDGenerator().Generate()
		upstream.Src = m.topPort().AsRemote()
		upstream.Dst = request.Src
		upstream.RspTo = request.ID
		m.topPort().Send(upstream)
		delete(m.requests, request.ID)
		delete(m.admittedAt, request.ID)
		m.stats.CompletedMembers++
		progress = true
	}
	return progress
}

func (m *latpMiddleware) occupancy() int {
	occupancy := len(m.requests)
	for _, group := range m.pendingGroups {
		occupancy += len(group.requests)
	}
	return occupancy
}

func (m *latpMiddleware) handleControl() bool {
	message := m.controlPort().PeekIncoming()
	if message == nil || m.pendingDrain != nil {
		return false
	}
	req, ok := message.(memcontrolprotocol.Req)
	if !ok {
		m.controlPort().RetrieveIncoming()
		return true
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

func (m *latpMiddleware) respondControl(
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

func (m *latpMiddleware) completeDrain() bool {
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

func (m *latpMiddleware) isIdle() bool {
	return len(m.pendingGroups) == 0 && len(m.ready) == 0 &&
		len(m.requests) == 0 && m.bottomPort().PeekIncoming() == nil
}

func (m *latpMiddleware) reset() {
	m.pendingGroups = make(map[latpGroupKey]*latpPendingGroup)
	m.ready = nil
	m.requests = make(map[uint64]vmprotocol.TranslationReq)
	m.admittedAt = make(map[uint64]timing.VTimeInPicoSec)
	m.draining = false
	m.pendingDrain = nil
	for m.topPort().PeekIncoming() != nil {
		m.topPort().RetrieveIncoming()
	}
	for m.bottomPort().PeekIncoming() != nil {
		m.bottomPort().RetrieveIncoming()
	}
}
