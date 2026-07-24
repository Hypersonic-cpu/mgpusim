package vm

import (
	"errors"
	"testing"
)

func TestPTEEncodeDecodeRoundTrip(t *testing.T) {
	want := PTE{
		Present:      true,
		ReadWrite:    true,
		User:         true,
		PageSize:     true,
		PhysicalBase: 0x1234_5000,
		NoExecute:    true,
	}
	value, err := EncodePTE(want)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodePTE(value)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != want {
		t.Fatalf("decoded entry: got %+v, want %+v", got, want)
	}
}

func TestPTERejectsInvalidPhysicalAddresses(t *testing.T) {
	tests := []uint64{
		0x123,
		0x0010_0000_0000_0000,
	}
	for _, address := range tests {
		_, err := EncodePTE(PTE{Present: true, PhysicalBase: address})
		if !errors.Is(err, ErrPTEPhysicalAddress) {
			t.Fatalf("address 0x%x: got %v", address, err)
		}
	}
}

func TestPTERejectsUnsupportedBits(t *testing.T) {
	_, err := DecodePTE(uint64(1) << 10)
	if !errors.Is(err, ErrMalformedPTE) {
		t.Fatalf("got %v", err)
	}
}

func TestZeroPTEIsAValidNonPresentEntry(t *testing.T) {
	entry, err := DecodePTE(0)
	if err != nil {
		t.Fatalf("decode zero: %v", err)
	}
	if entry.Present || entry.PhysicalBase != 0 {
		t.Fatalf("unexpected zero entry: %+v", entry)
	}
}
