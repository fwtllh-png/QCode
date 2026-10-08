// Package execsettle isolates dynamic directory writes and settles them
// through the existing File Broker / Journal three-way merge.
package execsettle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	"github.com/fwtllh-png/QCode/internal/orchestration/workspacemerge"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	securitypaths "github.com/fwtllh-png/QCode/internal/security/pathpolicy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// Isolator starts a command-scoped isolated workspace.
type Isolator = tool.Isolator

// Workspace is one isolated execution root. Settle writes approved tree
// changes into the parent through Broker/Journal. It never git-resets the
// parent and never copies isolate files over user concurrent edits.
type Workspace = tool.IsolatedWorkspace

type WorkspaceBroker interface {
	workspacemerge.WorkspaceBroker
	InitContentBaseline(context.Context, string) error
}

type Options struct {
	Repository        string
	Scratch           string
	Parent            *filetool.Tools
	Journal           *workspacejournal.Manager
	Gate              workspacemerge.WorkspaceGate
	Brokers           WorkspaceBroker
	AllowApply        bool
	NewBackend        func(sandbox.Options) (sandbox.Backend, error)
	MaxMergeDiffBytes int
}

type Service struct {
	repository string
	scratch    string
	merger     *workspacemerge.Service
	brokers    WorkspaceBroker
	newBackend func(sandbox.Options) (sandbox.Backend, error)

	mu   sync.Mutex
	live map[string]*session
}

type session struct {
	service *Service
	id      string
	root    string
	trees   []string
	// shadow marks a discard-only session: Settle reports the planned
	// changes and never applies them to the parent.
	shadow  bool
	backend sandbox.Backend
}

func New(options Options) *Service {
	if strings.TrimSpace(options.Repository) == "" ||
		strings.TrimSpace(options.Scratch) == "" ||
		options.Brokers == nil {
		return nil
	}
	repository, err := filepath.EvalSymlinks(options.Repository)
	if err != nil {
		return nil
	}
	merger := workspacemerge.New(
		repository, options.Scratch, options.Parent, options.Journal,
		options.Gate, options.Brokers, options.AllowApply,
		workspacemerge.Options{IncludeIgnored: true, MaxDiffBytes: options.MaxMergeDiffBytes},
	)
	if merger == nil {
		return nil
	}
	return &Service{
		repository: repository,
		scratch:    options.Scratch,
		merger:     merger,
		brokers:    options.Brokers,
		newBackend: options.NewBackend,
		live:       make(map[string]*session),
	}
}

func (s *Service) Begin(
	ctx context.Context, id string, trees []string,
) (Workspace, error) {
	return s.begin(ctx, id, trees, false)
}

// BeginShadow starts an isolated workspace whose Settle reports planned
// changes and discards them: shadow verification never settles writes.
func (s *Service) BeginShadow(
	ctx context.Context, id string, trees []string,
) (Workspace, error) {
	return s.begin(ctx, id, trees, true)
}

func (s *Service) begin(
	ctx context.Context, id string, trees []string, shadow bool,
) (Workspace, error) {
	if s == nil {
		return nil, errors.New("isolated command settlement is unavailable")
	}
	id = isolateID(id)
	if id == "" || len(trees) == 0 {
		return nil, errors.New("isolated command settlement requires an id and write trees")
	}
	root := filepath.Join(s.scratch, "exec-isolate", id)
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return nil, err
	}
	prefixes := normalizeTrees(trees)
	current := &session{service: s, id: id, root: root, trees: prefixes, shadow: shadow}
	// Commands need the current filesystem, including ignored dependencies
	// and empty cache directories. A private baseline also keeps these files
	// out of the parent repository's Git object store.
	if err := s.provisionContent(ctx, current); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(current.root)
	if err != nil {
		_ = current.Close()
		return nil, err
	}
	current.root = canonical
	s.mu.Lock()
	s.live[id] = current
	s.mu.Unlock()
	return current, nil
}

func (s *session) Root() string { return s.root }

