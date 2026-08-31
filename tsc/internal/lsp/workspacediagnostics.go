package lsp

import (
	"context"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/collections"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/diagnostics"
	"github.com/microsoft/TypeScript/tsc/internal/json"
	"github.com/microsoft/TypeScript/tsc/internal/ls"
	"github.com/microsoft/TypeScript/tsc/internal/ls/lsconv"
	"github.com/microsoft/TypeScript/tsc/internal/ls/lsutil"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/project"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"github.com/zeebo/xxh3"
)

const (
	// How much of a streamed report is buffered before being flushed.
	workspaceDiagnosticsChunkFiles    = 100
	workspaceDiagnosticsChunkInterval = 500 * time.Millisecond

	// Each concurrent project holds its own diagnostics checker, so this bounds peak memory.
	workspaceDiagnosticsMaxProjects = 4
)

// workspaceDiagnosticsConcurrency returns how many projects to check at once, mirroring the default
// the build orchestrator uses for --builders: four, or one under single threaded mode. Unlike a
// build, a pull runs while the user is typing, so it also leaves half the processors for the
// requests they are waiting on.
func workspaceDiagnosticsConcurrency(work []workspaceDiagnosticsProject) int {
	for _, pf := range work {
		if pf.languageService.GetProgram().SingleThreaded() {
			return 1
		}
	}
	return min(len(work), workspaceDiagnosticsMaxProjects, max(1, runtime.GOMAXPROCS(0)/2))
}

type workspaceDiagnosticReport = lsproto.WorkspaceFullDocumentDiagnosticReportOrUnchangedDocumentDiagnosticReport

// workspaceDiagnosticsCache remembers which program version produced the result id a client holds
// for a file. A program is rebuilt as a unit, so an unchanged generation means every file in the
// project can be answered "unchanged" without checking it.
type workspaceDiagnosticsCache struct {
	mu       sync.Mutex
	settings workspaceDiagnosticsSettings
	entries  map[lsproto.DocumentUri]workspaceDiagnosticsCacheEntry
}

// workspaceDiagnosticsSettings is the settings an entry was computed under. Unlike the equivalent
// in the auto-import registry, which lists the preferences it depends on, this compares all of
// them: a preference that changes what a diagnostic says but is missing from such a list would
// leave stale errors in the client's problem list, which is worse than the occasional extra sweep.
type workspaceDiagnosticsSettings struct {
	preferences lsutil.UserPreferences
	locale      string
}

func (s workspaceDiagnosticsSettings) Equal(other workspaceDiagnosticsSettings) bool {
	return s.locale == other.locale && reflect.DeepEqual(s.preferences, other.preferences)
}

type workspaceDiagnosticsCacheEntry struct {
	project    tspath.Path
	generation uint64
	resultID   string
}

func newWorkspaceDiagnosticsCache() *workspaceDiagnosticsCache {
	return &workspaceDiagnosticsCache{entries: map[lsproto.DocumentUri]workspaceDiagnosticsCacheEntry{}}
}

// useSettings discards the cache if the settings behind it changed. Comparing the whole preference
// set rather than the fields known to matter means a new preference cannot silently leave stale
// entries in place, and comparing the snapshot's copy rather than reacting to a configuration
// notification means a pull already in flight cannot repopulate under settings that have moved on.
func (c *workspaceDiagnosticsCache) useSettings(settings workspaceDiagnosticsSettings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.settings.Equal(settings) {
		c.settings = settings
		c.entries = map[lsproto.DocumentUri]workspaceDiagnosticsCacheEntry{}
	}
}

// unchangedResultID returns the result id to acknowledge, if the client still holds what we last
// computed for this generation.
func (c *workspaceDiagnosticsCache) unchangedResultID(uri lsproto.DocumentUri, project tspath.Path, generation uint64, clientHolds string) (string, bool) {
	if clientHolds == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[uri]
	if !ok || entry.project != project || entry.generation != generation || entry.resultID != clientHolds {
		return "", false
	}
	return entry.resultID, true
}

func (c *workspaceDiagnosticsCache) store(uri lsproto.DocumentUri, entry workspaceDiagnosticsCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[uri] = entry
}

// retain drops everything the sweep did not report.
func (c *workspaceDiagnosticsCache) retain(reported *collections.Set[lsproto.DocumentUri]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for uri := range c.entries {
		if !reported.Has(uri) {
			delete(c.entries, uri)
		}
	}
}

