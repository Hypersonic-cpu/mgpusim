package vm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/sarchlab/akita/v5/mem"
	akitavm "github.com/sarchlab/akita/v5/mem/vm"
)

var (
	// ErrTableRegionExhausted indicates insufficient page-table storage.
	ErrTableRegionExhausted = errors.New("page-table physical region exhausted")
	// ErrAddressSpaceNotFound indicates a PID without a radix root.
	ErrAddressSpaceNotFound = errors.New("radix address space not found")
	// ErrPageNotPresent indicates a missing entry during a physical walk.
	ErrPageNotPresent = errors.New("page-table entry is not present")
)

// AddressSpace associates one process with its physical radix-tree root.
type AddressSpace struct {
	PID       akitavm.PID
	RootPAddr uint64
}

// TablePageAllocator allocates physical pages from a page-table-only region.
type TablePageAllocator interface {
	AllocateTablePage(size uint64) (uint64, error)
}

// LinearTablePageAllocator is a sparse cursor allocator for page-table pages.
type LinearTablePageAllocator struct {
	mu sync.Mutex

	base uint64
	size uint64
	next uint64
}

// NewLinearTablePageAllocator creates an allocator over [base, base+size).
func NewLinearTablePageAllocator(base, size uint64) *LinearTablePageAllocator {
	return &LinearTablePageAllocator{base: base, size: size, next: base}
}

// AllocateTablePage returns an aligned, non-overlapping table page.
func (a *LinearTablePageAllocator) AllocateTablePage(size uint64) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if size == 0 || size&(size-1) != 0 {
		return 0, fmt.Errorf("%w: invalid table page size %d", ErrTableRegionExhausted, size)
	}
	aligned := alignUp(a.next, size)
	if aligned < a.base || aligned > a.base+a.size ||
		size > a.base+a.size-aligned {
		return 0, ErrTableRegionExhausted
	}
	a.next = aligned + size
	return aligned, nil
}

// WalkResult records the physical state observed by a radix walk.
type WalkResult struct {
	PID             akitavm.PID
	VAddr           uint64
	RootPAddr       uint64
	TablePAddrs     [MaxPageTableLevels]uint64
	EntryPAddrs     [MaxPageTableLevels]uint64
	Entries         [MaxPageTableLevels]PTE
	NumLevels       uint8
	PhysicalBase    uint64
	PhysicalAddress uint64
}

// RadixPageTable stores the authoritative hierarchy in simulated memory.
// Shadow is updated for legacy compatibility and validation, but Walk and Find
// obtain the physical base by decoding the hierarchy.
type RadixPageTable struct {
	Format         PageTableFormat
	TableAllocator TablePageAllocator
	Storage        *mem.Storage
	Shadow         akitavm.PageTable

	mu            sync.RWMutex
	addressSpaces map[akitavm.PID]AddressSpace
	pageMetadata  map[akitavm.PID]map[uint64]akitavm.Page
	entryValues   map[uint64]uint64
}

// NewRadixPageTable creates an empty memory-backed page table.
func NewRadixPageTable(
	format PageTableFormat,
	tableAllocator TablePageAllocator,
	storage *mem.Storage,
	shadow akitavm.PageTable,
) (*RadixPageTable, error) {
	if err := format.Validate(); err != nil {
		return nil, err
	}
	if tableAllocator == nil {
		return nil, fmt.Errorf("vm: nil table allocator")
	}
	if storage == nil {
		return nil, fmt.Errorf("vm: nil page-table storage")
	}
	if shadow == nil {
		return nil, fmt.Errorf("vm: nil shadow page table")
	}

	return &RadixPageTable{
		Format:         format,
		TableAllocator: tableAllocator,
		Storage:        storage,
		Shadow:         shadow,
		addressSpaces:  make(map[akitavm.PID]AddressSpace),
		pageMetadata:   make(map[akitavm.PID]map[uint64]akitavm.Page),
		entryValues:    make(map[uint64]uint64),
	}, nil
}

// Insert establishes a new physical hierarchy mapping.
func (t *RadixPageTable) Insert(page akitavm.Page) {
	if err := t.Map(page, false); err != nil {
		panic(err)
	}
	t.Shadow.Insert(page)
}

// Update replaces a leaf mapping while preserving its hierarchy.
func (t *RadixPageTable) Update(page akitavm.Page) {
	if err := t.Map(page, true); err != nil {
		panic(err)
	}
	t.Shadow.Update(page)
}

// Remove clears a leaf entry. Empty intermediate tables remain allocated.
func (t *RadixPageTable) Remove(pid akitavm.PID, vAddr uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	result, err := t.walkLocked(pid, vAddr)
	if err != nil {
		panic(err)
	}
	leaf := result.EntryPAddrs[result.NumLevels-1]
	if err := t.writeRawEntryLocked(leaf, 0); err != nil {
		panic(err)
	}
	pageBase := t.pageBase(vAddr)
	delete(t.pageMetadata[pid], pageBase)
	t.Shadow.Remove(pid, vAddr)
}

