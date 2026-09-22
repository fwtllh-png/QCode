package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/controlplane"
)

const ErrUnavailableCode = "sandbox_unavailable"

// MaxExactWorkspaceWritePaths bounds one explicitly approved sandbox policy.
// Exact write files, files settled under write trees, argument expansion,
// tool schema validation, and backend policy generation share this value so
// an approved call cannot fail at a later boundary.
const MaxExactWorkspaceWritePaths = 512

type Capability struct {
	Platform  string               `json:"platform"`
	Backend   string               `json:"backend"`
	Available bool                 `json:"available"`
	Effective controlmatrix.Matrix `json:"effective_controls"`
	// ManagedProxy is advertised only after an exact-port allow/deny probe.
	ManagedProxy bool   `json:"managed_proxy,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type Command struct {
	Path                    string
	Args                    []string
	Dir                     string
	Env                     []string
	DirectoryFD             int
	WorkspaceReadOnly       bool
	AdditionalReadPaths     []string
	WorkspaceWritePaths     []string
	WorkspaceHiddenPaths    []string
	DenyNetwork             bool
	AllowLoopback           bool
	LoopbackOnly            bool // Removes the workspace proxy grant for this command.
	AuthorityDigest         string
	PreparedPolicyID        string
	PreparedAuthorityDigest string
	PreparedControls        controlmatrix.Matrix
	PreparedReadOnly        bool
	PreparedReadPaths       []string
	PreparedWritePaths      []string
	PreparedHiddenPaths     []string
	PreparedNetworkDenied   bool
	PreparedLoopbackAllowed bool
	PreparedProxyPort       uint16
	// SessionProxyPort is the Process Session loopback port allocated by the
	// Workspace proxy. Prepare must write this exact port into Seatbelt and
	// PreparedProxyPort; it must not keep the Workspace shared port.
	SessionProxyPort uint16
}

type Backend interface {
	Capability() Capability
	Prepare(context.Context, Command) (Command, error)
}

type PolicyBackend interface {
	Backend
	Policy() Policy
}

// maxBackendWrapperDepth bounds how many backend wrapper layers
// BackendPolicy unwraps. The construction chain (managed, session-bound,
// close-binding, policy-binding) is four deep today; the bound stops a
// self-referential wrapper from looping forever. Public contract constant.
const maxBackendWrapperDepth = 8

func BackendPolicy(backend Backend) (Policy, bool) {
	current := backend
	for range maxBackendWrapperDepth {
		if current == nil {
			return Policy{}, false
		}
		if policyBackend, ok := current.(PolicyBackend); ok {
			policy := policyBackend.Policy()
			if policy.ID != "" {
				return policy, true
			}
		}
		wrapper, ok := current.(interface{ InnerBackend() Backend })
		if !ok {
			return Policy{}, false
		}
		next := wrapper.InnerBackend()
		if next == nil || next == current {
			return Policy{}, false
		}
		current = next
	}
	return Policy{}, false
}

type UnavailableError struct {
	Capability Capability
}

func (e *UnavailableError) Error() string {
	message := fmt.Sprintf(
		"%s: required OS sandbox controls are unavailable on %s (backend=%s)",
		ErrUnavailableCode,
		e.Capability.Platform,
		e.Capability.Backend,
	)
	if e.Capability.Reason != "" {
		message += ": " + e.Capability.Reason
	}
	return message
}

func DefaultProcessRequirements() controlmatrix.Requirements {
	return controlmatrix.Requirements{
		FilesystemRead:  controlmatrix.FilesystemReadDeclaredRoots,
		FilesystemWrite: controlmatrix.FilesystemWriteExactPaths,
		Network:         controlmatrix.NetworkDenied,
		ProcessTree:     controlmatrix.ProcessTreeGroupKill,
		PathIdentity:    controlmatrix.PathIdentityDescriptorRelative,
	}
}

func RequireControls(
	backend Backend,
	required controlmatrix.Requirements,
) error {
	capability := Capability{
		Platform: runtime.GOOS, Backend: "none",
	}
	if backend != nil {
		capability = backend.Capability()
	}
	if !capability.Available {
		return &UnavailableError{Capability: capability}
	}
	if err := required.SatisfiedBy(capability.Effective); err != nil {
		capability.Reason = err.Error()
		return &UnavailableError{Capability: capability}
	}
	return nil
}

func Probe() Capability {
	probeOnce.Do(func() {
		probedCapability = runAttackProbe()
	})
	return probedCapability
}

func platformControls(platform string) controlmatrix.Matrix {
	controls := controlmatrix.Matrix{
		FilesystemRead:  controlmatrix.FilesystemReadUnrestricted,
		FilesystemWrite: controlmatrix.FilesystemWriteUnrestricted,
		Network:         controlmatrix.NetworkDirect,
		ProcessTree:     controlmatrix.ProcessTreeUnmanaged,
		CrossProcess:    controlmatrix.CrossProcessUnrestricted,
		Syscall:         controlmatrix.SyscallUnrestricted,
		IPC:             controlmatrix.IPCUnrestricted,
		PathIdentity:    controlmatrix.PathIdentityLexical,
		ArtifactOrigin:  controlmatrix.ArtifactOriginUnverifiedPath,
		DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly,
	}
	switch platform {
	case "darwin":
		controls.FilesystemRead = controlmatrix.FilesystemReadDeclaredRoots
		controls.FilesystemWrite = controlmatrix.FilesystemWriteExactPaths
		controls.Network = controlmatrix.NetworkDenied
		controls.ProcessTree = controlmatrix.ProcessTreeGroupKill
		controls.PathIdentity = controlmatrix.PathIdentityDescriptorRelative
	}
	return controls
}

var (
	probeOnce        sync.Once
	probedCapability Capability
)

func NewPlatformBackend(options Options) (Backend, error) {
	policy, err := BuildPolicy(options)
	if err != nil {
		return nil, err
	}
	workspace, err := NewWorkspace(policy.WorkspaceRoot)
	if err != nil {
		// BuildPolicy may have created (and now owns) the private temp.
		_ = closePolicyTemp(policy)
		return nil, err
	}
	capability := Probe()
	if !capability.Available {
		return &unavailableBackend{capability: capability, policy: policy}, nil
	}
	switch capability.Backend {
	case "seatbelt":
		return &seatbeltBackend{workspace: workspace, policy: policy, capability: capability}, nil
	default:
		return &unavailableBackend{capability: capability, policy: policy}, nil
	}
}

type unavailableBackend struct {
	capability Capability
	policy     Policy
}

func (b *unavailableBackend) Capability() Capability {
	return b.capability
}

func (b *unavailableBackend) Policy() Policy { return b.policy }

func (b *unavailableBackend) Close() error { return closePolicyTemp(b.policy) }

func (b *unavailableBackend) Prepare(context.Context, Command) (Command, error) {
	return Command{}, &UnavailableError{Capability: b.capability}
}

type seatbeltBackend struct {
	workspace  *Workspace
	policy     Policy
	capability Capability
}

func (b *seatbeltBackend) Capability() Capability {
	return b.capability
}

func (b *seatbeltBackend) Policy() Policy { return b.policy }

func (b *seatbeltBackend) Close() error { return closePolicyTemp(b.policy) }

func (b *seatbeltBackend) Prepare(ctx context.Context, command Command) (Command, error) {
	if _, err := b.workspace.ResolveDirectory(command.Dir); err != nil {
		return Command{}, err
	}
	if err := validateWorkspaceLinks(ctx, b.workspace); err != nil {
		return Command{}, err
	}
	if err := seatbeltSystemProfileAudit.run(); err != nil {
		return Command{}, err
	}
	executable, err := resolveExecutableLiteral(command.Path, command.Env)
	if err != nil {
		return Command{}, err
	}
	writePaths, err := validateExactWorkspaceWritePaths(
		b.workspace, command.WorkspaceReadOnly, command.WorkspaceWritePaths,
	)
	if err != nil {
		return Command{}, err
	}
	readPaths, err := validateAdditionalReadPaths(b.policy, command.AdditionalReadPaths)
	if err != nil {
		return Command{}, err
	}
	hiddenPaths, err := validateWorkspaceHiddenPaths(
		b.workspace,
		command.WorkspaceHiddenPaths,
	)
	if err != nil {
		return Command{}, err
	}
	if err := refuseUndeliveredManagedNetwork(b.policy, command); err != nil {
		return Command{}, err
	}
	policy := ApplySessionProxyPort(b.policy, command)
	profile := seatbeltProfileForCommand(
		policy,
		executable,
		command.WorkspaceReadOnly,
		readPaths,
		writePaths,
		hiddenPaths,
		command.DenyNetwork,
		command.AllowLoopback,
	)
	sandboxExec, err := resolveExecutableLiteral("/usr/bin/sandbox-exec", command.Env)
	if err != nil {
		return Command{}, err
	}
	if err := materializeMissingExactWritePaths(b.workspace, writePaths); err != nil {
		return Command{}, err
	}
	args := []string{sandboxExec, "-p", profile, "--", executable}
	args = append(args, command.Args[1:]...)
	preparedProxyPort := policy.ManagedProxyPort
	return Command{
		Path: sandboxExec, Args: args, Dir: command.Dir, Env: command.Env,
		DirectoryFD: command.DirectoryFD, PreparedPolicyID: b.policy.ID,
		AuthorityDigest:         command.AuthorityDigest,
		PreparedAuthorityDigest: command.AuthorityDigest,
		PreparedControls:        CommandControls(b.capability, b.policy, command),
		WorkspaceReadOnly:       command.WorkspaceReadOnly,
		AdditionalReadPaths:     append([]string(nil), readPaths...),
		WorkspaceWritePaths:     writePathsString(writePaths),
		WorkspaceHiddenPaths:    append([]string(nil), hiddenPaths...),
		DenyNetwork:             command.DenyNetwork,
		PreparedReadOnly:        command.WorkspaceReadOnly,
		PreparedReadPaths:       append([]string(nil), readPaths...),
		PreparedWritePaths:      writePathsString(writePaths),
		PreparedHiddenPaths:     append([]string(nil), hiddenPaths...),
		PreparedNetworkDenied:   command.DenyNetwork,
		PreparedLoopbackAllowed: command.AllowLoopback && !command.DenyNetwork,
		PreparedProxyPort:       preparedProxyPort,
	}, nil
}

func seatbeltQuote(path string) string {
	return strconv.Quote(filepath.Clean(path))
}

func IsUnavailable(err error) bool {
	var target *UnavailableError
	return errors.As(err, &target)
}

func CloseBackend(backend Backend) error {
	if closer, ok := backend.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

type closeBinding struct {
	Backend
	close func() error
}

func WithClose(backend Backend, close func() error) Backend {
	if backend == nil || close == nil {
		return backend
	}
	return &closeBinding{Backend: backend, close: close}
}

func (b *closeBinding) Close() error {
	return errors.Join(CloseBackend(b.Backend), b.close())
}

func (b *closeBinding) Policy() Policy {
	policy, _ := BackendPolicy(b.Backend)
	return policy
}

func (b *closeBinding) InnerBackend() Backend { return b.Backend }

func seatbeltProfile(policy Policy, executable string) string {
	return seatbeltProfileForCommand(
		policy, executable, false, nil, nil, nil, false, false,
	)
}

func seatbeltProfileForCommand(
	policy Policy,
	executable string,
	workspaceReadOnly bool,
	additionalReadPaths []string,
	workspaceWritePaths []workspaceWritePath,
	workspaceHiddenPaths []string,
	denyNetwork bool,
	allowLoopback bool,
) string {
	var profile strings.Builder
	profile.WriteString("(version 1)\n(deny default)\n")
	profile.WriteString("(import \"system.sb\")\n")
	// signal stays unscoped: Seatbelt offers no self+descendants target
	// (empirically verified — (target self) denies killing the process's
	// own children), and unscoped signal is already the macOS same-UID
	// default. Same-UID process signaling is a documented platform
	// boundary, not a grant this profile widens.
	profile.WriteString("(allow process-exec process-fork process-info* signal)\n")
	profile.WriteString("(allow sysctl-read)\n")
	readRoots := append(append([]string{}, policy.RuntimeReadRoots...), policy.HostReadRoots...)
	readRoots = append(readRoots, policy.HostReadFiles...)
	readRoots = append(readRoots, additionalReadPaths...)
	readRoots = append(readRoots, policy.WorkspaceRoot, policy.PrivateTemp, executable)
	for _, root := range readRoots {
		info, err := os.Stat(root)
		if err == nil && info.IsDir() {
			fmt.Fprintf(&profile, "(allow file-read* (subpath %s))\n", seatbeltQuote(root))
		} else {
			fmt.Fprintf(&profile, "(allow file-read* (literal %s))\n", seatbeltQuote(root))
		}
	}
	if !workspaceReadOnly {
		fmt.Fprintf(
			&profile,
			"(allow file-write* (subpath %s))\n",
			seatbeltQuote(policy.WorkspaceRoot),
		)
	}
	for _, pinned := range workspaceWritePaths {
		profile.WriteString(seatbeltWriteGrantFor(pinned))
	}
	for _, root := range policy.HostWriteRoots {
		profile.WriteString(seatbeltWriteGrant(root))
	}
	fmt.Fprintf(
		&profile,
		"(allow file-write* (subpath %s))\n",
		seatbeltQuote(policy.PrivateTemp),
	)
	if filepath.Clean(executable) == "/bin/sh" {
		// Darwin's /bin/sh ignores TMPDIR for here-document backing files. The
		// system bash opens /var/tmp, which lsof reports as /private/var/tmp.
		// Retain /private/tmp for compatible sh variants. Keep every grant
		// filename-scoped.
		profile.WriteString("(allow file-write* (literal \"/var/tmp\"))\n")
		profile.WriteString(
			"(allow file-write* (regex #\"^/var/tmp/sh-thd-[0-9]+$\"))\n",
		)
		profile.WriteString(
			"(allow file-read* (regex #\"^/var/tmp/sh-thd-[0-9]+$\"))\n",
		)
		profile.WriteString("(allow file-write* (literal \"/private/var/tmp\"))\n")
		profile.WriteString(
			"(allow file-write* (regex #\"^/private/var/tmp/sh-thd-[0-9]+$\"))\n",
		)
		profile.WriteString(
			"(allow file-read-metadata (subpath \"/private/var/tmp\"))\n",
		)
		profile.WriteString(
			"(allow file-read* (regex #\"^/private/var/tmp/sh-thd-[0-9]+$\"))\n",
		)
		profile.WriteString("(allow file-write* (literal \"/private/tmp\"))\n")
		profile.WriteString(
			"(allow file-write* (regex #\"^/private/tmp/sh-thd-[0-9]+$\"))\n",
		)
		profile.WriteString(
			"(allow file-read* (regex #\"^/private/tmp/sh-thd-[0-9]+$\"))\n",
		)
	}
	// macOS tools often lstat ancestors (/private, /private/var, …) while
	// resolving realpaths. subpath grants do not cover those parents, which
	// surfaces as "lstat /private: operation not permitted". Metadata-only
	// grants preserve read isolation while allowing a permitted root to be
	// resolved.
	writeSeatbeltAncestorMetadata(&profile, readRoots...)
	// Write trees may contain control-plane entries (.git inside a generated
	// tree): the classifier protects them at settlement, so the OS grant
	// must not be broader than the approval model. Denies come after the
	// allows so last-match-wins rejects the write.
	for _, pinned := range workspaceWritePaths {
		if pinned.kind != writePathTree {
			continue
		}
		for _, name := range controlplane.ProtectedNames() {
			fmt.Fprintf(
				&profile,
				"(deny file-write* (subpath %s))\n",
				seatbeltQuote(filepath.Join(pinned.path, name)),
			)
		}
	}
	for _, path := range workspaceHiddenPaths {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			fmt.Fprintf(
				&profile,
				"(deny file-read* file-write* (subpath %s))\n",
				seatbeltQuote(path),
			)
		} else {
			fmt.Fprintf(
				&profile,
				"(deny file-read* file-write* (literal %s))\n",
				seatbeltQuote(path),
			)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, sensitive := range []string{
			filepath.Join(home, ".ssh"), filepath.Join(home, ".gnupg"),
			filepath.Join(home, "Library", "Keychains"), filepath.Join(home, ".aws"),
		} {
			fmt.Fprintf(&profile, "(deny file-read* file-write* (subpath %s))\n", seatbeltQuote(sensitive))
		}
	}
	if !denyNetwork && (policy.ManagedProxyPort != 0 || allowLoopback) {
		if policy.ManagedProxyPort != 0 {
			fmt.Fprintf(
				&profile,
				"(allow network-outbound (remote ip \"localhost:%d\"))\n",
				policy.ManagedProxyPort,
			)
		}
		if allowLoopback {
			profile.WriteString(
				"(allow network-inbound (local ip \"localhost:*\"))\n",
			)
			profile.WriteString(
				"(allow network-outbound (remote ip \"localhost:*\"))\n",
			)
		}
	} else if policy.AllowNetwork && !denyNetwork {
		profile.WriteString("(allow network-outbound)\n")
		profile.WriteString("(allow network-inbound)\n")
		profile.WriteString("(allow system-socket)\n")
	} else {
		profile.WriteString("(deny network*)\n")
	}
	return profile.String()
}

type writePathKind int

const (
	writePathFile writePathKind = iota
	writePathTree
)

// workspaceWritePath pins the approved shape of one exact write path. The
// kind is observed once at validation and every later stage (profile
// generation, materialization) must fail closed when the filesystem then
// disagrees: a path swapped to a directory between validation and exec can
// never widen a literal-file grant into a subtree grant, and a tree that
// turned into a file or symlink never executes with stale grants.
type workspaceWritePath struct {
	path string
	kind writePathKind
}

func (p workspaceWritePath) String() string { return p.path }

// writePathsString flattens pinned grants back to plain paths for the
// prepared Command and settlement layers.
func writePathsString(pinned []workspaceWritePath) []string {
	paths := make([]string, len(pinned))
	for index, entry := range pinned {
		paths[index] = entry.path
	}
	return paths
}

func seatbeltWriteGrant(path string) string {
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return fmt.Sprintf("(allow file-write* (subpath %s))\n", seatbeltQuote(path))
	}
	return fmt.Sprintf("(allow file-write* (literal %s))\n", seatbeltQuote(path))
}

// seatbeltWriteGrantFor renders a workspace write grant from the pinned
// kind. It deliberately does not observe the filesystem again: the pinned
// decision is the authorization, and any drift is caught by
// materializeMissingExactWritePaths failing closed.
func seatbeltWriteGrantFor(pinned workspaceWritePath) string {
	if pinned.kind == writePathTree {
		return fmt.Sprintf("(allow file-write* (subpath %s))\n", seatbeltQuote(pinned.path))
	}
	return fmt.Sprintf("(allow file-write* (literal %s))\n", seatbeltQuote(pinned.path))
}

func validateWorkspaceHiddenPaths(
	workspace *Workspace,
	paths []string,
) ([]string, error) {
	hidden := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved, err := workspace.Resolve(path, MustExist)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("hidden workspace path %q: %w", path, err)
		}
		if resolved == workspace.Root() {
			return nil, errors.New("cannot hide the entire workspace")
		}
		if !slices.Contains(hidden, resolved) {
			hidden = append(hidden, resolved)
		}
	}
	slices.Sort(hidden)
	return hidden, nil
}

func validateExactWorkspaceWritePaths(
	workspace *Workspace,
	workspaceReadOnly bool,
	paths []string,
) ([]workspaceWritePath, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if !workspaceReadOnly {
		return nil, errors.New("exact write paths require a read-only workspace base")
	}
	if len(paths) > MaxExactWorkspaceWritePaths {
		return nil, fmt.Errorf(
			"exact write paths exceed the %d-file limit",
			MaxExactWorkspaceWritePaths,
		)
	}
	classifier, err := controlplane.New(workspace.Root())
	if err != nil {
		return nil, err
	}
	canonical := make([]string, 0, len(paths))
	pinned := make([]workspaceWritePath, 0, len(paths))
	for _, path := range paths {
		resolved, err := workspace.Resolve(path, AllowMissing)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(resolved)
		if errors.Is(err, os.ErrNotExist) {
			parent, parentErr := os.Stat(filepath.Dir(resolved))
			if parentErr != nil {
				return nil, fmt.Errorf(
					"exact write path %q requires an existing parent directory: %w",
					path,
					parentErr,
				)
			}
			if !parent.IsDir() {
				return nil, fmt.Errorf(
					"exact write path %q parent is not a directory",
					path,
				)
			}
			if err := classifier.CheckWrite(resolved, false); err != nil {
				return nil, err
			}
			canonical = append(canonical, resolved)
			pinned = append(pinned, workspaceWritePath{path: resolved, kind: writePathFile})
			continue
		} else if err != nil {
			return nil, err
		} else if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("exact write path %q is a symlink", path)
		} else if info.IsDir() {
			if resolved == workspace.Root() {
				return nil, errors.New("write tree cannot cover the entire workspace")
			}
			if err := classifier.CheckWrite(resolved, true); err != nil {
				return nil, err
			}
			canonical = append(canonical, resolved)
			pinned = append(pinned, workspaceWritePath{path: resolved, kind: writePathTree})
			continue
		} else if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("exact write path %q is not a regular file", path)
		}
		if err := classifier.CheckWrite(resolved, false); err != nil {
			return nil, err
		}
		canonical = append(canonical, resolved)
		pinned = append(pinned, workspaceWritePath{path: resolved, kind: writePathFile})
	}
	sort.Strings(canonical)
	for index := 1; index < len(canonical); index++ {
		if canonical[index] == canonical[index-1] {
			return nil, fmt.Errorf("duplicate exact write path %q", canonical[index])
		}
	}
	sort.Slice(pinned, func(i, j int) bool { return pinned[i].path < pinned[j].path })
	return pinned, nil
}

func materializeMissingExactWritePaths(
	workspace *Workspace,
	pinned []workspaceWritePath,
) error {
	classifier, err := controlplane.New(workspace.Root())
	if err != nil {
		return err
	}
	var created []string
	cleanup := func() {
		for _, path := range created {
			_ = os.Remove(path)
		}
	}
	for _, entry := range pinned {
		path, kind := entry.path, entry.kind
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				cleanup()
				return fmt.Errorf("exact write path %q changed type", path)
			}
			switch kind {
			case writePathTree:
				// The tree was classified at validation; re-run the
				// control-plane check so drift (a protected entry created
				// under it, path reshuffling) fails closed before exec.
				if !info.IsDir() {
					cleanup()
					return fmt.Errorf("exact write tree %q is no longer a directory", path)
				}
				if err := classifier.CheckWrite(path, true); err != nil {
					cleanup()
					return err
				}
			default:
				if !info.Mode().IsRegular() {
					cleanup()
					return fmt.Errorf("exact write path %q changed type", path)
				}
				resolved, resolveErr := workspace.Resolve(path, MustExist)
				if resolveErr != nil || resolved != path {
					cleanup()
					if resolveErr != nil {
						return resolveErr
					}
					return fmt.Errorf("exact write path %q changed identity", path)
				}
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanup()
			return err
		}
		if kind == writePathTree {
			cleanup()
			return fmt.Errorf("exact write tree %q disappeared before execution", path)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			cleanup()
			return fmt.Errorf("materialize exact write path %q: %w", path, err)
		}
		if err := file.Close(); err != nil {
			cleanup()
			return err
		}
		created = append(created, path)
		resolved, err := workspace.Resolve(path, MustExist)
		if err != nil || resolved != path {
			cleanup()
			if err != nil {
				return err
			}
			return fmt.Errorf("materialized write path %q changed identity", path)
		}
	}
	return nil
}

func validateAdditionalReadPaths(policy Policy, paths []string) ([]string, error) {
	if len(paths) > MaxExactWorkspaceWritePaths {
		return nil, fmt.Errorf(
			"additional read paths exceed the %d-path limit",
			MaxExactWorkspaceWritePaths,
		)
	}
	canonical := make([]string, 0, len(paths))
	for _, path := range paths {
		_, resolved, err := canonicalHostReadRoot(path)
		if err != nil {
			return nil, err
		}
		if err := validateInjectedRoot(resolved, policy.WorkspaceRoot); err != nil {
			return nil, err
		}
		canonical = append(canonical, resolved)
	}
	sort.Strings(canonical)
	canonical = slices.Compact(canonical)
	return canonical, nil
}

func writeSeatbeltAncestorMetadata(profile *strings.Builder, roots ...string) {
	seen := make(map[string]bool)
	for _, root := range roots {
		for path := filepath.Clean(root); path != "" && path != string(filepath.Separator); path = filepath.Dir(path) {
			parent := filepath.Dir(path)
			if parent == path {
				break
			}
			if seen[parent] {
				continue
			}
			seen[parent] = true
			fmt.Fprintf(profile, "(allow file-read-metadata (literal %s))\n", seatbeltQuote(parent))
		}
	}
}

// maxSystemProfileBytes bounds the Seatbelt system profile the runtime will
// read and audit. /System/Library/Sandbox/Profiles/system.sb is a few KiB;
// 1 MiB is the safety ceiling so a replaced profile cannot stream unbounded
// data through the audit. Public contract constant; boundary tests pin it.
const maxSystemProfileBytes = 1 << 20

// auditSeatbeltSystemProfileAt audits a Seatbelt system profile file.
func auditSeatbeltSystemProfileAt(systemProfile string) error {
	file, err := os.Open(systemProfile)
	if err != nil {
		return fmt.Errorf("open Seatbelt system profile: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(maxSystemProfileBytes)+1))
	if err != nil {
		return err
	}
	if len(data) > maxSystemProfileBytes {
		return errors.New("Seatbelt system profile exceeds audit limit")
	}
	normalized := strings.Join(strings.Fields(string(data)), " ")
	normalized = stripSBPLDefinition(normalized, "(define (system-network)")
	for _, broad := range []string{
		"(allow file-read*)", "(allow file-read-metadata)",
		"(allow network*)", "(allow network-inbound)",
	} {
		if strings.Contains(normalized, broad) {
			return fmt.Errorf("Seatbelt system profile contains broad rule %q", broad)
		}
	}
	const allowedSyslog = `(allow network-outbound (literal "/private/var/run/syslog"))`
	normalized = strings.ReplaceAll(normalized, allowedSyslog, "")
	if strings.Contains(normalized, "(allow network-") {
		return errors.New("Seatbelt system profile contains an unaudited active network rule")
	}
	return nil
}

// systemProfileAudit caches a successful audit keyed by the profile file's
// size and modification time. The system profile is OS-owned; any content
// change arrives with a stat change, so an unchanged stat preserves the
// verdict. Failed audits are never cached.
type systemProfileAudit struct {
	path string

	mu      sync.Mutex
	size    int64
	modTime time.Time
	audited bool
}

func (a *systemProfileAudit) run() error {
	info, err := os.Stat(a.path)
	if err != nil {
		return auditSeatbeltSystemProfileAt(a.path)
	}
	a.mu.Lock()
	cached := a.audited && a.size == info.Size() &&
		a.modTime.Equal(info.ModTime())
	a.mu.Unlock()
	if cached {
		return nil
	}
	if err := auditSeatbeltSystemProfileAt(a.path); err != nil {
		return err
	}
	a.mu.Lock()
	a.size, a.modTime, a.audited = info.Size(), info.ModTime(), true
	a.mu.Unlock()
	return nil
}

var seatbeltSystemProfileAudit = &systemProfileAudit{
	path: "/System/Library/Sandbox/Profiles/system.sb",
}

func stripSBPLDefinition(profile, prefix string) string {
	start := strings.Index(profile, prefix)
	if start < 0 {
		return profile
	}
	depth := 0
	quoted := false
	escaped := false
	for index := start; index < len(profile); index++ {
		switch profile[index] {
		case '\\':
			escaped = quoted && !escaped
			continue
		case '"':
			if !escaped {
				quoted = !quoted
			}
		}
		escaped = false
		if quoted {
			continue
		}
		switch profile[index] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return profile[:start] + profile[index+1:]
			}
		}
	}
	return profile
}

func resolveExecutableLiteral(path string, environment []string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("sandbox executable path is empty or contains NUL")
	}
	candidate := path
	if !strings.ContainsRune(path, filepath.Separator) {
		searchPath := environmentValue(environment, "PATH")
		if searchPath == "" {
			searchPath = "/usr/bin:/bin:/usr/sbin:/sbin"
		}
		for _, directory := range filepath.SplitList(searchPath) {
			if directory == "" || !filepath.IsAbs(directory) {
				continue
			}
			value := filepath.Join(directory, path)
			info, err := os.Stat(value)
			if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
				candidate = value
				break
			}
		}
		if candidate == path {
			return "", fmt.Errorf("sandbox executable %q is not in the sanitized PATH", path)
		}
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("canonicalize executable %q: %w", path, err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("canonicalize executable %q: %w", path, err)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("sandbox executable %q is not an executable regular file", path)
	}
	return canonical, nil
}

func ResolveExecutable(path string, environment []string) (string, error) {
	return resolveExecutableLiteral(path, environment)
}

func environmentValue(environment []string, name string) string {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func runAttackProbe() Capability {
	base := Capability{Platform: runtime.GOOS, Backend: "none"}
	switch runtime.GOOS {
	case "darwin":
		base.Backend = "seatbelt"
	default:
		base.Reason = "no supported platform sandbox backend"
		return base
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		base.Reason = err.Error()
		return base
	}
	workspace, err := os.MkdirTemp("", "qcode-probe-workspace-")
	if err != nil {
		base.Reason = err.Error()
		return base
	}
	defer os.RemoveAll(workspace)
	privateTemp, err := os.MkdirTemp("", "qcode-probe-private-")
	if err != nil {
		base.Reason = err.Error()
		return base
	}
	defer os.RemoveAll(privateTemp)
	external, err := os.MkdirTemp("", "qcode-probe-external-")
	if err != nil {
		base.Reason = err.Error()
		return base
	}
	defer os.RemoveAll(external)
	if err := os.WriteFile(filepath.Join(workspace, "input"), []byte("workspace"), 0o600); err != nil {
		base.Reason = err.Error()
		return base
	}
	if err := os.WriteFile(filepath.Join(workspace, "output"), nil, 0o600); err != nil {
		base.Reason = err.Error()
		return base
	}
	secret := filepath.Join(external, "secret")
	outsideWrite := filepath.Join(external, "write")
	if err := os.WriteFile(secret, []byte("fixture-secret"), 0o600); err != nil {
		base.Reason = err.Error()
		return base
	}
	policy, err := BuildPolicy(Options{WorkspaceRoot: workspace, PrivateTemp: privateTemp})
	if err != nil {
		base.Reason = err.Error()
		return base
	}
	ws, _ := NewWorkspace(policy.WorkspaceRoot)
	candidate := base
	candidate.Available = true
	candidate.Effective = platformControls(runtime.GOOS)
	backend := &seatbeltBackend{workspace: ws, policy: policy, capability: candidate}
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	if listener != nil {
		defer listener.Close()
	}
	networkTest := "true"
	if listener != nil {
		port := listener.Addr().(*net.TCPAddr).Port
		networkTest = fmt.Sprintf("! /usr/bin/nc -w 1 127.0.0.1 %d", port)
	}
	if _, err := os.Stat("/private/var/run/syslog"); err == nil {
		networkTest += "; ! /usr/bin/nc -U /private/var/run/syslog </dev/null"
	}
	// The profile never grants file-link, so hard-link creation must be
	// denied everywhere: inside the workspace, and — the escape shape —
	// linking a readable host file into the writable workspace to write
	// through it. These assertions keep a future file-link grant (e.g. for
	// cache partitions) from silently opening the inode-identity escape.
	linkTest := "! ln input input-link 2>/dev/null; ! ln /etc/hosts hosts-escape 2>/dev/null"
	script := fmt.Sprintf(
		`set -eu; test "$(cat input)" = workspace; test "$(cat <<'EOF'
heredoc
EOF
)" = heredoc; printf ok > output; sh -c 'test "$(cat input)" = workspace'; ! cat %q >/dev/null 2>&1; ! printf bad > %q; ! printf bad > /private/tmp/qcode-sandbox-probe; ! printf bad > /var/tmp/qcode-sandbox-probe; ! printf bad > /private/var/tmp/qcode-sandbox-probe; %s; %s`,
		secret, outsideWrite, linkTest, networkTest,
	)
	prepared, err := backend.Prepare(context.Background(), Command{
		Path: "/bin/sh", Args: []string{"/bin/sh", "-c", script},
		Dir: policy.WorkspaceRoot, Env: []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"},
		WorkspaceReadOnly: true, WorkspaceWritePaths: []string{"output"},
	})
	if err != nil {
		candidate.Available = false
		candidate.Reason = err.Error()
		return candidate
	}
	// The probe command shares the public toolchain probe timeout: one
	// bounded sandboxed exec, same ceiling as any other probe.
	ctx, cancel := context.WithTimeout(context.Background(), ToolchainProbeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, prepared.Path, prepared.Args[1:]...)
	command.Dir, command.Env = prepared.Dir, prepared.Env
	if output, err := command.CombinedOutput(); err != nil {
		reason := fmt.Sprintf("attack probe failed: %v: %s", err, strings.TrimSpace(string(output)))
		candidate.Available = false
		candidate.Reason = reason
		return candidate
	}
	if listener != nil {
		candidate.ManagedProxy = probeManagedProxy(ctx, ws, policy, candidate, listener)
	}
	return candidate
}