func registerWorkspaceDiagnosticHandler(handlers handlerMap) {
	handlers[lsproto.WorkspaceDiagnosticInfo.Method] = func(s *Server, ctx context.Context, req *lsproto.RequestMessage) (func() error, error) {
		if s.session == nil {
			return nil, lsproto.ErrorCodeServerNotInitialized
		}
		params, err := lsproto.UnmarshalParams[*lsproto.WorkspaceDiagnosticParams](req)
		if err != nil {
			return nil, err
		}
		// A pull can run for minutes, so it stays off the dispatch loop.
		return func() error {
			defer s.recover(req)
			resp, lsErr := s.computeWorkspaceDiagnostics(ctx, params)
			if lsErr != nil {
				return lsErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return s.sendResult(req.ID, resp)
		}, nil
	}
}

func (s *Server) computeWorkspaceDiagnostics(ctx context.Context, params *lsproto.WorkspaceDiagnosticParams) (lsproto.WorkspaceDiagnosticResponse, error) {
	ctx = core.WithCheckerLifetime(ctx, core.CheckerLifetimeDiagnostics)
	run := newWorkspaceDiagnosticsRun(s, ctx, params)

	scope := s.session.Config().WorkspaceDiagnosticsScope
	// An empty (non-nil) set loads no trees beyond what is already loaded.
	var trees *collections.Set[tspath.Path]
	if scope != lsutil.WorkspaceDiagnosticsScopeAllProjects {
		trees = &collections.Set[tspath.Path]{}
		if scope == lsutil.WorkspaceDiagnosticsScopeOpenProjectsAndDependents {
			for _, open := range s.session.Snapshot().OpenProjects() {
				trees.Add(open.Id())
			}
		}
	}

	s.session.WithSnapshotLoadingProjectTree(ctx, trees, func(snapshot *project.Snapshot) {
		preferences := snapshot.UserPreferences()
		// A program generation cannot see a settings change, so the cache is keyed on them too.
		s.workspaceDiagnostics.useSettings(workspaceDiagnosticsSettings{
			preferences: preferences,
			locale:      s.GetLocale().String(),
		})
		if !scope.Enabled() || preferences.EnableValidation.IsFalse() {
			// Nothing is reported, and the cleanup pass below clears whatever the client holds.
			return
		}
		run.collect(snapshot, scope)
	})

	// A cancelled run covered only part of the workspace; the cleanup below would mistake the files
	// it never reached for files that no longer have diagnostics.
	if err := ctx.Err(); err != nil {
		run.endProgress()
		return nil, err
	}

	// Report empty for anything the client holds that no project reported, so it clears.
	for _, previous := range params.PreviousResultIds {
		if !run.reported.Has(previous.Uri) {
			run.add(workspaceDiagnosticReport{
				FullDocumentDiagnosticReport: &lsproto.WorkspaceFullDocumentDiagnosticReport{
					Uri:   previous.Uri,
					Items: []*lsproto.Diagnostic{},
				},
			})
		}
	}

	if run.collected {
		s.workspaceDiagnostics.retain(&run.reported)
	}

	// Checking can surface global diagnostics the owning tsconfig has not published yet.
	s.session.EnqueuePublishGlobalDiagnostics()

	return run.finish(), nil
}

// workspaceDiagnosticsRun accumulates the reports of one `workspace/diagnostic` request.
type workspaceDiagnosticsRun struct {
	server *Server
	ctx    context.Context

	partialResultToken *lsproto.IntegerOrString
	workDoneToken      *lsproto.IntegerOrString
	// Result ids the client already holds.
	previous map[lsproto.DocumentUri]string
	// Documents already covered, so a file in several projects is reported once.
	reported collections.Set[lsproto.DocumentUri]

	// Reports not yet flushed; without a partial result token this holds all of them.
	pending []workspaceDiagnosticReport
	// Paces flushes and progress so neither is sent per file.
	sinceTick int
	lastTick  time.Time

	filesDone  int
	filesTotal int
	begun      bool
	// Whether a sweep actually ran, so a disabled pull does not prune the cache.
	collected bool

	cache *workspaceDiagnosticsCache
}

func newWorkspaceDiagnosticsRun(server *Server, ctx context.Context, params *lsproto.WorkspaceDiagnosticParams) *workspaceDiagnosticsRun {
	previous := make(map[lsproto.DocumentUri]string, len(params.PreviousResultIds))
	for _, id := range params.PreviousResultIds {
		previous[id.Uri] = id.Value
	}
	return &workspaceDiagnosticsRun{
		server:             server,
		ctx:                ctx,
		partialResultToken: params.PartialResultToken,
		workDoneToken:      params.WorkDoneToken,
		previous:           previous,
		lastTick:           time.Now(),
		cache:              server.workspaceDiagnostics,
	}
}

// projectsInScope narrows the loaded projects to the ones the scope reports on, in snapshot order.
func projectsInScope(snapshot *project.Snapshot, scope lsutil.WorkspaceDiagnosticsScope) []*project.Project {
	all := snapshot.ProjectCollection.Projects()
	if scope == lsutil.WorkspaceDiagnosticsScopeAllProjects {
		return all
	}

	wanted := collections.Set[tspath.Path]{}
	for _, open := range snapshot.OpenProjects() {
		wanted.Add(open.Id())
	}
	if scope == lsutil.WorkspaceDiagnosticsScopeOpenProjectsAndDependents {
		// Walk reference edges backwards to a fixed point to find consumers of the open projects.
		// The graph is tiny, so repeated passes beat building a reverse index.
		for changed := true; changed; {
			changed = false
			for _, p := range all {
				if wanted.Has(p.Id()) {
					continue
				}
				for _, referenced := range p.ReferencedProjectPaths() {
					if wanted.Has(referenced) {
						wanted.Add(p.Id())
						changed = true
						break
					}
				}
			}
		}
	}

	inScope := make([]*project.Project, 0, wanted.Len())
	for _, p := range all {
		if wanted.Has(p.Id()) {
			inScope = append(inScope, p)
		}
	}
	return inScope
}

type workspaceDiagnosticsProject struct {
	languageService *ls.LanguageService
	project         *project.Project
	generation      uint64
	// Index aligned. A file answered from the cache has its report filled in and its entry nil.
	files   []*ast.SourceFile
	reports []workspaceDiagnosticReport
	toCheck int
}

// collect reports every file owned by every project in scope. Files within a project are checked
// one at a time because they share its single diagnostics checker, which exists to keep the walk
// order consistent; checker pools are per project, so whole projects run concurrently.
func (r *workspaceDiagnosticsRun) collect(snapshot *project.Snapshot, scope lsutil.WorkspaceDiagnosticsScope) {
	r.collected = true
	work := r.assignFilesToProjects(snapshot, projectsInScope(snapshot, scope))
	// Only files that still need checking count towards progress.
	r.filesTotal = 0
	for _, pf := range work {
		r.filesTotal += pf.toCheck
	}
	r.beginProgress()

	if concurrency := workspaceDiagnosticsConcurrency(work); concurrency > 1 {
		r.checkConcurrently(snapshot, work, concurrency)
	} else {
		r.checkSequentially(snapshot, work)
	}
}

// checkSequentially is the single threaded path: no goroutines are spawned at all, so a run can be
// stepped through. core.NewWorkGroup's single threaded form cannot serve here because it defers
// every task to RunAndWait, and this drains projects as they finish.
func (r *workspaceDiagnosticsRun) checkSequentially(snapshot *project.Snapshot, work []workspaceDiagnosticsProject) {
	for _, pf := range work {
		if pf.toCheck == 0 {
			r.emitProject(pf)
			continue
		}
		completed := r.checkProject(snapshot, pf)
		snapshot.ReleaseIdleDiagnosticsChecker(pf.project)
		if !completed {
			return
		}
		r.emitProject(pf)
	}
}

// checkConcurrently gives each project its own slot and drains them in project order as they fill,
// so reports stream as they finish but always come out in the same order.
func (r *workspaceDiagnosticsRun) checkConcurrently(snapshot *project.Snapshot, work []workspaceDiagnosticsProject, concurrency int) {
	completed := make([]bool, len(work))
	done := make([]chan struct{}, len(work))
	for i := range done {
		done[i] = make(chan struct{})
	}

	slots := make(chan struct{}, concurrency)
	wg := core.NewWorkGroup(false /*singleThreaded*/)
	for i, pf := range work {
		if pf.toCheck == 0 {
			// Answered entirely from the cache: no checker, no slot.
			completed[i] = true
			close(done[i])
			continue
		}
		wg.Queue(func() {
			defer close(done[i])
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-r.ctx.Done():
				return
			}
			// Give back the checker rather than let one that has visited every file idle out.
			defer snapshot.ReleaseIdleDiagnosticsChecker(pf.project)
			completed[i] = r.checkProject(snapshot, pf)
		})
	}

	for i, pf := range work {
		<-done[i]
		if !completed[i] {
			break
		}
		r.emitProject(pf)
	}
	wg.RunAndWait()
}

