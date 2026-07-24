// Package internal provides support for the driver implementation.
package internal

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/sarchlab/akita/v5/mem/vm"
	"github.com/sarchlab/mgpusim/v5/amd/simdebug"
	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

const (
	// PhysicalAllocationPolicyEnv selects "linear" or "randomized" mappings.
	PhysicalAllocationPolicyEnv = "MGPUSIM_PAGE_ALLOC_POLICY"
	// PhysicalAllocationSeedEnv selects the deterministic randomized seed.
	PhysicalAllocationSeedEnv = "MGPUSIM_PAGE_ALLOC_SEED"
)

// A MemoryAllocator can allocate memory on the CPU and GPUs
type MemoryAllocator interface {
	RegisterDevice(device *Device)
	GetDeviceIDByPAddr(pAddr uint64) int
	Allocate(pid vm.PID, byteSize uint64, deviceID int) uint64
	AllocateUnified(pid vm.PID, byteSize uint64) uint64
	Free(vAddr uint64)
	Remap(pid vm.PID, pageVAddr, byteSize uint64, deviceID int)
	RemovePage(vAddr uint64)
	AllocatePageWithGivenVAddr(
		pid vm.PID,
		deviceID int,
		vAddr uint64,
		unified bool,
	) vm.Page
}

// NewMemoryAllocator creates a new memory allocator.
func NewMemoryAllocator(
	pageTable vm.PageTable,
	log2PageSize uint64,
) MemoryAllocator {
	policy, seed := physicalAllocationConfigFromEnv()
	return newMemoryAllocator(pageTable, log2PageSize, policy, seed)
}

func newMemoryAllocator(
	pageTable vm.PageTable,
	log2PageSize uint64,
	policy mgpuvm.AllocationPolicy,
	seed uint64,
) *memoryAllocatorImpl {
	physicalAllocator, err := mgpuvm.NewPhysicalPageAllocator(policy, seed)
	if err != nil {
		panic(err)
	}
	a := &memoryAllocatorImpl{
		pageTable:            pageTable,
		totalStorageByteSize: 1 << log2PageSize, // Starting with a page to avoid 0 address.
		log2PageSize:         log2PageSize,
		processMemoryStates:  make(map[vm.PID]*processMemoryState),
		vAddrToPageMapping:   make(map[uint64]vm.Page),
		devices:              make(map[int]*Device),
		physicalAllocator:    physicalAllocator,
		mappingSequence:      make(map[int]uint64),
	}
	return a
}

func physicalAllocationConfigFromEnv() (mgpuvm.AllocationPolicy, uint64) {
	policy := mgpuvm.LinearAllocation
	if value := strings.TrimSpace(os.Getenv(PhysicalAllocationPolicyEnv)); value != "" {
		policy = mgpuvm.AllocationPolicy(value)
		if value == "pseudo-randomized" {
			policy = mgpuvm.RandomizedAllocation
		}
	}

	seed := uint64(0)
	if value := strings.TrimSpace(os.Getenv(PhysicalAllocationSeedEnv)); value != "" {
		parsed, err := strconv.ParseUint(value, 0, 64)
		if err != nil {
			panic(fmt.Errorf("parse %s: %w", PhysicalAllocationSeedEnv, err))
		}
		seed = parsed
	}

	return policy, seed
}

type processMemoryState struct {
	pid       vm.PID
	nextVAddr uint64
}

// A memoryAllocatorImpl provides the default implementation for
// memoryAllocator
type memoryAllocatorImpl struct {
	sync.Mutex
	pageTable            vm.PageTable
	log2PageSize         uint64
	vAddrToPageMapping   map[uint64]vm.Page
	processMemoryStates  map[vm.PID]*processMemoryState
	devices              map[int]*Device
	totalStorageByteSize uint64
	physicalAllocator    *mgpuvm.PhysicalPageAllocator
	mappingSequence      map[int]uint64
}

