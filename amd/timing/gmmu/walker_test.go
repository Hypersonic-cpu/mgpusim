package gmmu

import (
	"errors"
	"strings"
	"testing"

	"github.com/sarchlab/akita/v5/mem"
	akitavm "github.com/sarchlab/akita/v5/mem/vm"
	"github.com/sarchlab/akita/v5/timing"
	"github.com/sarchlab/mgpusim/v5/amd/timing/latpc"
	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

func makeCore(
	t *testing.T,
	format mgpuvm.PageTableFormat,
	configure func(*Config),
) (*Core, *mgpuvm.RadixPageTable) {
	t.Helper()
	storage := mem.NewStorageWithUnitSize(0x4000_0000, 4096)
	tableAllocator := mgpuvm.NewLinearTablePageAllocator(0x10_0000, 0x1000_0000)
	table, err := mgpuvm.NewRadixPageTable(
		format,
		tableAllocator,
		storage,
		akitavm.NewPageTable(uint64(format.PageOffsetBits)),
	)
	if err != nil {
		t.Fatalf("new radix table: %v", err)
	}
	config := DefaultConfig()
	config.Format = format
	if configure != nil {
		configure(&config)
	}
	core, err := NewCore(config, table)
	if err != nil {
		t.Fatalf("new core: %v", err)
	}
	return core, table
}

func mapPage(table *mgpuvm.RadixPageTable, pid akitavm.PID, vAddr, pAddr uint64) {
	table.Insert(akitavm.Page{
		PID:      pid,
		VAddr:    vAddr,
		PAddr:    pAddr,
		PageSize: 4096,
		Valid:    true,
		DeviceID: 1,
	})
}

func completeReads(t *testing.T, core *Core, reads []MemoryRead) {
	t.Helper()
	for _, read := range reads {
		data, err := core.Table.Storage.Read(read.PAddr, read.ByteSize)
		if err != nil {
			t.Fatalf("storage read: %v", err)
		}
		if err := core.CompleteMemoryRead(read.ID, data); err != nil {
			t.Fatalf("complete read %d: %v", read.ID, err)
		}
	}
}

func finishCore(t *testing.T, core *Core) {
	t.Helper()
	for {
		reads := core.DrainMemoryReads()
		if len(reads) == 0 {
			if core.IsDrained() {
				return
			}
			t.Fatal("core has work but no memory request")
		}
		completeReads(t, core, reads)
	}
}

func TestColdFourLevelWalkReadsEveryLevel(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	mapPage(table, 1, 0x4000, 0x2000_0000)

	if err := core.Submit(WalkRequest{
		ID: 1, PID: 1, VAddr: 0x4123, DeviceID: 1,
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	finishCore(t, core)

	translations := core.DrainTranslations()
	if len(translations) != 1 {
		t.Fatalf("translations: got %d, want 1", len(translations))
	}
	if translations[0].Reads != 4 ||
		translations[0].Page.PAddr != 0x2000_0000 {
		t.Fatalf("translation: %+v", translations[0])
	}
	if core.Stats.MemoryReads != 4 {
		t.Fatalf("memory reads: got %d, want 4", core.Stats.MemoryReads)
	}
}

func TestRepeatedPrefixWalkReadsOnlyLeaf(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	mapPage(table, 1, 0x4000, 0x2000_0000)
	mapPage(table, 1, 0x5000, 0x3000_0000)

	if err := core.Submit(WalkRequest{ID: 1, PID: 1, VAddr: 0x4000}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	finishCore(t, core)
	core.DrainTranslations()
	readsBefore := core.Stats.MemoryReads

	if err := core.Submit(WalkRequest{ID: 2, PID: 1, VAddr: 0x5000}); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	finishCore(t, core)
	translations := core.DrainTranslations()

	if got := core.Stats.MemoryReads - readsBefore; got != 1 {
		t.Fatalf("second-walk reads: got %d, want 1", got)
	}
	if len(translations) != 1 ||
		translations[0].Page.PAddr != 0x3000_0000 {
		t.Fatalf("translation: %+v", translations)
	}
	for level := 0; level < 3; level++ {
		if core.Stats.PWCHits[level] != 1 {
			t.Fatalf("PWC %d hits: got %d, want 1", level, core.Stats.PWCHits[level])
		}
	}
}

func TestLastIntermediatePWCMissRequiresTwoReads(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	mapPage(table, 1, 0x4000, 0x2000_0000)
	mapPage(table, 1, 0x5000, 0x3000_0000)
	if err := core.Submit(WalkRequest{ID: 1, PID: 1, VAddr: 0x4000}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	finishCore(t, core)
	core.DrainTranslations()
	core.pwcs[2].reset()
	readsBefore := core.Stats.MemoryReads

	if err := core.Submit(WalkRequest{ID: 2, PID: 1, VAddr: 0x5000}); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	finishCore(t, core)

	if got := core.Stats.MemoryReads - readsBefore; got != 2 {
		t.Fatalf("second-walk reads: got %d, want 2", got)
	}
	if core.PWCOccupancy(2) != 1 {
		t.Fatalf("last PWC occupancy: got %d, want 1", core.PWCOccupancy(2))
	}
}

func TestPageWalkCacheUsesDeterministicLRU(t *testing.T) {
	cache := newPageWalkCache(2)
	first := pwcKey{PID: 1, Prefix: 1}
	second := pwcKey{PID: 1, Prefix: 2}
	third := pwcKey{PID: 1, Prefix: 3}
	cache.insert(first, pwcValue{ChildTablePAddr: 0x1000})
	cache.insert(second, pwcValue{ChildTablePAddr: 0x2000})
	if _, ok := cache.lookup(first); !ok {
		t.Fatal("first entry missing")
	}
	cache.insert(third, pwcValue{ChildTablePAddr: 0x3000})

	if _, ok := cache.lookup(second); ok {
		t.Fatal("least-recently-used entry was not evicted")
	}
	if _, ok := cache.lookup(first); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if _, ok := cache.lookup(third); !ok {
		t.Fatal("new entry missing")
	}
}

func TestPWQAndWalkerPressureAndFCFSDispatch(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	mapPage(table, 1, 0x4000, 0x2000_0000)

	for id := uint64(1); id <= DefaultNumWalkers+DefaultPWQEntries; id++ {
		if err := core.Submit(WalkRequest{ID: id, PID: 1, VAddr: 0x4000}); err != nil {
			t.Fatalf("submit %d: %v", id, err)
		}
	}
	if err := core.Submit(WalkRequest{
		ID: 999, PID: 1, VAddr: 0x4000,
	}); !errors.Is(err, ErrPWQFull) {
		t.Fatalf("overflow submit: got %v", err)
	}
	if core.Stats.PeakWalkersBusy != DefaultNumWalkers ||
		core.Stats.PeakPWQOccupancy != DefaultPWQEntries {
		t.Fatalf("pressure stats: %+v", core.Stats)
	}

	for {
		reads := core.DrainMemoryReads()
		for _, read := range reads {
			if read.Walker != 0 {
				continue
			}
			completeReads(t, core, []MemoryRead{read})
			if core.Walkers[0].Req.ID == DefaultNumWalkers+1 {
				return
			}
		}
		if len(reads) == 0 {
			t.Fatal("walker 0 made no progress")
		}
	}
}

func TestFaultsAreStructured(t *testing.T) { //nolint:gocognit,funlen
	t.Run("missing root", func(t *testing.T) {
		core, _ := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
		if err := core.Submit(WalkRequest{ID: 1, PID: 99, VAddr: 0x4000}); err != nil {
			t.Fatalf("submit: %v", err)
		}
		faults := core.DrainFaults()
		if len(faults) != 1 ||
			faults[0].Reason != mgpuvm.ErrAddressSpaceNotFound.Error() {
			t.Fatalf("faults: %+v", faults)
		}
	})

	t.Run("non-present leaf", func(t *testing.T) {
		core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
		mapPage(table, 1, 0x4000, 0x2000_0000)
		if err := core.Submit(WalkRequest{ID: 1, PID: 1, VAddr: 0x5000}); err != nil {
			t.Fatalf("submit: %v", err)
		}
		finishCore(t, core)
		faults := core.DrainFaults()
		if len(faults) != 1 || faults[0].Reason != "non-present entry" {
			t.Fatalf("faults: %+v", faults)
		}
	})

	t.Run("permissions and malformed entry", func(t *testing.T) {
		tests := []struct {
			name   string
			access AccessType
			value  uint64
			reason string
		}{
			{
				name:   "write denied",
				access: AccessWrite,
				value:  0x2000_0000 | 1 | 4,
				reason: "permission denied",
			},
			{
				name:   "execute denied",
				access: AccessExecute,
				value:  0x2000_0000 | 1 | 2 | 4 | uint64(1)<<63,
				reason: "permission denied",
			},
			{
				name:   "malformed",
				access: AccessRead,
				value:  uint64(1) << 10,
				reason: mgpuvm.ErrMalformedPTE.Error(),
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
				mapPage(table, 1, 0x4000, 0x2000_0000)
				walk, err := table.Walk(1, 0x4000)
				if err != nil {
					t.Fatalf("locate leaf: %v", err)
				}
				if err := table.WriteRawEntry(
					walk.EntryPAddrs[3], test.value); err != nil {
					t.Fatalf("write leaf: %v", err)
				}
				if err := core.Submit(WalkRequest{
					ID: 1, PID: 1, VAddr: 0x4000, Access: test.access,
				}); err != nil {
					t.Fatalf("submit: %v", err)
				}
				finishCore(t, core)
				faults := core.DrainFaults()
				if len(faults) != 1 {
					t.Fatalf("faults: %+v", faults)
				}
				if test.name == "malformed" {
					if !strings.Contains(
						faults[0].Reason, mgpuvm.ErrMalformedPTE.Error()) {
						t.Fatalf("fault reason: %s", faults[0].Reason)
					}
				} else if faults[0].Reason != test.reason {
					t.Fatalf("fault reason: got %q, want %q", faults[0].Reason, test.reason)
				}
			})
		}
	})
}

func TestFourAndFiveLevelWalkersShareImplementation(t *testing.T) {
	formats := []mgpuvm.PageTableFormat{
		mgpuvm.X86FourLevel4KFormat(),
		mgpuvm.X86FiveLevel4KFormat(),
	}
	for _, format := range formats {
		core, table := makeCore(t, format, nil)
		mapPage(table, 1, 0x1234_5678_9000, 0x2000_0000)
		if err := core.Submit(WalkRequest{
			ID: 1, PID: 1, VAddr: 0x1234_5678_9abc,
		}); err != nil {
			t.Fatalf("%d-level submit: %v", format.NumLevels, err)
		}
		finishCore(t, core)
		translations := core.DrainTranslations()
		if len(translations) != 1 ||
			translations[0].Reads != uint64(format.NumLevels) {
			t.Fatalf("%d-level translations: %+v", format.NumLevels, translations)
		}
	}
}

func TestOutOfOrderResponsesAdvanceOwningWalkers(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	mapPage(table, 1, 0x4000, 0x2000_0000)
	mapPage(table, 2, 0x4000, 0x3000_0000)
	if err := core.Submit(WalkRequest{ID: 1, PID: 1, VAddr: 0x4000}); err != nil {
		t.Fatalf("submit PID 1: %v", err)
	}
	if err := core.Submit(WalkRequest{ID: 2, PID: 2, VAddr: 0x4000}); err != nil {
		t.Fatalf("submit PID 2: %v", err)
	}

	for !core.IsDrained() {
		reads := core.DrainMemoryReads()
		for index := len(reads) - 1; index >= 0; index-- {
			completeReads(t, core, []MemoryRead{reads[index]})
		}
	}
	translations := core.DrainTranslations()
	if len(translations) != 2 {
		t.Fatalf("translations: %+v", translations)
	}
	byID := map[uint64]uint64{}
	for _, translation := range translations {
		byID[translation.RequestID] = translation.Page.PAddr
	}
	if byID[1] != 0x2000_0000 || byID[2] != 0x3000_0000 {
		t.Fatalf("correlation: %+v", byID)
	}
}

func TestResetRejectsStaleResponsesAndInvalidatesPWCs(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	mapPage(table, 1, 0x4000, 0x2000_0000)
	if err := core.Submit(WalkRequest{ID: 1, PID: 1, VAddr: 0x4000}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	read := core.DrainMemoryReads()[0]
	core.Reset()

	if err := core.CompleteMemoryRead(read.ID, make([]byte, 8)); !errors.Is(
		err, ErrUnknownMemoryResponse) {
		t.Fatalf("stale response: got %v", err)
	}
	for level := uint8(0); level < core.Config.Format.NumLevels-1; level++ {
		if core.PWCOccupancy(level) != 0 {
			t.Fatalf("PWC %d not reset", level)
		}
	}
}

func TestStatisticsTrackExactColdWalkCounts(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	mapPage(table, 1, 0x4000, 0x2000_0000)

	if err := core.SubmitAt(
		WalkRequest{ID: 1, PID: 1, VAddr: 0x4000}, 100); err != nil {
		t.Fatalf("submit: %v", err)
	}
	for level := 0; level < 4; level++ {
		reads := core.DrainMemoryReads()
		if len(reads) != 1 {
			t.Fatalf("level %d reads: %+v", level, reads)
		}
		data, err := core.Table.Storage.Read(reads[0].PAddr, reads[0].ByteSize)
		if err != nil {
			t.Fatalf("read entry: %v", err)
		}
		if err := core.CompleteMemoryReadAt(
			reads[0].ID, data, timing.VTimeInPicoSec(110+10*level)); err != nil {
			t.Fatalf("complete level %d: %v", level, err)
		}
	}

	stats := core.Stats
	if stats.TranslationRequests != 1 || stats.WalksStarted != 1 ||
		stats.WalksCompleted != 1 || stats.MemoryReads != 4 ||
		stats.PTWBytes != 32 || stats.WalksByMemoryReads[4] != 1 {
		t.Fatalf("cold walk statistics: %+v", stats)
	}
	if stats.PWCMisses[0] != 1 || stats.PWCMisses[1] != 1 ||
		stats.PWCMisses[2] != 1 || stats.PWCFills[2] != 1 {
		t.Fatalf("PWC statistics: %+v", stats)
	}
	if stats.AverageWalkLatency() != 40 || stats.AveragePTWMemoryLatency() != 10 {
		t.Fatalf("latency statistics: %+v", stats)
	}
}

func TestStatisticsTrackPWQDelayAndPWCInvalidation(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), func(c *Config) {
		c.NumWalkers = 1
		c.PWQEntries = 1
	})
	mapPage(table, 1, 0x4000, 0x2000_0000)
	mapPage(table, 1, 0x5000, 0x3000_0000)

	if err := core.SubmitAt(WalkRequest{ID: 1, PID: 1, VAddr: 0x4000}, 0); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if err := core.SubmitAt(WalkRequest{ID: 2, PID: 1, VAddr: 0x5000}, 5); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if err := core.SubmitAt(WalkRequest{ID: 3, PID: 1, VAddr: 0x4000}, 6); !errors.Is(err, ErrPWQFull) {
		t.Fatalf("full queue: %v", err)
	}

	for level := 0; level < 4; level++ {
		reads := core.DrainMemoryReads()
		data, err := core.Table.Storage.Read(reads[0].PAddr, reads[0].ByteSize)
		if err != nil {
			t.Fatalf("read entry: %v", err)
		}
		if err := core.CompleteMemoryReadAt(
			reads[0].ID, data, timing.VTimeInPicoSec(10*(level+1))); err != nil {
			t.Fatalf("complete entry: %v", err)
		}
	}
	if core.Stats.PWQQueueDelay != 35 || core.Stats.PWQFullStalls != 1 {
		t.Fatalf("PWQ statistics: %+v", core.Stats)
	}

	core.InvalidatePID(1)
	for level := 0; level < 3; level++ {
		if core.Stats.PWCInvalidations[level] != 1 {
			t.Fatalf("PWC %d invalidations: %+v", level, core.Stats)
		}
	}
}

func TestPageTableWriteSnoopRepairsStaleCachedLeaf(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	mapPage(table, 1, 0x4000, 0x2000_0000)

	if err := core.Submit(WalkRequest{ID: 1, PID: 1, VAddr: 0x4000}); err != nil {
		t.Fatalf("warm submit: %v", err)
	}
	finishCore(t, core)
	core.DrainTranslations()

	firstWalk, err := table.Walk(1, 0x4000)
	if err != nil {
		t.Fatalf("locate leaf table: %v", err)
	}
	staleLeafPAddr, err := table.Format.EntryAddress(
		firstWalk.TablePAddrs[3], 3, 5)
	if err != nil {
		t.Fatalf("stale leaf address: %v", err)
	}
	staleData := make([]byte, table.Format.EntryBytes)

	mapPage(table, 1, 0x5000, 0x3000_0000)
	if err := core.Submit(WalkRequest{ID: 2, PID: 1, VAddr: 0x5000}); err != nil {
		t.Fatalf("updated submit: %v", err)
	}
	reads := core.DrainMemoryReads()
	if len(reads) != 1 || reads[0].PAddr != staleLeafPAddr {
		t.Fatalf("leaf read after PWC hits: %+v", reads)
	}
	if err := core.CompleteMemoryRead(reads[0].ID, staleData); err != nil {
		t.Fatalf("complete stale response: %v", err)
	}

	translations := core.DrainTranslations()
	if len(translations) != 1 ||
		translations[0].Page.PAddr != 0x3000_0000 {
		t.Fatalf("coherent translation: %+v", translations)
	}
	if core.Stats.PTECoherenceRepairs != 1 {
		t.Fatalf("coherence repairs: got %d, want 1",
			core.Stats.PTECoherenceRepairs)
	}
}

func TestIdealModeBypassesDetailedWalk(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), func(c *Config) {
		c.TranslationMode = latpc.ModeIdeal
	})
	mapPage(table, 1, 0x4000, 0x9000)

	if err := core.Submit(WalkRequest{ID: 7, PID: 1, VAddr: 0x4000}); err != nil {
		t.Fatal(err)
	}
	completed := core.DrainTranslations()
	if len(completed) != 1 || completed[0].Page.PAddr != 0x9000 {
		t.Fatalf("unexpected ideal translation: %+v", completed)
	}
	if len(core.DrainMemoryReads()) != 0 {
		t.Fatal("ideal translation must not issue page-table reads")
	}
	if core.Stats.WalksStarted != 0 || core.Stats.MemoryReads != 0 {
		t.Fatalf("ideal mode used detailed walker: %+v", core.Stats)
	}
}

func TestLATPBatchSharesUpperTraversal(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	for i := uint64(0); i < 3; i++ {
		mapPage(table, 1, (10+i)*4096, (100+i)*4096)
	}
	for i := uint64(0); i < 3; i++ {
		member := latpc.GroupMember{
			InstructionID:  1,
			RequestID:      i + 1,
			Regular:        true,
			GroupCount:     3,
			GroupPosition:  uint16(i),
			LATPBatchID:    9,
			LATPBatchCount: 3,
		}
		if err := core.Submit(WalkRequest{
			ID:       i + 1,
			PID:      1,
			VAddr:    (10 + i) * 4096,
			Group:    member,
			HasGroup: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	finishCore(t, core)

	translations := core.DrainTranslations()
	if len(translations) != 3 {
		t.Fatalf("translations: %+v", translations)
	}
	stats := core.Stats
	if stats.MemoryReads != 6 || stats.WalksStarted != 1 ||
		stats.WalksCompleted != 1 || stats.LATPGroups != 1 ||
		stats.LATPMembers != 3 || stats.IndependentWalksAvoided != 2 ||
		stats.UpperReadsAvoided != 6 || stats.LeafPTEReads != 3 {
		t.Fatalf("grouped-walk statistics: %+v", stats)
	}
}

func TestLATPLeafResponsesMayCompleteOutOfOrder(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	for i := uint64(0); i < 3; i++ {
		mapPage(table, 1, (20+i)*4096, (200+i)*4096)
		if err := core.Submit(WalkRequest{
			ID:    i + 1,
			PID:   1,
			VAddr: (20 + i) * 4096,
			Group: latpc.GroupMember{
				LATPBatchID:    4,
				LATPBatchCount: 3,
			},
			HasGroup: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for level := 0; level < 3; level++ {
		reads := core.DrainMemoryReads()
		if len(reads) != 1 {
			t.Fatalf("upper level %d reads: %+v", level, reads)
		}
		completeReads(t, core, reads)
	}
	leafReads := core.DrainMemoryReads()
	if len(leafReads) != 3 {
		t.Fatalf("leaf reads were not issued in parallel: %+v", leafReads)
	}
	for i := len(leafReads) - 1; i >= 0; i-- {
		completeReads(t, core, leafReads[i:i+1])
	}
	if translations := core.DrainTranslations(); len(translations) != 3 {
		t.Fatalf("out-of-order leaf completions: %+v", translations)
	}
	if !core.IsDrained() {
		t.Fatal("grouped walker did not drain")
	}
}

func latpWalkRequest(
	id, batchID uint64,
	position, count uint16,
	vpn uint64,
) WalkRequest {
	return WalkRequest{
		ID:    id,
		PID:   1,
		VAddr: vpn * 4096,
		Group: latpc.GroupMember{
			LATPBatchID:    batchID,
			LATPBatchCount: count,
			GroupPosition:  position,
		},
		HasGroup: true,
	}
}

func TestLATPBatchMergesWhileQueued(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), func(config *Config) {
		config.NumWalkers = 1
	})
	mapPage(table, 1, 1*4096, 101*4096)
	mapPage(table, 1, 40*4096, 140*4096)
	mapPage(table, 1, 41*4096, 141*4096)
	if err := core.Submit(WalkRequest{ID: 1, PID: 1, VAddr: 1 * 4096}); err != nil {
		t.Fatal(err)
	}
	if err := core.Submit(latpWalkRequest(2, 10, 0, 2, 40)); err != nil {
		t.Fatal(err)
	}
	if err := core.Submit(latpWalkRequest(3, 10, 1, 2, 41)); err != nil {
		t.Fatal(err)
	}
	if len(core.pwq) != 2 {
		t.Fatalf("queued batch members: got %d, want 2", len(core.pwq))
	}
	finishCore(t, core)
	if core.Stats.LATPGroups != 1 ||
		core.Stats.LATPMembers != 2 ||
		core.Stats.IndependentWalksAvoided != 1 {
		t.Fatalf("queued merge statistics: %+v", core.Stats)
	}
}

func TestLATPBatchMergesDuringUpperWalk(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	mapPage(table, 1, 50*4096, 150*4096)
	mapPage(table, 1, 51*4096, 151*4096)
	if err := core.Submit(latpWalkRequest(1, 11, 0, 2, 50)); err != nil {
		t.Fatal(err)
	}
	firstUpperRead := core.DrainMemoryReads()
	if len(firstUpperRead) != 1 {
		t.Fatalf("initial upper reads: %+v", firstUpperRead)
	}
	if err := core.Submit(latpWalkRequest(2, 11, 1, 2, 51)); err != nil {
		t.Fatal(err)
	}
	completeReads(t, core, firstUpperRead)
	finishCore(t, core)
	if core.Stats.MemoryReads != 5 ||
		core.Stats.WalksStarted != 1 ||
		core.Stats.LATPMembers != 2 ||
		core.Stats.UpperReadsAvoided != 3 {
		t.Fatalf("upper-phase merge statistics: %+v", core.Stats)
	}
}

func TestLATPBatchMergesDuringLeafWalk(t *testing.T) {
	core, table := makeCore(t, mgpuvm.X86FourLevel4KFormat(), nil)
	mapPage(table, 1, 60*4096, 160*4096)
	mapPage(table, 1, 61*4096, 161*4096)
	if err := core.Submit(latpWalkRequest(1, 12, 0, 2, 60)); err != nil {
		t.Fatal(err)
	}
	for level := 0; level < 3; level++ {
		reads := core.DrainMemoryReads()
		if len(reads) != 1 {
			t.Fatalf("upper level %d reads: %+v", level, reads)
		}
		completeReads(t, core, reads)
	}
	firstLeafRead := core.DrainMemoryReads()
	if len(firstLeafRead) != 1 {
		t.Fatalf("initial leaf reads: %+v", firstLeafRead)
	}
	if err := core.Submit(latpWalkRequest(2, 12, 1, 2, 61)); err != nil {
		t.Fatal(err)
	}
	secondLeafRead := core.DrainMemoryReads()
	if len(secondLeafRead) != 1 {
		t.Fatalf("late leaf reads: %+v", secondLeafRead)
	}
	completeReads(t, core, secondLeafRead)
	completeReads(t, core, firstLeafRead)
	translations := core.DrainTranslations()
	if len(translations) != 2 || !core.IsDrained() {
		t.Fatalf("late leaf completion: translations=%+v drained=%t",
			translations, core.IsDrained())
	}
	if core.Stats.MemoryReads != 5 ||
		core.Stats.WalksStarted != 1 ||
		core.Stats.LATPMembers != 2 ||
		core.Stats.IndependentWalksAvoided != 1 {
		t.Fatalf("leaf-phase merge statistics: %+v", core.Stats)
	}
}