// checkProject fills in the reports for the files of one project, reporting whether it got through
// them all. A cancelled project must not be emitted: its remaining reports are still zero values.
func (r *workspaceDiagnosticsRun) checkProject(snapshot *project.Snapshot, pf workspaceDiagnosticsProject) bool {
	for j, file := range pf.files {
		if file == nil {
			continue
		}
		if r.ctx.Err() != nil {
			return false
		}
		pf.reports[j] = r.reportForFile(snapshot, pf.languageService, file)
	}
	return true
}

// emitProject hands a finished project's reports to the client and remembers which program version
// produced each result id, so the next pull can skip the file.
func (r *workspaceDiagnosticsRun) emitProject(pf workspaceDiagnosticsProject) {
	for j, report := range pf.reports {
		if pf.files[j] != nil {
			r.filesDone++
			if full := report.FullDocumentDiagnosticReport; full != nil && full.ResultId != nil {
				r.cache.store(lsconv.FileNameToDocumentURI(pf.files[j].FileName()), workspaceDiagnosticsCacheEntry{
					project:    pf.project.Id(),
					generation: pf.generation,
					resultID:   *full.ResultId,
				})
			}
		}
		r.add(report)
	}
}

// assignFilesToProjects decides which project reports which file and answers from the cache where
// it can. Enumerating files needs the program but not a checker, so this runs before any checking.
func (r *workspaceDiagnosticsRun) assignFilesToProjects(snapshot *project.Snapshot, projects []*project.Project) []workspaceDiagnosticsProject {
	var work []workspaceDiagnosticsProject
	for _, p := range projects {
		program := p.GetProgram()
		if program == nil {
			continue
		}
		// Id rather than ConfigFilePath: the inferred project has no config file and would panic.
		projectPath := p.Id()
		generation := p.ProgramLastUpdate
		languageService := ls.NewLanguageService(projectPath, program, snapshot, "")

		pf := workspaceDiagnosticsProject{languageService: languageService, project: p, generation: generation}
		for _, file := range languageService.WorkspaceDiagnosticFiles() {
			if handle := snapshot.GetFile(file.FileName()); handle != nil && handle.IsOverlay() {
				// The client pulls open documents directly. Reporting them here too would duplicate
				// every problem, since the client only reconciles the two within one provider.
				// Leaving the file out of `reported` clears anything it still holds.
				continue
			}
			uri := lsconv.FileNameToDocumentURI(file.FileName())
			if !r.reported.AddIfAbsent(uri) {
				continue
			}
			if resultID, ok := r.cache.unchangedResultID(uri, projectPath, generation, r.previous[uri]); ok {
				pf.files = append(pf.files, nil)
				pf.reports = append(pf.reports, workspaceDiagnosticReport{
					UnchangedDocumentDiagnosticReport: &lsproto.WorkspaceUnchangedDocumentDiagnosticReport{
						Uri:      uri,
						Version:  openDocumentVersion(snapshot, file.FileName()),
						ResultId: resultID,
					},
				})
				continue
			}
			pf.files = append(pf.files, file)
			pf.reports = append(pf.reports, workspaceDiagnosticReport{})
			pf.toCheck++
		}
		if len(pf.files) > 0 {
			work = append(work, pf)
		}
	}
	return work
}

