package latpc

import (
	"testing"

	"github.com/sarchlab/akita/v5/hooking"
	"github.com/sarchlab/akita/v5/mem"
	"github.com/sarchlab/akita/v5/mem/memcontrolprotocol"
	"github.com/sarchlab/akita/v5/mem/vm"
	"github.com/sarchlab/akita/v5/mem/vm/vmprotocol"
	"github.com/sarchlab/akita/v5/messaging"
	"github.com/sarchlab/akita/v5/modeling"
	"github.com/sarchlab/akita/v5/timing"
	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

type latcTestConnection struct {
	hooking.HookableBase
}

func (c *latcTestConnection) Name() string                   { return "latc-test" }
func (c *latcTestConnection) PlugIn(port messaging.Port)     { port.SetConnection(c) }
func (c *latcTestConnection) Unplug(messaging.Port)          {}
func (c *latcTestConnection) NotifyAvailable(messaging.Port) {}
func (c *latcTestConnection) NotifySend()                    {}

func makeLATCTestComponent(
	t *testing.T,
	mshrSize int,
) (*LATCComp, map[string]messaging.Port) {
	t.Helper()
	ResetRuntimeMetadata()
	registrar := modeling.NewStandaloneRegistrar(timing.NewSerialEngine())
	spec := LATCSpec{
		Freq:           1 * timing.GHz,
		MSHRSize:       mshrSize,
		NumReqPerCycle: 8,
	}
	comp := MakeLATCBuilder().
		WithRegistrar(registrar).
		WithSpec(spec).
		WithResources(LATCResources{
			TranslationProviderMapper: &mem.SinglePortMapper{Port: "L1TLB.Top"},
			Format:                    mgpuvm.X86FourLevel4KFormat(),
		}).
		Build("LATC")
	ports := make(map[string]messaging.Port)
	for _, name := range []string{
		LATCTopPortName, LATCBottomPortName, LATCControlPortName,
	} {
		port := messaging.NewPort(comp, 16, 16, comp.Name()+"."+name)
		comp.AssignPort(name, port)
		(&latcTestConnection{}).PlugIn(port)
		ports[name] = port
	}
	return comp, ports
}

func deliverLATCGroup(
	t *testing.T,
	top messaging.Port,
	instructionID uint64,
	vpns ...uint64,
) []vmprotocol.TranslationReq {
	t.Helper()
	requests := make([]vmprotocol.TranslationReq, len(vpns))
	for i, vpn := range vpns {
		req := vmprotocol.TranslationReq{
			PID:   1,
			VAddr: vpn * 4096,
		}
		req.ID = timing.GetIDGenerator().Generate()
		req.Src = "AddressTranslator.Translation"
		req.Dst = top.AsRemote()
		requests[i] = req
		RegisterRequestMetadata(req.ID, GroupMember{
			InstructionID: instructionID,
			RequestID:     req.ID,
			Position:      uint16(i),
			Count:         uint16(len(vpns)),
			VAddr:         req.VAddr,
			LaneMask:      uint64(1) << i,
		})
		top.Deliver(req)
	}
	return requests
}

func collectLATCForwards(
	comp *LATCComp,
	bottom messaging.Port,
	count int,
) []vmprotocol.TranslationReq {
	var requests []vmprotocol.TranslationReq
	for len(requests) < count {
		comp.Tick()
		for {
			message := bottom.RetrieveOutgoing()
			if message == nil {
				break
			}
			requests = append(requests, message.(vmprotocol.TranslationReq))
		}
	}
	return requests
}

func deliverTranslationResponse(
	bottom messaging.Port,
	req vmprotocol.TranslationReq,
) {
	rsp := vmprotocol.TranslationRsp{
		Page: vm.Page{
			PID:      req.PID,
			VAddr:    req.VAddr,
			PAddr:    req.VAddr + 0x1000_0000,
			PageSize: 4096,
			Valid:    true,
		},
	}
	rsp.ID = timing.GetIDGenerator().Generate()
	rsp.Src = "L1TLB.Top"
	rsp.Dst = bottom.AsRemote()
	rsp.RspTo = req.ID
	bottom.Deliver(rsp)
}

func TestLATCCompressesRegularGroupAndReplaysEveryWaiter(t *testing.T) {
	comp, ports := makeLATCTestComponent(t, 2)
	requests := deliverLATCGroup(t, ports[LATCTopPortName], 7, 10, 11, 12)
	forwards := collectLATCForwards(comp, ports[LATCBottomPortName], 3)

	stats := comp.Stats()
	if stats.CompressedAllocations != 1 || stats.MSHRAllocations != 1 ||
		stats.RepresentedTranslations != 3 || stats.PeakMSHROccupancy != 1 {
		t.Fatalf("unexpected compression stats: %+v", stats)
	}

	for i := len(forwards) - 1; i >= 0; i-- {
		deliverTranslationResponse(ports[LATCBottomPortName], forwards[i])
		comp.Tick()
	}
	seen := make(map[uint64]bool)
	for {
		message := ports[LATCTopPortName].RetrieveOutgoing()
		if message == nil {
			break
		}
		seen[message.Meta().RspTo] = true
	}
	for _, req := range requests {
		if !seen[req.ID] {
			t.Fatalf("waiter %d was not replayed", req.ID)
		}
	}
	stats = comp.Stats()
	if stats.CompletedGroups != 1 || stats.CompletedMembers != 3 {
		t.Fatalf("completion statistics: %+v", stats)
	}
	if !comp.IsDrained() {
		t.Fatal("completed LATC retained live state")
	}
	if err := comp.ValidateInvariants(); err != nil {
		t.Fatal(err)
	}
}

func TestLATCHandlesDuplicateVPNAndPartialCompletion(t *testing.T) {
	comp, ports := makeLATCTestComponent(t, 1)
	deliverLATCGroup(t, ports[LATCTopPortName], 8, 20, 20, 20)
	forwards := collectLATCForwards(comp, ports[LATCBottomPortName], 3)
	if comp.Stats().MSHRAllocations != 1 ||
		comp.Stats().RepresentedTranslations != 1 {
		t.Fatalf("same VPN did not merge: %+v", comp.Stats())
	}
	deliverTranslationResponse(ports[LATCBottomPortName], forwards[1])
	comp.Tick()
	if len(comp.middleware.reservations) != 1 {
		t.Fatal("partial completion released grouped entry")
	}
	deliverTranslationResponse(ports[LATCBottomPortName], forwards[0])
	comp.Tick()
	deliverTranslationResponse(ports[LATCBottomPortName], forwards[2])
	comp.Tick()
	if len(comp.middleware.reservations) != 0 {
		t.Fatal("final completion did not release grouped entry")
	}
	if err := comp.ValidateInvariants(); err != nil {
		t.Fatal(err)
	}
}

func TestLATCReplaysPreFlushAndRestartedRequests(t *testing.T) {
	comp, ports := makeLATCTestComponent(t, 2)
	makeRequest := func(position uint16) vmprotocol.TranslationReq {
		req := vmprotocol.TranslationReq{PID: 1, VAddr: uint64(position+20) * 4096}
		req.ID = timing.GetIDGenerator().Generate()
		req.Src = "AddressTranslator.Translation"
		req.Dst = ports[LATCTopPortName].AsRemote()
		RegisterRequestMetadata(req.ID, GroupMember{
			InstructionID: 9,
			RequestID:     req.ID,
			Position:      position,
			Count:         2,
			VAddr:         req.VAddr,
		})
		return req
	}

	preFlush := makeRequest(0)
	restarted := makeRequest(0)
	sibling := makeRequest(1)
	ports[LATCTopPortName].Deliver(preFlush)
	ports[LATCTopPortName].Deliver(restarted)
	ports[LATCTopPortName].Deliver(sibling)

	forwards := collectLATCForwards(comp, ports[LATCBottomPortName], 3)
	for _, forward := range forwards {
		deliverTranslationResponse(ports[LATCBottomPortName], forward)
		comp.Tick()
	}

	seen := make(map[uint64]bool)
	for {
		message := ports[LATCTopPortName].RetrieveOutgoing()
		if message == nil {
			break
		}
		seen[message.Meta().RspTo] = true
	}
	for _, req := range []vmprotocol.TranslationReq{preFlush, restarted, sibling} {
		if !seen[req.ID] {
			t.Fatalf("request %d was not replayed", req.ID)
		}
	}
	if !comp.IsDrained() {
		t.Fatal("LATC retained a pre-flush or restarted waiter")
	}
}

func TestLATCReservationBackpressureAndReset(t *testing.T) {
	comp, ports := makeLATCTestComponent(t, 1)
	deliverLATCGroup(t, ports[LATCTopPortName], 1, 10)
	deliverLATCGroup(t, ports[LATCTopPortName], 2, 20)
	first := collectLATCForwards(comp, ports[LATCBottomPortName], 1)
	comp.Tick()
	if comp.Stats().ReservationFailures != 1 ||
		len(comp.middleware.ready) != 1 {
		t.Fatalf("reservation backpressure missing: %+v", comp.Stats())
	}

	deliverTranslationResponse(ports[LATCBottomPortName], first[0])
	comp.Tick()
	second := collectLATCForwards(comp, ports[LATCBottomPortName], 1)
	if len(second) != 1 {
		t.Fatal("blocked instruction did not resume")
	}

	reset := memcontrolprotocol.Req{Command: memcontrolprotocol.CmdReset}
	reset.ID = timing.GetIDGenerator().Generate()
	reset.Src = "CommandProcessor"
	reset.Dst = ports[LATCControlPortName].AsRemote()
	ports[LATCControlPortName].Deliver(reset)
	comp.Tick()
	if len(comp.middleware.reservations) != 0 ||
		len(comp.middleware.requests) != 0 ||
		len(comp.middleware.pendingForward) != 0 {
		t.Fatal("reset leaked LATC state")
	}
	if err := comp.ValidateInvariants(); err != nil {
		t.Fatal(err)
	}
	rsp := ports[LATCControlPortName].RetrieveOutgoing().(memcontrolprotocol.Rsp)
	if !rsp.Success || rsp.Command != memcontrolprotocol.CmdReset {
		t.Fatalf("reset response: %+v", rsp)
	}
}