func (a *memoryAllocatorImpl) RegisterDevice(device *Device) {
	a.Lock()
	defer a.Unlock()

	state := device.MemState
	state.setInitialAddress(a.totalStorageByteSize)

	if device.Type == DeviceTypeUnifiedGPU {
		a.devices[device.ID] = device
		return
	}

	pageSize := uint64(1) << a.log2PageSize
	err := a.physicalAllocator.RegisterDeviceRange(mgpuvm.DeviceMemoryRange{
		DeviceID: device.ID,
		Base:     a.totalStorageByteSize,
		Size:     state.getStorageSize(),
		PageSize: pageSize,
	})
	if err != nil {
		panic(err)
	}

	a.totalStorageByteSize += state.getStorageSize()

	a.devices[device.ID] = device
}

func (a *memoryAllocatorImpl) GetDeviceIDByPAddr(pAddr uint64) int {
	a.Lock()
	defer a.Unlock()

	return a.deviceIDByPAddr(pAddr)
}

func (a *memoryAllocatorImpl) deviceIDByPAddr(pAddr uint64) int {
	for id, dev := range a.devices {
		state := dev.MemState
		if isPAddrOnDevice(pAddr, state) {
			return id
		}
	}

	panic("device not found")
}

func isPAddrOnDevice(
	pAddr uint64,
	state DeviceMemoryState,
) bool {
	return pAddr >= state.getInitialAddress() &&
		pAddr < state.getInitialAddress()+state.getStorageSize()
}

func (a *memoryAllocatorImpl) Allocate(
	pid vm.PID,
	byteSize uint64,
	deviceID int,
) uint64 {
	if byteSize == 0 {
		panic("Allocating 0 bytes.")
	}

	a.Lock()
	defer a.Unlock()

	pageSize := uint64(1 << a.log2PageSize)
	numPages := (byteSize-1)/pageSize + 1
	return a.allocatePages(int(numPages), pid, deviceID, false)
}

func (a *memoryAllocatorImpl) AllocateUnified(
	pid vm.PID,
	byteSize uint64,
) uint64 {
	if byteSize == 0 {
		panic("Allocating 0 bytes.")
	}

	a.Lock()
	defer a.Unlock()

	pageSize := uint64(1 << a.log2PageSize)
	numPages := (byteSize-1)/pageSize + 1
	return a.allocatePages(int(numPages), pid, 1, true)
}

func (a *memoryAllocatorImpl) allocatePages(
	numPages int,
	pid vm.PID,
	deviceID int,
	unified bool,
) (firstPageVAddr uint64) {
	pState, found := a.processMemoryStates[pid]
	if !found {
		a.processMemoryStates[pid] = &processMemoryState{
			pid:       pid,
			nextVAddr: uint64(1 << a.log2PageSize),
		}
		pState = a.processMemoryStates[pid]
	}
	pageSize := uint64(1 << a.log2PageSize)
	nextVAddr := pState.nextVAddr

	for i := 0; i < numPages; i++ {
		pAddr, physicalDeviceID := a.allocatePhysicalPage(deviceID)
		vAddr := nextVAddr + uint64(i)*pageSize

		page := vm.Page{
			PID:      pid,
			VAddr:    vAddr,
			PAddr:    pAddr,
			PageSize: pageSize,
			Valid:    true,
			Unified:  unified,
			DeviceID: uint64(physicalDeviceID),
		}

		a.pageTable.Insert(page)
		a.vAddrToPageMapping[page.VAddr] = page
		a.logMapping(page, physicalDeviceID)
	}

	pState.nextVAddr += pageSize * uint64(numPages)

	return nextVAddr
}

func (a *memoryAllocatorImpl) Remap(
	pid vm.PID,
	pageVAddr, byteSize uint64,
	deviceID int,
) {
	a.Lock()
	defer a.Unlock()

	pageSize := uint64(1 << a.log2PageSize)
	addr := pageVAddr
	vAddrs := make([]uint64, 0)
	for addr < pageVAddr+byteSize {
		vAddrs = append(vAddrs, addr)
		addr += pageSize
	}

	a.allocateMultiplePagesWithGivenVAddrs(pid, deviceID, vAddrs, false)
}

func (a *memoryAllocatorImpl) RemovePage(vAddr uint64) {
	a.Lock()
	defer a.Unlock()

	a.removePage(vAddr)
}

