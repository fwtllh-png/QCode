package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"github.com/Masterminds/semver/v3"
)

const LockSchemaV1 = 1

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type LockEntry struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Source       Source            `json:"source"`
	Digest       string            `json:"digest"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
}

type Lockfile struct {
	SchemaVersion  int         `json:"schema_version"`
	RuntimeVersion string      `json:"runtime_version"`
	Skills         []LockEntry `json:"skills"`
}

type LockStore struct {
	path string
	lock *sync.Mutex
}

var lockStoreLocks = struct {
	sync.Mutex
	values map[string]*sync.Mutex
}{values: make(map[string]*sync.Mutex)}

func NewLockStore(path string) (*LockStore, error) {
	if path == "" {
		return nil, errors.New("skill lock path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(absolute)
	if err := secureStateParent(parent); err != nil {
		return nil, err
	}
	lockStoreLocks.Lock()
	lock := lockStoreLocks.values[absolute]
	if lock == nil {
		lock = &sync.Mutex{}
		lockStoreLocks.values[absolute] = lock
	}
	lockStoreLocks.Unlock()
	return &LockStore{path: filepath.Clean(absolute), lock: lock}, nil
}

func (s *LockStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *LockStore) Read() (Lockfile, error) {
	if s == nil {
		return Lockfile{}, errors.New("skill lock store is required")
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	fileLock, err := acquireStateFileLock(s.path + ".guard")
	if err != nil {
		return Lockfile{}, err
	}
	defer fileLock.Close()
	return s.read()
}

func (s *LockStore) Write(lockfile Lockfile) error {
	if s == nil {
		return errors.New("skill lock store is required")
	}
	if err := validateLockfile(lockfile); err != nil {
		return err
	}
	skills := make([]LockEntry, len(lockfile.Skills))
	copy(skills, lockfile.Skills)
	for index := range skills {
		skills[index].Dependencies = cloneDependencies(skills[index].Dependencies)
	}
	lockfile.Skills = skills
	sort.Slice(lockfile.Skills, func(i, j int) bool {
		return lockfile.Skills[i].Name < lockfile.Skills[j].Name
	})
	data, err := json.MarshalIndent(lockfile, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	s.lock.Lock()
	defer s.lock.Unlock()
	fileLock, err := acquireStateFileLock(s.path + ".guard")
	if err != nil {
		return err
	}
	defer fileLock.Close()
	return atomicWriteFile(s.path, data)
}

func (s *LockStore) read() (Lockfile, error) {
	info, err := os.Lstat(s.path)
	if err != nil {
		return Lockfile{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Lockfile{}, errors.New("skill lock is not a regular file")
	}
	if err := rejectMultiplyLinkedState(s.path, info); err != nil {
		return Lockfile{}, err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return Lockfile{}, err
	}
	var lockfile Lockfile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lockfile); err != nil {
		return Lockfile{}, fmt.Errorf("decode skill lock: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Lockfile{}, errors.New("decode skill lock: multiple JSON values")
	}
	if err := validateLockfile(lockfile); err != nil {
		return Lockfile{}, err
	}
	return lockfile, nil
}

func validateLockfile(lockfile Lockfile) error {
	if lockfile.SchemaVersion != LockSchemaV1 {
		return fmt.Errorf("skill lock schema_version must be %d", LockSchemaV1)
	}
	if normalizeRuntimeVersion(lockfile.RuntimeVersion) != lockfile.RuntimeVersion {
		return errors.New("skill lock runtime_version must be normalized SemVer")
	}
	if _, err := semverVersion(lockfile.RuntimeVersion); err != nil {
		return fmt.Errorf("skill lock runtime_version: %w", err)
	}
	seen := make(map[string]bool, len(lockfile.Skills))
	versions := make(map[string]string, len(lockfile.Skills))
	for _, entry := range lockfile.Skills {
		if !namePattern.MatchString(entry.Name) || seen[entry.Name] {
			return fmt.Errorf("skill lock entry name %q is invalid or duplicated", entry.Name)
		}
		seen[entry.Name] = true
		if _, err := semverVersion(entry.Version); err != nil {
			return fmt.Errorf("skill lock entry %q: %w", entry.Name, err)
		}
		if !digestPattern.MatchString(entry.Digest) {
			return fmt.Errorf("skill lock entry %q digest is invalid", entry.Name)
		}
		versions[entry.Name] = entry.Version
		switch entry.Source {
		case SourceWorkspace, SourceConfigured, SourceUser:
		default:
			return fmt.Errorf("skill lock entry %q source is invalid", entry.Name)
		}
		for dependency, constraint := range entry.Dependencies {
			if !namePattern.MatchString(dependency) || constraint == "" {
				return fmt.Errorf("skill lock entry %q dependency is invalid", entry.Name)
			}
			if _, err := semver.NewConstraint(constraint); err != nil {
				return fmt.Errorf("skill lock entry %q dependency %q: %w", entry.Name, dependency, err)
			}
		}
	}
	for _, entry := range lockfile.Skills {
		for dependency, constraint := range entry.Dependencies {
			version, exists := versions[dependency]
			if !exists {
				return fmt.Errorf(
					"skill lock entry %q dependency %q is missing", entry.Name, dependency,
				)
			}
			if err := checkVersion(constraint, version); err != nil {
				return fmt.Errorf(
					"skill lock entry %q dependency %q does not satisfy %s",
					entry.Name, dependency, constraint,
				)
			}
		}
	}
	return nil
}

func semverVersion(version string) (string, error) {
	parsed, err := semver.StrictNewVersion(version)
	if err != nil || parsed.Original() != version {
		return "", fmt.Errorf("version %q is not strict SemVer", version)
	}
	return parsed.String(), nil
}

func atomicWriteFile(path string, data []byte) error {
	parent := filepath.Dir(path)
	if err := secureStateParent(parent); err != nil {
		return err
	}
	temp, err := os.CreateTemp(parent, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	if err := temp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(tempPath)
		return errors.New("skill lock symlink rejected")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	directory, err := os.Open(parent)
	if err == nil {
		err = errors.Join(directory.Sync(), directory.Close())
	}
	return err
}

func secureStateParent(parent string) error {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("skill lock parent must be a real directory")
	}
	return nil
}
