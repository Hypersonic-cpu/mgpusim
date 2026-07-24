package internal

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sarchlab/akita/v5/mem/vm"
	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

var _ = Describe("MemoryAllocatorImpl", func() {

	var (
		allocator *memoryAllocatorImpl
		pageTable vm.PageTable
	)

	BeforeEach(func() {
		pageTable = vm.NewPageTable(12)

		allocator = newMemoryAllocator(pageTable, 12, mgpuvm.LinearAllocation, 0)
		configAFourGPUSystem(allocator)
	})

	It("should allocate memory", func() {
		ptr := allocator.Allocate(1, 8, 1)
		Expect(ptr).To(Equal(uint64(4096)))
		page, found := pageTable.Find(1, ptr)
		Expect(found).To(BeTrue())
		Expect(page).To(Equal(vm.Page{
			PID:      1,
			PAddr:    0x1_0000_1000,
			VAddr:    4096,
			PageSize: 4096,
			DeviceID: 1,
			Valid:    true,
		}))
		Expect(allocator.vAddrToPageMapping[ptr]).To(Equal(page))
	})

	It("should allocate unified memory", func() {
		ptr := allocator.AllocateUnified(1, 8)
		Expect(ptr).To(Equal(uint64(4096)))
		page, found := pageTable.Find(1, ptr)
		Expect(found).To(BeTrue())
		Expect(page.PAddr).To(Equal(uint64(0x1_0000_1000)))
		Expect(page.Unified).To(BeTrue())
	})

	It("should allocate memory larger than a page", func() {
		ptr := allocator.Allocate(1, 8196, 1)
		Expect(ptr).To(Equal(uint64(4096)))
		for i := uint64(0); i < 3; i++ {
			page, found := pageTable.Find(1, ptr+0x1000*i)
			Expect(found).To(BeTrue())
			Expect(page.PAddr).To(Equal(0x1_0000_1000 + 0x1000*i))
		}
	})

	It("should remap page to another device", func() {
		ptr := allocator.Allocate(1, 4000, 1)

		allocator.Remap(1, ptr, 4000, 2)
		page, found := pageTable.Find(1, ptr)
		Expect(found).To(BeTrue())
		Expect(page.PAddr).To(Equal(uint64(0x2_0000_1000)))
		Expect(page.DeviceID).To(Equal(uint64(2)))
		Expect(allocator.vAddrToPageMapping[ptr]).To(Equal(page))
	})

	It("should use the deterministic randomized policy for mappings", func() {
		pageTable = vm.NewPageTable(12)
		allocator = newMemoryAllocator(
			pageTable, 12, mgpuvm.RandomizedAllocation, 42)
		configAFourGPUSystem(allocator)

		ptr := allocator.Allocate(1, 4096, 1)
		page, found := pageTable.Find(1, ptr)
		Expect(found).To(BeTrue())

		expectedAllocator, err := mgpuvm.NewPhysicalPageAllocator(
			mgpuvm.RandomizedAllocation, 42)
		Expect(err).NotTo(HaveOccurred())
		err = expectedAllocator.RegisterDeviceRange(mgpuvm.DeviceMemoryRange{
			DeviceID: 1,
			Base:     0x1_0000_1000,
			Size:     0x1_0000_0000,
			PageSize: 4096,
		})
		Expect(err).NotTo(HaveOccurred())
		expectedPAddr, err := expectedAllocator.AllocatePage(1)
		Expect(err).NotTo(HaveOccurred())

		Expect(page.PAddr).To(Equal(expectedPAddr))
		Expect(allocator.vAddrToPageMapping[ptr]).To(Equal(page))
	})
})

func configAFourGPUSystem(allocator *memoryAllocatorImpl) {
	cpu := &Device{
		ID:       0,
		Type:     DeviceTypeCPU,
		MemState: NewDeviceMemoryState(12),
	}
	cpu.SetTotalMemSize(0x1_0000_0000)
	allocator.RegisterDevice(cpu)

	for i := 0; i < 4; i++ { // 5 devices = 1 CPU + 4 GPUs
		gpu := &Device{
			ID:       i + 1,
			Type:     DeviceTypeGPU,
			MemState: NewDeviceMemoryState(12),
		}
		gpu.SetTotalMemSize(0x1_0000_0000)
		allocator.RegisterDevice(gpu)
	}
}
