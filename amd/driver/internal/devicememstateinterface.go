package internal

// A DeviceMemoryState handles the internal memory allocation algorithms
type DeviceMemoryState interface {
	setInitialAddress(addr uint64)
	getInitialAddress() uint64
	setStorageSize(size uint64)
	getStorageSize() uint64
	addSinglePAddr(addr uint64)
	popNextAvailablePAddrs() uint64
	noAvailablePAddrs() bool
	allocateMultiplePages(numPages int) []uint64
}

// NewDeviceMemoryState creates a new device memory state based on allocator type.
func NewDeviceMemoryState(log2pagesize uint64) DeviceMemoryState {
	switch MemoryAllocatorType {
	case AllocatorTypeDefault:
		return newDeviceRegularMemoryState(log2pagesize)
	case AllocatorTypeBuddy:
		return newDeviceBuddyMemoryState(log2pagesize)
	default:
		panic("Invalid memory allocator type")
	}
}

func newDeviceRegularMemoryState(log2pagesize uint64) DeviceMemoryState {
	return &deviceMemoryStateImpl{
		log2PageSize: log2pagesize,
	}
}

// deviceMemoryStateImpl allocates sequentially without materializing every
// physical frame in the device range. Freed pages are retained in a small
// recycle stack.
type deviceMemoryStateImpl struct {
	log2PageSize   uint64
	initialAddress uint64
	storageSize    uint64
	nextPAddr      uint64
	recycledPAddrs []uint64
}

func (dms *deviceMemoryStateImpl) setInitialAddress(addr uint64) {
	dms.initialAddress = addr
	dms.nextPAddr = addr
	dms.recycledPAddrs = nil
}

func (dms *deviceMemoryStateImpl) getInitialAddress() uint64 {
	return dms.initialAddress
}

func (dms *deviceMemoryStateImpl) setStorageSize(size uint64) {
	dms.storageSize = size
}

func (dms *deviceMemoryStateImpl) getStorageSize() uint64 {
	return dms.storageSize
}

func (dms *deviceMemoryStateImpl) addSinglePAddr(addr uint64) {
	dms.recycledPAddrs = append(dms.recycledPAddrs, addr)
}

func (dms *deviceMemoryStateImpl) popNextAvailablePAddrs() uint64 {
	if len(dms.recycledPAddrs) > 0 {
		nextPAddr := dms.recycledPAddrs[0]
		dms.recycledPAddrs = dms.recycledPAddrs[1:]
		return nextPAddr
	}

	if dms.noAvailablePAddrs() {
		panic("out of memory")
	}
	nextPAddr := dms.nextPAddr
	dms.nextPAddr += uint64(1) << dms.log2PageSize
	return nextPAddr
}

func (dms *deviceMemoryStateImpl) noAvailablePAddrs() bool {
	return len(dms.recycledPAddrs) == 0 &&
		dms.nextPAddr >= dms.initialAddress+dms.storageSize
}

func (dms *deviceMemoryStateImpl) allocateMultiplePages(
	numPages int,
) (pAddrs []uint64) {
	for i := 0; i < numPages; i++ {
		pAddr := dms.popNextAvailablePAddrs()
		pAddrs = append(pAddrs, pAddr)
	}
	return pAddrs
}
