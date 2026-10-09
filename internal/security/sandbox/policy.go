package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/envpolicy"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/pathpolicy"
)

const policyVersion = 2

func SupportsManagedNetworkProxy() bool { return runtime.GOOS == "darwin" }

func ManagedNetworkProxyPort(port uint16) uint16 {
	if SupportsManagedNetworkProxy() {
		return port
	}
	return 0
}

func BackendManagedProxyPort(backend Backend) uint16 {
	policy, ok := BackendPolicy(backend)
	if !ok {
		return 0
	}
	return policy.ManagedProxyPort
}

func BackendManagedProxyCredential(backend Backend) string {
	policy, ok := BackendPolicy(backend)
	if !ok {
		return ""
	}
	return policy.ManagedProxyCredential
}

// ManagedProxyUser is the fixed user name in a managed proxy credential; the
// per-channel secret is the password.
const ManagedProxyUser = "qcode"

// ManagedProxyURL is the loopback proxy URL a sandboxed command is given.
// The credential travels as URL userinfo, which HTTP clients send as Basic
// Proxy-Authorization; origin-form requests are rejected.
func ManagedProxyURL(port uint16, credential string) string {
	endpoint := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))}
	if credential != "" {
		endpoint.User = url.UserPassword(ManagedProxyUser, credential)
	}
	return endpoint.String()
}

type Options struct {
	WorkspaceRoot string
	// RuntimeStateRoots are trusted runtime-owned directories. Full Access
	// preserves only the existing environment grants inside these roots.
	RuntimeStateRoots []string
	HostReadRoots     []string
	HostReadFiles     []string
	PrivateTemp       string
	// AllowNetwork permits outbound/inbound sockets inside the OS sandbox.
	// qcode enables it for the interactive tool session so host processes like
	// ubomcli can reach their APIs.
	AllowNetwork     bool
	ManagedProxyPort uint16
	// ManagedProxyCredential authenticates commands to ManagedProxyPort.
	ManagedProxyCredential string
	// SkipPATHReadRoots disables inheriting absolute PATH directories as
	// HostReadRoots. Default (false) lets user-installed tools run (e.g.
	// ~/.local/bin/ubomcli) without opening the entire home directory.
	SkipPATHReadRoots bool
	// Toolchains is an inherited exposure, for example the parent policy of
	// an isolated settlement backend. When SkipPATHReadRoots skips host
	// discovery it is applied verbatim so child commands keep the parent's
	// PATH prefix and preparer environment; roots are re-validated and a
	// vanished installation is dropped instead of failing the policy.
	// Discovery stays authoritative when it runs.
	Toolchains *ToolchainExposure
	// EnvironmentContract is the preparation chain identity. The only
	// accepted value is v1; empty is stamped as v1.
	EnvironmentContract string
	EnvironmentProfile  string
	SharedUserTemp      bool
	HostWriteRoots      []string
	// EnvironmentNetwork is the user-declared Session Gate Grant. Adapter
	// targets inferred from tool configuration stay off this list.
	EnvironmentNetwork []EnvironmentNetworkTarget
	// EnvironmentValues are selected NAME=value entries from preparation.
	// Nil captures a source once here; non-nil (including empty) never
	// inherits the live host environment. Raw source snapshots are not retained.
	EnvironmentValues []string
}

// EnvironmentNetworkTarget is one user-declared host the Session Gate may
// grant. It is not a Workspace process-Gate accumulation record.
type EnvironmentNetworkTarget struct {
	Host         string   `json:"host"`
	Protocol     string   `json:"protocol,omitempty"`
	Port         uint16   `json:"port,omitempty"`
	Methods      []string `json:"methods,omitempty"`
	AllowPrivate bool     `json:"allow_private,omitempty"`
}

