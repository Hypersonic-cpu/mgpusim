package vm

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/sarchlab/akita/v5/mem"
	akitavm "github.com/sarchlab/akita/v5/mem/vm"
)

const (
	testTableRegionBase = uint64(0x10_0000)
	testTableRegionSize = uint64(0x10_0000)
	testStorageSize     = uint64(0x1000_0000)
)

func makeRadixPageTable(
	t *testing.T,
	format PageTableFormat,
) (*RadixPageTable, *LinearTablePageAllocator) {
	t.Helper()
	storage := mem.NewStorageWithUnitSize(testStorageSize, 4096)
	allocator := NewLinearTablePageAllocator(
		testTableRegionBase, testTableRegionSize)
	shadow := akitavm.NewPageTable(uint64(format.PageOffsetBits))
	table, err := NewRadixPageTable(format, allocator, storage, shadow)
	if err != nil {
		t.Fatalf("new radix page table: %v", err)
	}
	return table, allocator
}

func testPage(pid akitavm.PID, vAddr, pAddr uint64) akitavm.Page {
	return akitavm.Page{
		PID:      pid,
		VAddr:    vAddr,
		PAddr:    pAddr,
		PageSize: 4096,
		Valid:    true,
		DeviceID: 1,
	}
}

func allocatedTablePages(allocator *LinearTablePageAllocator) uint64 {
	return (allocator.next - allocator.base) / 4096
}

func TestFirstMappingCreatesFullFourLevelHierarchy(t *testing.T) {
	table, allocator := makeRadixPageTable(t, X86FourLevel4KFormat())
	page := testPage(1, 0x1234_5000, 0x800_0000)
	table.Insert(page)

	if got := allocatedTablePages(allocator); got != 4 {
		t.Fatalf("allocated table pages: got %d, want 4", got)
	}
	result, err := table.Walk(page.PID, page.VAddr+0x321)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if result.PhysicalBase != page.PAddr ||
		result.PhysicalAddress != page.PAddr+0x321 {
		t.Fatalf("walk result: %+v", result)
	}
	for level := range int(table.Format.NumLevels) {
		if !result.Entries[level].Present {
			t.Fatalf("level %d is not present", level)
		}
	}

	root, ok := table.AddressSpace(page.PID)
	if !ok {
		t.Fatal("missing address space")
	}
	unused, err := table.Storage.Read(root.RootPAddr+8, 8)
	if err != nil {
		t.Fatalf("read zeroed root entry: %v", err)
	}
	if binary.LittleEndian.Uint64(unused) != 0 {
		t.Fatalf("unused root entry is not zero: %x", unused)
	}
}

func TestAdjacentMappingsReuseUpperTables(t *testing.T) {
	table, allocator := makeRadixPageTable(t, X86FourLevel4KFormat())
	table.Insert(testPage(1, 0x4000, 0x800_0000))
	table.Insert(testPage(1, 0x5000, 0x900_0000))

	if got := allocatedTablePages(allocator); got != 4 {
		t.Fatalf("allocated table pages: got %d, want 4", got)
	}
}

func TestMappingsAcrossBoundariesCreateExpectedTables(t *testing.T) {
	table, allocator := makeRadixPageTable(t, X86FourLevel4KFormat())
	table.Insert(testPage(1, 0, 0x800_0000))
	if got := allocatedTablePages(allocator); got != 4 {
		t.Fatalf("initial table pages: got %d, want 4", got)
	}

	table.Insert(testPage(1, 0x20_0000, 0x810_0000))
	if got := allocatedTablePages(allocator); got != 5 {
		t.Fatalf("after PT boundary: got %d, want 5", got)
	}

	table.Insert(testPage(1, 0x4000_0000, 0x820_0000))
	if got := allocatedTablePages(allocator); got != 7 {
		t.Fatalf("after higher boundary: got %d, want 7", got)
	}
}

func TestTwoPIDsHaveIndependentRootsAndMappings(t *testing.T) {
	table, allocator := makeRadixPageTable(t, X86FourLevel4KFormat())
	table.Insert(testPage(1, 0x4000, 0x800_0000))
	table.Insert(testPage(2, 0x4000, 0x900_0000))

	first, err := table.Walk(1, 0x4000)
	if err != nil {
		t.Fatalf("walk PID 1: %v", err)
	}
	second, err := table.Walk(2, 0x4000)
	if err != nil {
		t.Fatalf("walk PID 2: %v", err)
	}
	if first.RootPAddr == second.RootPAddr {
		t.Fatal("PIDs share a root")
	}
	if first.PhysicalBase == second.PhysicalBase {
		t.Fatal("PIDs unexpectedly share a leaf mapping")
	}
	if got := allocatedTablePages(allocator); got != 8 {
		t.Fatalf("allocated table pages: got %d, want 8", got)
	}
}

