package latpc

import (
	"reflect"
	"testing"
)

func TestModeMechanisms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want Mechanisms
	}{
		{"baseline", Mechanisms{DetailedWalk: true}},
		{"latc", Mechanisms{Detector: true, LATC: true, DetailedWalk: true}},
		{"latp", Mechanisms{Detector: true, LATP: true, DetailedWalk: true}},
		{
			"latpc",
			Mechanisms{
				Detector: true, LATC: true, LATP: true, DetailedWalk: true,
			},
		},
		{"ideal", Mechanisms{}},
	}
	for _, test := range tests {
		mode, err := ParseMode(test.name)
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", test.name, err)
		}
		if got := mode.Mechanisms(); got != test.want {
			t.Errorf("%s mechanisms: got %+v, want %+v", test.name, got, test.want)
		}
	}
}

func TestInvalidNamesFailClearly(t *testing.T) {
	t.Parallel()

	if _, err := ParseMode("magic"); err == nil {
		t.Fatal("invalid mode must fail")
	}
	if _, err := ParseProfile("huge"); err == nil {
		t.Fatal("invalid profile must fail")
	}
}

func TestResourceProfiles(t *testing.T) {
	t.Parallel()

	paper := ResourcesForProfile(ProfilePaper)
	large := ResourcesForProfile(ProfileLargeResource)

	if paper.PageSizeBytes != 4096 || paper.L1TLBEntries != 32 ||
		paper.L2TLBEntries != 1024 || paper.PWQEntries != 128 ||
		paper.Walkers != 16 || !reflect.DeepEqual(paper.PWCEntries, []int{16, 16, 16}) {
		t.Fatalf("unexpected paper resources: %+v", paper)
	}
	if large.PageSizeBytes != 4096 || large.L1TLBEntries != 64 ||
		large.L2TLBEntries != 4096 || large.PWQEntries != 256 ||
		large.Walkers != 32 || !reflect.DeepEqual(large.PWCEntries, []int{32, 32, 32}) {
		t.Fatalf("unexpected large resources: %+v", large)
	}
}

func TestModesShareNonLATPCResources(t *testing.T) {
	t.Parallel()

	for _, profile := range []string{"paper", "large-resource"} {
		var want Resources
		for i, mode := range []string{"baseline", "latc", "latp", "latpc", "ideal"} {
			config, err := NewConfig(mode, profile)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				want = config.Resources
			} else if !reflect.DeepEqual(config.Resources, want) {
				t.Errorf("%s/%s resources differ from baseline", profile, mode)
			}
		}
	}
}
