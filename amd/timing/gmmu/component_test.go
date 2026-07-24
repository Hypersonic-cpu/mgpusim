package gmmu

import (
	"testing"

	"github.com/sarchlab/akita/v5/hooking"
	"github.com/sarchlab/akita/v5/mem/memcontrolprotocol"
	"github.com/sarchlab/akita/v5/mem/memprotocol"
	"github.com/sarchlab/akita/v5/mem/vm/vmprotocol"
	"github.com/sarchlab/akita/v5/messaging"
	"github.com/sarchlab/akita/v5/modeling"
	"github.com/sarchlab/akita/v5/timing"
	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

type componentTestConnection struct {
	hooking.HookableBase
}

func (c *componentTestConnection) Name() string {
	return "gmmu-test-connection"
}

func (c *componentTestConnection) PlugIn(port messaging.Port) {
	port.SetConnection(c)
}

func (c *componentTestConnection) Unplug(messaging.Port) {}

func (c *componentTestConnection) NotifyAvailable(messaging.Port) {}

func (c *componentTestConnection) NotifySend() {}

func makeComponent(t *testing.T) (*Comp, map[string]messaging.Port) {
	t.Helper()
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	registrar := modeling.NewStandaloneRegistrar(timing.NewSerialEngine())
	spec := Spec{
		Freq:         1 * timing.GHz,
		MemoryModule: "L2.Top",
		FaultModule:  "FaultSink.Top",
	}
	comp := MakeBuilder().
		WithRegistrar(registrar).
		WithSpec(spec).
		WithResources(Resources{PageTable: table, WalkerConfig: core.Config}).
		Build("GMMU")

	ports := make(map[string]messaging.Port)
	for _, name := range []string{
		TopPortName, MemoryPortName, ControlPortName, FaultPortName,
	} {
		port := messaging.NewPort(comp, 8, 8, comp.Name()+"."+name)
		comp.AssignPort(name, port)
		(&componentTestConnection{}).PlugIn(port)
		ports[name] = port
	}
	return comp, ports
}

func TestComponentRoutesEveryPTEReadThroughMemoryPort(t *testing.T) {
	comp, ports := makeComponent(t)
	mapPage(comp.Resources().PageTable, 1, 0x4000, 0x2000_0000)
	top := ports[TopPortName]
	memory := ports[MemoryPortName]

	req := vmprotocol.TranslationReq{
		VAddr:    0x4123,
		PID:      1,
		DeviceID: 1,
	}
	req.ID = 100
	req.Src = "L2TLB.Bottom"
	req.Dst = top.AsRemote()
	top.Deliver(req)
	if !comp.Tick() {
		t.Fatal("request admission made no progress")
	}

	readCount := 0
	for {
		comp.Tick()
		outgoing := memory.RetrieveOutgoing()
		if outgoing == nil {
			if response := top.RetrieveOutgoing(); response != nil {
				rsp := response.(vmprotocol.TranslationRsp)
				if rsp.RspTo != req.ID ||
					rsp.Page.PAddr != 0x2000_0000 {
					t.Fatalf("translation response: %+v", rsp)
				}
				break
			}
			continue
		}

		read := outgoing.(memprotocol.ReadReq)
		readCount++
		if read.TrafficClass != PTETrafficClass ||
			read.AccessByteSize != 8 ||
			read.Dst != messaging.RemotePort("L2.Top") {
			t.Fatalf("memory read: %+v", read)
		}
		data, err := comp.Resources().PageTable.Storage.Read(
			read.Address, read.AccessByteSize)
		if err != nil {
			t.Fatalf("storage read: %v", err)
		}
		rsp := memprotocol.DataReadyRsp{Data: data}
		rsp.ID = uint64(200 + readCount)
		rsp.Src = "L2.Top"
		rsp.Dst = memory.AsRemote()
		rsp.RspTo = read.ID
		memory.Deliver(rsp)
	}

	if readCount != 4 {
		t.Fatalf("memory reads: got %d, want 4", readCount)
	}
}

func TestComponentHonorsTranslationResponseBackpressure(t *testing.T) {
	comp, ports := makeComponent(t)
	mapPage(comp.Resources().PageTable, 1, 0x4000, 0x2000_0000)
	top := ports[TopPortName]
	memory := ports[MemoryPortName]

	for top.CanSend() {
		filler := vmprotocol.TranslationRsp{}
		filler.ID = timing.GetIDGenerator().Generate()
		filler.Src = top.AsRemote()
		filler.Dst = "Blocked"
		top.Send(filler)
	}
	req := vmprotocol.TranslationReq{VAddr: 0x4000, PID: 1}
	req.ID = 100
	req.Src = "L2TLB.Bottom"
	req.Dst = top.AsRemote()
	top.Deliver(req)

	for level := 0; level < 4; level++ {
		comp.Tick()
		comp.Tick()
		read := memory.RetrieveOutgoing().(memprotocol.ReadReq)
		data, err := comp.Resources().PageTable.Storage.Read(read.Address, 8)
		if err != nil {
			t.Fatalf("storage read: %v", err)
		}
		rsp := memprotocol.DataReadyRsp{Data: data}
		rsp.ID = uint64(200 + level)
		rsp.Src = "L2.Top"
		rsp.Dst = memory.AsRemote()
		rsp.RspTo = read.ID
		memory.Deliver(rsp)
	}
	comp.Tick()
	if len(comp.Core.DrainTranslations()) != 0 {
		t.Fatal("component did not retain completion")
	}

	for top.RetrieveOutgoing() != nil {
	}
	if !comp.Tick() {
		t.Fatal("unblocked response made no progress")
	}
	response := top.RetrieveOutgoing()
	if response == nil || response.Meta().RspTo != req.ID {
		t.Fatalf("response after unblock: %+v", response)
	}
}

func TestComponentControlResetDropsStaleMemoryResponses(t *testing.T) {
	comp, ports := makeComponent(t)
	mapPage(comp.Resources().PageTable, 1, 0x4000, 0x2000_0000)
	top := ports[TopPortName]
	memory := ports[MemoryPortName]
	control := ports[ControlPortName]

	req := vmprotocol.TranslationReq{VAddr: 0x4000, PID: 1}
	req.ID = 100
	req.Src = "L2TLB.Bottom"
	req.Dst = top.AsRemote()
	top.Deliver(req)
	comp.Tick()
	comp.Tick()
	read := memory.RetrieveOutgoing().(memprotocol.ReadReq)

	reset := memcontrolprotocol.Req{Command: memcontrolprotocol.CmdReset}
	reset.ID = 300
	reset.Src = "Driver.Control"
	reset.Dst = control.AsRemote()
	control.Deliver(reset)
	if !comp.Tick() {
		t.Fatal("reset made no progress")
	}
	response := control.RetrieveOutgoing().(memcontrolprotocol.Rsp)
	if !response.Success || response.Command != memcontrolprotocol.CmdReset {
		t.Fatalf("reset response: %+v", response)
	}

	stale := memprotocol.DataReadyRsp{Data: make([]byte, 8)}
	stale.ID = 301
	stale.Src = "L2.Top"
	stale.Dst = memory.AsRemote()
	stale.RspTo = read.ID
	memory.Deliver(stale)
	if !comp.Tick() {
		t.Fatal("stale response was not consumed")
	}
	if !comp.Core.IsDrained() {
		t.Fatal("reset core retained work")
	}
}
