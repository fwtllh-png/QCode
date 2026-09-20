package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/repository"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func newPlatformBackend(options sandbox.Options) (sandbox.Backend, error) {
	return sandbox.NewPlatformBackend(options)
}

func openRuntimeStateStore(
	ctx context.Context,
	persistent *state.Store,
	workspace string,
	workspaceScoped bool,
) (*sqlitestate.Store, *sqlitestate.Store, string, error) {
	if persistent != nil {
		return persistent.SQLite(), nil, "", nil
	}
	var dir string
	var err error
	if workspaceScoped {
		root := strings.TrimSpace(workspace)
		if root == "" {
			root = "."
		}
		dir = filepath.Join(root, ".qcode")
		if err = os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, "", err
		}
	} else {
		dir, err = os.MkdirTemp("", "qcode-state-")
		if err != nil {
			return nil, nil, "", err
		}
	}
	store, err := sqlitestate.Open(ctx, filepath.Join(dir, "runtime-state.db"))
	if err != nil {
		if !workspaceScoped {
			_ = os.RemoveAll(dir)
		}
		return nil, nil, "", err
	}
	cleanupDir := ""
	if !workspaceScoped {
		cleanupDir = dir
	}
	return store, store, cleanupDir, nil
}

// openRepositoryIndex builds the repository index over whichever state database
// this session has. A session without a persistent store still gets one from the
// ephemeral database, so benchmarks and one-off runs exercise the same tools.
//
// A failure here is not fatal: the symbol tools report an unavailable index and
// text search is unaffected, which is a better session than none at all.
func openRepositoryIndex(
	workspace string,
	backend sandbox.Backend,
	store *sqlitestate.Store,
	settings config.Index,
) (*repoindex.Index, string) {
	if !settings.Enabled {
		return nil, repoindex.StatusDisabled
	}
	if store == nil {
		return nil, repoindex.StatusDisabled
	}
	root := strings.TrimSpace(workspace)
	if root == "" {
		root = "."
	}
	space, err := sandbox.NewWorkspace(root)
	if err != nil {
		return nil, repoindex.StatusDegraded
	}
	rows, err := repoindex.NewStore(store.DB(), space.Root())
	if err != nil {
		return nil, repoindex.StatusDegraded
	}
	walker, err := repowalk.New(space.Root(), backend)
	if err != nil {
		return nil, repoindex.StatusDegraded
	}
	index, err := repoindex.NewIndex(rows, walker, repoindex.Options{
		MaxFileBytes: settings.MaxFileBytes, MaxFiles: settings.MaxFiles,
		SignatureMaxBytes: settings.SignatureMaxBytes,
		DocstringMaxBytes: settings.DocstringMaxBytes,
		ReferenceMaxCount: settings.ReferenceMaxCount,
		Rank: repoindex.RankOptions{
			Damping:     settings.RankDamping,
			Iterations:  settings.RankIterations,
			Convergence: settings.RankConvergence,
		},
		Impact: repoindex.ImpactOptions{
			MaxDepth:   settings.ImpactMaxDepth,
			MaxResults: settings.ImpactMaxResults,
		},
	})
	if err != nil {
		return nil, repoindex.StatusDegraded
	}
	// The first build runs in the background rather than inside the first
	// query: that way it delays no session, including the ones that never
	// search, and Ensure's overlap dedupe shares this one build with any
	// query that arrives while it is still running.
	go func() {
		_, _ = index.Ensure(context.Background())
	}()
	return index, repoindex.StatusPending
}

// newRepoContext builds the provider that appends the repository map and working
// set to every request. It returns nil when both sections are off, which leaves
// requests shaped exactly as they were before the feature existed.
//
// A nil index is not an error: the map degrades to a line saying so and the
// working set, which is pure bookkeeping, is unaffected.
func newRepoContext(
	index *repoindex.Index,
	settings config.Context,
	budgets map[string]promptcontext.Budget,
	workspaceRoot string,
) *promptcontext.RepositoryProvider {
	if !settings.RepoMap.Enabled && !settings.WorkingSet.Enabled && !settings.Evidence.Enabled {
		return nil
	}
	scoped := make(map[string]promptcontext.Budget, len(budgets)+2)
	for kind, budget := range budgets {
		scoped[kind] = budget
	}
	if settings.RepoMap.MaxBytes > 0 {
		scoped[promptcontext.PartitionRepoMap] = promptcontext.Budget{
			MaxBytes: settings.RepoMap.MaxBytes,
		}
	}
	if settings.WorkingSet.MaxBytes > 0 {
		scoped[promptcontext.PartitionWorkingSetLedger] = promptcontext.Budget{
			MaxBytes: settings.WorkingSet.MaxBytes,
		}
	}
	if settings.Evidence.MaxBytes > 0 {
		scoped[promptcontext.PartitionEvidence] = promptcontext.Budget{
			MaxBytes: settings.Evidence.MaxBytes,
		}
	}
	// A typed nil *repoindex.Index would satisfy the interface and then panic on
	// use, so an absent index has to be passed as an untyped nil.
	var source repository.Index
	if index != nil {
		source = index
	}
	root := ""
	if absolute, err := filepath.Abs(workspaceRoot); err == nil {
		root = absolute
	}
	return promptcontext.NewRepositoryProvider(source, promptcontext.RepositoryOptions{
		RepoMap:    settings.RepoMap.Enabled,
		WorkingSet: settings.WorkingSet.Enabled,
		Evidence:   settings.Evidence.Enabled,
		Root:       root,
		Map: repository.Options{
			MaxDirectories: settings.RepoMap.MaxDirectories,
		},
		Budgets: scoped,
	})
}

// promptWorkingSet translates the host's vocabulary into the prompt assembler's.
func promptWorkingSet(files []ContextFile) []promptcontext.FileContext {
	if len(files) == 0 {
		return nil
	}
	converted := make([]promptcontext.FileContext, 0, len(files))
	for _, file := range files {
		converted = append(converted, promptcontext.FileContext{
			Path: file.Path, Content: file.Content, Critical: file.Critical,
		})
	}
	return converted
}

func writeMetricSnapshot(path string, metrics *telemetry.Metrics) error {
	data, err := json.MarshalIndent(metrics.Snapshot(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func loadRepositoryRules(path string) ([]policy.Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rules []policy.Rule
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("repository rules contain multiple JSON values")
		}
		return nil, err
	}
	if err := policy.ValidateRules(policy.SourceRepository, rules); err != nil {
		return nil, err
	}
	return rules, nil
}

type execRouteOptions struct {
	ProviderID string
	ModelID    string
	BaseURL    string
	Protocol   model.WireProtocol
	APIKeyEnv  string
	Credential model.CredentialRef
	Fixture    bool
	Model      *model.Model
}
