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

func BackendPolicy(backend Backend) (Policy, bool) {
	current := backend
	for range 8 {
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
		helper, _ := os.Executable()
		probedCapability = runAttackProbe(helper)
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
	case "linux":
		controls.FilesystemRead = controlmatrix.FilesystemReadDeclaredRoots
		controls.FilesystemWrite = controlmatrix.FilesystemWriteExactPaths
		controls.Network = controlmatrix.NetworkDenied
		controls.ProcessTree = controlmatrix.ProcessTreePIDNamespace
		controls.CrossProcess = controlmatrix.CrossProcessIsolated
		controls.Syscall = controlmatrix.SyscallDenyDangerous
		controls.IPC = controlmatrix.IPCPrivateNamespace
		controls.PathIdentity = controlmatrix.PathIdentityDescriptorRelative
	case "windows":
		controls.ProcessTree = controlmatrix.ProcessTreeJobObject
		controls.CrossProcess = controlmatrix.CrossProcessRestricted
		controls.PathIdentity = controlmatrix.PathIdentityCanonical
	}
	return controls
}

var (
	probeOnce        sync.Once
	probedCapability Capability
)

func NewPlatformBackend(options Options) (Backend, error) {
	helperPath := options.HelperPath
	if runtime.GOOS == "linux" && helperPath == "" {
		helperPath, _ = os.Executable()
	}
	policy, err := BuildPolicy(options)
	if err != nil {
		return nil, err
	}
	workspace, err := NewWorkspace(policy.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	var capability Capability
	if runtime.GOOS == "linux" {
		capability = runAttackProbe(helperPath)
	} else {
		capability = Probe()
	}
	if !capability.Available {
		return &unavailableBackend{capability: capability, policy: policy}, nil
	}
	switch capability.Backend {
	case "seatbelt":
		return &seatbeltBackend{workspace: workspace, policy: policy, capability: capability}, nil
	case "bwrap+landlock":
		requestRoot, requestErr := createLandlockRequestRoot()
		if requestErr != nil {
			_ = closePolicyTemp(policy)
			return nil, fmt.Errorf("create Landlock request root: %w", requestErr)
		}
		return &bubblewrapBackend{
			workspace: workspace, policy: policy, capability: capability,
			helperPath: helperPath, requestRoot: requestRoot, useLandlock: true,
		}, nil
	case "bwrap":
		return &bubblewrapBackend{
			workspace: workspace, policy: policy, capability: capability,
			helperPath: helperPath,
		}, nil
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
		WorkspaceWritePaths:     append([]string(nil), writePaths...),
		WorkspaceHiddenPaths:    append([]string(nil), hiddenPaths...),
		DenyNetwork:             command.DenyNetwork,
		PreparedReadOnly:        command.WorkspaceReadOnly,
		PreparedReadPaths:       append([]string(nil), readPaths...),
		PreparedWritePaths:      append([]string(nil), writePaths...),
		PreparedHiddenPaths:     append([]string(nil), hiddenPaths...),
		PreparedNetworkDenied:   command.DenyNetwork,
		PreparedLoopbackAllowed: command.AllowLoopback && !command.DenyNetwork,
		PreparedProxyPort:       preparedProxyPort,
	}, nil
}

type bubblewrapBackend struct {
	workspace   *Workspace
	policy      Policy
	capability  Capability
	helperPath  string
	requestRoot string
	probeReads  []string
	useLandlock bool
}

func (b *bubblewrapBackend) Capability() Capability {
	return b.capability
}

func (b *bubblewrapBackend) Policy() Policy { return b.policy }

func (b *bubblewrapBackend) Close() error {
	var requestErr error
	if b.requestRoot != "" {
		requestErr = os.RemoveAll(b.requestRoot)
	}
	return errors.Join(closePolicyTemp(b.policy), requestErr)
}

func (b *bubblewrapBackend) Prepare(ctx context.Context, command Command) (Command, error) {
	if _, err := b.workspace.ResolveDirectory(command.Dir); err != nil {
		return Command{}, err
	}
	if err := validateWorkspaceLinks(ctx, b.workspace); err != nil {
		return Command{}, err
	}
	executable, err := resolveExecutableLiteral(command.Path, command.Env)
	if err != nil {
		return Command{}, err
	}
	bwrap, err := resolveExecutableLiteral("bwrap", command.Env)
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
	var helper, requestPath string
	if b.useLandlock {
		helper, requestPath, err = prepareLandlockInvocation(
			b.policy, b.helperPath, b.requestRoot,
			executable, command.Args[1:], command.Env, command.WorkspaceReadOnly,
			readPaths, writePaths,
		)
		if err != nil {
			return Command{}, err
		}
	}
	if err := materializeMissingExactWritePaths(b.workspace, writePaths); err != nil {
		return Command{}, err
	}
	args := []string{
		bwrap, "--die-with-parent", "--new-session", "--unshare-all",
	}
	if b.policy.AllowNetwork && !command.DenyNetwork {
		args = append(args, "--share-net")
	}
	args = append(args, "--tmpfs", "/", "--proc", "/proc", "--dev", "/dev")
	created := map[string]bool{"/": true, "/proc": true, "/dev": true}
	args = appendRuntimeSymlinks(args, created, b.policy.RuntimeReadRoots)
	for _, root := range append(append([]string{}, b.policy.RuntimeReadRoots...), b.policy.HostReadRoots...) {
		args = appendMount(args, created, root, root, true)
	}
	for _, root := range b.probeReads {
		args = appendMount(args, created, root, root, true)
	}
	for _, root := range readPaths {
		args = appendMount(args, created, root, root, true)
	}
	args = appendMount(
		args,
		created,
		b.policy.WorkspaceRoot,
		b.policy.WorkspaceRoot,
		command.WorkspaceReadOnly,
	)
	for _, path := range writePaths {
		args = appendMount(args, created, path, path, false)
	}
	for _, path := range hiddenPaths {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return Command{}, statErr
		}
		if info.IsDir() {
			args = append(args, "--tmpfs", path)
		} else {
			args = append(args, "--ro-bind", "/dev/null", path)
		}
	}
	args = appendMount(args, created, b.policy.PrivateTemp, b.policy.PrivateTemp, false)
	executableMountedLiteral := !coveredByRoots(
		executable, append(b.policy.RuntimeReadRoots, b.policy.HostReadRoots...),
	)
	if executableMountedLiteral {
		args = appendMount(args, created, executable, executable, true)
	}
	if b.useLandlock {
		args = appendMount(args, created, filepath.Dir(requestPath), filepath.Dir(requestPath), false)
		if helper != executable || !executableMountedLiteral {
			args = appendMount(args, created, helper, helper, true)
		}
	}
	args = append(args, "--setenv", "TMPDIR", b.policy.PrivateTemp)
	directory := command.Dir
	if command.DirectoryFD > 0 {
		directory = fmt.Sprintf("/proc/self/fd/%d", command.DirectoryFD)
	}
	args = append(args, "--chdir", directory, "--")
	if b.useLandlock {
		args = append(args, landlockHelperArgs(helper, requestPath, b.policy.ID)...)
	} else {
		args = append(args, executable)
		args = append(args, command.Args[1:]...)
	}
	return Command{
		Path: bwrap, Args: args, Dir: command.Dir, Env: command.Env,
		DirectoryFD: command.DirectoryFD, PreparedPolicyID: b.policy.ID,
		AuthorityDigest:         command.AuthorityDigest,
		PreparedAuthorityDigest: command.AuthorityDigest,
		PreparedControls:        CommandControls(b.capability, b.policy, command),
		WorkspaceReadOnly:       command.WorkspaceReadOnly,
		AdditionalReadPaths:     append([]string(nil), readPaths...),
		WorkspaceWritePaths:     append([]string(nil), writePaths...),
		WorkspaceHiddenPaths:    append([]string(nil), hiddenPaths...),
		DenyNetwork:             command.DenyNetwork,
		PreparedReadOnly:        command.WorkspaceReadOnly,
		PreparedReadPaths:       append([]string(nil), readPaths...),
		PreparedWritePaths:      append([]string(nil), writePaths...),
		PreparedHiddenPaths:     append([]string(nil), hiddenPaths...),
		PreparedNetworkDenied:   command.DenyNetwork,
		PreparedLoopbackAllowed: false,
		PreparedProxyPort:       0,
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
	workspaceWritePaths []string,
	workspaceHiddenPaths []string,
	denyNetwork bool,
	allowLoopback bool,
) string {
	var profile strings.Builder
	profile.WriteString("(version 1)\n(deny default)\n")
	profile.WriteString("(import \"system.sb\")\n")
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
	for _, path := range workspaceWritePaths {
		profile.WriteString(seatbeltWriteGrant(path))
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

func seatbeltWriteGrant(path string) string {
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return fmt.Sprintf("(allow file-write* (subpath %s))\n", seatbeltQuote(path))
	}
	return fmt.Sprintf("(allow file-write* (literal %s))\n", seatbeltQuote(path))
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
) ([]string, error) {
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
			continue
		} else if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("exact write path %q is not a regular file", path)
		}
		if err := classifier.CheckWrite(resolved, false); err != nil {
			return nil, err
		}
		canonical = append(canonical, resolved)
	}
	sort.Strings(canonical)
	for index := 1; index < len(canonical); index++ {
		if canonical[index] == canonical[index-1] {
			return nil, fmt.Errorf("duplicate exact write path %q", canonical[index])
		}
	}
	return canonical, nil
}

