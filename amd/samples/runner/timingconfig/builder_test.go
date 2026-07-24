package timingconfig

import (
	"testing"

	"github.com/sarchlab/akita/v5/mem/cache/writethroughcache"
	"github.com/sarchlab/akita/v5/mem/vm/addresstranslator"
	"github.com/sarchlab/akita/v5/mem/vm/tlb"
	"github.com/sarchlab/akita/v5/simulation"
	"github.com/sarchlab/mgpusim/v5/amd/timing/cp"
	"github.com/sarchlab/mgpusim/v5/amd/timing/gmmu"
	"github.com/sarchlab/mgpusim/v5/amd/timing/latpc"
	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

// buildPlatform assembles a platform of the given GPU type and count. The
// assembly exercises the port naming, the port assignment, and the mapper
// snapshotting of all the components, all of which panic on error.
func buildPlatform(t *testing.T, gpuType string, numGPUs int) {
	t.Helper()

	s := simulation.MakeBuilder().
		WithoutMonitoring().
		WithOutputFileName(t.TempDir() + "/sim").
		Build()
	defer s.Terminate()

	gpuDriver := MakeBuilder().
		WithSimulation(s).
		WithNumGPUs(numGPUs).
		WithGPUType(gpuType).
		Build()

	if gpuDriver == nil {
		t.Fatal("driver must not be nil")
	}

	if len(gpuDriver.GPUs) != numGPUs {
		t.Fatalf("expected %d GPUs, got %d", numGPUs, len(gpuDriver.GPUs))
	}

	if s.GetComponentByName("Driver") == nil {
		t.Fatal("driver must be registered with the simulation")
	}

	cpName := "GPU[1].CommandProcessor"
	if s.GetComponentByName(cpName) == nil {
		t.Fatalf("component %s must be registered", cpName)
	}
}

func TestBuildR9NanoPlatform(t *testing.T) {
	buildPlatform(t, "r9nano", 1)
}

func TestBuildR9NanoMultiGPUPlatform(t *testing.T) {
	buildPlatform(t, "r9nano", 2)
}

func TestBuildMI300XPlatform(t *testing.T) {
	buildPlatform(t, "mi300x", 1)
}

func TestTranslationSelectionReachesBuiltResources(t *testing.T) {
	s := simulation.MakeBuilder().
		WithoutMonitoring().
		WithOutputFileName(t.TempDir() + "/sim").
		Build()
	defer s.Terminate()

	MakeBuilder().
		WithSimulation(s).
		WithNumGPUs(1).
		WithGPUType("mi300x").
		WithTranslationSelection("latpc", "large-resource").
		Build()

	gmmuComp, ok := gmmu.Lookup("GMMU")
	if !ok {
		t.Fatal("GMMU wrapper not registered")
	}
	if gmmuComp.Core.Config.TranslationMode != latpc.ModeLATPC ||
		gmmuComp.Core.Config.PWQEntries != 256 ||
		gmmuComp.Core.Config.NumWalkers != 32 ||
		gmmuComp.Core.Config.PWCEntries != 32 {
		t.Fatalf("unexpected GMMU config: %+v", gmmuComp.Core.Config)
	}

	l1Name := "GPU[1].SA[0].L1VTLB[0]"
	l1, ok := s.GetComponentByName(l1Name).(*tlb.Comp)
	if !ok {
		t.Fatalf("component %s has unexpected type", l1Name)
	}
	if l1.Spec().NumSets*l1.Spec().NumWays != 64 ||
		l1.Spec().MSHRSize != 16*64 || l1.Spec().Log2PageSize != 12 {
		t.Fatalf("unexpected L1 TLB spec: %+v", l1.Spec())
	}
	latcComponents := latpc.LATCComponents()
	if len(latcComponents) == 0 ||
		latcComponents[0].Spec().MSHRSize != 16 {
		t.Fatalf("LATC does not own the logical 16-entry capacity")
	}

	l2Name := "GPU[1].L2TLB"
	l2, ok := s.GetComponentByName(l2Name).(*tlb.Comp)
	if !ok {
		t.Fatalf("component %s has unexpected type", l2Name)
	}
	if l2.Spec().NumSets*l2.Spec().NumWays != 4096 ||
		l2.Spec().MSHRSize != 128 || l2.Spec().Log2PageSize != 12 {
		t.Fatalf("unexpected L2 TLB spec: %+v", l2.Spec())
	}
}

//nolint:funlen // One integration test checks the complete timing wiring invariant.
func TestDetailedTranslationUsesRadixAndPhysicalL1I(t *testing.T) {
	s := simulation.MakeBuilder().
		WithoutMonitoring().
		WithOutputFileName(t.TempDir() + "/sim").
		Build()
	defer s.Terminate()

	gpuDriver := MakeBuilder().
		WithSimulation(s).
		WithNumGPUs(1).
		WithGPUType("mi300x").
		Build()

	table, ok := gpuDriver.Resources().PageTable.(*mgpuvm.RadixPageTable)
	if !ok {
		t.Fatalf("page table has type %T, want *vm.RadixPageTable",
			gpuDriver.Resources().PageTable)
	}
	if table.Format != mgpuvm.X86FourLevel4KFormat() {
		t.Fatalf("unexpected page-table format: %+v", table.Format)
	}
	if gpuDriver.Spec().Log2PageSize != 12 {
		t.Fatalf("page size: got log2=%d, want 12", gpuDriver.Spec().Log2PageSize)
	}
	if s.GetComponentByName("GMMU") == nil {
		t.Fatal("detailed GMMU must be registered")
	}
	cpComp, ok := s.GetComponentByName("GPU[1].CommandProcessor").(*cp.Comp)
	if !ok {
		t.Fatal("command processor has unexpected type")
	}
	gmmuControlRegistered := false
	for _, controlPort := range cpComp.State.TLBs {
		if controlPort == "GMMU.Control" {
			gmmuControlRegistered = true
			break
		}
	}
	if !gmmuControlRegistered {
		t.Fatal("GMMU control must participate in TLB shootdowns")
	}

	atName := "GPU[1].SA[0].L1IAddrTrans"
	at, ok := s.GetComponentByName(atName).(*addresstranslator.Comp)
	if !ok {
		t.Fatalf("component %s has unexpected type", atName)
	}
	if got := at.Resources().MemProviderMapper.Find(0x1_0000_0000); got !=
		"GPU[1].SA[0].L1ICache.Top" {
		t.Fatalf("L1I translator destination: got %s", got)
	}

	cacheName := "GPU[1].SA[0].L1ICache"
	cache, ok := s.GetComponentByName(cacheName).(*writethroughcache.Comp)
	if !ok {
		t.Fatalf("component %s has unexpected type", cacheName)
	}
	cacheSpec := cache.Spec()
	if cacheSpec.AddressMapperType != "interleaved" ||
		len(cacheSpec.RemotePortNames) != 16 ||
		cacheSpec.RemotePortNames[0] != "GPU[1].L2Cache[0].Top" {
		t.Fatalf("physical L1I mapper: %+v", cacheSpec)
	}
}
