package gmmu

import (
	"fmt"
	"log"
	"sync"

	"github.com/sarchlab/akita/v5/mem"
	"github.com/sarchlab/akita/v5/mem/memcontrolprotocol"
	"github.com/sarchlab/akita/v5/mem/memprotocol"
	"github.com/sarchlab/akita/v5/mem/vm/vmprotocol"
	"github.com/sarchlab/akita/v5/messaging"
	"github.com/sarchlab/akita/v5/modeling"
	"github.com/sarchlab/akita/v5/timing"
	"github.com/sarchlab/mgpusim/v5/amd/simdebug"
	"github.com/sarchlab/mgpusim/v5/amd/timing/latpc"
	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

var registeredComps sync.Map // map[string]*Comp, keyed by component name.

const (
	// TopPortName accepts L2-TLB translation misses.
	TopPortName = "Top"
	// MemoryPortName emits physical PTE reads.
	MemoryPortName = "Memory"
	// ControlPortName implements the memory-agent lifecycle.
	ControlPortName = "Control"
	// FaultPortName emits structured fatal faults.
	FaultPortName = "Fault"
)

var (
	faultProtocol = messaging.DefineProtocol(
		"mgpusim.gmmu.fault",
		messaging.RoleDef{Name: "source", Sends: []messaging.Msg{FaultMessage{}}},
		messaging.RoleDef{Name: "sink", Sends: []messaging.Msg{FaultAck{}}},
	)
	faultSource = faultProtocol.Role("source")
)

// FaultMessage carries a structured fatal page-walk failure.
type FaultMessage struct {
	messaging.MsgMeta
	Fault TranslationFaultError
}

// FaultAck is reserved for fault sinks that acknowledge diagnostics.
type FaultAck struct {
	messaging.MsgMeta
}

// Spec contains immutable component and routing configuration.
type Spec struct {
	Freq        timing.Freq          `json:"freq"`
	FaultModule messaging.RemotePort `json:"fault_module"`
}

// State contains checkpointable control state.
type State struct {
	ControlState  memcontrolprotocol.State `json:"control_state"`
	CurrentCmdID  uint64                   `json:"current_cmd_id"`
	CurrentCmdSrc messaging.RemotePort     `json:"current_cmd_src"`
}

// Resources holds the shared authoritative radix page table.
type Resources struct {
	PageTable    *mgpuvm.RadixPageTable  `json:"-"`
	WalkerConfig Config                  `json:"-"`
	MemoryMapper mem.AddressToPortMapper `json:"-"`
}

// Comp wraps an Akita component and exposes its detailed walker core.
type Comp struct {
	*modeling.Component[Spec, State, Resources]
	Core *Core
}

// Builder constructs a detailed GMMU.
type Builder struct {
	spec      Spec
	resources Resources
	registrar modeling.Registrar
}

// MakeBuilder returns a builder with baseline translation resources.
func MakeBuilder() Builder {
	return Builder{
		spec:      Spec{Freq: 1 * timing.GHz},
		resources: Resources{WalkerConfig: DefaultConfig()},
	}
}

// WithRegistrar supplies the simulation registrar.
func (b Builder) WithRegistrar(registrar modeling.Registrar) Builder {
	b.registrar = registrar
	return b
}

// WithSpec replaces the component configuration.
func (b Builder) WithSpec(spec Spec) Builder {
	b.spec = spec
	return b
}

// WithResources supplies the authoritative radix page table.
func (b Builder) WithResources(resources Resources) Builder {
	b.resources = resources
	return b
}

// Build creates the component and declares all four required ports.
func (b Builder) Build(name string) *Comp {
	if b.registrar == nil {
		panic("gmmu: WithRegistrar is required")
	}
	core, err := NewCore(b.resources.WalkerConfig, b.resources.PageTable)
	if err != nil {
		panic(err)
	}
	modelComp := modeling.NewBuilder[Spec, State, Resources]().
		WithEngine(b.registrar.GetEngine()).
		WithFreq(b.spec.Freq).
		WithSpec(b.spec).
		WithResources(b.resources).
		Build(name)
	modelComp.State.ControlState = memcontrolprotocol.StateEnabled
	modelComp.DeclarePort(TopPortName, vmprotocol.Responder)
	modelComp.DeclarePort(MemoryPortName, memprotocol.Requester)
	modelComp.DeclarePort(ControlPortName, memcontrolprotocol.Responder)
	modelComp.DeclarePort(FaultPortName, faultSource)

	comp := &Comp{Component: modelComp, Core: core}
	modelComp.AddMiddleware(newComponentMiddleware(comp))
	b.registrar.RegisterComponent(modelComp)
	registeredComps.Store(name, comp)
	return comp
}

// Lookup returns the detailed GMMU wrapper registered under name. Modeling
// registers the embedded generic component, so reporters use this lookup to
// obtain the wrapper's walker statistics without changing the framework's
// component ownership model.
func Lookup(name string) (*Comp, bool) {
	value, ok := registeredComps.Load(name)
	if !ok {
		return nil, false
	}
	comp, ok := value.(*Comp)
	return comp, ok
}

type pendingMemoryRead struct {
	coreID uint64
	msg    memprotocol.ReadReq
}

type componentMiddleware struct {
	comp *Comp

	topRequests      map[uint64]vmprotocol.TranslationReq
	memoryRequestIDs map[uint64]uint64
	pendingMemory    []pendingMemoryRead
	pendingResponses []vmprotocol.TranslationRsp
	pendingFaults    []FaultMessage
}

func newComponentMiddleware(comp *Comp) *componentMiddleware {
	return &componentMiddleware{
		comp:             comp,
		topRequests:      make(map[uint64]vmprotocol.TranslationReq),
		memoryRequestIDs: make(map[uint64]uint64),
	}
}

func (m *componentMiddleware) Tick() bool {
	m.comp.Core.AdvanceTime(m.comp.CurrentTime())
	progress := m.handleControl()
	if m.comp.State.ControlState == memcontrolprotocol.StatePaused {
		return progress
	}

	progress = m.receiveMemoryResponse() || progress
	progress = m.collectCoreOutput() || progress
	progress = m.sendFault() || progress
	progress = m.sendTranslationResponse() || progress
	progress = m.sendMemoryRead() || progress
	if m.comp.State.ControlState == memcontrolprotocol.StateEnabled {
		progress = m.receiveTranslationRequest() || progress
	}
	return progress
}

func (m *componentMiddleware) topPort() messaging.Port {
	return m.comp.GetPortByName(TopPortName)
}

func (m *componentMiddleware) memoryPort() messaging.Port {
	return m.comp.GetPortByName(MemoryPortName)
}

func (m *componentMiddleware) controlPort() messaging.Port {
	return m.comp.GetPortByName(ControlPortName)
}

func (m *componentMiddleware) faultPort() messaging.Port {
	return m.comp.GetPortByName(FaultPortName)
}

func (m *componentMiddleware) receiveTranslationRequest() bool {
	message := m.topPort().PeekIncoming()
	if message == nil {
		return false
	}
	req, ok := message.(vmprotocol.TranslationReq)
	if !ok {
		log.Panicf("gmmu: unexpected Top message %T", message)
	}
	walkReq := WalkRequest{
		ID: req.ID, PID: req.PID, VAddr: req.VAddr, DeviceID: req.DeviceID,
	}
	if group, found := latpc.RequestMetadata(req.ID); found {
		walkReq.Group = group
		walkReq.HasGroup = true
	}
	if !m.comp.Core.CanSubmitRequest(walkReq) {
		return false
	}
	m.topPort().RetrieveIncoming()
	m.topRequests[req.ID] = req
	err := m.comp.Core.SubmitAt(walkReq, m.comp.CurrentTime())
	if err != nil {
		panic(err)
	}
	simdebug.DPrintf(
		simdebug.GMMUWalk,
		"admit req=%d pid=%d va=0x%x pwq=%d",
		req.ID, req.PID, req.VAddr, len(m.comp.Core.pwq))
	return true
}

func (m *componentMiddleware) collectCoreOutput() bool {
	progress := false
	for _, read := range m.comp.Core.DrainMemoryReads() {
		msg := memprotocol.ReadReq{
			Address:        read.PAddr,
			AccessByteSize: read.ByteSize,
			PID:            read.PID,
			Info:           read,
		}
		msg.ID = timing.GetIDGenerator().Generate()
		msg.Src = m.memoryPort().AsRemote()
		msg.Dst = m.comp.Resources().MemoryMapper.Find(read.PAddr)
		msg.TrafficClass = read.TrafficClass
		msg.TrafficBytes = int(read.ByteSize)
		m.pendingMemory = append(m.pendingMemory, pendingMemoryRead{
			coreID: read.ID,
			msg:    msg,
		})
		progress = true
	}
	for _, translation := range m.comp.Core.DrainTranslations() {
		latpc.ReleaseRequestMetadata(translation.RequestID)
		req, ok := m.topRequests[translation.RequestID]
		if !ok {
			continue
		}
		delete(m.topRequests, translation.RequestID)
		rsp := vmprotocol.TranslationRsp{Page: translation.Page}
		rsp.ID = timing.GetIDGenerator().Generate()
		rsp.Src = m.topPort().AsRemote()
		rsp.Dst = req.Src
		rsp.RspTo = req.ID
		rsp.TrafficClass = "vmprotocol.TranslationRsp"
		m.pendingResponses = append(m.pendingResponses, rsp)
		simdebug.DPrintf(
			simdebug.GMMUWalk,
			"complete req=%d pid=%d va=0x%x pa=0x%x reads=%d",
			req.ID, req.PID, req.VAddr, translation.Page.PAddr, translation.Reads)
		progress = true
	}
	for _, fault := range m.comp.Core.DrainFaults() {
		latpc.ReleaseRequestMetadata(fault.RequestID)
		req := m.topRequests[fault.RequestID]
		delete(m.topRequests, fault.RequestID)
		if m.comp.Spec().FaultModule == "" {
			panic(fault)
		}
		msg := FaultMessage{Fault: fault}
		msg.ID = timing.GetIDGenerator().Generate()
		msg.Src = m.faultPort().AsRemote()
		msg.Dst = m.comp.Spec().FaultModule
		msg.TrafficClass = "gmmu.fault"
		msg.RspTo = req.ID
		m.pendingFaults = append(m.pendingFaults, msg)
		simdebug.DPrintf(simdebug.Fault, "%s", fault.Error())
		progress = true
	}
	return progress
}

func (m *componentMiddleware) sendMemoryRead() bool {
	if len(m.pendingMemory) == 0 || !m.memoryPort().CanSend() {
		return false
	}
	pending := m.pendingMemory[0]
	m.memoryPort().Send(pending.msg)
	m.memoryRequestIDs[pending.msg.ID] = pending.coreID
	m.pendingMemory = m.pendingMemory[1:]
	simdebug.DPrintf(
		simdebug.PTWMem,
		"send req=%d core=%d pid=%d pa=0x%x bytes=%d class=%s",
		pending.msg.ID,
		pending.coreID,
		pending.msg.PID,
		pending.msg.Address,
		pending.msg.AccessByteSize,
		pending.msg.TrafficClass,
	)
	return true
}

func (m *componentMiddleware) receiveMemoryResponse() bool {
	message := m.memoryPort().PeekIncoming()
	if message == nil {
		return false
	}
	rsp, ok := message.(memprotocol.DataReadyRsp)
	if !ok {
		log.Panicf("gmmu: unexpected Memory message %T", message)
	}
	coreID, ok := m.memoryRequestIDs[rsp.RspTo]
	m.memoryPort().RetrieveIncoming()
	if !ok {
		return true
	}
	delete(m.memoryRequestIDs, rsp.RspTo)
	if err := m.comp.Core.CompleteMemoryReadAt(
		coreID, rsp.Data, m.comp.CurrentTime()); err != nil {
		panic(err)
	}
	simdebug.DPrintf(
		simdebug.PTWMem,
		"recv rsp=%d req=%d core=%d bytes=%d",
		rsp.ID, rsp.RspTo, coreID, len(rsp.Data))
	return true
}

func (m *componentMiddleware) sendTranslationResponse() bool {
	if len(m.pendingResponses) == 0 {
		return false
	}
	if !m.topPort().CanSend() {
		m.comp.Core.RecordResponseBackpressure()
		return false
	}
	rsp := m.pendingResponses[0]
	m.topPort().Send(rsp)
	m.pendingResponses = m.pendingResponses[1:]
	simdebug.DPrintf(
		simdebug.TLBFill,
		"rsp=%d req=%d va=0x%x pa=0x%x",
		rsp.ID, rsp.RspTo, rsp.Page.VAddr, rsp.Page.PAddr)
	simdebug.DPrintf(
		simdebug.TLBReplay,
		"release req=%d pid=%d va=0x%x pa=0x%x to-l2-tlb=%s",
		rsp.RspTo,
		rsp.Page.PID,
		rsp.Page.VAddr,
		rsp.Page.PAddr,
		rsp.Dst,
	)
	return true
}

func (m *componentMiddleware) sendFault() bool {
	if len(m.pendingFaults) == 0 || !m.faultPort().CanSend() {
		return false
	}
	m.faultPort().Send(m.pendingFaults[0])
	m.pendingFaults = m.pendingFaults[1:]
	return true
}

func (m *componentMiddleware) handleControl() bool {
	if m.completeDrain() {
		return true
	}
	message := m.controlPort().PeekIncoming()
	if message == nil || m.comp.State.ControlState == memcontrolprotocol.StateDraining {
		return false
	}
	req, ok := message.(memcontrolprotocol.Req)
	if !ok {
		m.controlPort().RetrieveIncoming()
		return true
	}
	if req.Command == memcontrolprotocol.CmdDrain {
		m.comp.Core.Drain()
		m.comp.State.ControlState = memcontrolprotocol.StateDraining
		m.comp.State.CurrentCmdID = req.ID
		m.comp.State.CurrentCmdSrc = req.Src
		m.controlPort().RetrieveIncoming()
		return true
	}
	if !m.controlPort().CanSend() {
		return false
	}

	success, errorText := m.applyControl(req)
	rsp := memcontrolprotocol.Rsp{
		Command: req.Command,
		Success: success,
		Error:   errorText,
	}
	rsp.ID = timing.GetIDGenerator().Generate()
	rsp.Src = m.controlPort().AsRemote()
	rsp.Dst = req.Src
	rsp.RspTo = req.ID
	rsp.TrafficClass = "memcontrolprotocol.Rsp"
	m.controlPort().Send(rsp)
	m.controlPort().RetrieveIncoming()
	return true
}

func (m *componentMiddleware) applyControl(
	req memcontrolprotocol.Req,
) (bool, string) {
	switch req.Command {
	case memcontrolprotocol.CmdPause:
		m.comp.Core.Pause()
		m.comp.State.ControlState = memcontrolprotocol.StatePaused
	case memcontrolprotocol.CmdEnable:
		m.comp.Core.Enable()
		m.comp.State.ControlState = memcontrolprotocol.StateEnabled
	case memcontrolprotocol.CmdReset:
		m.resetRuntime()
		m.comp.State.ControlState = memcontrolprotocol.StateEnabled
	case memcontrolprotocol.CmdInvalidate:
		if m.comp.State.ControlState == memcontrolprotocol.StateEnabled {
			return false, memcontrolprotocol.ErrMustBePausedOrDrained
		}
		if req.PID == 0 {
			m.comp.Core.InvalidateAll()
		} else {
			m.comp.Core.InvalidatePID(req.PID)
		}
	default:
		return false, memcontrolprotocol.ErrUnsupported
	}
	return true, ""
}

func (m *componentMiddleware) completeDrain() bool {
	if m.comp.State.ControlState != memcontrolprotocol.StateDraining ||
		!m.comp.Core.IsDrained() ||
		len(m.pendingMemory) != 0 ||
		len(m.memoryRequestIDs) != 0 ||
		len(m.pendingResponses) != 0 {
		return false
	}
	if !m.controlPort().CanSend() {
		return false
	}
	rsp := memcontrolprotocol.Rsp{
		Command: memcontrolprotocol.CmdDrain,
		Success: true,
	}
	rsp.ID = timing.GetIDGenerator().Generate()
	rsp.Src = m.controlPort().AsRemote()
	rsp.Dst = m.comp.State.CurrentCmdSrc
	rsp.RspTo = m.comp.State.CurrentCmdID
	rsp.TrafficClass = "memcontrolprotocol.Rsp"
	m.controlPort().Send(rsp)
	m.comp.State.ControlState = memcontrolprotocol.StatePaused
	m.comp.Core.Pause()
	return true
}

func (m *componentMiddleware) resetRuntime() {
	m.comp.Core.Reset()
	clear(m.topRequests)
	clear(m.memoryRequestIDs)
	m.pendingMemory = nil
	m.pendingResponses = nil
	m.pendingFaults = nil
	for m.topPort().RetrieveIncoming() != nil {
	}
	for m.memoryPort().RetrieveIncoming() != nil {
	}
}

// Stats returns a snapshot of detailed translation activity.
func (c *Comp) Stats() Stats {
	return c.Core.Stats
}

// ValidateWiring checks the required downstream routes.
func (c *Comp) ValidateWiring() error {
	if c.Resources().MemoryMapper == nil {
		return fmt.Errorf("gmmu: MemoryMapper is required")
	}
	return nil
}