func (r *workspaceDiagnosticsRun) reportForFile(snapshot *project.Snapshot, languageService *ls.LanguageService, file *ast.SourceFile) workspaceDiagnosticReport {
	uri := lsconv.FileNameToDocumentURI(file.FileName())
	items := languageService.ProvideDiagnosticsForFile(r.ctx, file)
	resultID := workspaceDiagnosticsResultID(items)
	version := openDocumentVersion(snapshot, file.FileName())

	if previous, ok := r.previous[uri]; ok && resultID != "" && previous == resultID {
		return workspaceDiagnosticReport{
			UnchangedDocumentDiagnosticReport: &lsproto.WorkspaceUnchangedDocumentDiagnosticReport{
				Uri:      uri,
				Version:  version,
				ResultId: resultID,
			},
		}
	}
	full := &lsproto.WorkspaceFullDocumentDiagnosticReport{
		Uri:     uri,
		Version: version,
		Items:   items,
	}
	if resultID != "" {
		full.ResultId = &resultID
	}
	return workspaceDiagnosticReport{FullDocumentDiagnosticReport: full}
}

func (r *workspaceDiagnosticsRun) add(report workspaceDiagnosticReport) {
	r.pending = append(r.pending, report)
	r.sinceTick++
	if r.sinceTick < workspaceDiagnosticsChunkFiles && time.Since(r.lastTick) < workspaceDiagnosticsChunkInterval {
		return
	}
	r.sinceTick = 0
	r.lastTick = time.Now()
	r.flush()
	r.reportProgress()
}

