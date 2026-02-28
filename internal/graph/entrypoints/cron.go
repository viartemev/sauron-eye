package entrypoints

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// cronPatterns maps qualified method names to their handler argument index.
// In SSA, Args[0] is the receiver for method calls.
var cronPatterns = map[string]int{
	// robfig/cron v3 — Cron.AddFunc(spec, cmd func())
	"(*github.com/robfig/cron/v3.Cron).AddFunc": 2, // Args: [receiver, spec, func]
	// robfig/cron v2
	"(*github.com/robfig/cron.Cron).AddFunc": 2,
	// gocron — Scheduler.Do(jobFun interface{}, params ...interface{})
	"(*github.com/go-co-op/gocron.Scheduler).Do": 1,
}

// namedCronPattern describes a registration call that includes both a string name
// and a handler function arg, so we can populate DetectedEntry.Path with the task name.
type namedCronPattern struct {
	handlerIdx int
	nameIdx    int
}

// cronNamedPatterns are registration calls of the form (receiver, ..., name, handler).
var cronNamedPatterns = map[string]namedCronPattern{
	// libscheduler (internal Magnit library) — Scheduler.RegisterAction(ctx, name, actionFunc)
	// Args: [receiver, ctx, name, actionFunc]
	"(*gitlab.platform.corp/magnitonline/ecom/backend/libs/libscheduler.Scheduler).RegisterAction": {handlerIdx: 3, nameIdx: 2},
}

// cronJobPatterns maps qualified method names where the arg implements cron.Job interface.
// The Job interface has a Run() method, so we look for that.
var cronJobPatterns = map[string]int{
	// robfig/cron v3 — Cron.AddJob(spec, job Job)
	"(*github.com/robfig/cron/v3.Cron).AddJob": 2, // Args: [receiver, spec, job]
	"(*github.com/robfig/cron.Cron).AddJob":    2,
}

// FindCronJobs scans for cron job registrations and returns their handlers.
func FindCronJobs(prog *ssa.Program, allFuncs []*ssa.Function, fset *token.FileSet) []DetectedEntry {
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

				// AddFunc style — direct function argument
				if handlerArgIdx, matched := cronPatterns[qualName]; matched {
					args := call.Args
					if len(args) <= handlerArgIdx {
						continue
					}

					handler := extractFunction(args[handlerArgIdx])
					if handler == nil {
						continue
					}

					file := ""
					line := 0
					if pos := instr.Pos(); pos.IsValid() {
						p := fset.Position(pos)
						file = p.Filename
						line = p.Line
					}

					entries = append(entries, DetectedEntry{
						Source:  "cron",
						Handler: handler,
						File:    file,
						Line:    line,
					})
					continue
				}

				// Named registration style — also captures the task name as Path
				if p, matched := cronNamedPatterns[qualName]; matched {
					args := call.Args
					if len(args) <= p.handlerIdx {
						continue
					}

					handler := extractFunction(args[p.handlerIdx])
					if handler == nil {
						continue
					}

					name := ""
					if p.nameIdx < len(args) {
						name = extractStringConst(args[p.nameIdx])
					}

					file := ""
					line := 0
					if pos := instr.Pos(); pos.IsValid() {
						pp := fset.Position(pos)
						file = pp.Filename
						line = pp.Line
					}

					entries = append(entries, DetectedEntry{
						Source:  "cron",
						Path:    name,
						Handler: handler,
						File:    file,
						Line:    line,
					})
					continue
				}

				// AddJob style — finds the Run() method on the Job implementation
				if jobArgIdx, matched := cronJobPatterns[qualName]; matched {
					args := call.Args
					if len(args) <= jobArgIdx {
						continue
					}

					jobVal := args[jobArgIdx]
					runFn := findRunMethod(prog, jobVal)
					if runFn == nil {
						continue
					}

					file := ""
					line := 0
					if pos := instr.Pos(); pos.IsValid() {
						p := fset.Position(pos)
						file = p.Filename
						line = p.Line
					}

					entries = append(entries, DetectedEntry{
						Source:  "cron",
						Handler: runFn,
						File:    file,
						Line:    line,
					})
				}
			}
		}
	}

	return entries
}

// findRunMethod finds the Run() method on the concrete type of jobVal.
func findRunMethod(prog *ssa.Program, jobVal ssa.Value) *ssa.Function {
	// Try to get the concrete type
	t := jobVal.Type()
	namedType := extractNamedType(t)
	if namedType == nil {
		return nil
	}

	// Look for a Run() method with no parameters and no return values
	for i := 0; i < namedType.NumMethods(); i++ {
		method := namedType.Method(i)
		if method.Name() == "Run" {
			if fn := prog.FuncValue(method); fn != nil {
				return fn
			}
		}
	}

	return nil
}
