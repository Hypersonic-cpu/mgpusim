package vm

import (
	"errors"
	"fmt"
)

const (
	ptePresentBit   = uint64(1) << 0
	pteReadWriteBit = uint64(1) << 1
	pteUserBit      = uint64(1) << 2
	ptePageSizeBit  = uint64(1) << 7
	pteNoExecuteBit = uint64(1) << 63

	ptePhysicalBaseMask = uint64(0x000f_ffff_ffff_f000)
	pteAllowedMask      = ptePresentBit |
		pteReadWriteBit |
		pteUserBit |
		ptePageSizeBit |
		ptePhysicalBaseMask |
		pteNoExecuteBit
)

var (
	// ErrMalformedPTE indicates unsupported or inconsistent entry bits.
	ErrMalformedPTE = errors.New("malformed page-table entry")
	// ErrPTEPhysicalAddress indicates an unaligned or unencodable address.
	ErrPTEPhysicalAddress = errors.New("invalid PTE physical address")
)

// PTE is the decoded subset of an x86-style 64-bit page-table entry used by
// the simulator.
type PTE struct {
	Present      bool
	ReadWrite    bool
	User         bool
	PageSize     bool
	PhysicalBase uint64
	NoExecute    bool
}

// EncodePTE encodes an x86-style 64-bit page-table entry.
func EncodePTE(entry PTE) (uint64, error) {
	if entry.PhysicalBase&0xfff != 0 ||
		entry.PhysicalBase&^ptePhysicalBaseMask != 0 {
		return 0, fmt.Errorf(
			"%w: 0x%x", ErrPTEPhysicalAddress, entry.PhysicalBase)
	}

	value := entry.PhysicalBase
	if entry.Present {
		value |= ptePresentBit
	}
	if entry.ReadWrite {
		value |= pteReadWriteBit
	}
	if entry.User {
		value |= pteUserBit
	}
	if entry.PageSize {
		value |= ptePageSizeBit
	}
	if entry.NoExecute {
		value |= pteNoExecuteBit
	}
	return value, nil
}

// DecodePTE decodes the simulator-supported x86-style entry subset.
func DecodePTE(value uint64) (PTE, error) {
	if value&^pteAllowedMask != 0 {
		return PTE{}, fmt.Errorf(
			"%w: unsupported bits 0x%x", ErrMalformedPTE, value&^pteAllowedMask)
	}

	return PTE{
		Present:      value&ptePresentBit != 0,
		ReadWrite:    value&pteReadWriteBit != 0,
		User:         value&pteUserBit != 0,
		PageSize:     value&ptePageSizeBit != 0,
		PhysicalBase: value & ptePhysicalBaseMask,
		NoExecute:    value&pteNoExecuteBit != 0,
	}, nil
}