// Find decodes the physical hierarchy and returns compatibility metadata.
func (t *RadixPageTable) Find(
	pid akitavm.PID,
	vAddr uint64,
) (akitavm.Page, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result, err := t.walkLocked(pid, vAddr)
	if err != nil {
		return akitavm.Page{}, false
	}
	page, ok := t.pageMetadata[pid][t.pageBase(vAddr)]
	if !ok {
		return akitavm.Page{}, false
	}
	page.PAddr = result.PhysicalBase
	return page, true
}

// ReverseLookup uses metadata only to provide the legacy compatibility API.
func (t *RadixPageTable) ReverseLookup(pAddr uint64) (akitavm.Page, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, processPages := range t.pageMetadata {
		for _, page := range processPages {
			if page.PAddr == t.pageBase(pAddr) {
				return page, true
			}
		}
	}
	return akitavm.Page{}, false
}

// AddressSpace returns the root registered for pid.
func (t *RadixPageTable) AddressSpace(pid akitavm.PID) (AddressSpace, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	addressSpace, ok := t.addressSpaces[pid]
	return addressSpace, ok
}

// MappingMetadata returns non-address leaf attributes retained for response
// compatibility. The physical translation itself must come from Walk.
func (t *RadixPageTable) MappingMetadata(
	pid akitavm.PID,
	vAddr uint64,
) (akitavm.Page, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	page, ok := t.pageMetadata[pid][t.pageBase(vAddr)]
	return page, ok
}

// ResolveCoherentEntryData applies the most recent simulated page-table write
// to bytes returned by the memory hierarchy. Driver page-table updates write
// backing storage directly, while an older copy of the containing cache line
// may still reside in L2 or MALL. The entry-value journal models the coherence
// snoop from that write without bypassing the timed PTW memory request.
func (t *RadixPageTable) ResolveCoherentEntryData(
	pAddr uint64,
	observed []byte,
) ([]byte, bool) {
	if len(observed) != int(t.Format.EntryBytes) ||
		t.Format.EntryBytes != 8 {
		return observed, false
	}

	t.mu.RLock()
	value, ok := t.entryValues[pAddr]
	t.mu.RUnlock()
	if !ok || binary.LittleEndian.Uint64(observed) == value {
		return observed, false
	}

	coherent := make([]byte, t.Format.EntryBytes)
	binary.LittleEndian.PutUint64(coherent, value)
	return coherent, true
}

// WriteRawEntry updates one physical page-table entry and publishes the write
// to the coherence journal. It is primarily useful for permission/fault
// mutation tests and future accessed/dirty-bit maintenance.
func (t *RadixPageTable) WriteRawEntry(pAddr, value uint64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writeRawEntryLocked(pAddr, value)
}

// Walk performs an authoritative physical-memory radix traversal.
func (t *RadixPageTable) Walk(pid akitavm.PID, vAddr uint64) (WalkResult, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.walkLocked(pid, vAddr)
}

// Map creates or updates the hierarchy without touching the shadow table.
func (t *RadixPageTable) Map(page akitavm.Page, allowReplace bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	pageSize, err := t.Format.PageSize()
	if err != nil {
		return err
	}
	if !page.Valid || page.PageSize != pageSize ||
		page.VAddr%pageSize != 0 || page.PAddr%pageSize != 0 {
		return fmt.Errorf("vm: invalid mapping: %+v", page)
	}

	indices, err := t.Format.ExtractIndices(page.VAddr)
	if err != nil {
		return err
	}
	addressSpace, err := t.ensureAddressSpaceLocked(page.PID)
	if err != nil {
		return err
	}
	if err := t.mapHierarchyLocked(addressSpace, indices, page, allowReplace); err != nil {
		return err
	}

	if _, ok := t.pageMetadata[page.PID]; !ok {
		t.pageMetadata[page.PID] = make(map[uint64]akitavm.Page)
	}
	t.pageMetadata[page.PID][page.VAddr] = page
	return nil
}

func (t *RadixPageTable) ensureAddressSpaceLocked(
	pid akitavm.PID,
) (AddressSpace, error) {
	if addressSpace, ok := t.addressSpaces[pid]; ok {
		return addressSpace, nil
	}

	root, err := t.allocateAndZeroTable(0)
	if err != nil {
		return AddressSpace{}, err
	}
	addressSpace := AddressSpace{PID: pid, RootPAddr: root}
	t.addressSpaces[pid] = addressSpace
	return addressSpace, nil
}

func (t *RadixPageTable) mapHierarchyLocked(
	addressSpace AddressSpace,
	indices [MaxPageTableLevels]uint64,
	page akitavm.Page,
	allowReplace bool,
) error {
	tablePAddr := addressSpace.RootPAddr
	for level := uint8(0); level < t.Format.NumLevels-1; level++ {
		child, err := t.ensureChildTableLocked(tablePAddr, level, indices[level])
		if err != nil {
			return err
		}
		tablePAddr = child
	}

	leafLevel := t.Format.NumLevels - 1
	entryPAddr, err := t.Format.EntryAddress(
		tablePAddr, leafLevel, indices[leafLevel])
	if err != nil {
		return err
	}
	current, err := t.readEntry(entryPAddr)
	if err != nil {
		return err
	}
	if current.Present && !allowReplace {
		return fmt.Errorf(
			"vm: mapping already exists: pid=%d va=0x%x", page.PID, page.VAddr)
	}
	return t.writeEntry(entryPAddr, PTE{
		Present:      true,
		ReadWrite:    true,
		User:         true,
		PhysicalBase: page.PAddr,
	})
}

