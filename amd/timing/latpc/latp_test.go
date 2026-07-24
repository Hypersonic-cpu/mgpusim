package latpc

import (
	"testing"

	"github.com/sarchlab/akita/v5/mem"
	"github.com/sarchlab/akita/v5/mem/memcontrolprotocol"
	"github.com/sarchlab/akita/v5/mem/vm"
	"github.com/sarchlab/akita/v5/mem/vm/vmprotocol"
	"github.com/sarchlab/akita/v5/messaging"
	"github.com/sarchlab/akita/v5/modeling"
	"github.com/sarchlab/akita/v5/timing"
)

func makeLATPTestComponent(
	t *testing.T,
	entries int,
) (*LATPComp, map[string]messaging.Port) {
	t.Helper()
	ResetRuntimeMetadata()
	registrar := modeling.NewStandaloneRegistrar(timing.NewSerialEngine())
	spec := LATPSpec{
		Freq:           1 * timing.GHz,
		Entries:        entries,
		NumReqPerCycle: 8,
		BatchWindow:    2,
		NumUpperLevels: 3,
	}
	comp := MakeLATPBuilder().
		WithRegistrar(registrar).
		WithSpec(spec).
		WithResources(LATPResources{
			TranslationProviderMapper: &mem.SinglePortMapper{Port: "GMMU.Top"},
		}).
		Build("LATP")
	ports := make(map[string]messaging.Port)
	for _, name := range []string{
		LATPTopPortName, LATPBottomPortName, LATPControlPortName,
	} {
		port := messaging.NewPort(comp, 16, 16, comp.Name()+"."+name)
		comp.AssignPort(name, port)
		(&latcTestConnection{}).PlugIn(port)
		ports[name] = port
	}
	return comp, ports
}

func deliverLATPGroup(
	top messaging.Port,
	instructionID uint64,
	vpns ...uint64,
) []vmprotocol.TranslationReq {
	requests := make([]vmprotocol.TranslationReq, len(vpns))
	for i, vpn := range vpns {
		req := vmprotocol.TranslationReq{PID: 1, VAddr: vpn * 4096}
		req.ID = timing.GetIDGenerator().Generate()
		req.Src = "L2TLB.Bottom"
		req.Dst = top.AsRemote()
		requests[i] = req
		RegisterRequestMetadata(req.ID, GroupMember{
			InstructionID: instructionID,
			RequestID:     req.ID,
			Position:      uint16(i),
			Count:         uint16(len(vpns)),
			VAddr:         req.VAddr,
			Regular:       true,
			GroupIndex:    0,
			GroupPosition: uint16(i),
			GroupCount:    uint16(len(vpns)),
			BaseVPN:       vpns[0],
			Stride:        1,
			LeafPage:      vpns[0] >> 9,
		})
		top.Deliver(req)
	}
	return requests
}

func collectLATPForwards(
	comp *LATPComp,
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

func deliverLATPResponse(
	bottom messaging.Port,
	req vmprotocol.TranslationReq,
) {
	rsp := vmprotocol.TranslationRsp{
		Page: vm.Page{
			PID:      req.PID,
			VAddr:    req.VAddr,
			PAddr:    req.VAddr + 0x2000_0000,
			PageSize: 4096,
			Valid:    true,
		},
	}
	rsp.ID = timing.GetIDGenerator().Generate()
	rsp.Src = "GMMU.Top"
	rsp.Dst = bottom.AsRemote()
	rsp.RspTo = req.ID
	bottom.Deliver(rsp)
}

func TestLATPFormsBatchAndPreservesOutOfOrderResponses(t *testing.T) {
	comp, ports := makeLATPTestComponent(t, 8)
	requests := deliverLATPGroup(ports[LATPTopPortName], 5, 10, 11, 12)
	forwards := collectLATPForwards(comp, ports[LATPBottomPortName], 3)

	stats := comp.Stats()
	if stats.Groups != 1 || stats.Members != 3 ||
		stats.IndependentWalksAvoided != 2 || stats.UpperReadsAvoided != 6 ||
		stats.LatePrefetches != 2 || stats.PrefetchAccuracy() != 1 {
		t.Fatalf("group statistics: %+v", stats)
	}
	for _, request := range forwards {
		member, ok := RequestMetadata(request.ID)
		if !ok || member.LATPBatchID == 0 || member.LATPBatchCount != 3 {
			t.Fatalf("batch metadata missing: %+v, %t", member, ok)
		}
	}

	for i := len(forwards) - 1; i >= 0; i-- {
		deliverLATPResponse(ports[LATPBottomPortName], forwards[i])
		comp.Tick()
	}
	seen := make(map[uint64]bool)
	for {
		message := ports[LATPTopPortName].RetrieveOutgoing()
		if message == nil {
			break
		}
		seen[message.Meta().RspTo] = true
	}
	for _, request := range requests {
		if !seen[request.ID] {
			t.Fatalf("response for %d lost", request.ID)
		}
	}
	if !comp.IsDrained() {
		t.Fatal("completed LATP retained live state")
	}
	if err := comp.ValidateInvariants(); err != nil {
		t.Fatal(err)
	}
}

func TestLATPWindowClosesPartialMissGroup(t *testing.T) {
	comp, ports := makeLATPTestComponent(t, 8)
	requests := deliverLATPGroup(ports[LATPTopPortName], 6, 20, 21, 22)
	// Model two L2 hits by removing their miss requests before LATP receives
	// them. Only the first request reaches the grouped walk buffer.
	ports[LATPTopPortName].RetrieveIncoming()
	ports[LATPTopPortName].RetrieveIncoming()
	forwards := collectLATPForwards(comp, ports[LATPBottomPortName], 1)
	if forwards[0].ID != requests[2].ID {
		t.Fatalf("unexpected surviving miss: %+v", forwards)
	}
	if comp.Stats().Groups != 0 {
		t.Fatal("a one-member partial group must use the baseline walk")
	}
}

func TestLATPBackpressureAndReset(t *testing.T) {
	comp, ports := makeLATPTestComponent(t, 1)
	deliverLATPGroup(ports[LATPTopPortName], 7, 30, 31, 32)
	comp.Tick()
	if comp.Stats().PWBufferFullStalls == 0 {
		t.Fatal("finite PW buffer did not apply backpressure")
	}

	reset := memcontrolprotocol.Req{Command: memcontrolprotocol.CmdReset}
	reset.ID = timing.GetIDGenerator().Generate()
	reset.Src = "CommandProcessor"
	reset.Dst = ports[LATPControlPortName].AsRemote()
	ports[LATPControlPortName].Deliver(reset)
	comp.Tick()
	if !comp.middleware.isIdle() {
		t.Fatal("reset leaked LATP state")
	}
	if err := comp.ValidateInvariants(); err != nil {
		t.Fatal(err)
	}
	rsp := ports[LATPControlPortName].RetrieveOutgoing().(memcontrolprotocol.Rsp)
	if !rsp.Success || rsp.Command != memcontrolprotocol.CmdReset {
		t.Fatalf("reset response: %+v", rsp)
	}
}