type Policy struct {
	// FullAccess is command-local, derived only from a bound execution authority.
	FullAccess          bool                       `json:"-"`
	Version             int                        `json:"version"`
	ID                  string                     `json:"id"`
	WorkspaceRoot       string                     `json:"workspace_root"`
	RuntimeStateRoots   []string                   `json:"runtime_state_roots,omitempty"`
	PrivateTemp         string                     `json:"private_temp"`
	RuntimeReadRoots    []string                   `json:"runtime_read_roots"`
	HostReadRoots       []string                   `json:"host_read_roots"`
	HostReadFiles       []string                   `json:"host_read_files,omitempty"`
	Toolchains          ToolchainExposure          `json:"toolchains,omitempty"`
	AllowNetwork        bool                       `json:"allow_network,omitempty"`
	ManagedProxyPort    uint16                     `json:"managed_proxy_port,omitempty"`
	EnvironmentContract string                     `json:"environment_contract,omitempty"`
	EnvironmentProfile  string                     `json:"environment_profile,omitempty"`
	SharedUserTemp      bool                       `json:"shared_user_temp,omitempty"`
	HostWriteRoots      []string                   `json:"host_write_roots,omitempty"`
	EnvironmentNetwork  []EnvironmentNetworkTarget `json:"environment_network,omitempty"`
	EnvironmentValues   []string                   `json:"environment_values,omitempty"`
	// ManagedProxyCredential is a secret: it is excluded from the policy
	// identity and from every serialized form.
	ManagedProxyCredential string `json:"-"`
	ownsPrivateTemp        bool
}

