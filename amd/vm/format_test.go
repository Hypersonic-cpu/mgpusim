package vm

import (
	"errors"
	"testing"
)

func TestFourLevelFormatIndices(t *testing.T) {
	format := X86FourLevel4KFormat()
	vAddr := uint64(0x0000_ff92_3456_7abc)

	indices, err := format.ExtractIndices(vAddr)
	if err != nil {
		t.Fatalf("extract indices: %v", err)
	}

	expected := [MaxPageTableLevels]uint64{
		(vAddr >> 39) & 0x1ff,
		(vAddr >> 30) & 0x1ff,
		(vAddr >> 21) & 0x1ff,
		(vAddr >> 12) & 0x1ff,
	}
	if indices != expected {
		t.Fatalf("indices: got %v, want %v", indices, expected)
	}
}

func TestFiveLevelFormatIndices(t *testing.T) {
	format := X86FiveLevel4KFormat()
	vAddr := uint64(0x0123_4567_89ab_cdef)

	indices, err := format.ExtractIndices(vAddr)
	if err != nil {
		t.Fatalf("extract indices: %v", err)
	}

	expected := [MaxPageTableLevels]uint64{
		(vAddr >> 48) & 0x1ff,
		(vAddr >> 39) & 0x1ff,
		(vAddr >> 30) & 0x1ff,
		(vAddr >> 21) & 0x1ff,
		(vAddr >> 12) & 0x1ff,
	}
	if indices != expected {
		t.Fatalf("indices: got %v, want %v", indices, expected)
	}
}

func TestPageOffsetAndTableSize(t *testing.T) {
	format := X86FourLevel4KFormat()

	offset, err := format.PageOffset(0x12345abc)
	if err != nil {
		t.Fatalf("page offset: %v", err)
	}
	if offset != 0xabc {
		t.Fatalf("offset: got 0x%x, want 0xabc", offset)
	}
	for level := uint8(0); level < format.NumLevels; level++ {
		size, sizeErr := format.TablePageSize(level)
		if sizeErr != nil {
			t.Fatalf("level %d table size: %v", level, sizeErr)
		}
		if size != 4096 {
			t.Fatalf("level %d size: got %d, want 4096", level, size)
		}
	}
}

func TestFormatValidation(t *testing.T) {
	tests := []PageTableFormat{
		{},
		{NumLevels: 6, PageOffsetBits: 12, EntryBytes: 8},
		{NumLevels: 1, PageOffsetBits: 0, EntryBytes: 8, IndexBits: [5]uint8{9}},
		{NumLevels: 1, PageOffsetBits: 12, EntryBytes: 3, IndexBits: [5]uint8{9}},
		{NumLevels: 1, PageOffsetBits: 12, EntryBytes: 8},
		{
			NumLevels: 1, PageOffsetBits: 12, EntryBytes: 8,
			IndexBits: [5]uint8{9, 9},
		},
	}
	for i, format := range tests {
		if err := format.Validate(); !errors.Is(err, ErrInvalidPageTableFormat) {
			t.Fatalf("case %d: expected invalid format, got %v", i, err)
		}
	}
}

func TestEntryAddress(t *testing.T) {
	format := X86FourLevel4KFormat()

	address, err := format.EntryAddress(0x4000, 2, 17)
	if err != nil {
		t.Fatalf("entry address: %v", err)
	}
	if address != 0x4088 {
		t.Fatalf("entry address: got 0x%x, want 0x4088", address)
	}

	_, err = format.EntryAddress(0x4000, 2, 512)
	if !errors.Is(err, ErrPageTableIndexOutOfRange) {
		t.Fatalf("expected index error, got %v", err)
	}
}
