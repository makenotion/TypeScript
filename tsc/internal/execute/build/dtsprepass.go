package build

import (
	"context"
	"os"

	"github.com/microsoft/TypeScript/tsc/internal/compiler"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/diagnostics"
)

// EXPERIMENTAL. Gated behind an environment variable rather than a real option while the
// approach is being evaluated; a shipping version needs a proper command line flag.
func dtsPrepassEnabled() bool {
	return os.Getenv("TSGO_EXPERIMENTAL_DTS_PREPASS") == "1" //nolint:forbidigo
}

// canPrepassDeclarations reports whether this project's .d.ts can be produced without its
// upstream projects' outputs being present.
//
// isolatedDeclarations guarantees every export carries enough annotation that declaration
// emit never falls back to cross-file type inference, so the output does not depend on
// upstream types. Without it the same emit silently produces `any` for inferred exports.
func (t *BuildTask) canPrepassDeclarations(orchestrator *Orchestrator) bool {
	if t.resolved == nil {
		return false
	}
	options := t.resolved.CompilerOptions()
	if !options.IsolatedDeclarations.IsTrue() || !options.GetEmitDeclarations() {
		return false
	}
	// Solution-only projects have nothing to emit.
	if len(t.resolved.FileNames()) == 0 {
		return false
	}
	// Content mappers can fail to resolve, which the pre-pass has no way to report.
	if len(t.resolved.ContentMappers()) != 0 {
		return false
	}
	// Only projects with no buildinfo. This is what makes skipping the upstream wait safe:
	// getUpToDateStatus returns OutputMissing at the buildinfo check, before the loop that
	// dereferences upstream task status, which is not yet computed during the pre-pass.
	return !orchestrator.host.FS().FileExists(t.resolved.GetBuildInfoFileName())
}

// prepassDeclarations emits only this project's .d.ts, without waiting on upstream and
// without type checking. Reports whether the declarations were fully written.
func (t *BuildTask) prepassDeclarations(orchestrator *Orchestrator) bool {
	host := &compilerHost{
		host:  orchestrator.host,
		trace: func(msg *diagnostics.Message, args ...any) {},
	}
	program := compiler.NewProgram(compiler.ProgramOptions{
		Config: t.resolved,
		Host:   host,
	})
	// Deliberately never asks for semantic diagnostics; the checker is consulted only for
	// whatever declaration emit itself needs.
	result := program.Emit(context.Background(), compiler.EmitOptions{
		EmitOnly: compiler.EmitOnlyDts,
		WriteFile: func(fileName string, text string, data *compiler.WriteFileData) error {
			return orchestrator.host.FS().WriteFile(fileName, text)
		},
	})
	// Declaration emit diagnostics (isolatedDeclarations violations included) block the
	// .d.ts write, so treat any diagnostic as not covered and let the main pass report it.
	return result != nil && !result.EmitSkipped && len(result.Diagnostics) == 0
}

// emitDeclarationsPrepass emits .d.ts for every eligible project concurrently, ignoring the
// reference graph entirely, so the main pass can check projects without being serialized by
// graph depth.
func (o *Orchestrator) emitDeclarationsPrepass() {
	if !dtsPrepassEnabled() ||
		o.opts.Command.CompilerOptions.Watch.IsTrue() ||
		o.opts.Command.BuildOptions.Clean.IsTrue() ||
		o.opts.Command.BuildOptions.Dry.IsTrue() ||
		// Upstream error propagation reads upstream task status before the buildinfo check.
		o.opts.Command.BuildOptions.StopBuildOnErrors.IsTrue() {
		return
	}

	var eligible []*BuildTask
	for _, config := range o.Order() {
		task := o.getTask(o.toPath(config))
		if task.canPrepassDeclarations(o) {
			eligible = append(eligible, task)
		}
	}
	if len(eligible) < 2 {
		return
	}

	wg := core.NewWorkGroup(o.opts.Command.CompilerOptions.SingleThreaded.IsTrue())
	for _, task := range eligible {
		wg.Queue(func() {
			if task.prepassDeclarations(o) {
				task.dtsPrepassed.Store(true)
			}
		})
	}
	wg.RunAndWait()
}

// canSkipUpstreamWait reports whether this project can start without waiting on upstream.
//
// Requires this project to have been pre-passed itself, not just its upstreams: that is what
// guarantees it has no buildinfo, so getUpToDateStatus short-circuits at OutputMissing rather
// than reading upstream task status that no longer has a happens-before edge.
func (t *BuildTask) canSkipUpstreamWait() bool {
	if !t.dtsPrepassed.Load() {
		return false
	}
	for _, upstream := range t.upStream {
		if !upstream.task.dtsPrepassed.Load() {
			return false
		}
	}
	return true
}