func BuildPolicy(options Options) (Policy, error) {
	sourceEnv := options.EnvironmentValues
	if sourceEnv == nil {
		sourceEnv = os.Environ()
	}

	if strings.TrimSpace(options.WorkspaceRoot) == "" {
		return Policy{}, errors.New("sandbox workspace root is required")
	}
	if options.AllowNetwork && options.ManagedProxyPort != 0 {
		return Policy{}, errors.New("broad and managed sandbox network modes conflict")
	}
	workspace, err := canonicalDirectory(options.WorkspaceRoot)
	if err != nil {
		return Policy{}, fmt.Errorf("canonicalize sandbox workspace: %w", err)
	}
	if isFilesystemRoot(workspace) {
		return Policy{}, errors.New("sandbox workspace cannot be the filesystem root")
	}
	privateTemp := options.PrivateTemp
	ownsPrivateTemp := false
	if privateTemp == "" {
		tempRoot := envpolicy.Value(sourceEnv, "TMPDIR")
		if tempRoot == "" {
			// macOS documented default when TMPDIR is absent.
			tempRoot = "/tmp"
		}
		created, createErr := os.MkdirTemp(tempRoot, "qcode-sandbox-")
		if createErr != nil {
			return Policy{}, fmt.Errorf("create private sandbox temp: %w", createErr)
		}
		if err := os.Chmod(created, 0o700); err != nil {
			_ = os.RemoveAll(created)
			return Policy{}, fmt.Errorf("protect private sandbox temp: %w", err)
		}
		// macOS MkdirTemp returns /var/folders/... while /var is a symlink to
		// /private/var. Seatbelt + Go's MkdirAll then fail with
		// "mkdir /var: file exists" when creating a private cache under that path.
		// Store the realpath so private directories remain writable.
		privateTemp, err = canonicalDirectory(created)
		if err != nil {
			_ = os.RemoveAll(created)
			return Policy{}, fmt.Errorf("canonicalize private sandbox temp: %w", err)
		}
		ownsPrivateTemp = true
	} else {
		privateTemp, err = canonicalDirectory(privateTemp)
		if err != nil {
			return Policy{}, fmt.Errorf("canonicalize private sandbox temp: %w", err)
		}
	}
	if pathContains(workspace, privateTemp) || pathContains(privateTemp, workspace) {
		return Policy{}, errors.New("sandbox private temp must be separate from the workspace")
	}

	runtimeRoots := make([]string, 0, 8)
	for _, root := range platformRuntimeRoots(runtime.GOOS) {
		canonical, canonicalErr := canonicalRuntimeRoot(root)
		if canonicalErr == nil && !slices.Contains(runtimeRoots, canonical) {
			runtimeRoots = append(runtimeRoots, canonical)
		}
	}
	hostRoots := make([]string, 0, len(options.HostReadRoots))
	for _, root := range options.HostReadRoots {
		lexical, canonical, canonicalErr := canonicalHostReadRoot(root)
		if canonicalErr != nil {
			return Policy{}, fmt.Errorf("canonicalize host read root %q: %w", root, canonicalErr)
		}
		if err := validateInjectedRoot(canonical, workspace); err != nil {
			return Policy{}, fmt.Errorf("host read root %q: %w", root, err)
		}
		for _, candidate := range []string{lexical, canonical} {
			if err := validateSensitivePath(candidate); err != nil {
				return Policy{}, fmt.Errorf("host read root %q: %w", root, err)
			}
			if !slices.Contains(runtimeRoots, candidate) &&
				!slices.Contains(hostRoots, candidate) {
				hostRoots = append(hostRoots, candidate)
			}
		}
	}
	hostFiles := make([]string, 0, len(options.HostReadFiles)*2)
	for _, path := range options.HostReadFiles {
		lexical, canonical, fileErr := canonicalHostReadFile(path)
		if fileErr != nil {
			return Policy{}, fmt.Errorf("canonicalize host read file %q: %w", path, fileErr)
		}
		if err := validateInjectedHostFile(canonical, workspace); err != nil {
			return Policy{}, fmt.Errorf("host read file %q: %w", path, err)
		}
		for _, candidate := range []string{lexical, canonical} {
			if err := validateSensitivePath(candidate); err != nil {
				return Policy{}, fmt.Errorf("host read file %q: %w", path, err)
			}
			if !slices.Contains(hostFiles, candidate) {
				hostFiles = append(hostFiles, candidate)
			}
		}
	}
	if options.EnvironmentContract != "" &&
		options.EnvironmentContract != "v1" {
		return Policy{}, fmt.Errorf(
			"environment contract %q is not supported",
			options.EnvironmentContract,
		)
	}
	toolchains := ToolchainExposure{}
	if options.Toolchains != nil {
		toolchains = ToolchainExposure{
			BinDirs:     append([]string(nil), options.Toolchains.BinDirs...),
			ReadRoots:   append([]string(nil), options.Toolchains.ReadRoots...),
			ReadFiles:   append([]string(nil), options.Toolchains.ReadFiles...),
			Environment: append([]string(nil), options.Toolchains.Environment...),
		}
	}
	environmentValues := options.EnvironmentValues
	if environmentValues == nil {
		environmentValues = envpolicy.WithoutManagedProxy(envpolicy.Baseline(sourceEnv))
	}
	if err := envpolicy.ValidatePreparedEnvironment(environmentValues); err != nil {
		return Policy{}, err
	}
	if !options.SkipPATHReadRoots {
		var platformValues []string
		toolchains, platformValues, err = PreparePlatformEnvironment(workspace, sourceEnv)
		if err != nil {
			return Policy{}, err
		}
		environmentValues, err = envpolicy.Merge(platformValues, options.EnvironmentValues)
		if err != nil {
			return Policy{}, err
		}

		for _, root := range append(
			append([]string(nil), toolchains.BinDirs...),
			toolchains.ReadRoots...,
		) {
			hostRoots = append(hostRoots, root)
		}
		hostFiles = append(hostFiles, toolchains.ReadFiles...)
	} else if options.Toolchains != nil {
		seen := make(map[string]bool, len(hostRoots))
		for _, root := range hostRoots {
			seen[root] = true
		}
		for _, root := range append(
			append([]string(nil), toolchains.BinDirs...),
			toolchains.ReadRoots...,
		) {
			addToolchainReadDirectory(&hostRoots, root, workspace, seen)
		}
		hostFiles = append(hostFiles, toolchains.ReadFiles...)
	}
	if err := envpolicy.ValidatePreparedEnvironment(toolchains.Environment); err != nil {
		return Policy{}, err
	}
	environmentValues, err = envpolicy.Merge(envpolicy.SandboxDefaults(), toolchains.Environment, environmentValues)
	if err != nil {
		return Policy{}, err
	}
	environmentValues = envpolicy.WithoutManagedProxy(environmentValues)

	slices.Sort(runtimeRoots)
	slices.Sort(hostRoots)
	slices.Sort(hostFiles)
	writeRoots := make([]string, 0, len(options.HostWriteRoots)*2)
	for _, root := range options.HostWriteRoots {
		lexical, canonical, canonicalErr := canonicalHostReadRoot(root)
		if canonicalErr != nil {
			return Policy{}, fmt.Errorf("canonicalize host write root %q: %w", root, canonicalErr)
		}
		if err := validateInjectedRoot(canonical, workspace); err != nil {
			return Policy{}, fmt.Errorf("host write root %q: %w", root, err)
		}
		for _, candidate := range []string{lexical, canonical} {
			if err := validateSensitivePath(candidate); err != nil {
				return Policy{}, fmt.Errorf("host write root %q: %w", root, err)
			}
			if !slices.Contains(writeRoots, candidate) {
				writeRoots = append(writeRoots, candidate)
			}
		}
	}
	slices.Sort(writeRoots)
	// The private temp is same-UID: any injected root that equals, contains,
	// or sits inside it would read or tamper with every other live sandbox
	// session's temp area. Check centrally here — every source (declared
	// roots, PATH-derived, toolchain, certificate) flows through these
	// slices.
	if violating := injectedRootOverlappingTemp(
		privateTemp, hostRoots, hostFiles, writeRoots,
	); violating != "" {
		return Policy{}, fmt.Errorf(
			"injected %q overlaps the private sandbox temp", violating,
		)
	}
	stateRoots := make([]string, 0, len(options.RuntimeStateRoots))
	for _, root := range options.RuntimeStateRoots {
		canonical, err := ExternalStateDirectory(workspace, root)
		if err != nil {
			return Policy{}, fmt.Errorf("runtime state root: %w", err)
		}
		stateRoots = append(stateRoots, canonical)
	}
	slices.Sort(stateRoots)
	policy := Policy{
		Version: policyVersion, WorkspaceRoot: workspace, PrivateTemp: privateTemp,
		RuntimeStateRoots: slices.Compact(stateRoots),
		RuntimeReadRoots:  runtimeRoots, HostReadRoots: hostRoots,
		HostReadFiles:       hostFiles,
		Toolchains:          toolchains,
		AllowNetwork:        options.AllowNetwork,
		ManagedProxyPort:    options.ManagedProxyPort,
		EnvironmentContract: "v1",
		EnvironmentProfile:  options.EnvironmentProfile,
		SharedUserTemp:      options.SharedUserTemp,
		HostWriteRoots:      writeRoots,
		EnvironmentNetwork:  normalizeEnvironmentNetwork(options.EnvironmentNetwork),
		EnvironmentValues:   environmentValues,
		ownsPrivateTemp:     ownsPrivateTemp,
	}
	if policy.ManagedProxyPort != 0 {
		policy.ManagedProxyCredential = options.ManagedProxyCredential
	}
	hashInput := policy
	hashInput.ID = ""
	encoded, err := json.Marshal(hashInput)
	if err != nil {
		return Policy{}, err
	}
	sum := sha256.Sum256(encoded)
	policy.ID = "sandbox-v2-" + hex.EncodeToString(sum[:16])
	return policy, nil
}

