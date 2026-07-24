// Package latpc defines the selectable translation mechanisms and resource
// profiles used by the LATPC evaluation.
package latpc

import (
	"fmt"
	"strings"
)

// Mode selects which LATPC mechanisms participate in translation.
type Mode string

const (
	// ModeBaseline uses the detailed page-table walker without LATPC.
	ModeBaseline Mode = "baseline"
	// ModeLATC enables regularity detection and L1-TLB MSHR compression.
	ModeLATC Mode = "latc"
	// ModeLATP enables regularity detection and grouped page-table walks.
	ModeLATP Mode = "latp"
	// ModeLATPC enables both LATC and LATP.
	ModeLATPC Mode = "latpc"
	// ModeIdeal bypasses TLB-miss walking to measure the ideal-translation gap.
	ModeIdeal Mode = "ideal"
)

// Mechanisms is the mode decoded into individual implementation gates.
type Mechanisms struct {
	Detector     bool
	LATC         bool
	LATP         bool
	DetailedWalk bool
}

// ParseMode validates and normalizes a translation mode.
func ParseMode(name string) (Mode, error) {
	mode := Mode(strings.ToLower(strings.TrimSpace(name)))
	switch mode {
	case ModeBaseline, ModeLATC, ModeLATP, ModeLATPC, ModeIdeal:
		return mode, nil
	default:
		return "", fmt.Errorf(
			"invalid translation mode %q (want baseline, latc, latp, latpc, or ideal)",
			name)
	}
}

// Mechanisms returns the mechanism gates selected by the mode.
func (m Mode) Mechanisms() Mechanisms {
	switch m {
	case ModeLATC:
		return Mechanisms{Detector: true, LATC: true, DetailedWalk: true}
	case ModeLATP:
		return Mechanisms{Detector: true, LATP: true, DetailedWalk: true}
	case ModeLATPC:
		return Mechanisms{
			Detector: true, LATC: true, LATP: true, DetailedWalk: true,
		}
	case ModeIdeal:
		return Mechanisms{}
	default:
		return Mechanisms{DetailedWalk: true}
	}
}

// ProfileName identifies one translation-resource preset.
type ProfileName string

const (
	// ProfilePaper reproduces the translation-pressure resources from the
	// design document.
	ProfilePaper ProfileName = "paper"
	// ProfileLargeResource is the larger sensitivity configuration.
	ProfileLargeResource ProfileName = "large-resource"
)

// Resources contains all non-mechanism translation capacities.
type Resources struct {
	PageSizeBytes uint64

	L1TLBEntries int
	L1TLBWays    int
	L1TLBMSHRs   int
	L1TLBPorts   int
	L1TLBLatency int

	L2TLBEntries int
	L2TLBWays    int
	L2TLBMSHRs   int
	L2TLBPorts   int
	L2TLBLatency int

	PWQEntries int
	Walkers    int
	PWCEntries []int
}

// Config is the complete translation experiment configuration.
type Config struct {
	Mode        Mode
	ProfileName ProfileName
	Resources   Resources
}

// ParseProfile validates a resource-profile name.
func ParseProfile(name string) (ProfileName, error) {
	profile := ProfileName(strings.ToLower(strings.TrimSpace(name)))
	switch profile {
	case ProfilePaper, ProfileLargeResource:
		return profile, nil
	default:
		return "", fmt.Errorf(
			"invalid translation profile %q (want paper or large-resource)",
			name)
	}
}

// ResourcesForProfile returns a fresh copy of the named preset.
func ResourcesForProfile(profile ProfileName) Resources {
	switch profile {
	case ProfileLargeResource:
		return Resources{
			PageSizeBytes: 4096,
			L1TLBEntries:  64,
			L1TLBWays:     64,
			L1TLBMSHRs:    16,
			L1TLBPorts:    4,
			L1TLBLatency:  20,
			L2TLBEntries:  4096,
			L2TLBWays:     16,
			L2TLBMSHRs:    128,
			L2TLBPorts:    16,
			L2TLBLatency:  80,
			PWQEntries:    256,
			Walkers:       32,
			PWCEntries:    []int{32, 32, 32},
		}
	default:
		return Resources{
			PageSizeBytes: 4096,
			L1TLBEntries:  32,
			L1TLBWays:     32,
			L1TLBMSHRs:    16,
			L1TLBPorts:    4,
			L1TLBLatency:  20,
			L2TLBEntries:  1024,
			L2TLBWays:     16,
			L2TLBMSHRs:    128,
			L2TLBPorts:    16,
			L2TLBLatency:  80,
			PWQEntries:    128,
			Walkers:       16,
			PWCEntries:    []int{16, 16, 16},
		}
	}
}

// NewConfig validates names and constructs a complete configuration.
func NewConfig(modeName, profileName string) (Config, error) {
	mode, err := ParseMode(modeName)
	if err != nil {
		return Config{}, err
	}
	profile, err := ParseProfile(profileName)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Mode: mode, ProfileName: profile, Resources: ResourcesForProfile(profile),
	}, nil
}

// DefaultConfig returns the mechanism-fidelity baseline configuration.
func DefaultConfig() Config {
	config, err := NewConfig(string(ModeBaseline), string(ProfilePaper))
	if err != nil {
		panic(err)
	}
	return config
}