func materializeMissingExactWritePaths(
	workspace *Workspace,
	paths []string,
) error {
	var created []string
	cleanup := func() {
		for _, path := range created {
			_ = os.Remove(path)
		}
	}
	for _, path := range paths {
		if info, err := os.Lstat(path); err == nil {
			if info.IsDir() {
				if info.Mode()&os.ModeSymlink != 0 {
					cleanup()
					return fmt.Errorf("exact write path %q changed type", path)
				}
				continue
			}
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
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanup()
			return err
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

// auditSeatbeltSystemProfileAt audits a Seatbelt system profile file.
func auditSeatbeltSystemProfileAt(systemProfile string) error {
	file, err := os.Open(systemProfile)
	if err != nil {
		return fmt.Errorf("open Seatbelt system profile: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
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

func coveredByRoots(path string, roots []string) bool {
	for _, root := range roots {
		if pathContains(root, path) {
			return true
		}
	}
	return false
}

func appendMount(args []string, created map[string]bool, source, destination string, readOnly bool) []string {
	info, err := os.Stat(source)
	if err != nil {
		return args
	}
	parent := filepath.Dir(destination)
	var parents []string
	for current := parent; !isFilesystemRoot(current) && !created[current]; {
		parents = append(parents, current)
		next := filepath.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	for index := len(parents) - 1; index >= 0; index-- {
		args = append(args, "--dir", parents[index])
		created[parents[index]] = true
	}
	if info.IsDir() {
		if !created[destination] {
			args = append(args, "--dir", destination)
			created[destination] = true
		}
	}
	flag := "--bind"
	if readOnly {
		flag = "--ro-bind"
	}
	return append(args, flag, source, destination)
}

func appendRuntimeSymlinks(args []string, created map[string]bool, runtimeRoots []string) []string {
	for _, alias := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
		resolved, err := filepath.EvalSymlinks(alias)
		if err != nil {
			continue
		}
		resolved = filepath.Clean(resolved)
		if resolved == alias || !coveredByRoots(resolved, runtimeRoots) {
			continue
		}
		target := strings.TrimPrefix(resolved, string(filepath.Separator))
		if target == "" || target == resolved {
			continue
		}
		args = append(args, "--symlink", target, alias)
		created[alias] = true
	}
	return args
}

func runAttackProbe(helperPath string) Capability {
	base := Capability{Platform: runtime.GOOS, Backend: "none"}
	switch runtime.GOOS {
	case "darwin":
		base.Backend = "seatbelt"
	case "linux":
		base.Backend = "bwrap"
		base.Reason = "bwrap is available but the Landlock helper has not passed its strict ABI probe"
	case "windows":
		base.Backend = "restricted-token"
		base.Reason = "strong restricted-token filesystem and network controls are unavailable"
		return base
	default:
		base.Reason = "no supported platform sandbox backend"
		return base
	}
	if _, err := exec.LookPath(map[string]string{"darwin": "sandbox-exec", "linux": "bwrap"}[runtime.GOOS]); err != nil {
		base.Reason = err.Error()
		return base
	}
	if runtime.GOOS == "linux" {
		base.Available = true
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
	var backend Backend
	if runtime.GOOS == "darwin" {
		backend = &seatbeltBackend{workspace: ws, policy: policy, capability: candidate}
	} else {
		requestRoot, requestErr := createLandlockRequestRoot()
		if requestErr != nil {
			base.Reason = "create Landlock request root: " + requestErr.Error()
			return base
		}
		defer os.RemoveAll(requestRoot)
		candidate.Backend = "bwrap+landlock"
		candidate.Reason = ""
		backend = &bubblewrapBackend{
			workspace: ws, policy: policy, capability: candidate,
			helperPath: helperPath, requestRoot: requestRoot,
			probeReads: []string{external}, useLandlock: true,
		}
	}
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	if listener != nil {
		defer listener.Close()
	}
	networkTest := "true"
	if listener != nil {
		port := listener.Addr().(*net.TCPAddr).Port
		networkTest = fmt.Sprintf("! /usr/bin/nc -w 1 127.0.0.1 %d", port)
	}
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/private/var/run/syslog"); err == nil {
			networkTest += "; ! /usr/bin/nc -U /private/var/run/syslog </dev/null"
		}
	}
	script := fmt.Sprintf(
		`set -eu; test "$(cat input)" = workspace; test "$(cat <<'EOF'
heredoc
EOF
)" = heredoc; printf ok > output; sh -c 'test "$(cat input)" = workspace'; ! cat %q >/dev/null 2>&1; ! printf bad > %q; ! printf bad > /private/tmp/qcode-sandbox-probe; ! printf bad > /var/tmp/qcode-sandbox-probe; ! printf bad > /private/var/tmp/qcode-sandbox-probe; %s`,
		secret, outsideWrite, networkTest,
	)
	if runtime.GOOS == "linux" {
		script = `test "$QCODE_LANDLOCK_ACTIVE" = 1; ` +
			`test "$QCODE_NO_NEW_PRIVS_ACTIVE" = 1; ` +
			`test "$QCODE_SECCOMP_ACTIVE" = restricted; ` + script
	}
	prepared, err := backend.Prepare(context.Background(), Command{
		Path: "/bin/sh", Args: []string{"/bin/sh", "-c", script},
		Dir: policy.WorkspaceRoot, Env: []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"},
		WorkspaceReadOnly: true, WorkspaceWritePaths: []string{"output"},
	})
	if err != nil {
		if runtime.GOOS == "linux" {
			base.Reason = "Landlock helper prepare failed: " + err.Error()
			return base
		}
		candidate.Available = false
		candidate.Reason = err.Error()
		return candidate
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, prepared.Path, prepared.Args[1:]...)
	command.Dir, command.Env = prepared.Dir, prepared.Env
	if output, err := command.CombinedOutput(); err != nil {
		reason := fmt.Sprintf("attack probe failed: %v: %s", err, strings.TrimSpace(string(output)))
		if runtime.GOOS == "linux" {
			base.Reason = "Landlock helper " + reason
			return base
		}
		candidate.Available = false
		candidate.Reason = reason
		return candidate
	}
	if runtime.GOOS == "darwin" && listener != nil {
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