func (a *memoryAllocatorImpl) removePage(vAddr uint64) {
	page, ok := a.vAddrToPageMapping[vAddr]

	if !ok {
		panic("page not found")
	}

	deviceID := a.deviceIDByPAddr(page.PAddr)
	if err := a.physicalAllocator.FreePage(deviceID, page.PAddr); err != nil {
		panic(err)
	}

	a.pageTable.Remove(page.PID, page.VAddr)
	delete(a.vAddrToPageMapping, page.VAddr)
}

func (a *memoryAllocatorImpl) AllocatePageWithGivenVAddr(
	pid vm.PID,
	deviceID int,
	vAddr uint64,
	isUnified bool,
) vm.Page {
	a.Lock()
	defer a.Unlock()

	return a.allocatePageWithGivenVAddr(pid, deviceID, vAddr, isUnified)
}

func (a *memoryAllocatorImpl) allocatePageWithGivenVAddr(
	pid vm.PID,
	deviceID int,
	vAddr uint64,
	isUnified bool,
) vm.Page {
	pageSize := uint64(1 << a.log2PageSize)

	pAddr, physicalDeviceID := a.allocatePhysicalPage(deviceID)

	page := vm.Page{
		PID:      pid,
		VAddr:    vAddr,
		PAddr:    pAddr,
		PageSize: pageSize,
		Valid:    true,
		DeviceID: uint64(physicalDeviceID),
		Unified:  isUnified,
	}
	a.vAddrToPageMapping[page.VAddr] = page
	a.pageTable.Update(page)
	a.logMapping(page, physicalDeviceID)

	return page
}

func (a *memoryAllocatorImpl) allocateMultiplePagesWithGivenVAddrs(
	pid vm.PID,
	deviceID int,
	vAddrs []uint64,
	isUnified bool,
) (pages []vm.Page) {
	pageSize := uint64(1 << a.log2PageSize)

	for _, vAddr := range vAddrs {
		pAddr, physicalDeviceID := a.allocatePhysicalPage(deviceID)
		page := vm.Page{
			PID:      pid,
			VAddr:    vAddr,
			PAddr:    pAddr,
			PageSize: pageSize,
			Valid:    true,
			DeviceID: uint64(physicalDeviceID),
			Unified:  isUnified,
		}
		a.vAddrToPageMapping[page.VAddr] = page
		a.pageTable.Update(page)
		a.logMapping(page, physicalDeviceID)
		pages = append(pages, page)
	}

	return pages
}

func (a *memoryAllocatorImpl) Free(ptr uint64) {
	a.Lock()
	defer a.Unlock()

	a.removePage(ptr)
}

func (a *memoryAllocatorImpl) allocatePhysicalPage(deviceID int) (uint64, int) {
	device, ok := a.devices[deviceID]
	if !ok {
		panic(fmt.Sprintf("device %d not found", deviceID))
	}

	if device.Type != DeviceTypeUnifiedGPU {
		pAddr, err := a.physicalAllocator.AllocatePage(deviceID)
		if err != nil {
			panic(err)
		}
		return pAddr, deviceID
	}

	for range device.ActualGPUs {
		index := device.nextActualGPUIndex % len(device.ActualGPUs)
		actualDeviceID := device.ActualGPUs[index].ID
		device.nextActualGPUIndex = (index + 1) % len(device.ActualGPUs)
		pAddr, err := a.physicalAllocator.AllocatePage(actualDeviceID)
		if err == nil {
			return pAddr, actualDeviceID
		}
	}

	panic("out of memory")
}

func (a *memoryAllocatorImpl) logMapping(page vm.Page, deviceID int) {
	if !simdebug.Enabled(simdebug.VMMap) {
		return
	}

	sequence := a.mappingSequence[deviceID]
	a.mappingSequence[deviceID]++
	simdebug.DPrintf(
		simdebug.VMMap,
		"pid=%d dev=%d policy=%s seq=%d va=0x%x pa=0x%x size=%d seed=%d",
		page.PID,
		deviceID,
		a.physicalAllocator.Policy(),
		sequence,
		page.VAddr,
		page.PAddr,
		page.PageSize,
		a.physicalAllocator.Seed(),
	)
}