func (t *RadixPageTable) ensureChildTableLocked(
	tablePAddr uint64,
	level uint8,
	index uint64,
) (uint64, error) {
	entryPAddr, err := t.Format.EntryAddress(tablePAddr, level, index)
	if err != nil {
		return 0, err
	}
	entry, err := t.readEntry(entryPAddr)
	if err != nil {
		return 0, err
	}
	if entry.PageSize {
		return 0, fmt.Errorf("%w: huge intermediate page", ErrMalformedPTE)
	}
	if entry.Present {
		return entry.PhysicalBase, nil
	}

	child, err := t.allocateAndZeroTable(level + 1)
	if err != nil {
		return 0, err
	}
	entry = PTE{
		Present:      true,
		ReadWrite:    true,
		User:         true,
		PhysicalBase: child,
	}
	if err := t.writeEntry(entryPAddr, entry); err != nil {
		return 0, err
	}
	return child, nil
}

func (t *RadixPageTable) walkLocked(
	pid akitavm.PID,
	vAddr uint64,
) (WalkResult, error) {
	result := WalkResult{PID: pid, VAddr: vAddr, NumLevels: t.Format.NumLevels}
	addressSpace, ok := t.addressSpaces[pid]
	if !ok {
		return result, fmt.Errorf("%w: pid=%d", ErrAddressSpaceNotFound, pid)
	}
	result.RootPAddr = addressSpace.RootPAddr
	indices, err := t.Format.ExtractIndices(vAddr)
	if err != nil {
		return result, err
	}

	tablePAddr := addressSpace.RootPAddr
	for level := uint8(0); level < t.Format.NumLevels; level++ {
		entryPAddr, addrErr := t.Format.EntryAddress(
			tablePAddr, level, indices[level])
		if addrErr != nil {
			return result, addrErr
		}
		entry, readErr := t.readEntry(entryPAddr)
		if readErr != nil {
			return result, readErr
		}
		result.TablePAddrs[level] = tablePAddr
		result.EntryPAddrs[level] = entryPAddr
		result.Entries[level] = entry
		if !entry.Present {
			return result, fmt.Errorf(
				"%w: pid=%d va=0x%x level=%d", ErrPageNotPresent, pid, vAddr, level)
		}
		if level != t.Format.NumLevels-1 && entry.PageSize {
			return result, fmt.Errorf("%w: unexpected page-size bit", ErrMalformedPTE)
		}
		tablePAddr = entry.PhysicalBase
	}

	offset, err := t.Format.PageOffset(vAddr)
	if err != nil {
		return result, err
	}
	result.PhysicalBase = result.Entries[result.NumLevels-1].PhysicalBase
	result.PhysicalAddress = result.PhysicalBase + offset
	return result, nil
}

func (t *RadixPageTable) allocateAndZeroTable(level uint8) (uint64, error) {
	tableSize, err := t.Format.TablePageSize(level)
	if err != nil {
		return 0, err
	}
	pAddr, err := t.TableAllocator.AllocateTablePage(tableSize)
	if err != nil {
		return 0, err
	}
	if pAddr+tableSize > t.Storage.Capacity() {
		return 0, fmt.Errorf(
			"%w: table [0x%x,0x%x) exceeds storage", ErrTableRegionExhausted, pAddr, pAddr+tableSize)
	}
	if err := t.Storage.Write(pAddr, make([]byte, tableSize)); err != nil {
		return 0, err
	}
	return pAddr, nil
}

func (t *RadixPageTable) readEntry(pAddr uint64) (PTE, error) {
	data, err := t.Storage.Read(pAddr, uint64(t.Format.EntryBytes))
	if err != nil {
		return PTE{}, err
	}
	return DecodePTE(binary.LittleEndian.Uint64(data))
}

func (t *RadixPageTable) writeEntry(pAddr uint64, entry PTE) error {
	value, err := EncodePTE(entry)
	if err != nil {
		return err
	}
	return t.writeRawEntryLocked(pAddr, value)
}

func (t *RadixPageTable) writeRawEntryLocked(pAddr, value uint64) error {
	data := make([]byte, t.Format.EntryBytes)
	binary.LittleEndian.PutUint64(data, value)
	if err := t.Storage.Write(pAddr, data); err != nil {
		return err
	}
	t.entryValues[pAddr] = value
	return nil
}

func (t *RadixPageTable) pageBase(addr uint64) uint64 {
	pageSize := uint64(1) << t.Format.PageOffsetBits
	return addr &^ (pageSize - 1)
}

func alignUp(value, alignment uint64) uint64 {
	return (value + alignment - 1) &^ (alignment - 1)
}