func canonicalRuntimeRoot(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func normalizeEnvironmentNetwork(
	targets []EnvironmentNetworkTarget,
) []EnvironmentNetworkTarget {
	if len(targets) == 0 {
		return nil
	}
	out := make([]EnvironmentNetworkTarget, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		host := strings.TrimSpace(target.Host)
		if host == "" {
			continue
		}
		methods := append([]string(nil), target.Methods...)
		slices.Sort(methods)
		key := fmt.Sprintf(
			"%s\x00%s\x00%d\x00%t\x00%s",
			host, strings.TrimSpace(target.Protocol), target.Port,
			target.AllowPrivate, strings.Join(methods, ","),
		)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, EnvironmentNetworkTarget{
			Host:         host,
			Protocol:     strings.TrimSpace(target.Protocol),
			Port:         target.Port,
			Methods:      methods,
			AllowPrivate: target.AllowPrivate,
		})
	}
	slices.SortFunc(out, func(left, right EnvironmentNetworkTarget) int {
		if left.Host != right.Host {
			return strings.Compare(left.Host, right.Host)
		}
		if left.Port != right.Port {
			return int(left.Port) - int(right.Port)
		}
		return strings.Compare(left.Protocol, right.Protocol)
	})
	return out
}

// refuseUndeliveredManagedNetwork reports honestly when a v1 environment
// network grant or proxy port cannot be delivered. The
// stable workspace channel delivers declared environment targets through
// its Gate, so a managed port satisfies delivery without a per-command session.
func refuseUndeliveredManagedNetwork(policy Policy, command Command) error {
	if command.DenyNetwork {
		return nil
	}
	if SupportsManagedNetworkProxy() &&
		(command.SessionProxyPort != 0 || policy.ManagedProxyPort != 0) {
		return nil
	}
	if len(policy.EnvironmentNetwork) == 0 && command.SessionProxyPort == 0 {
		return nil
	}
	return fmt.Errorf(
		"managed process network is unavailable: backend_capability_unsupported",
	)
}

