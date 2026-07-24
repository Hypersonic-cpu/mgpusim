package latpc

import (
	"fmt"
	"sort"
	"sync"

	"github.com/sarchlab/akita/v5/hooking"
	"github.com/sarchlab/akita/v5/tracing"
	"github.com/sarchlab/mgpusim/v5/amd/simdebug"
	mgpuvm "github.com/sarchlab/mgpusim/v5/amd/vm"
)

const minRegularMembers = 3

// TranslationMember is one ordered unique VPN and all original transactions
// that wait for it.
type TranslationMember struct {
	VPN       uint64
	LaneMask  uint64
	Positions []uint16
	Waiters   []uint64
}

// TranslationGroup is a maximal regular run or an irregular remainder within
// one final-level page-table page.
type TranslationGroup struct {
	InstructionID uint64
	BaseVPN       uint64
	Stride        int64
	Regular       bool
	LeafPage      uint64
	Members       []TranslationMember
}

// DetectionResult describes every group found in one wavefront instruction.
type DetectionResult struct {
	InstructionID uint64
	RawMembers    int
	UniqueVPNs    int
	Duplicates    int
	Groups        []TranslationGroup
}

// Detector groups ordered unique VPNs without assuming a four-level format.
type Detector struct {
	PageSizeBytes uint64
	LeafIndexBits uint8
}

// NewDetector derives the final-level group boundary from the page-table
// format.
func NewDetector(format mgpuvm.PageTableFormat) (Detector, error) {
	if err := format.Validate(); err != nil {
		return Detector{}, err
	}
	return Detector{
		PageSizeBytes: uint64(1) << format.PageOffsetBits,
		LeafIndexBits: format.IndexBits[format.NumLevels-1],
	}, nil
}

// Detect analyzes one complete wavefront instruction.
func (d Detector) Detect(members []GroupMember) DetectionResult {
	if len(members) == 0 {
		return DetectionResult{}
	}
	ordered := append([]GroupMember(nil), members...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Position < ordered[j].Position
	})

	result := DetectionResult{
		InstructionID: ordered[0].InstructionID,
		RawMembers:    len(ordered),
	}
	unique := d.mergeDuplicateVPNs(ordered)
	result.UniqueVPNs = len(unique)
	result.Duplicates = result.RawMembers - result.UniqueVPNs
	result.Groups = d.formGroups(result.InstructionID, unique)
	return result
}

// AnnotateGroupMembers attaches detector output to every raw transaction.
// Duplicate-VPN transactions receive the same unique-translation position.
func (d Detector) AnnotateGroupMembers(members []GroupMember) []GroupMember {
	annotated := append([]GroupMember(nil), members...)
	result := d.Detect(annotated)
	byPosition := make(map[uint16]int, len(annotated))
	for index := range annotated {
		byPosition[annotated[index].Position] = index
	}
	for groupIndex, group := range result.Groups {
		if !group.Regular {
			continue
		}
		for memberIndex, member := range group.Members {
			for _, rawPosition := range member.Positions {
				index := byPosition[rawPosition]
				annotated[index].Regular = true
				annotated[index].GroupIndex = uint16(groupIndex)
				annotated[index].GroupPosition = uint16(memberIndex)
				annotated[index].GroupCount = uint16(len(group.Members))
				annotated[index].BaseVPN = group.BaseVPN
				annotated[index].Stride = group.Stride
				annotated[index].LeafPage = group.LeafPage
			}
		}
	}
	return annotated
}

func (d Detector) mergeDuplicateVPNs(
	ordered []GroupMember,
) []TranslationMember {
	unique := make([]TranslationMember, 0, len(ordered))
	indices := make(map[uint64]int)
	for _, raw := range ordered {
		vpn := raw.VAddr / d.PageSizeBytes
		if index, found := indices[vpn]; found {
			member := &unique[index]
			member.LaneMask |= raw.LaneMask
			member.Positions = append(member.Positions, raw.Position)
			member.Waiters = append(member.Waiters, raw.RequestID)
			continue
		}
		indices[vpn] = len(unique)
		unique = append(unique, TranslationMember{
			VPN:       vpn,
			LaneMask:  raw.LaneMask,
			Positions: []uint16{raw.Position},
			Waiters:   []uint64{raw.RequestID},
		})
	}
	return unique
}

func (d Detector) formGroups(
	instructionID uint64,
	members []TranslationMember,
) []TranslationGroup {
	var groups []TranslationGroup
	for start := 0; start < len(members); {
		leafPage := members[start].VPN >> d.LeafIndexBits
		end := start + 1
		for end < len(members) &&
			members[end].VPN>>d.LeafIndexBits == leafPage {
			end++
		}
		groups = append(groups,
			d.formGroupsWithinLeaf(instructionID, leafPage, members[start:end])...)
		start = end
	}
	return groups
}

func (d Detector) formGroupsWithinLeaf(
	instructionID, leafPage uint64,
	members []TranslationMember,
) []TranslationGroup {
	var groups []TranslationGroup
	irregularStart := 0
	flushIrregular := func(end int) {
		if irregularStart >= end {
			return
		}
		groups = append(groups, makeTranslationGroup(
			instructionID, leafPage, 0, false, members[irregularStart:end]))
	}

	for i := 0; i+2 < len(members); {
		stride := signedVPNDelta(members[i+1].VPN, members[i].VPN)
		nextStride := signedVPNDelta(members[i+2].VPN, members[i+1].VPN)
		if stride == 0 || stride != nextStride {
			i++
			continue
		}
		end := i + minRegularMembers
		for end < len(members) &&
			signedVPNDelta(members[end].VPN, members[end-1].VPN) == stride {
			end++
		}
		flushIrregular(i)
		groups = append(groups, makeTranslationGroup(
			instructionID, leafPage, stride, true, members[i:end]))
		i = end
		irregularStart = end
	}
	flushIrregular(len(members))
	return groups
}