func TestFourAndFiveLevelFormatsUseGenericTraversal(t *testing.T) {
	formats := []PageTableFormat{
		X86FourLevel4KFormat(),
		X86FiveLevel4KFormat(),
	}
	for _, format := range formats {
		table, allocator := makeRadixPageTable(t, format)
		page := testPage(7, 0x1234_5678_9000, 0xa000_0000)
		table.Insert(page)

		result, err := table.Walk(page.PID, page.VAddr+0xabc)
		if err != nil {
			t.Fatalf("%d-level walk: %v", format.NumLevels, err)
		}
		if result.NumLevels != format.NumLevels ||
			result.PhysicalAddress != page.PAddr+0xabc {
			t.Fatalf("%d-level result: %+v", format.NumLevels, result)
		}
		if got := allocatedTablePages(allocator); got != uint64(format.NumLevels) {
			t.Fatalf("%d-level allocation: got %d", format.NumLevels, got)
		}
	}
}

func TestPhysicalEntriesAndShadowAgree(t *testing.T) {
	table, _ := makeRadixPageTable(t, X86FourLevel4KFormat())
	page := testPage(1, 0x7000, 0x9abc_0000)
	table.Insert(page)

	result, err := table.Walk(page.PID, page.VAddr)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	leafData, err := table.Storage.Read(
		result.EntryPAddrs[result.NumLevels-1], 8)
	if err != nil {
		t.Fatalf("read leaf: %v", err)
	}
	leaf, err := DecodePTE(binary.LittleEndian.Uint64(leafData))
	if err != nil {
		t.Fatalf("decode leaf: %v", err)
	}
	if leaf.PhysicalBase != page.PAddr {
		t.Fatalf("leaf base: got 0x%x, want 0x%x", leaf.PhysicalBase, page.PAddr)
	}

	radixPage, found := table.Find(page.PID, page.VAddr)
	if !found {
		t.Fatal("radix lookup failed")
	}
	shadowPage, found := table.Shadow.Find(page.PID, page.VAddr)
	if !found {
		t.Fatal("shadow lookup failed")
	}
	if radixPage != shadowPage || radixPage != page {
		t.Fatalf("mapping disagreement: radix=%+v shadow=%+v", radixPage, shadowPage)
	}
}

func TestMissingAndMalformedEntriesFault(t *testing.T) {
	table, _ := makeRadixPageTable(t, X86FourLevel4KFormat())
	page := testPage(1, 0x4000, 0x800_0000)
	table.Insert(page)

	_, err := table.Walk(1, 0x20_0000)
	if !errors.Is(err, ErrPageNotPresent) {
		t.Fatalf("missing mapping: got %v", err)
	}

	result, err := table.Walk(1, page.VAddr)
	if err != nil {
		t.Fatalf("initial walk: %v", err)
	}
	if err := table.Storage.Write(result.EntryPAddrs[0], []byte{
		0x01, 0x04, 0, 0, 0, 0, 0, 0,
	}); err != nil {
		t.Fatalf("write malformed entry: %v", err)
	}
	_, err = table.Walk(1, page.VAddr)
	if !errors.Is(err, ErrMalformedPTE) {
		t.Fatalf("malformed mapping: got %v", err)
	}
}

func TestRemoveClearsLeafAndShadow(t *testing.T) {
	table, _ := makeRadixPageTable(t, X86FourLevel4KFormat())
	page := testPage(1, 0x4000, 0x800_0000)
	table.Insert(page)
	table.Remove(page.PID, page.VAddr)

	if _, found := table.Find(page.PID, page.VAddr); found {
		t.Fatal("removed radix mapping still found")
	}
	if _, found := table.Shadow.Find(page.PID, page.VAddr); found {
		t.Fatal("removed shadow mapping still found")
	}
}

func TestTableRegionExhaustion(t *testing.T) {
	allocator := NewLinearTablePageAllocator(0x1000, 0x1000)
	if _, err := allocator.AllocateTablePage(4096); err != nil {
		t.Fatalf("first allocation: %v", err)
	}
	if _, err := allocator.AllocateTablePage(4096); !errors.Is(err, ErrTableRegionExhausted) {
		t.Fatalf("second allocation: got %v", err)
	}
}
