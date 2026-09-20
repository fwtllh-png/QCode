package permissions

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fwtllh-png/QCode/internal/security/policy"
)

var ErrAuthorityInsideWorkspace = errors.New("security authority data directory must be outside the workspace")

func Path(dataDir, workspace string) (string, error) {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", errors.New("permissions workspace must be an existing directory")
	}
	dataDir, err = filepath.EvalSymlinks(dataDir)
	if err != nil {
		return "", err
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	if relative, relErr := filepath.Rel(root, dataDir); relErr == nil &&
		(relative == "." || (relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
		return "", ErrAuthorityInsideWorkspace
	}
	identity := reflect.Indirect(reflect.ValueOf(info.Sys()))
	device, inode := identityField(identity, "Dev"), identityField(identity, "Ino")
	sum := sha256.Sum256([]byte(root + "\x00" + device + "\x00" + inode))
	return filepath.Join(
		dataDir, "security", "workspaces", hex.EncodeToString(sum[:]), FileName,
	), nil
}
func identityField(value reflect.Value, name string) string {
	if value.IsValid() {
		if field := value.FieldByName(name); field.IsValid() && field.CanInterface() {
			return fmt.Sprint(field.Interface())
		}
	}
	return "0"
}
func OpenWorkspaceStore(dataDir, workspace string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path, err := Path(dataDir, workspace)
	if err != nil {
		return nil, err
	}
	return OpenStore(path)
}

type Store struct {
	Path     string
	mu       sync.Mutex
	snapshot atomic.Pointer[ruleSnapshot]
}

type ruleSnapshot struct {
	rules   []policy.Rule
	version uint64
}

func OpenStore(path string) (*Store, error) {
	bundle, err := Load(path)
	if err != nil {
		return nil, err
	}
	store := &Store{Path: path}
	store.snapshot.Store(&ruleSnapshot{rules: bundle.Rules, version: 1})
	return store, nil
}
func (s *Store) Rules() []policy.Rule {
	rules, _ := s.UserRulesSince(0)
	return rules
}

// UserRulesSince is the in-memory policy source shared by workspace Guards.
// Unchanged versions allocate nothing and never wait for a disk write.
func (s *Store) UserRulesSince(version uint64) ([]policy.Rule, uint64) {
	if s == nil {
		return nil, 0
	}
	snapshot := s.snapshot.Load()
	if snapshot == nil {
		return nil, 0
	}
	if snapshot.version == version {
		return nil, version
	}
	return append([]policy.Rule(nil), snapshot.rules...), snapshot.version
}
func (s *Store) AppendAllow(
	invocation policy.Invocation,
) (policy.Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rule, err := RuleFromInvocation(invocation)
	if err != nil {
		return policy.Rule{}, err
	}
	bundle, err := AppendAllow(s.Path, rule)
	if err != nil {
		return policy.Rule{}, err
	}
	current := s.snapshot.Load()
	if !slices.Equal(current.rules, bundle.Rules) {
		s.snapshot.Store(&ruleSnapshot{rules: bundle.Rules, version: current.version + 1})
	}
	return rule, nil
}
