package latpc

import (
	"testing"

	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

func makeMembers(vpns ...uint64) []GroupMember {
	members := make([]GroupMember, len(vpns))
	for i, vpn := range vpns {
		members[i] = GroupMember{
			InstructionID: 1,
			RequestID:     uint64(100 + i),
			Position:      uint16(i),
			Count:         uint16(len(vpns)),
			VAddr:         vpn * 4096,
			LaneMask:      uint64(1) << i,
		}
	}
	return members
}

func fourLevelDetector(t *testing.T) Detector {
	t.Helper()
	detector, err := NewDetector(mgpuvm.X86FourLevel4KFormat())
	if err != nil {
		t.Fatal(err)
	}
	return detector
}

func assertOneRegularGroup(
	t *testing.T,
	detector Detector,
	wantStride int64,
	vpns ...uint64,
) {
	t.Helper()
	result := detector.Detect(makeMembers(vpns...))
	if len(result.Groups) != 1 || !result.Groups[0].Regular ||
		result.Groups[0].Stride != wantStride {
		t.Fatalf("unexpected detection: %+v", result)
	}
}

func TestDetectorRecognizesAscendingDescendingAndNonUnitStride(t *testing.T) {
	detector := fourLevelDetector(t)
	assertOneRegularGroup(t, detector, 1, 10, 11, 12, 13)
	assertOneRegularGroup(t, detector, -1, 13, 12, 11, 10)
	assertOneRegularGroup(t, detector, 3, 10, 13, 16, 19)
}

func TestDetectorKeepsIrregularPatternUncompressed(t *testing.T) {
	result := fourLevelDetector(t).Detect(makeMembers(10, 12, 15, 19))
	if len(result.Groups) != 1 || result.Groups[0].Regular {
		t.Fatalf("irregular pattern classified regular: %+v", result)
	}
}

func TestDetectorFindsMultipleGroupsInOneInstruction(t *testing.T) {
	result := fourLevelDetector(t).Detect(
		makeMembers(10, 11, 12, 30, 32, 34))
	if len(result.Groups) != 2 ||
		!result.Groups[0].Regular || result.Groups[0].Stride != 1 ||
		!result.Groups[1].Regular || result.Groups[1].Stride != 2 {
		t.Fatalf("unexpected groups: %+v", result.Groups)
	}
}

func TestDetectorMergesDuplicateVPNWaitersAndLaneMasks(t *testing.T) {
	members := makeMembers(10, 10, 11, 12)
	result := fourLevelDetector(t).Detect(members)
	if result.UniqueVPNs != 3 || result.Duplicates != 1 ||
		len(result.Groups) != 1 || !result.Groups[0].Regular {
		t.Fatalf("unexpected duplicate handling: %+v", result)
	}
	first := result.Groups[0].Members[0]
	if first.LaneMask != 0b11 || len(first.Waiters) != 2 ||
		len(first.Positions) != 2 {
		t.Fatalf("duplicate waiter metadata lost: %+v", first)
	}
}

func TestDetectorSplitsAtFinalLevelPageBoundary(t *testing.T) {
	result := fourLevelDetector(t).Detect(makeMembers(510, 511, 512, 513))
	if len(result.Groups) != 2 ||
		result.Groups[0].LeafPage == result.Groups[1].LeafPage {
		t.Fatalf("leaf boundary not preserved: %+v", result.Groups)
	}
	for _, group := range result.Groups {
		for _, member := range group.Members {
			if member.VPN>>9 != group.LeafPage {
				t.Fatalf("member crossed leaf boundary: %+v", group)
			}
		}
	}
}

func TestDetectorPreservesWave64LaneMask(t *testing.T) {
	members := makeMembers(4, 5, 6)
	members[0].LaneMask = ^uint64(0)
	result := fourLevelDetector(t).Detect(members)
	if result.Groups[0].Members[0].LaneMask != ^uint64(0) {
		t.Fatal("wave64 lane mask was truncated")
	}
}

func TestDetectorSupportsFourAndFiveLevelFormats(t *testing.T) {
	formats := []mgpuvm.PageTableFormat{
		mgpuvm.X86FourLevel4KFormat(),
		{
			PageOffsetBits: 12,
			IndexBits:      [mgpuvm.MaxPageTableLevels]uint8{9, 9, 9, 9, 9},
			NumLevels:      5,
			EntryBytes:     8,
		},
	}
	for _, format := range formats {
		detector, err := NewDetector(format)
		if err != nil {
			t.Fatal(err)
		}
		assertOneRegularGroup(t, detector, 1, 100, 101, 102)
	}
}

func TestDetectorBypassModes(t *testing.T) {
	if ModeBaseline.Mechanisms().Detector ||
		ModeIdeal.Mechanisms().Detector {
		t.Fatal("baseline and ideal must bypass detector")
	}
	if !ModeLATC.Mechanisms().Detector ||
		!ModeLATP.Mechanisms().Detector ||
		!ModeLATPC.Mechanisms().Detector {
		t.Fatal("LATPC mechanism modes must enable detector")
	}
}
