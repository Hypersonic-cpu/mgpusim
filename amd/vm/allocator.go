package vm

import (
	"errors"
	"fmt"
	"sync"
)

// PhysicalPageAllocationPolicy allocates simulated physical data pages.
type PhysicalPageAllocationPolicy interface {
	AllocatePage(deviceID int) (uint64, error)
	FreePage(deviceID int, pAddr uint64) error
}

// AllocationPolicy identifies the physical-frame ordering.
type AllocationPolicy string

const (
	// LinearAllocation selects frames in increasing physical-address order.
	LinearAllocation AllocationPolicy = "linear"
	// RandomizedAllocation selects a deterministic seed-keyed permutation.
	RandomizedAllocation AllocationPolicy = "randomized"
)

var (
	// ErrUnknownAllocationPolicy indicates an unsupported policy name.
	ErrUnknownAllocationPolicy = errors.New("unknown physical-page allocation policy")
	// ErrDeviceRangeNotFound indicates an unregistered device.
	ErrDeviceRangeNotFound = errors.New("physical-page device range not found")
	// ErrPhysicalPageExhausted indicates that no unallocated frame remains.
	ErrPhysicalPageExhausted = errors.New("physical-page range exhausted")
	// ErrInvalidPhysicalPage indicates an invalid free request.
	ErrInvalidPhysicalPage = errors.New("invalid physical page")
)

// DeviceMemoryRange describes frames owned by one device.
type DeviceMemoryRange struct {
	DeviceID int
	Base     uint64
	Size     uint64
	PageSize uint64
}

type deviceAllocationState struct {
	memoryRange DeviceMemoryRange
	numFrames   uint64
	nextFrame   uint64
	multiplier  uint64
	increment   uint64
	freeFrames  []uint64
	allocated   map[uint64]struct{}
}

// PhysicalPageAllocator allocates from registered device ranges without
// materializing a full frame array. Randomized ordering uses an affine
// permutation (a*x+b mod N), with a chosen coprime to N, which is a bijection
// for every frame count N.
type PhysicalPageAllocator struct {
	mu sync.Mutex

	policy  AllocationPolicy
	seed    uint64
	devices map[int]*deviceAllocationState
}

// NewPhysicalPageAllocator creates an empty multi-device allocator.
func NewPhysicalPageAllocator(
	policy AllocationPolicy,
	seed uint64,
) (*PhysicalPageAllocator, error) {
	if policy != LinearAllocation && policy != RandomizedAllocation {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAllocationPolicy, policy)
	}
	return &PhysicalPageAllocator{
		policy:  policy,
		seed:    seed,
		devices: make(map[int]*deviceAllocationState),
	}, nil
}

// Policy returns the configured allocation policy.
func (a *PhysicalPageAllocator) Policy() AllocationPolicy {
	return a.policy
}

// Seed returns the randomized-policy seed.
func (a *PhysicalPageAllocator) Seed() uint64 {
	return a.seed
}

// RegisterDeviceRange adds a non-overlapping allocation range.
func (a *PhysicalPageAllocator) RegisterDeviceRange(memoryRange DeviceMemoryRange) error {
	if err := validateDeviceMemoryRange(memoryRange); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.devices[memoryRange.DeviceID]; exists {
		return fmt.Errorf("%w: duplicate device %d", ErrInvalidPhysicalPage, memoryRange.DeviceID)
	}
	for _, state := range a.devices {
		if rangesOverlap(memoryRange, state.memoryRange) {
			return fmt.Errorf(
				"%w: device %d overlaps device %d",
				ErrInvalidPhysicalPage,
				memoryRange.DeviceID,
				state.memoryRange.DeviceID,
			)
		}
	}

	numFrames := memoryRange.Size / memoryRange.PageSize
	multiplier, increment := permutationParameters(
		a.policy, a.seed, memoryRange.DeviceID, numFrames)
	a.devices[memoryRange.DeviceID] = &deviceAllocationState{
		memoryRange: memoryRange,
		numFrames:   numFrames,
		multiplier:  multiplier,
		increment:   increment,
		allocated:   make(map[uint64]struct{}),
	}

	return nil
}