func BindPolicy(backend Backend, options Options) (Backend, error) {
	if backend == nil {
		return nil, errors.New("sandbox backend injection is required")
	}
	if existing, ok := BackendPolicy(backend); ok {
		workspace, err := canonicalDirectory(options.WorkspaceRoot)
		if err != nil {
			return nil, err
		}
		if existing.WorkspaceRoot != workspace {
			return nil, errors.New("sandbox backend policy belongs to a different workspace")
		}
		return backend, nil
	}
	policy, err := BuildPolicy(options)
	if err != nil {
		return nil, err
	}
	return &policyBinding{Backend: backend, policy: policy}, nil
}

type policyBinding struct {
	Backend
	policy Policy
}

func (b *policyBinding) Policy() Policy { return b.policy }

func (b *policyBinding) Prepare(ctx context.Context, command Command) (Command, error) {
	prepared, err := b.Backend.Prepare(ctx, command)
	if err != nil {
		return Command{}, err
	}
	prepared.PreparedPolicyID = b.policy.ID
	prepared.PreparedAuthorityDigest = command.AuthorityDigest
	capability := b.Backend.Capability()
	prepared.PreparedControls, err = CommandControls(capability, b.policy, command)
	if err != nil {
		return Command{}, err
	}
	return prepared, nil
}

func EffectiveControls(
	capability Capability,
	policy Policy,
) securitymodel.Controls {
	controls := capability.Effective
	controls.Network = NetworkControl(capability, policy, ReachTargets)
	return controls
}

// NetworkReach is the network reach one execution asks for.
type NetworkReach uint8

const (
	ReachNone NetworkReach = iota
	ReachLoopback
	ReachTargets
)

// NetworkControl is the single derivation of the network control an execution
// runs under. Authority compilation and backend Prepare both call it, so the
// prepared control can be checked against the compiled one instead of being
// computed independently.
func NetworkControl(
	capability Capability,
	policy Policy,
	reach NetworkReach,
) securitymodel.Network {
	desired := securitymodel.NetworkDenied
	switch reach {
	case ReachLoopback:
		desired = securitymodel.NetworkLoopbackAny
	case ReachTargets:
		switch {
		case policy.ManagedProxyPort != 0:
			desired = securitymodel.NetworkProxyTargets
		case policy.AllowNetwork:
			desired = securitymodel.NetworkDirect
		}
	}
	if CanEnforceNetwork(capability, desired) {
		return desired
	}
	return capability.Effective.Network
}