func makeTranslationGroup(
	instructionID, leafPage uint64,
	stride int64,
	regular bool,
	members []TranslationMember,
) TranslationGroup {
	copied := append([]TranslationMember(nil), members...)
	return TranslationGroup{
		InstructionID: instructionID,
		BaseVPN:       copied[0].VPN,
		Stride:        stride,
		Regular:       regular,
		LeafPage:      leafPage,
		Members:       copied,
	}
}

func signedVPNDelta(next, current uint64) int64 {
	if next >= current {
		return int64(next - current)
	}
	return -int64(current - next)
}

// OpportunityStats records detector-visible translation opportunity.
type OpportunityStats struct {
	Instructions       uint64
	RawMembers         uint64
	UniqueVPNs         uint64
	DuplicateVPNs      uint64
	RegularGroups      uint64
	RegularMembers     uint64
	SameLeafPageGroups uint64
	PageDivergence     map[int]uint64
	StrideDistribution map[int64]uint64
}

type runtimeDetector struct {
	mu       sync.Mutex
	detector Detector
	pending  map[uint64]map[uint16]GroupMember
	stats    OpportunityStats
}

func newRuntimeDetector(detector Detector) *runtimeDetector {
	return &runtimeDetector{
		detector: detector,
		pending:  make(map[uint64]map[uint16]GroupMember),
		stats: OpportunityStats{
			PageDivergence:     make(map[int]uint64),
			StrideDistribution: make(map[int64]uint64),
		},
	}
}

func (d *runtimeDetector) observe(member GroupMember) {
	d.mu.Lock()
	defer d.mu.Unlock()

	instruction := d.pending[member.InstructionID]
	if instruction == nil {
		instruction = make(map[uint16]GroupMember)
		d.pending[member.InstructionID] = instruction
	}
	instruction[member.Position] = member
	if len(instruction) != int(member.Count) {
		return
	}

	members := make([]GroupMember, 0, len(instruction))
	for _, pending := range instruction {
		members = append(members, pending)
	}
	delete(d.pending, member.InstructionID)
	result := d.detector.Detect(members)
	d.record(result)
}

func (d *runtimeDetector) record(result DetectionResult) {
	d.stats.Instructions++
	d.stats.RawMembers += uint64(result.RawMembers)
	d.stats.UniqueVPNs += uint64(result.UniqueVPNs)
	d.stats.DuplicateVPNs += uint64(result.Duplicates)
	d.stats.PageDivergence[result.UniqueVPNs]++
	for _, group := range result.Groups {
		d.stats.SameLeafPageGroups++
		if group.Regular {
			d.stats.RegularGroups++
			d.stats.RegularMembers += uint64(len(group.Members))
			d.stats.StrideDistribution[group.Stride]++
		}
		simdebug.DPrintf(
			simdebug.LATPCDetector,
			"inst=%d base-vpn=0x%x stride=%d members=%d regular=%t leaf=%d",
			group.InstructionID,
			group.BaseVPN,
			group.Stride,
			len(group.Members),
			group.Regular,
			group.LeafPage,
		)
	}
}

func (d *runtimeDetector) snapshot() OpportunityStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	stats := d.stats
	stats.PageDivergence = copyMap(d.stats.PageDivergence)
	stats.StrideDistribution = copyMap(d.stats.StrideDistribution)
	return stats
}

func copyMap[K comparable](source map[K]uint64) map[K]uint64 {
	result := make(map[K]uint64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

var activeDetector *runtimeDetector

// ResetRuntimeDetector initializes opportunity collection for a new platform.
func ResetRuntimeDetector(format mgpuvm.PageTableFormat) {
	detector, err := NewDetector(format)
	if err != nil {
		panic(fmt.Errorf("latpc detector: %w", err))
	}
	activeDetector = newRuntimeDetector(detector)
}

// RuntimeOpportunityStats returns a stable copy of detector counters.
func RuntimeOpportunityStats() OpportunityStats {
	if activeDetector == nil {
		return OpportunityStats{
			PageDivergence:     make(map[int]uint64),
			StrideDistribution: make(map[int64]uint64),
		}
	}
	return activeDetector.snapshot()
}

type detectorHook struct{}

func (h *detectorHook) Func(ctx hooking.HookCtx) {
	if activeDetector == nil ||
		ctx.Pos != tracing.HookPosTaskStart {
		return
	}
	task, ok := ctx.Item.(tracing.TaskStart)
	if !ok || task.Kind != tracing.ReqInTaskKind {
		return
	}
	member, ok := RequestMetadata(task.ID)
	if ok {
		activeDetector.observe(member)
	}
}

// AttachDetector observes complete vector-memory instruction groups at an L1
// TLB. AttachMetadataBridge must be registered first.
func AttachDetector(component tracing.NamedHookable) {
	component.AcceptHook(&detectorHook{})
}
