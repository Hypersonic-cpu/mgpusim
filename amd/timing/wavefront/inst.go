// Package wavefront defines concepts related to a wavefront.
package wavefront

import (
	"github.com/sarchlab/akita/v5/timing"
	"github.com/sarchlab/mgpusim/v5/amd/insts"
)

// Inst in the timing package is a wrapper of the insts.Inst.
type Inst struct {
	*insts.Inst

	ID uint64

	// VectorMemDataReady is set once every coalesced vector-memory
	// transaction for this dynamic instruction has returned. The CU retires
	// ready instructions in issue order to implement s_waitcnt semantics.
	VectorMemDataReady bool
}

// NewInst creates a newly created Inst
func NewInst(raw *insts.Inst) *Inst {
	i := new(Inst)
	i.Inst = raw

	i.ID = timing.GetIDGenerator().Generate()

	return i
}
