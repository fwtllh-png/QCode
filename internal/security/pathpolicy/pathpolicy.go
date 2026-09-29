// Package pathpolicy is the single table of filesystem locations the security
// layer treats specially: workspace control-plane entries that workload writes
// may never touch, and well-known credential stores that sandbox grants may
// never expose. It depends on no QCode package.
package pathpolicy

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Control-plane entry names. A workspace path is control plane when any of
// its components equals one of these, compared case-insensitively because
// the default macOS filesystem is case-insensitive.
const (
	// AgentsDir holds cross-agent skills and instructions (agents.md convention).
	AgentsDir = ".agents"
	// CodexDir is the Codex CLI project configuration directory.
	CodexDir = ".codex"
	// GitDir is Git repository metadata; a hook or config write there executes
	// on the next Git command.
	GitDir = ".git"
	// StateDir is QCode's own workspace and user state directory.
	StateDir = ".qcode"
	// WorktreeDir holds QCode-managed child worktrees.
	WorktreeDir = ".qcode-worktree"
)

var controlPlaneNames = []string{AgentsDir, CodexDir, GitDir, StateDir, WorktreeDir}

// ControlPlaneNames returns the control-plane entry names, sorted.
func ControlPlaneNames() []string {
	return slices.Clone(controlPlaneNames)
}

// ControlPlaneName returns the canonical control-plane name that component
// spells, if any.
func ControlPlaneName(component string) (string, bool) {
	for _, name := range controlPlaneNames {
		if strings.EqualFold(component, name) {
			return name, true
		}
	}
	return "", false
}

// Anchor is where a credential location is rooted.
type Anchor uint8

const (
	// AnchorHome locations sit directly below the user's home directory.
	AnchorHome Anchor = iota + 1
	// AnchorAnywhere locations are recognised by their path segments at any
	// depth, because the tools that own them honour relocation variables.
	AnchorAnywhere
)

// CredentialLocation is one well-known credential store: a directory whose
// contents a sandboxed command must never read through a grant.
type CredentialLocation struct {
	Anchor   Anchor
	Segments []string
	// Owner names the tool whose documentation places secrets here.
	Owner string
}

var credentialLocations = []CredentialLocation{
	{AnchorAnywhere, []string{".ssh"}, "OpenSSH"},
	{AnchorAnywhere, []string{".gnupg"}, "GnuPG"},
	{AnchorAnywhere, []string{"keychains"}, "macOS Keychain Services"},
	{AnchorAnywhere, []string{"credentials"}, "generic credential directory"},
	{AnchorAnywhere, []string{"secrets"}, "generic secret directory"},
	{AnchorAnywhere, []string{".kube"}, "kubectl"},
	{AnchorAnywhere, []string{".docker"}, "Docker CLI"},
	{AnchorAnywhere, []string{".azure"}, "Azure CLI"},
	{AnchorAnywhere, []string{".gcloud"}, "Google Cloud SDK (legacy location)"},
	{AnchorAnywhere, []string{".config", "gh"}, "GitHub CLI"},
	{AnchorHome, []string{".aws"}, "AWS CLI"},
	{AnchorHome, []string{"Library", "Keychains"}, "macOS Keychain Services"},
}

// NetrcFile is the home-relative netrc read by ftp(1), curl, and Go modules
// when $NETRC is unset.
const NetrcFile = ".netrc"

// credentialFileNames are credential files recognised by base name wherever
// they appear.
var credentialFileNames = []string{
	".git-credentials", // git-credential-store
	NetrcFile,          // ftp(1), curl, Go modules
	".netrc.gpg",       // encrypted netrc
	".npmrc",           // npm auth tokens
	".wgetrc",          // wget credentials
	// OpenSSH private keys
	"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
}

// CredentialLocations returns the credential store table.
func CredentialLocations() []CredentialLocation {
	locations := make([]CredentialLocation, 0, len(credentialLocations))
	for _, location := range credentialLocations {
		location.Segments = slices.Clone(location.Segments)
		locations = append(locations, location)
	}
	return locations
}

// IsCredentialFileName reports whether base names a credential file.
func IsCredentialFileName(base string) bool {
	return slices.ContainsFunc(credentialFileNames, func(name string) bool {
		return strings.EqualFold(base, name)
	})
}

// InCredentialLocation reports whether path is, or sits inside, a credential
// store. Segments compare case-insensitively; home may be empty when the
// user's home directory is unknown, which disables only the home-anchored
// entries.
func InCredentialLocation(path, home string) bool {
	segments := splitSegments(path)
	for _, location := range credentialLocations {
		switch location.Anchor {
		case AnchorAnywhere:
			if containsRun(segments, location.Segments) {
				return true
			}
		case AnchorHome:
			if home == "" {
				continue
			}
			root := append(splitSegments(home), location.Segments...)
			if hasPrefixFold(segments, root) {
				return true
			}
		}
	}
	return false
}

// HomeCredentialRoots returns every credential location as a directory below
// home. The sandbox denies these outright so a grant on one of their
// ancestors never exposes them.
func HomeCredentialRoots(home string) []string {
	if home == "" {
		return nil
	}
	roots := make([]string, 0, len(credentialLocations))
	for _, location := range credentialLocations {
		roots = append(roots, filepath.Join(append([]string{home}, location.Segments...)...))
	}
	return roots
}

// CanonicalAllowMissing resolves symlinks in the longest existing prefix of
// path and appends the missing remainder unchanged, so a not-yet-created
// write target canonicalizes to where it will actually be created.
func CanonicalAllowMissing(path string) (string, error) {
	current := filepath.Clean(path)
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

func splitSegments(path string) []string {
	var segments []string
	for segment := range strings.SplitSeq(filepath.ToSlash(filepath.Clean(path)), "/") {
		if segment != "" && segment != "." {
			segments = append(segments, segment)
		}
	}
	return segments
}

func containsRun(segments, run []string) bool {
	for start := 0; start+len(run) <= len(segments); start++ {
		if hasPrefixFold(segments[start:], run) {
			return true
		}
	}
	return false
}

func hasPrefixFold(segments, prefix []string) bool {
	if len(prefix) == 0 || len(segments) < len(prefix) {
		return false
	}
	for index, segment := range prefix {
		if !strings.EqualFold(segments[index], segment) {
			return false
		}
	}
	return true
}
