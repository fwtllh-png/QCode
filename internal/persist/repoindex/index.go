package repoindex

import (
	"context"
	"errors"
	"path"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
)

// IndexerVersion identifies the extraction rules that produced the stored rows.
// Raise it whenever a change to the symbol rules would make old rows disagree
// with new ones: the next refresh then rebuilds instead of trusting them.
//
// Version 2 added the layered extractor: signature, docstring and per-row
// resolution, file-level reference counts, C and C++ rule tables, and the
// generic engine that gives every other language heuristic rows.
//
// Version 3 added the reference graph: recorded import specifiers, resolved
// import and reference edges, file ranks, and entry-point classification on
// the file rows.
// Version 4 adds grammar-based declarations, identifiers and imports.
// Version 5 records scoped reference sites for Go and JS/TS.
const IndexerVersion = 5

// Index states a consumer can see.
const (
	// StatusReady means the stored rows describe the workspace as it is now.
	StatusReady = "ready"
	// StatusDegraded means the index could not be built or trusted. Symbol tools
	// report themselves unavailable; text search is unaffected.
	StatusDegraded = "degraded"
	// StatusDisabled means no index was configured for this session.
	StatusDisabled = "disabled"
	// StatusPending means the index exists but has not been built yet. It is not a
	// failure: the first query builds it.
	StatusPending = "pending"
)

// Defaults for an index whose options leave a bound unset.
const (
	DefaultMaxFiles     = 20000
	DefaultBatchSize    = 128
	defaultMaxFileBytes = 512 << 10
)

// Options bound the work one refresh may do.
type Options struct {
	// MaxFileBytes is the largest file whose contents are read.
	MaxFileBytes int64
	// MaxFiles bounds how many files are indexed. Beyond it the refresh reports
	// itself truncated rather than growing without limit.
	MaxFiles int
	// Concurrency is the number of files read and scanned at once.
	Concurrency int
	// BatchSize is how many files one write transaction carries.
	BatchSize int
	// SignatureMaxBytes, DocstringMaxBytes and ReferenceMaxCount carry the
	// extractor's detail bounds through from configuration; zero selects the
	// extractor defaults.
	SignatureMaxBytes int64
	DocstringMaxBytes int64
	ReferenceMaxCount int
	// Rank carries the graph bounds: damping, iteration limit and convergence
	// threshold for the file PageRank. Zero selects the defaults; the damping
	// default is the standard value from Brin & Page (1998).
	Rank RankOptions
	// Impact bounds the reverse dependency walk behind affected-test answers.
	// Zero selects the defaults.
	Impact ImpactOptions
	// Now replaces the clock in tests.
	Now func() time.Time
}

// Snapshot describes the index at the end of a refresh.
type Snapshot struct {
	Status string `json:"status"`
	Meta   Meta   `json:"meta"`
	// Detail explains a degraded status in the words a model or a reader needs.
	Detail string `json:"detail,omitempty"`
}

// Ready reports whether a symbol query can be answered from the index.
func (s Snapshot) Ready() bool { return s.Status == StatusReady }

// Index keeps the stored rows in step with the workspace and answers queries
// against them. A nil *Index behaves as a disabled index, so a caller that was
// configured without one needs no special case.
type Index struct {
	store   *Store
	walker  *repowalk.Walker
	options Options

	mu       sync.Mutex
	snapshot Snapshot
	// graphFiles is set only after a complete graph build. Comparing content
	// digests also handles a previous refresh canceled after writing file rows.
	graphFiles map[string]string
	// failures counts consecutive refreshes that could not trust the store. After
	// the second one the index stays degraded rather than rebuilding on every
	// call, because a database that fails twice will not start working.
	failures int
}

// maxResetAttempts is how often a refresh may discard the stored rows and start
// again before the index gives up for the rest of the session.
const maxResetAttempts = 2

