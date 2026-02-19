package graph

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// ChannelConsumerIndex maps a channel element-type string to all project
// functions that receive from a channel of that element type.
// This lets the traversal follow the implicit data flow through channels.
type ChannelConsumerIndex map[string][]*ssa.Function

// BuildChannelConsumerIndex scans every SSA function in prog for channel
// receive operations (v := <-ch and for v := range ch) and returns an index
// keyed by the element type of the channel.
//
// ssautil.AllFunctions is used instead of pkg.Members iteration because
// Members only covers top-level declarations — methods on types and anonymous
// goroutine closures live elsewhere in the SSA representation.
func BuildChannelConsumerIndex(prog *ssa.Program) ChannelConsumerIndex {
	index := make(ChannelConsumerIndex)

	for fn := range ssautil.AllFunctions(prog) {
		if fn == nil {
			continue
		}
		scanForReceives(fn, index)
	}

	return index
}

// scanForReceives records fn in the index under each channel element type it
// receives from.
func scanForReceives(fn *ssa.Function, index ChannelConsumerIndex) {
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			// select { case v := <-ch1: … case v := <-ch2: … } can have
			// multiple receive cases, each with a different element type.
			if sel, ok := instr.(*ssa.Select); ok {
				for _, state := range sel.States {
					if state.Dir == types.RecvOnly {
						if ch, ok := state.Chan.Type().Underlying().(*types.Chan); ok {
							elemType := ch.Elem().String()
							index[elemType] = appendUniqueFunc(index[elemType], fn)
						}
					}
				}
				continue
			}

			elemType := chanRecvElemType(instr)
			if elemType == "" {
				continue
			}
			index[elemType] = appendUniqueFunc(index[elemType], fn)
		}
	}
}

// chanRecvElemType returns the element-type string if instr is a simple
// channel receive (UnOp or Range), or "" otherwise.
// *ssa.Select is handled separately in scanForReceives because one select
// statement can carry multiple receive cases of different types.
func chanRecvElemType(instr ssa.Instruction) string {
	switch v := instr.(type) {
	case *ssa.UnOp:
		// v := <-ch
		if v.Op != token.ARROW {
			return ""
		}
		if ch, ok := v.X.Type().Underlying().(*types.Chan); ok {
			return ch.Elem().String()
		}
	case *ssa.Range:
		// for v := range ch
		if ch, ok := v.X.Type().Underlying().(*types.Chan); ok {
			return ch.Elem().String()
		}
	}
	return ""
}

// appendUniqueFunc appends fn only if it is not already in the slice.
func appendUniqueFunc(fns []*ssa.Function, fn *ssa.Function) []*ssa.Function {
	for _, f := range fns {
		if f == fn {
			return fns
		}
	}
	return append(fns, fn)
}
