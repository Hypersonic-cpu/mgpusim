// Package vm provides the detailed GPU virtual-memory model.
package vm

import (
	"errors"
	"fmt"
)

// MaxPageTableLevels is the maximum supported radix-tree depth.
const MaxPageTableLevels = 5

var (
	// ErrInvalidPageTableFormat indicates an internally inconsistent format.
	ErrInvalidPageTableFormat = errors.New("invalid page-table format")
	// ErrInvalidPageTableLevel indicates a level outside the configured depth.
	ErrInvalidPageTableLevel = errors.New("invalid page-table level")
	// ErrPageTableIndexOutOfRange indicates an index wider than its level.
	ErrPageTableIndexOutOfRange = errors.New("page-table index out of range")
)

// PageTableFormat describes a radix page table from root to leaf.
type PageTableFormat struct {
	NumLevels      uint8
	PageOffsetBits uint8
	EntryBytes     uint8
	IndexBits      [MaxPageTableLevels]uint8
}

// X86FourLevel4KFormat returns the standard four-level 4 KiB format.
func X86FourLevel4KFormat() PageTableFormat {
	return PageTableFormat{
		NumLevels:      4,
		PageOffsetBits: 12,
		EntryBytes:     8,
		IndexBits:      [MaxPageTableLevels]uint8{9, 9, 9, 9},
	}
}

// X86FiveLevel4KFormat returns the x86-style five-level 4 KiB format.
func X86FiveLevel4KFormat() PageTableFormat {
	return PageTableFormat{
		NumLevels:      5,
		PageOffsetBits: 12,
		EntryBytes:     8,
		IndexBits:      [MaxPageTableLevels]uint8{9, 9, 9, 9, 9},
	}
}

// Validate checks that all address calculations are well defined.
func (f PageTableFormat) Validate() error {
	if f.NumLevels == 0 || f.NumLevels > MaxPageTableLevels {
		return fmt.Errorf("%w: NumLevels=%d", ErrInvalidPageTableFormat, f.NumLevels)
	}
	if f.PageOffsetBits == 0 || f.PageOffsetBits >= 64 {
		return fmt.Errorf(
			"%w: PageOffsetBits=%d", ErrInvalidPageTableFormat, f.PageOffsetBits)
	}
	if f.EntryBytes == 0 || f.EntryBytes&(f.EntryBytes-1) != 0 {
		return fmt.Errorf("%w: EntryBytes=%d", ErrInvalidPageTableFormat, f.EntryBytes)
	}

	totalBits := uint16(f.PageOffsetBits)
	for level := range int(f.NumLevels) {
		bits := f.IndexBits[level]
		if bits == 0 || bits >= 64 {
			return fmt.Errorf(
				"%w: IndexBits[%d]=%d", ErrInvalidPageTableFormat, level, bits)
		}
		totalBits += uint16(bits)
	}
	for level := int(f.NumLevels); level < MaxPageTableLevels; level++ {
		if f.IndexBits[level] != 0 {
			return fmt.Errorf(
				"%w: inactive IndexBits[%d]=%d",
				ErrInvalidPageTableFormat, level, f.IndexBits[level])
		}
	}
	if totalBits > 64 {
		return fmt.Errorf("%w: address bits=%d", ErrInvalidPageTableFormat, totalBits)
	}

	return nil
}

// PageSize returns the mapped base-page size.
func (f PageTableFormat) PageSize() (uint64, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	return uint64(1) << f.PageOffsetBits, nil
}

// PageOffset extracts the byte offset within a mapped page.
func (f PageTableFormat) PageOffset(vAddr uint64) (uint64, error) {
	pageSize, err := f.PageSize()
	if err != nil {
		return 0, err
	}
	return vAddr & (pageSize - 1), nil
}

// TablePageSize returns the byte size of the table at level.
func (f PageTableFormat) TablePageSize(level uint8) (uint64, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	if level >= f.NumLevels {
		return 0, fmt.Errorf("%w: %d", ErrInvalidPageTableLevel, level)
	}
	return (uint64(1) << f.IndexBits[level]) * uint64(f.EntryBytes), nil
}

// ExtractIndices returns root-to-leaf indices for vAddr.
func (f PageTableFormat) ExtractIndices(
	vAddr uint64,
) ([MaxPageTableLevels]uint64, error) {
	var indices [MaxPageTableLevels]uint64
	if err := f.Validate(); err != nil {
		return indices, err
	}

	shift := uint64(f.PageOffsetBits)
	for level := int(f.NumLevels) - 1; level >= 0; level-- {
		bits := uint64(f.IndexBits[level])
		indices[level] = (vAddr >> shift) & ((uint64(1) << bits) - 1)
		shift += bits
	}

	return indices, nil
}

// EntryAddress returns the physical address of one entry within a table page.
func (f PageTableFormat) EntryAddress(
	tableBase uint64,
	level uint8,
	index uint64,
) (uint64, error) {
	tableSize, err := f.TablePageSize(level)
	if err != nil {
		return 0, err
	}
	numEntries := uint64(1) << f.IndexBits[level]
	if index >= numEntries {
		return 0, fmt.Errorf(
			"%w: level=%d index=%d", ErrPageTableIndexOutOfRange, level, index)
	}
	if tableBase%tableSize != 0 {
		return 0, fmt.Errorf(
			"%w: unaligned table base 0x%x", ErrInvalidPageTableFormat, tableBase)
	}
	return tableBase + index*uint64(f.EntryBytes), nil
}
