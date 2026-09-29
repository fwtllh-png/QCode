package lsp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/symbols"
)

// A resident session keeps one language server per server binary alive across
// queries, so the second question about a repository answers without paying
// the startup again. The session is a host process, which is why the feature
// is off until a configuration says otherwise — the same trust stance stdio
// MCP takes.
//
// Queries on one session are serialized: the JSON-RPC client discards
// responses it does not recognize, so two concurrent calls on one server
// would eat each other's answers. The session lock spans the whole exchange,
// didOpen included, because the open-document state is as shared as the
// process.

// ResidentOptions bound the session pool.
type ResidentOptions struct {
	// IdleTimeout is how long an unused session survives before the janitor
	// closes it. Zero selects the default.
	IdleTimeout time.Duration
	// MaxServers bounds how many language servers one workspace keeps alive.
	MaxServers int
	// CacheCapacity bounds how many query results the pool remembers.
	CacheCapacity int
}

// Defaults for resident options left unset. The idle window trades a little
// host memory for not restarting a server between bursts of questions; the
// server bound keeps a polyglot repository from starting one server per
// language at once.
const (
	DefaultIdleTimeout   = 10 * time.Minute
	DefaultMaxServers    = 2
	DefaultCacheCapacity = 256
	// maxConsecutiveFailures is how often a server may crash on this pool
	// before the pool stops restarting it for the rest of its life: a server
	// that dies twice in a row is broken, not unlucky.
	maxConsecutiveFailures = 2
)

func (o ResidentOptions) withDefaults() ResidentOptions {
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultIdleTimeout
	}
	if o.MaxServers <= 0 {
		o.MaxServers = DefaultMaxServers
	}
	if o.CacheCapacity <= 0 {
		o.CacheCapacity = DefaultCacheCapacity
	}
	return o
}

// Resident is a pool of language-server sessions answering semantic queries.
// It implements symbols.Provider, so a caller that today hands a Checker to
// the search tools can hand a Resident instead and decide, by configuration,
// which one it built.
type Resident struct {
	base    Checker
	// root is the resolved workspace root every document path and every
	// returned location is measured against, captured once so the pool and
	// its sessions cannot disagree about it mid-life.
	root    string
	options ResidentOptions
	// resolver names the server binary for a path. The default consults the
	// installed server table; tests replace it to point at a fixture process.
	resolver func(path string) (ServerSpec, error)

	mu       sync.Mutex
	sessions map[string]*residentSession
	failures map[string]int
	cache    map[cacheKey]symbols.SemanticResult
	order    []cacheKey
	stop     chan struct{}
	stopOnce sync.Once
}

type residentSession struct {
	spec ServerSpec
	// exchange serializes every query on this session's process.
	exchange sync.Mutex
	client   *rpcClient
	server   string
	versions map[string]int
	texts    map[string]string
	lastUsed time.Time
}

type cacheKey struct {
	server    string
	method    string
	path      string
	line      int
	character int
	// digest pins the answer to the document's content: an edited file asks
	// again rather than being served what it used to say.
	digest string
}

// NewResident returns a pool over the workspace described by base. The pool
// starts nothing: a server exists once a query needs its language.
func NewResident(base Checker, options ResidentOptions) *Resident {
	options = options.withDefaults()
	_, root, err := resolveWorkspaceRoot(base.Sandbox, base.Root)
	if err != nil {
		root = base.Root
	}
	resident := &Resident{
		base: base, root: root, options: options, resolver: ResolveServer,
		sessions: make(map[string]*residentSession),
		failures: make(map[string]int),
		cache:    make(map[cacheKey]symbols.SemanticResult),
		stop:     make(chan struct{}),
	}
	go resident.janitor()
	return resident
}

// Definition resolves where a name is declared.
func (r *Resident) Definition(
	ctx context.Context, query symbols.SemanticQuery,
) (symbols.SemanticResult, error) {
	return r.query(ctx, "textDocument/definition", query, false)
}

