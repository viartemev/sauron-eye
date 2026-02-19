package entrypoints

import (
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// grpcRegisterPattern is the qualified name of grpc.Server.RegisterService.
const grpcRegisterPattern = "(*google.golang.org/grpc.Server).RegisterService"

// FindGRPCHandlers scans for gRPC service registrations and extracts handler methods.
func FindGRPCHandlers(prog *ssa.Program, allFuncs []*ssa.Function, fset *token.FileSet) []DetectedEntry {
	var entries []DetectedEntry

	for _, fn := range allFuncs {
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				callInstr, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}

				call := callInstr.Common()
				qualName := calleeQualName(call)
				if qualName != grpcRegisterPattern {
					continue
				}

				// RegisterService(srv *grpc.Server, desc *ServiceDesc, impl interface{})
				// In SSA Args: [receiver_srv, desc, impl]
				args := call.Args
				if len(args) < 3 {
					continue
				}

				// impl is the concrete service implementation
				impl := args[len(args)-1]
				implType := impl.Type()

				// Walk the pointer chain to get the underlying named type
				namedType := extractNamedType(implType)
				if namedType == nil {
					continue
				}

				// Find all methods on this concrete type that match gRPC handler signatures
				mset := types.NewMethodSet(types.NewPointer(namedType))
				for i := 0; i < mset.Len(); i++ {
					sel := mset.At(i)
					methodObj, ok := sel.Obj().(*types.Func)
					if !ok {
						continue
					}

					// Skip methods that don't look like gRPC handlers
					// gRPC handlers take (context.Context, *RequestType) (ResponseType, error)
					if !looksLikeGRPCHandler(methodObj) {
						continue
					}

					// Find the *ssa.Function for this method
					ssaFn := prog.FuncValue(methodObj)
					if ssaFn == nil {
						continue
					}

					file, line := funcPosition(ssaFn, fset)
					entries = append(entries, DetectedEntry{
						Source:  "grpc",
						Method:  methodObj.Name(),
						Handler: ssaFn,
						File:    file,
						Line:    line,
					})
				}
			}
		}
	}

	return entries
}

// extractNamedType unwraps pointer types to find the underlying *types.Named.
func extractNamedType(t types.Type) *types.Named {
	switch v := t.(type) {
	case *types.Named:
		return v
	case *types.Pointer:
		if named, ok := v.Elem().(*types.Named); ok {
			return named
		}
	}
	return nil
}

// looksLikeGRPCHandler checks if the function signature resembles a gRPC handler.
// gRPC handlers have signature: func(ctx context.Context, req *Proto) (*Proto, error)
func looksLikeGRPCHandler(fn *types.Func) bool {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return false
	}
	params := sig.Params()
	results := sig.Results()

	// Must have 2 params and 2 results
	if params.Len() != 2 || results.Len() != 2 {
		return false
	}

	// First param should be context.Context
	firstParam := params.At(0).Type().String()
	if !strings.Contains(firstParam, "context.Context") {
		return false
	}

	// Last result should be error
	lastResult := results.At(1).Type().String()
	return lastResult == "error"
}