func probeManagedProxy(
	ctx context.Context,
	workspace *Workspace,
	policy Policy,
	capability Capability,
	allowed net.Listener,
) bool {
	denied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return false
	}
	defer denied.Close()
	policy.ManagedProxyPort = uint16(allowed.Addr().(*net.TCPAddr).Port)
	backend := &seatbeltBackend{workspace: workspace, policy: policy, capability: capability}
	script := fmt.Sprintf(
		"/usr/bin/nc -z -w 1 127.0.0.1 %d && ! /usr/bin/nc -z -w 1 127.0.0.1 %d",
		policy.ManagedProxyPort, denied.Addr().(*net.TCPAddr).Port,
	)
	prepared, err := backend.Prepare(ctx, Command{
		Path: "/bin/sh", Args: []string{"/bin/sh", "-c", script},
		Dir: policy.WorkspaceRoot, Env: []string{"PATH=/usr/bin:/bin"},
		WorkspaceReadOnly: true,
	})
	if err != nil {
		return false
	}
	command := exec.CommandContext(ctx, prepared.Path, prepared.Args[1:]...)
	command.Dir, command.Env = prepared.Dir, prepared.Env
	return command.Run() == nil
}

func validateWorkspaceLinks(ctx context.Context, workspace *Workspace) error {
	type objectKey struct {
		device uint64
		inode  uint64
	}
	type hardLinkSet struct {
		path     string
		expected uint64
		observed uint64
	}
	hardLinks := make(map[objectKey]hardLinkSet)
	err := filepath.WalkDir(workspace.Root(), func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if errors.Is(err, os.ErrNotExist) {
				target, readErr := os.Readlink(path)
				if readErr != nil {
					return fmt.Errorf("read workspace symbolic link %q: %w", path, readErr)
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(path), target)
				}
				resolved, err = evalSymlinksAllowMissing(target)
			}
			if err != nil {
				return fmt.Errorf("workspace symbolic link %q is invalid: %w", path, err)
			}
			if !pathContains(workspace.Root(), resolved) {
				return fmt.Errorf("workspace symbolic link %q escapes the sandbox", path)
			}
			return nil
		}
		if info.Mode().IsRegular() {
			identity, err := identityOf(path, info)
			if err != nil {
				return err
			}
			if identity.links > 1 {
				key := objectKey{device: identity.device, inode: identity.inode}
				set, exists := hardLinks[key]
				if exists && set.expected != identity.links {
					return fmt.Errorf("workspace file %q hard link count changed during validation", path)
				}
				if !exists {
					set = hardLinkSet{path: path, expected: identity.links}
				}
				set.observed++
				hardLinks[key] = set
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for key, set := range hardLinks {
		if set.observed != set.expected {
			return fmt.Errorf(
				"workspace file %q has hard links outside the workspace",
				set.path,
			)
		}
		info, err := os.Lstat(set.path)
		if err != nil {
			return err
		}
		identity, err := identityOf(set.path, info)
		if err != nil {
			return err
		}
		if identity.device != key.device || identity.inode != key.inode ||
			identity.links != set.expected {
			return fmt.Errorf(
				"workspace file %q hard link identity changed during validation",
				set.path,
			)
		}
	}
	return nil
}

func evalSymlinksAllowMissing(value string) (string, error) {
	current := filepath.Clean(value)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