// References resolves where a name is used.
func (r *Resident) References(
	ctx context.Context, query symbols.SemanticQuery, includeDeclaration bool,
) (symbols.SemanticResult, error) {
	return r.query(ctx, "textDocument/references", query, includeDeclaration)
}

// Close terminates the janitor and every live session. It is idempotent and
// safe to register on a resource stack.
func (r *Resident) Close() error {
	r.stopOnce.Do(func() { close(r.stop) })
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, session := range r.sessions {
		session.client.finish(500 * time.Millisecond)
		delete(r.sessions, name)
	}
	return nil
}

func (r *Resident) query(
	ctx context.Context,
	method string,
	query symbols.SemanticQuery,
	includeDeclaration bool,
) (symbols.SemanticResult, error) {
	if query.Path == "" || query.Line < 1 || query.Character < 1 {
		return symbols.SemanticResult{}, errors.New(
			"semantic query requires a relative path and 1-based line/character",
		)
	}
	spec, err := r.resolver(query.Path)
	if err != nil {
		return symbols.SemanticResult{}, err
	}
	path, text, err := semanticDocument(r.root, query.Path)
	if err != nil {
		return symbols.SemanticResult{}, err
	}
	digest := digestOf(text)
	key := cacheKey{
		server: spec.Name, method: method, path: query.Path,
		line: query.Line, character: query.Character, digest: digest,
	}
	r.mu.Lock()
	if cached, found := r.cache[key]; found {
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	result, err := r.exchange(ctx, spec, method, path, text, digest, query, includeDeclaration)
	if err != nil {
		return symbols.SemanticResult{}, err
	}
	r.remember(key, result)
	return result, nil
}

// exchange runs one query against the session for the server, opening or
// updating the document first. The whole exchange holds the session lock, and
// a failure retires the session so the next query starts a fresh one — twice,
// and the pool gives up on that server.
func (r *Resident) exchange(
	ctx context.Context,
	spec ServerSpec,
	method, path, text, digest string,
	query symbols.SemanticQuery,
	includeDeclaration bool,
) (symbols.SemanticResult, error) {
	session, err := r.acquire(ctx, spec)
	if err != nil {
		return symbols.SemanticResult{}, err
	}
	session.exchange.Lock()
	defer session.exchange.Unlock()

	uri := pathURI(path)
	opened, seen := session.texts[uri]
	switch {
	case !seen:
		session.versions[uri] = 1
		if err := session.client.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri, "languageId": languageID(path),
				"version": 1, "text": text,
			},
		}); err != nil {
			r.retire(spec)
			return symbols.SemanticResult{}, err
		}
	case opened != digest:
		session.versions[uri]++
		if err := session.client.notify("textDocument/didChange", map[string]any{
			"textDocument": map[string]any{"uri": uri, "version": session.versions[uri]},
			"contentChanges": []map[string]any{{"text": text}},
		}); err != nil {
			r.retire(spec)
			return symbols.SemanticResult{}, err
		}
	}
	session.texts[uri] = digest
	session.lastUsed = time.Now()

	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position": map[string]any{
			"line": query.Line - 1, "character": query.Character - 1,
		},
	}
	if method == "textDocument/references" {
		params["context"] = map[string]any{"includeDeclaration": includeDeclaration}
	}
	var raw json.RawMessage
	if err := session.client.call(ctx, method, params, &raw, nil); err != nil {
		r.retire(spec)
		return symbols.SemanticResult{}, err
	}
	locations, err := semanticLocations(session.client.root, raw)
	if err != nil {
		return symbols.SemanticResult{}, err
	}
	// The server answered a query, which is what "working" means here: the
	// crash count restarts from this moment, not from the session start.
	r.mu.Lock()
	r.failures[spec.Name] = 0
	r.mu.Unlock()
	return symbols.SemanticResult{
		Locations: locations, Source: "lsp:" + session.server,
		Confidence: "high",
	}, nil
}