// CanEnforceNetwork separates a backend's probed ability to allow a managed
// proxy from its default network-denied execution posture.
func CanEnforceNetwork(capability Capability, desired securitymodel.Network) bool {
	if desired == securitymodel.NetworkProxyTargets && capability.ManagedProxy {
		return capability.Available
	}
	return securitymodel.CanEnforceNetwork(capability.Effective.Network, desired)
}

// CommandNetworkPolicy narrows the base policy for one execution. The base
// identity is retained; the command's authority digest binds the reduction.
func CommandNetworkPolicy(policy Policy, command Command) Policy {
	if command.FullAccess {
		policy.FullAccess = true
		policy.AllowNetwork = command.CompiledNetwork == securitymodel.NetworkDirect
		if command.CompiledNetwork != securitymodel.NetworkProxyTargets {
			policy.ManagedProxyPort = 0
			policy.ManagedProxyCredential = ""
		}
	}
	if command.DenyNetwork || command.LoopbackOnly {
		policy.AllowNetwork = false
		policy.ManagedProxyPort = 0
		policy.ManagedProxyCredential = ""
	}
	return policy
}

// ApplySessionProxyPort binds one Process Session port onto the command policy
// after command-level network narrowing. A missing session port on a proxied
// command keeps the Workspace port only for capability identity; Prepare still
// reports whatever port the command actually received.
func ApplySessionProxyPort(policy Policy, command Command) Policy {
	policy = CommandNetworkPolicy(policy, command)
	if command.SessionProxyPort != 0 && policy.ManagedProxyPort != 0 {
		policy.ManagedProxyPort = command.SessionProxyPort
		policy.ManagedProxyCredential = command.SessionProxyCredential
	}
	return policy
}

func CommandControls(
	capability Capability,
	policy Policy,
	command Command,
) (securitymodel.Controls, error) {
	policy = CommandNetworkPolicy(policy, command)
	controls := capability.Effective
	if command.FullAccess {
		if !capability.Available || capability.Backend != "seatbelt" || command.AuthorityDigest == "" {
			return securitymodel.Controls{}, errors.New("full access requires an authorized Seatbelt execution")
		}
		controls.FilesystemRead = securitymodel.FilesystemReadUnrestricted
		controls.Syscall = securitymodel.SyscallUnrestricted
		controls.IPC = securitymodel.IPCPlatformFiltered
	}
	// These are the flags the backend actually emits. For an authorized
	// execution they validate the already-compiled network ceiling; they do
	// not reinterpret resource declarations or select a new authority.
	requested := securitymodel.NetworkDenied
	switch {
	case command.DenyNetwork:
	case policy.ManagedProxyPort != 0:
		requested = securitymodel.NetworkProxyTargets
	case command.AllowLoopback:
		requested = securitymodel.NetworkLoopbackAny
	case policy.AllowNetwork:
		requested = securitymodel.NetworkDirect
	}
	if command.AuthorityDigest != "" {
		if command.CompiledNetwork == "" {
			return securitymodel.Controls{}, errors.New("command has no compiled network control")
		}
		if !CanEnforceNetwork(capability, requested) && !(command.FullAccess && requested == securitymodel.NetworkDirect) {
			return securitymodel.Controls{}, errors.New("backend cannot enforce the command network control")
		}
		controls.Network = requested
		if err := (securitymodel.RequiredControls{Network: command.CompiledNetwork}).SatisfiedBy(controls); err != nil {
			return securitymodel.Controls{}, fmt.Errorf("command network exceeds compiled control: %w", err)
		}
	} else {
		// Standalone capability probes carry no execution lease and report the
		// backend's actual control, including an inability to isolate networking.
		if CanEnforceNetwork(capability, requested) {
			controls.Network = requested
		}
	}
	desiredWrite := securitymodel.FilesystemWriteWorkspace
	if command.WorkspaceReadOnly {
		if len(command.WorkspaceWritePaths) == 0 {
			desiredWrite = securitymodel.FilesystemWriteDenied
		} else {
			desiredWrite = securitymodel.FilesystemWriteExactPaths
		}
	}
	if command.FullAccess {
		controls.FilesystemWrite = securitymodel.FilesystemWriteUnrestricted
		if command.WorkspaceReadOnly {
			controls.FilesystemWrite = desiredWrite
		}
	} else if securitymodel.CanEnforceFilesystemWrite(
		controls.FilesystemWrite,
		desiredWrite,
	) {
		controls.FilesystemWrite = desiredWrite
	}
	return controls, nil
}