// AllocatePage allocates one aligned physical page on deviceID.
func (a *PhysicalPageAllocator) AllocatePage(deviceID int) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	state, ok := a.devices[deviceID]
	if !ok {
		return 0, fmt.Errorf("%w: %d", ErrDeviceRangeNotFound, deviceID)
	}

	var frame uint64
	if len(state.freeFrames) > 0 {
		last := len(state.freeFrames) - 1
		frame = state.freeFrames[last]
		state.freeFrames = state.freeFrames[:last]
	} else {
		if state.nextFrame >= state.numFrames {
			return 0, fmt.Errorf("%w: device %d", ErrPhysicalPageExhausted, deviceID)
		}
		frame = permuteFrame(
			state.nextFrame, state.numFrames, state.multiplier, state.increment)
		state.nextFrame++
	}

	pAddr := state.memoryRange.Base + frame*state.memoryRange.PageSize
	state.allocated[pAddr] = struct{}{}
	return pAddr, nil
}

// FreePage releases one live page for deterministic LIFO reuse.
func (a *PhysicalPageAllocator) FreePage(deviceID int, pAddr uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	state, ok := a.devices[deviceID]
	if !ok {
		return fmt.Errorf("%w: %d", ErrDeviceRangeNotFound, deviceID)
	}
	if pAddr < state.memoryRange.Base ||
		pAddr >= state.memoryRange.Base+state.memoryRange.Size ||
		(pAddr-state.memoryRange.Base)%state.memoryRange.PageSize != 0 {
		return fmt.Errorf(
			"%w: device=%d address=0x%x", ErrInvalidPhysicalPage, deviceID, pAddr)
	}
	if _, allocated := state.allocated[pAddr]; !allocated {
		return fmt.Errorf(
			"%w: page is not allocated: device=%d address=0x%x",
			ErrInvalidPhysicalPage, deviceID, pAddr)
	}

	delete(state.allocated, pAddr)
	frame := (pAddr - state.memoryRange.Base) / state.memoryRange.PageSize
	state.freeFrames = append(state.freeFrames, frame)
	return nil
}

func validateDeviceMemoryRange(memoryRange DeviceMemoryRange) error {
	if memoryRange.DeviceID < 0 ||
		memoryRange.PageSize == 0 ||
		memoryRange.PageSize&(memoryRange.PageSize-1) != 0 ||
		memoryRange.Size == 0 ||
		memoryRange.Base%memoryRange.PageSize != 0 ||
		memoryRange.Size%memoryRange.PageSize != 0 {
		return fmt.Errorf("%w: %+v", ErrInvalidPhysicalPage, memoryRange)
	}
	return nil
}

func rangesOverlap(a, b DeviceMemoryRange) bool {
	return a.Base < b.Base+b.Size && b.Base < a.Base+a.Size
}

func permutationParameters(
	policy AllocationPolicy,
	seed uint64,
	deviceID int,
	numFrames uint64,
) (uint64, uint64) {
	if policy == LinearAllocation || numFrames == 1 {
		return 1, 0
	}

	key := splitMix64(seed ^ uint64(deviceID)*0x9e3779b97f4a7c15)
	multiplier := key % numFrames
	if multiplier == 0 {
		multiplier = 1
	}
	for greatestCommonDivisor(multiplier, numFrames) != 1 {
		multiplier++
		if multiplier == numFrames {
			multiplier = 1
		}
	}
	increment := splitMix64(key) % numFrames
	return multiplier, increment
}

func permuteFrame(logical, numFrames, multiplier, increment uint64) uint64 {
	if numFrames == 1 {
		return 0
	}
	return addModulo(
		multiplyModulo(multiplier, logical, numFrames),
		increment,
		numFrames,
	)
}

func multiplyModulo(a, b, modulus uint64) uint64 {
	result := uint64(0)
	a %= modulus
	for b > 0 {
		if b&1 == 1 {
			result = addModulo(result, a, modulus)
		}
		a = addModulo(a, a, modulus)
		b >>= 1
	}
	return result
}

func addModulo(a, b, modulus uint64) uint64 {
	if a >= modulus-b {
		return a - (modulus - b)
	}
	return a + b
}

func greatestCommonDivisor(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func splitMix64(value uint64) uint64 {
	value += 0x9e3779b97f4a7c15
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}