// flush streams buffered reports to the partial result token, if the client gave one.
func (r *workspaceDiagnosticsRun) flush() {
	if r.partialResultToken == nil || len(r.pending) == 0 {
		return
	}
	_ = sendNotification(r.server, lsproto.WorkspaceDiagnosticPartialResultInfo, &lsproto.WorkspaceDiagnosticPartialResultParams{
		Token: *r.partialResultToken,
		Value: lsproto.WorkspaceDiagnosticReportPartialResult{Items: r.pending},
	})
	r.pending = nil
}

func (r *workspaceDiagnosticsRun) finish() lsproto.WorkspaceDiagnosticResponse {
	// With a partial result token everything was streamed already; without one, pending holds it all.
	r.flush()
	r.endProgress()
	items := r.pending
	if items == nil {
		items = []workspaceDiagnosticReport{}
	}
	r.pending = nil
	return &lsproto.WorkspaceDiagnosticReport{Items: items}
}

func (r *workspaceDiagnosticsRun) beginProgress() {
	if r.workDoneToken == nil || r.filesTotal == 0 {
		return
	}
	r.begun = true
	r.sendProgress(lsproto.WorkDoneProgressBeginOrReportOrEnd{
		Begin: &lsproto.WorkDoneProgressBegin{
			Title:      diagnostics.Checking_workspace.Localize(r.server.GetLocale()),
			Percentage: new(uint32(0)),
		},
	})
}

func (r *workspaceDiagnosticsRun) reportProgress() {
	if !r.begun {
		return
	}
	r.sendProgress(lsproto.WorkDoneProgressBeginOrReportOrEnd{
		Report: &lsproto.WorkDoneProgressReport{
			Percentage: new(uint32(r.filesDone * 100 / r.filesTotal)),
		},
	})
}

func (r *workspaceDiagnosticsRun) endProgress() {
	if !r.begun {
		return
	}
	r.begun = false
	r.sendProgress(lsproto.WorkDoneProgressBeginOrReportOrEnd{End: &lsproto.WorkDoneProgressEnd{}})
}

func (r *workspaceDiagnosticsRun) sendProgress(value lsproto.WorkDoneProgressBeginOrReportOrEnd) {
	_ = sendNotification(r.server, lsproto.ProgressInfo, &lsproto.ProgressParams{
		Token: *r.workDoneToken,
		Value: value,
	})
}

// openDocumentVersion returns the LSP version of an open file, and null otherwise.
func openDocumentVersion(snapshot *project.Snapshot, fileName string) lsproto.IntegerOrNull {
	if handle := snapshot.GetFile(fileName); handle != nil && handle.IsOverlay() {
		return lsproto.IntegerOrNull{Integer: new(handle.Version())}
	}
	return lsproto.IntegerOrNull{}
}

// workspaceDiagnosticsResultID hashes a file's diagnostics, so the next pull can tell whether they
// moved. Returns "" if they cannot be hashed, which forces a full report.
func workspaceDiagnosticsResultID(items []*lsproto.Diagnostic) string {
	if len(items) == 0 {
		return "empty"
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return ""
	}
	return strconv.FormatUint(xxh3.Hash(encoded), 36)
}
