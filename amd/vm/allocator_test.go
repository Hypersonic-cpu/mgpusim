package vm

import (
	"errors"
	"testing"
)

const testPageSize = uint64(4096)

func makeAllocator(
	t *testing.T,
	policy AllocationPolicy,
	seed uint64,
	ranges ...DeviceMemoryRange,
) *PhysicalPageAllocator {
	t.Helper()
	allocator, err := NewPhysicalPageAllocator(policy, seed)
	if err != nil {
		t.Fatalf("new allocator: %v", err)
	}
	for _, memoryRange := range ranges {
		if err := allocator.RegisterDeviceRange(memoryRange); err != nil {
			t.Fatalf("register range: %v", err)
		}
	}
	return allocator
}

func allocatePages(t *testing.T, allocator *PhysicalPageAllocator, deviceID, count int) []uint64 {
	t.Helper()
	pages := make([]uint64, count)
	for i := range count {
		page, err := allocator.AllocatePage(deviceID)
		if err != nil {
			t.Fatalf("allocate page %d: %v", i, err)
		}
		pages[i] = page
	}
	return pages
}

func TestLinearFirstPages(t *testing.T) {
	memoryRange := DeviceMemoryRange{DeviceID: 1, Base: 0x100000, Size: 8 * testPageSize, PageSize: testPageSize}
	allocator := makeAllocator(t, LinearAllocation, 0, memoryRange)

	got := allocatePages(t, allocator, 1, 4)
	want := []uint64{0x100000, 0x101000, 0x102000, 0x103000}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("page %d: got 0x%x, want 0x%x", i, got[i], want[i])
		}
	}
}

func TestRandomizedGoldenSequence(t *testing.T) {
	memoryRange := DeviceMemoryRange{DeviceID: 1, Base: 0x200000, Size: 16 * testPageSize, PageSize: testPageSize}
	allocator := makeAllocator(t, RandomizedAllocation, 42, memoryRange)

	got := allocatePages(t, allocator, 1, 8)
	want := []uint64{
		0x20e000, 0x201000, 0x204000, 0x207000,
		0x20a000, 0x20d000, 0x200000, 0x203000,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("page %d: got 0x%x, want 0x%x", i, got[i], want[i])
		}
	}
}

func TestRandomizedDeterminismUniquenessAndRange(t *testing.T) {
	memoryRange := DeviceMemoryRange{DeviceID: 3, Base: 0x800000, Size: 257 * testPageSize, PageSize: testPageSize}
	first := makeAllocator(t, RandomizedAllocation, 99, memoryRange)
	second := makeAllocator(t, RandomizedAllocation, 99, memoryRange)
	firstPages := allocatePages(t, first, 3, 257)
	secondPages := allocatePages(t, second, 3, 257)

	seen := make(map[uint64]struct{}, len(firstPages))
	for i, page := range firstPages {
		if page != secondPages[i] {
			t.Fatalf("sequence differs at %d: 0x%x != 0x%x", i, page, secondPages[i])
		}
		if page < memoryRange.Base || page >= memoryRange.Base+memoryRange.Size {
			t.Fatalf("page out of range: 0x%x", page)
		}
		if page%testPageSize != 0 {
			t.Fatalf("page is not aligned: 0x%x", page)
		}
		if _, exists := seen[page]; exists {
			t.Fatalf("duplicate page: 0x%x", page)
		}
		seen[page] = struct{}{}
	}
	if _, err := first.AllocatePage(3); !errors.Is(err, ErrPhysicalPageExhausted) {
		t.Fatalf("expected exhaustion, got %v", err)
	}
}

func TestDifferentSeedsProduceDifferentSequences(t *testing.T) {
	memoryRange := DeviceMemoryRange{DeviceID: 1, Base: 0x100000, Size: 64 * testPageSize, PageSize: testPageSize}
	first := makeAllocator(t, RandomizedAllocation, 1, memoryRange)
	second := makeAllocator(t, RandomizedAllocation, 2, memoryRange)
	firstPages := allocatePages(t, first, 1, 8)
	secondPages := allocatePages(t, second, 1, 8)

	equal := true
	for i := range firstPages {
		if firstPages[i] != secondPages[i] {
			equal = false
			break
		}
	}
	if equal {
		t.Fatal("different seeds produced identical prefixes")
	}
}

func TestFreeAndSafeReuse(t *testing.T) {
	memoryRange := DeviceMemoryRange{DeviceID: 1, Base: 0x300000, Size: 4 * testPageSize, PageSize: testPageSize}
	allocator := makeAllocator(t, LinearAllocation, 0, memoryRange)
	page := allocatePages(t, allocator, 1, 1)[0]

	if err := allocator.FreePage(1, page); err != nil {
		t.Fatalf("free: %v", err)
	}
	reused, err := allocator.AllocatePage(1)
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if reused != page {
		t.Fatalf("reused page: got 0x%x, want 0x%x", reused, page)
	}
	if err := allocator.FreePage(1, reused); err != nil {
		t.Fatalf("second free: %v", err)
	}
	if err := allocator.FreePage(1, reused); !errors.Is(err, ErrInvalidPhysicalPage) {
		t.Fatalf("expected double-free error, got %v", err)
	}
}

func TestPerDeviceRangeIsolation(t *testing.T) {
	firstRange := DeviceMemoryRange{DeviceID: 1, Base: 0x100000, Size: 4 * testPageSize, PageSize: testPageSize}
	secondRange := DeviceMemoryRange{DeviceID: 2, Base: 0x200000, Size: 4 * testPageSize, PageSize: testPageSize}
	allocator := makeAllocator(t, RandomizedAllocation, 7, firstRange, secondRange)

	first := allocatePages(t, allocator, 1, 4)
	second := allocatePages(t, allocator, 2, 4)
	for _, page := range first {
		if page < firstRange.Base || page >= firstRange.Base+firstRange.Size {
			t.Fatalf("device 1 page out of range: 0x%x", page)
		}
	}
	for _, page := range second {
		if page < secondRange.Base || page >= secondRange.Base+secondRange.Size {
			t.Fatalf("device 2 page out of range: 0x%x", page)
		}
	}
}