// acquire returns the live session for a server, starting one when none
// exists. A pool at its bound retires the least recently used session rather
// than growing past it.
func (r *Resident) acquire(ctx context.Context, spec ServerSpec) (*residentSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session, found := r.sessions[spec.Name]; found {
		if session.healthy() {
			session.lastUsed = time.Now()
			return session, nil
		}
		// A dead session is a crash to count, not just litter to clear: the
		// pool restarts a server once, and refuses after it dies twice.
		session.client.finish(500 * time.Millisecond)
		delete(r.sessions, spec.Name)
		r.failures[spec.Name]++
	}
	if r.failures[spec.Name] >= maxConsecutiveFailures {
		return nil, fmt.Errorf(
			"language server %s stopped after %d consecutive crashes", spec.Name, r.failures[spec.Name],
		)
	}
	for len(r.sessions) >= r.options.MaxServers {
		var oldest *residentSession
		var oldestName string
		for name, candidate := range r.sessions {
			if oldest == nil || candidate.lastUsed.Before(oldest.lastUsed) {
				oldest, oldestName = candidate, name
			}
		}
		if oldest == nil {
			break
		}
		oldest.client.finish(500 * time.Millisecond)
		delete(r.sessions, oldestName)
	}

	checker := r.base
	checker.Binary = spec.Binary
	checker.Args = spec.Args
	client, err := checker.start(ctx)
	if err != nil {
		r.failures[spec.Name]++
		return nil, err
	}
	var initialized struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := client.call(ctx, "initialize", map[string]any{
		"processId": nil,
		"rootUri":   pathURI(client.root),
		"capabilities": map[string]any{"textDocument": map[string]any{
			"definition": map[string]any{"linkSupport": true},
			"references": map[string]any{},
		}},
	}, &initialized, nil); err != nil {
		client.finish(500 * time.Millisecond)
		r.failures[spec.Name]++
		return nil, fmt.Errorf("initialize language server: %w", err)
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		client.finish(500 * time.Millisecond)
		r.failures[spec.Name]++
		return nil, err
	}
	name := initialized.ServerInfo.Name
	if name == "" {
		name = spec.Name
	}
	session := &residentSession{
		spec: spec, client: client, server: name,
		versions: make(map[string]int), texts: make(map[string]string),
		lastUsed: time.Now(),
	}
	r.sessions[spec.Name] = session
	return session, nil
}

// retire drops a session that failed and counts the crash. The failed query
// already returned an error; the next one decides whether to restart.
func (r *Resident) retire(spec ServerSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session, found := r.sessions[spec.Name]; found {
		session.client.finish(500 * time.Millisecond)
		delete(r.sessions, spec.Name)
	}
	r.failures[spec.Name]++
}

// healthy reports whether the session's process is still answering — the
// done channel closes when the command has exited.
func (s *residentSession) healthy() bool {
	select {
	case <-s.client.done:
		return false
	default:
		return true
	}
}

// remember stores a result under its key, evicting the oldest entry when the
// capacity is reached.
func (r *Resident) remember(key cacheKey, result symbols.SemanticResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.cache[key]; !exists {
		r.order = append(r.order, key)
	}
	r.cache[key] = result
	for len(r.order) > r.options.CacheCapacity {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.cache, oldest)
	}
}

// janitor closes sessions that have been idle longer than the timeout. The
// sweep interval is half the idle window with a floor of one second, which
// bounds both the extra lifetime of a forgotten session and the checking
// cost; a disabled timeout (negative) leaves the janitor parked.
func (r *Resident) janitor() {
	interval := r.options.IdleTimeout / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-r.options.IdleTimeout)
			r.mu.Lock()
			for name, session := range r.sessions {
				if session.lastUsed.Before(cutoff) {
					session.client.finish(500 * time.Millisecond)
					delete(r.sessions, name)
				}
			}
			r.mu.Unlock()
		}
	}
}

func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
