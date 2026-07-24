package latpc

import (
	"testing"

	"github.com/sarchlab/akita/v5/hooking"
	"github.com/sarchlab/akita/v5/tracing"
)

func TestMetadataRegistryMovesWithoutLeakingParent(t *testing.T) {
	registry := NewMetadataRegistry()
	member := GroupMember{
		InstructionID: 9,
		Position:      2,
		Count:         4,
		VAddr:         0x3000,
		LaneMask:      0xf0,
	}
	registry.Put(10, member)

	if !registry.Move(10, 11) {
		t.Fatal("metadata move failed")
	}
	if _, ok := registry.Get(10); ok {
		t.Fatal("parent metadata leaked")
	}
	if got, ok := registry.Get(11); !ok || got != member {
		t.Fatalf("child metadata: got %+v, %t", got, ok)
	}
}

func TestMetadataBridgeFollowsRequestTaskChain(t *testing.T) {
	registry := NewMetadataRegistry()
	member := GroupMember{InstructionID: 42, Count: 1}
	registry.Put(100, member)
	bridge := &metadataBridge{registry: registry}

	bridge.Func(hooking.HookCtx{
		Pos: tracing.HookPosTaskStart,
		Item: tracing.TaskStart{
			ID: 200, ParentID: 100, Kind: tracing.ReqInTaskKind,
		},
	})
	bridge.Func(hooking.HookCtx{
		Pos: tracing.HookPosTaskStart,
		Item: tracing.TaskStart{
			ID: 300, ParentID: 200, Kind: tracing.ReqOutTaskKind,
		},
	})

	if got, ok := registry.Get(300); !ok || got != member {
		t.Fatalf("metadata did not reach child request: %+v, %t", got, ok)
	}
	if registry.Len() != 1 {
		t.Fatalf("registry has %d records, want 1", registry.Len())
	}
}

func TestGroupMemberLast(t *testing.T) {
	if (GroupMember{Position: 1, Count: 3}).Last() {
		t.Fatal("middle member reported last")
	}
	if !(GroupMember{Position: 2, Count: 3}).Last() {
		t.Fatal("final member not reported last")
	}
}