func (b *policyBinding) Close() error {
	// Join both: an inner-backend failure must not strand the owned private
	// temp (0700 session scratch) on disk.
	var errs []error
	if closer, ok := b.Backend.(interface{ Close() error }); ok {
		errs = append(errs, closer.Close())
	}
	errs = append(errs, closePolicyTemp(b.policy))
	return errors.Join(errs...)
}

func closePolicyTemp(policy Policy) error {
	if !policy.ownsPrivateTemp {
		return nil
	}
	return os.RemoveAll(policy.PrivateTemp)
}

func canonicalDirectory(path string) (string, error) {
	canonical, err := canonicalExisting(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return canonical, nil
}

func canonicalExisting(path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("path is empty or contains NUL")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("literal path must not be a symbolic link")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func canonicalHostReadFile(path string) (string, string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", "", errors.New("path is empty or contains NUL")
	}
	lexical, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	lexical = filepath.Clean(lexical)
	canonical, err := filepath.EvalSymlinks(lexical)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", errors.New("host read file is not regular")
	}
	return lexical, filepath.Clean(canonical), nil
}

func canonicalHostReadRoot(path string) (string, string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", "", errors.New("path is empty or contains NUL")
	}
	lexical, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	lexical = filepath.Clean(lexical)
	canonical, err := filepath.EvalSymlinks(lexical)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(canonical); err != nil {
		return "", "", err
	}
	return lexical, filepath.Clean(canonical), nil
}

func validateInjectedHostFile(path, workspace string) error {
	if err := validateSensitivePath(path); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if home := resolvedUserHome(); home != "" && parent == filepath.Clean(home) {
		return nil
	}
	return validateInjectedRoot(parent, workspace)
}

func validateInjectedRoot(root, workspace string) error {
	if isFilesystemRoot(root) {
		return errors.New("filesystem root is forbidden")
	}
	if home := resolvedUserHome(); home != "" {
		if root == filepath.Clean(home) {
			return errors.New("home root is forbidden")
		}
		for _, store := range pathpolicy.HomeCredentialRoots(home) {
			if pathContains(root, store) {
				return errors.New("roots containing a credential store are forbidden")
			}
		}
	}
	parent := filepath.Dir(workspace)
	if root == parent || pathContains(root, workspace) {
		return errors.New("workspace parents and workspace-wide host injection are forbidden")
	}
	return validateSensitivePath(root)
}

// validateSensitivePath rejects credential files and anything inside a
// credential store. The table is deliberately a denylist: unknown files stay
// declarable as host_config.
func validateSensitivePath(path string) error {
	if pathpolicy.IsCredentialFileName(filepath.Base(path)) {
		return errors.New("sensitive credential file is forbidden")
	}
	if pathpolicy.InCredentialLocation(path, resolvedUserHome()) {
		return errors.New("sensitive credential root is forbidden")
	}
	return nil
}

// resolvedUserHome keeps the lexical home when symlink resolution fails:
// collapsing the error to "" would silently disable every home-anchored
// protection.
func resolvedUserHome() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		return resolved
	}
	return home
}