func (s *session) PrepareBackend(
	parent sandbox.Backend,
) (sandbox.Backend, func() error, error) {
	policy, ok := sandbox.BackendPolicy(parent)
	if !ok {
		return nil, nil, errors.New("isolated command settlement requires a sandbox policy")
	}
	if s.service.newBackend == nil {
		return nil, nil, errors.New("isolated command settlement requires a sandbox backend factory")
	}
	// The isolated backend keeps the parent's toolchain exposure and
	// preparer environment values. Skipping the host PATH scan must not
	// strip the toolchain binaries and cache variables that the command
	// was already authorized to use through the parent policy.
	inherited := sandbox.ToolchainExposure{
		BinDirs:     append([]string(nil), policy.Toolchains.BinDirs...),
		ReadRoots:   append([]string(nil), policy.Toolchains.ReadRoots...),
		ReadFiles:   append([]string(nil), policy.Toolchains.ReadFiles...),
		Environment: append([]string(nil), policy.Toolchains.Environment...),
	}
	backend, err := s.service.newBackend(sandbox.Options{
		WorkspaceRoot:          s.root,
		PrivateTemp:            policy.PrivateTemp,
		HostReadRoots:          append([]string(nil), policy.HostReadRoots...),
		HostReadFiles:          append([]string(nil), policy.HostReadFiles...),
		HostWriteRoots:         append([]string(nil), policy.HostWriteRoots...),
		ManagedProxyPort:       policy.ManagedProxyPort,
		ManagedProxyCredential: policy.ManagedProxyCredential,
		AllowNetwork:           policy.AllowNetwork,
		EnvironmentContract:    policy.EnvironmentContract,
		EnvironmentProfile:     policy.EnvironmentProfile,
		SharedUserTemp:         policy.SharedUserTemp,
		EnvironmentValues:      append([]string{}, policy.EnvironmentValues...),
		Toolchains:             &inherited,
		SkipPATHReadRoots:      true,
	})
	if err != nil {
		return nil, nil, err
	}
	s.backend = backend
	return backend, func() error { return sandbox.CloseBackend(backend) }, nil
}

func (s *session) Settle(ctx context.Context) ([]tool.WorkspaceChange, error) {
	plan, err := s.service.merger.PlanPaths(ctx, s.root, s.trees)
	if err != nil {
		if errors.Is(err, workspacemerge.ErrWorkspaceClean) {
			return nil, nil
		}
		return nil, err
	}
	if s.shadow {
		// Shadow verification summarizes the plan and discards every write:
		// nothing reaches the parent, the journal, or the workspace gate.
		return editPlanChanges(plan), nil
	}
	applied, err := s.service.merger.ApplyPaths(
		ctx, "exec-settle-"+s.id, s.root, plan.ID, s.trees,
	)
	if err != nil {
		return nil, err
	}
	return editPlanChanges(applied), nil
}

func editPlanChanges(plan tool.EditPlan) []tool.WorkspaceChange {
	changes := make([]tool.WorkspaceChange, 0, len(plan.Files))
	for _, file := range plan.Files {
		kind := tool.WorkspaceModified
		switch {
		case !file.BeforeExists && file.AfterExists:
			kind = tool.WorkspaceCreated
		case file.BeforeExists && !file.AfterExists:
			kind = tool.WorkspaceDeleted
		}
		changes = append(changes, tool.WorkspaceChange{Path: file.Path, Kind: kind})
	}
	return changes
}

func (s *session) Close() error {
	if s == nil {
		return nil
	}
	s.service.mu.Lock()
	delete(s.service.live, s.id)
	s.service.mu.Unlock()
	var backendErr error
	if s.backend != nil {
		backendErr = sandbox.CloseBackend(s.backend)
		s.backend = nil
	}
	return errors.Join(backendErr, os.RemoveAll(s.root))
}

func (s *Service) provisionContent(ctx context.Context, current *session) error {
	if err := copyWorkspace(ctx, s.repository, current.root); err != nil {
		return err
	}
	if err := s.brokers.InitContentBaseline(ctx, current.root); err != nil {
		return err
	}
	return nil
}

func copyWorkspace(ctx context.Context, source, target string) error {
	if resolved, err := filepath.EvalSymlinks(source); err == nil {
		source = resolved
	}
	classifier, err := securitypaths.NewControlPlane(source)
	if err != nil {
		return err
	}
	return filepath.WalkDir(classifier.Workspace(), func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(classifier.Workspace(), path)
		if err != nil {
			relative, err = filepath.Rel(source, path)
			if err != nil {
				return err
			}
		}
		relative = filepath.Clean(relative)
		if relative == "." {
			return os.MkdirAll(target, 0o700)
		}
		if _, protected, err := classifier.Classify(path); err != nil {
			return err
		} else if protected {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		destination := filepath.Join(target, relative)
		if entry.Type()&fs.ModeSymlink != 0 {
			// Recreate symlinks as-is (absolute targets stay absolute):
			// builds that rely on them (include shims, .bin links) must see
			// the same shape in the isolate. WalkDir never descends into a
			// symlink, so a dangling target copies as a dangling link —
			// matching the parent's behavior.
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			return os.Symlink(link, destination)
		}
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		return copyRegularFile(ctx, path, destination)
	})
}

func copyRegularFile(ctx context.Context, source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, cancelableReader{ctx: ctx, reader: in}); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

type cancelableReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r cancelableReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func isolateID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

func normalizeTrees(trees []string) []string {
	unique := make(map[string]struct{}, len(trees))
	result := make([]string, 0, len(trees))
	for _, tree := range trees {
		tree = filepath.ToSlash(filepath.Clean(strings.TrimSpace(tree)))
		if tree == "" || tree == "." {
			continue
		}
		if _, exists := unique[tree]; exists {
			continue
		}
		unique[tree] = struct{}{}
		result = append(result, tree)
	}
	return result
}