// NewIndex returns an index over store, refreshed from walker.
func NewIndex(store *Store, walker *repowalk.Walker, options Options) (*Index, error) {
	if store == nil {
		return nil, errors.New("repository index requires a store")
	}
	if walker == nil {
		return nil, errors.New("repository index requires a workspace walker")
	}
	if options.MaxFileBytes <= 0 {
		options.MaxFileBytes = defaultMaxFileBytes
	}
	if options.MaxFiles <= 0 {
		options.MaxFiles = DefaultMaxFiles
	}
	if options.Concurrency <= 0 {
		options.Concurrency = min(4, max(1, runtime.NumCPU()))
	}
	if options.BatchSize <= 0 {
		options.BatchSize = DefaultBatchSize
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Index{store: store, walker: walker, options: options}, nil
}

// Ensure brings the index up to date and reports the resulting state. The first
// call builds it; later calls only re-read the files whose size or modification
// time moved, and confirm with a digest before rewriting anything.
func (i *Index) Ensure(ctx context.Context) (Snapshot, error) {
	if i == nil {
		return Snapshot{Status: StatusDisabled}, nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.failures >= maxResetAttempts {
		return i.snapshot, nil
	}
	snapshot, err := i.refresh(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// A cancelled refresh records no completion, so the next call picks up
			// where this one stopped rather than trusting a half-built index.
			return i.cancelled(), err
		}
		i.failures++
		i.snapshot = Snapshot{Status: StatusDegraded, Detail: err.Error()}
		// A store that cannot be read is discarded rather than trusted. The error
		// is not returned: a missing index degrades the session, it does not fail
		// the turn.
		_ = i.store.Reset(context.WithoutCancel(ctx))
		return i.snapshot, nil
	}
	i.failures = 0
	i.snapshot = snapshot
	return snapshot, nil
}

// Symbols answers a query after making sure the index is current. A degraded or
// disabled index yields its snapshot and no rows, which is what lets a tool
// report itself unavailable instead of returning a wrong answer.
func (i *Index) Symbols(ctx context.Context, query Query) ([]Symbol, Snapshot, error) {
	snapshot, err := i.Ensure(ctx)
	if err != nil || !snapshot.Ready() {
		return nil, snapshot, err
	}
	found, err := i.store.Symbols(ctx, query)
	if err != nil {
		return nil, Snapshot{Status: StatusDegraded, Detail: err.Error()}, nil
	}
	return found, snapshot, nil
}

// SymbolsWithTotal refreshes once and returns a bounded page with its filtered
// total. Unavailable indexes follow the same contract as Symbols.
func (i *Index) SymbolsWithTotal(ctx context.Context, query Query) ([]Symbol, int, Snapshot, error) {
	snapshot, err := i.Ensure(ctx)
	if err != nil || !snapshot.Ready() {
		return nil, 0, snapshot, err
	}
	found, total, err := i.store.SymbolsWithTotal(ctx, query)
	if err != nil {
		return nil, 0, Snapshot{Status: StatusDegraded, Detail: err.Error()}, nil
	}
	return found, total, snapshot, nil
}

// Paths returns the indexed paths, optionally restricted to one language.
func (i *Index) Paths(ctx context.Context, language string) ([]string, Snapshot, error) {
	snapshot, err := i.Ensure(ctx)
	if err != nil || !snapshot.Ready() {
		return nil, snapshot, err
	}
	paths, err := i.store.Paths(ctx, language)
	if err != nil {
		return nil, Snapshot{Status: StatusDegraded, Detail: err.Error()}, nil
	}
	return paths, snapshot, nil
}

// Files returns the indexed files by path.
func (i *Index) Files(ctx context.Context) (map[string]File, Snapshot, error) {
	snapshot, err := i.Ensure(ctx)
	if err != nil || !snapshot.Ready() {
		return nil, snapshot, err
	}
	files, err := i.store.Files(ctx)
	if err != nil {
		return nil, Snapshot{Status: StatusDegraded, Detail: err.Error()}, nil
	}
	return files, snapshot, nil
}

// Root is the workspace the index covers.
func (i *Index) Root() string {
	if i == nil {
		return ""
	}
	return i.store.Root()
}

// Snapshot reports the last known state without touching the workspace.
func (i *Index) Snapshot() Snapshot {
	if i == nil {
		return Snapshot{Status: StatusDisabled}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.snapshot.Status == "" {
		return Snapshot{Status: StatusPending, Detail: "the repository index builds on first use"}
	}
	return i.snapshot
}

func (i *Index) refresh(ctx context.Context) (Snapshot, error) {
	meta, found, err := i.store.Meta(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if found && meta.IndexerVersion != IndexerVersion {
		// The rules that produced these rows are gone; keeping them would mix two
		// vocabularies in one answer.
		if err := i.store.Reset(ctx); err != nil {
			return Snapshot{}, err
		}
		found = false
	}
	if !found {
		i.graphFiles = nil
	}
	existing, err := i.store.Files(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if !found {
		existing = map[string]File{}
	}

	listing, err := i.walker.List(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	entries := listing.Files
	truncated := false
	if len(entries) > i.options.MaxFiles {
		entries = entries[:i.options.MaxFiles]
		truncated = true
	}

	if err := i.reindex(ctx, i.stale(entries, existing)); err != nil {
		return Snapshot{}, err
	}
	if err := i.prune(ctx, entries, existing); err != nil {
		return Snapshot{}, err
	}

	files, err := i.store.Files(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	// The graph follows the file set: ranks a map would show must describe
	// the files the refresh just confirmed. A graph failure degrades the
	// ranks alone — the map falls back to declaration counts — and never
	// fails the refresh.
	if !i.graphMatches(files) {
		i.graphFiles = nil
		if err := i.rebuildGraph(ctx, files); err == nil {
			i.graphFiles = make(map[string]string, len(files))
			for path, file := range files {
				i.graphFiles[path] = file.Digest
			}
		}
	}
	total := 0
	for _, file := range files {
		total += file.SymbolCount
	}
	refreshed := Meta{
		IndexerVersion: IndexerVersion, Source: listing.Source,
		FileCount: len(files), SymbolCount: total,
		Truncated: truncated, RefreshedAt: i.options.Now(),
	}
	if err := i.store.SetMeta(ctx, refreshed); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Status: StatusReady, Meta: refreshed}, nil
}

func (i *Index) graphMatches(files map[string]File) bool {
	if i.graphFiles == nil || len(i.graphFiles) != len(files) {
		return false
	}
	for path, file := range files {
		if digest, ok := i.graphFiles[path]; !ok || digest != file.Digest {
			return false
		}
	}
	return true
}

// stale returns the entries that have to be read again. Size and modification
// time decide, because stat is cheap and reading every file on every refresh is
// not; the digest then decides whether anything is rewritten.
func (i *Index) stale(entries []repowalk.Entry, existing map[string]File) []repowalk.Entry {
	var stale []repowalk.Entry
	for _, entry := range entries {
		indexed, known := existing[entry.Path]
		if known && indexed.Size == entry.Size && indexed.Modified.Equal(entry.Modified) {
			continue
		}
		stale = append(stale, entry)
	}
	return stale
}

// reindex reads and scans the stale entries with bounded concurrency, writing
// them in batches so a large refresh never holds one long transaction against
// the shared database.
func (i *Index) reindex(ctx context.Context, stale []repowalk.Entry) error {
	if len(stale) == 0 {
		return nil
	}
	work := make(chan repowalk.Entry)
	results := make(chan Record)
	var producers sync.WaitGroup
	for worker := 0; worker < i.options.Concurrency; worker++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			for entry := range work {
				record, ok := i.scan(ctx, entry)
				if !ok {
					continue
				}
				select {
				case results <- record:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(work)
		for _, entry := range stale {
			select {
			case work <- entry:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		producers.Wait()
		close(results)
	}()

	batch := make([]Record, 0, i.options.BatchSize)
	var writeErr error
	for record := range results {
		if writeErr != nil {
			continue
		}
		batch = append(batch, record)
		if len(batch) < i.options.BatchSize {
			continue
		}
		writeErr = i.store.Apply(ctx, batch)
		batch = batch[:0]
	}
	if writeErr != nil {
		return writeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return i.store.Apply(ctx, batch)
}

// scan reads one file and extracts its declarations. A file the read policy
// rejects still enters the index with no symbols, so the next refresh does not
// read it again and consumers can still see that it exists.
//
// A file whose language is not identified is recorded as the generic tier and
// gets heuristic rows rather than none.
func (i *Index) scan(ctx context.Context, entry repowalk.Entry) (Record, bool) {
	language := symbols.Language(entry.Path)
	file := File{
		Path: entry.Path, Language: language, Size: entry.Size,
		Modified: entry.Modified, IndexedAt: i.options.Now(),
		EntryPoint: EntryPoint(entry.Path),
	}
	content, reason, err := i.walker.Read(entry, i.options.MaxFileBytes)
	if err != nil || reason == repowalk.SkipMissing {
		// A file that vanished between listing and reading is not indexed at all.
		return Record{}, false
	}
	if reason != repowalk.SkipNone {
		file.Digest = "skipped:" + string(reason)
		return Record{File: file}, true
	}
	file.Digest = content.Digest
	if language == "" {
		// The extension said nothing; a shebang may still name the language,
		// and failing that the generic tier gives the file heuristic rows.
		language = symbols.DetectLanguage(entry.Path, content.Data)
		if language == "" {
			language = symbols.LanguageGeneric
		}
		file.Language = language
	}
	extracted := symbols.ExtractResultContext(ctx, language, content.Data, symbols.Options{
		SourcePath:        entry.Path,
		SignatureMaxBytes: int(i.options.SignatureMaxBytes),
		DocstringMaxBytes: int(i.options.DocstringMaxBytes),
		ReferenceMaxCount: i.options.ReferenceMaxCount,
	})
	file.ScopeAware, file.PackageName = extracted.ScopeAware, extracted.PackageName
	file.ReferenceSitesTruncated = extracted.ReferenceSitesTruncated
	record := Record{File: file, ReferenceSites: extracted.ReferenceSites, Symbols: make([]Symbol, 0, len(extracted.Symbols))}
	for _, found := range extracted.Symbols {
		record.Symbols = append(record.Symbols, Symbol{
			Path: entry.Path, Name: found.Name, Kind: found.Kind,
			Container: found.Container, Line: found.Line, Exported: found.Exported,
			Signature: found.Signature, Docstring: found.Docstring,
			Resolution: found.Resolution,
		})
	}
	record.References = make([]Reference, 0, len(extracted.References))
	for _, counted := range extracted.References {
		record.References = append(record.References, Reference{
			Name: counted.Name, Count: counted.Count,
		})
	}
	// Import specifiers are recorded raw; the graph build resolves them once
	// the whole file set is confirmed.
	if extracted.Resolution == symbols.ResolutionSyntax {
		record.Imports = extracted.Imports
	} else {
		record.Imports = parseImports(language, content.Data)
	}
	return record, true
}

// prune drops the files the workspace no longer has, so a deleted definition
// stops being findable.
func (i *Index) prune(ctx context.Context, entries []repowalk.Entry, existing map[string]File) error {
	if len(existing) == 0 {
		return nil
	}
	present := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		present[entry.Path] = struct{}{}
	}
	var gone []string
	for path := range existing {
		if _, found := present[path]; !found {
			gone = append(gone, path)
		}
	}
	if len(gone) == 0 {
		return nil
	}
	sort.Strings(gone)
	for start := 0; start < len(gone); start += i.options.BatchSize {
		end := min(start+i.options.BatchSize, len(gone))
		if err := i.store.Delete(ctx, gone[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// Resolution labels grammar-derived syntax, lexical rules or generic heuristics.
// Consumers propagate the weakest tier instead of claiming semantic precision.
const (
	ResolutionHeuristic = symbols.ResolutionHeuristic
	ResolutionLexical   = symbols.ResolutionLexical
	ResolutionSyntax    = symbols.ResolutionSyntax
)

// WeakestResolution names the least trusted tier a result set carries, so a
// reply can state one resolution without hiding that some rows guess.
func WeakestResolution(found []Symbol) string {
	weakest := ""
	for _, symbol := range found {
		switch symbol.Resolution {
		case ResolutionHeuristic:
			return ResolutionHeuristic
		case ResolutionLexical:
			weakest = ResolutionLexical
		case ResolutionSyntax:
			if weakest == "" {
				weakest = ResolutionSyntax
			}
		}
	}
	return weakest
}

// cancelled reports the state to show when a build was interrupted.
func (i *Index) cancelled() Snapshot {
	if i.snapshot.Status != "" {
		return i.snapshot
	}
	return Snapshot{
		Status: StatusDegraded,
		Detail: "the repository index build was cancelled before it completed",
	}
}

// entryPointNames are the file names that usually start a program. The
// classification lives with the index — like the build-manifest list — so the
// map, the graph seeds and any other consumer answer it the same way.
var entryPointNames = map[string]struct{}{
	"main.go": {}, "main.py": {}, "__main__.py": {}, "main.rs": {},
	"main.ts": {}, "main.js": {}, "index.ts": {}, "index.js": {},
	"lib.rs": {}, "Main.java": {}, "main.c": {}, "main.cpp": {},
}

// EntryPoint reports whether a path is a recognized program entry point. Entry
// points seed the graph's personalized PageRank: a walk that restarts from
// them ranks what a program actually reaches over what merely sits in the
// repository.
func EntryPoint(candidate string) bool {
	_, found := entryPointNames[path.Base(candidate)]
	return found
}