// injectedRootOverlappingTemp returns the first injected path that equals,
// contains, or sits inside the private sandbox temp, or "" when disjoint.
func injectedRootOverlappingTemp(
	privateTemp string,
	hostRoots, hostFiles, writeRoots []string,
) string {
	if privateTemp == "" {
		return ""
	}
	check := func(paths []string) string {
		for _, path := range paths {
			if pathContains(path, privateTemp) || pathContains(privateTemp, path) {
				return path
			}
		}
		return ""
	}
	for _, group := range [][]string{hostRoots, hostFiles, writeRoots} {
		if violating := check(group); violating != "" {
			return violating
		}
	}
	return ""
}

func isFilesystemRoot(path string) bool {
	cleaned := filepath.Clean(path)
	volume := filepath.VolumeName(cleaned)
	remainder := strings.TrimPrefix(cleaned, volume)
	return remainder == string(filepath.Separator)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func platformRuntimeRoots(goos string) []string {
	switch goos {
	case "darwin":
		return []string{
			"/bin", "/sbin", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/share",
			"/System", "/Library/Apple", "/Library/Filesystems/NetFSPlugins",
			"/Library/Preferences/Logging",
			// Apple /usr/bin/{git,clang,…} are shims that exec into Command Line Tools
			// (or Xcode). Without these roots, seatbelt makes git report
			// "xcode-select: No developer tools were found".
			"/Library/Developer/CommandLineTools",
			"/Applications/Xcode.app/Contents/Developer",
			"/private/var/db/DarwinDirectory/local/recordStore.data",
			"/private/var/db/timezone", "/private/etc", "/etc",
			"/private/var/select",
			"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/fd",
		}
	default:
		return nil
	}
}

// ExecutableReadable reports whether the OS sandbox profile built from this
// policy grants file-read on the executable at path. It models the profile's
// effective read set — runtime roots, host read roots and files, the
// command's additional read paths, the workspace, and the private temp —
// on the symlink-resolved path, mirroring how the profile filters resolve
// at open time. Callers use it to fail fast with a structured denial
// instead of letting the child die on a bare EPERM errno.
func (p Policy) ExecutableReadable(path string, additionalReadPaths []string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return os.IsNotExist(err) && p.executablePathReadable(path, additionalReadPaths)
	}
	for _, candidate := range dependencyReadPaths(path, resolved) {
		if !p.executablePathReadable(candidate, additionalReadPaths) {
			return false
		}
	}
	return true
}

func (p Policy) executablePathReadable(candidate string, additionalReadPaths []string) bool {
	if validateSensitivePath(candidate) != nil {
		return false
	}
	// Resolve directory aliases (such as /var) but preserve the last link:
	// the OS must be allowed to traverse that intermediate alias itself.
	if parent, err := filepath.EvalSymlinks(filepath.Dir(candidate)); err == nil {
		candidate = filepath.Join(parent, filepath.Base(candidate))
	}
	roots := make([]string, 0, 8)
	roots = append(roots, p.RuntimeReadRoots...)
	roots = append(roots, p.HostReadRoots...)
	roots = append(roots, additionalReadPaths...)
	roots = append(roots, p.WorkspaceRoot, p.PrivateTemp)
	for _, root := range roots {
		root = filepath.Clean(root)
		if pathWithinRoot(candidate, root) {
			return true
		}
		// The candidate is symlink-resolved, so a lexical root must be
		// resolved too (macOS /var -> /private/var) before comparing.
		if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil &&
			pathWithinRoot(candidate, resolvedRoot) {
			return true
		}
	}
	for _, file := range p.HostReadFiles {
		file = filepath.Clean(file)
		if candidate == file {
			return true
		}
		if resolvedFile, err := filepath.EvalSymlinks(file); err == nil &&
			candidate == resolvedFile {
			return true
		}
	}
	return false
}

func pathWithinRoot(candidate, root string) bool {
	root = filepath.Clean(root)
	if root == "" {
		return false
	}
	if candidate == root {
		return true
	}
	return strings.HasPrefix(candidate, root+string(os.PathSeparator))
}
