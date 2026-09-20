package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	skillruntime "github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/buildinfo"
	"github.com/fwtllh-png/QCode/internal/persist/extensioncontrol"
	extensionapp "github.com/fwtllh-png/QCode/internal/runtime/app/extension"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// SkillOptions is the explicit production configuration surface for
// skills. Empty fields are resolved to safe QCode-owned locations by
// ResolveSkillPaths.
type SkillOptions struct {
	DataDir             string
	SkillsConfiguredDir string
	SkillsStatePath     string
	SkillsLockPath      string
	SkillsLocale        string
	UserHome            string
}

type SkillPaths struct {
	DataDir             string
	SkillsConfiguredDir string
	SkillsStatePath     string
	SkillsLockPath      string
	SkillsLocale        string
	UserHome            string
}

type SkillControlHandle struct {
	Service *extensionapp.SkillService
}

func (s *Session) OpenSkillControl(paths SkillPaths, workspace string) (*SkillControlHandle, error) {
	policy, _ := sandbox.BackendPolicy(s.sandbox)
	return openSkillControl(paths, workspace, policy.PrivateTemp)
}

func openSkillControl(
	paths SkillPaths,
	workspace, sandboxHome string,
) (*SkillControlHandle, error) {
	state, err := skillruntime.NewStateStore(paths.SkillsStatePath)
	if err != nil {
		return nil, err
	}
	lock, err := skillruntime.NewLockStore(paths.SkillsLockPath)
	if err != nil {
		return nil, err
	}
	skills, err := skillruntime.Discover(skillruntime.DiscoveryOptions{
		Workspace: workspace, ConfiguredDir: paths.SkillsConfiguredDir,
		UserHome: paths.UserHome, Locale: paths.SkillsLocale,
		SandboxHome:     sandboxHome,
		IncludeBuiltins: true,
		State:           state, Lock: lock, RuntimeVersion: buildinfo.Version,
	})
	if err != nil {
		return nil, err
	}
	store, err := extensioncontrol.Open(
		filepath.Join(paths.DataDir, extensioncontrol.FileName),
	)
	if err != nil {
		return nil, err
	}
	service, err := extensionapp.NewSkillService(skills, store)
	if err != nil {
		return nil, err
	}
	return &SkillControlHandle{Service: service}, nil
}

func (h *SkillControlHandle) Close() error { return nil }

func ResolveSkillPaths(options SkillOptions, workspace string) (SkillPaths, error) {
	workspace, err := realDirectory(workspace)
	if err != nil {
		return SkillPaths{}, fmt.Errorf("resolve skills workspace: %w", err)
	}
	home := strings.TrimSpace(options.UserHome)
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return SkillPaths{}, fmt.Errorf("resolve user home: %w", err)
		}
	}
	home, err = absoluteClean(home)
	if err != nil {
		return SkillPaths{}, fmt.Errorf("resolve user home: %w", err)
	}
	dataDir := strings.TrimSpace(options.DataDir)
	if dataDir == "" {
		dataDir = filepath.Join(workspace, ".qcode", "data")
	}
	dataDir, err = absoluteClean(dataDir)
	if err != nil {
		return SkillPaths{}, fmt.Errorf("resolve skills data directory: %w", err)
	}
	workspaceDigest := sha256.Sum256([]byte(workspace))
	workspaceID := hex.EncodeToString(workspaceDigest[:8])
	paths := SkillPaths{
		DataDir:             dataDir,
		SkillsConfiguredDir: firstPath(options.SkillsConfiguredDir, filepath.Join(dataDir, "skills", "configured")),
		SkillsStatePath:     firstPath(options.SkillsStatePath, filepath.Join(dataDir, "skills", "state.json")),
		SkillsLockPath: firstPath(
			options.SkillsLockPath,
			filepath.Join(dataDir, "skills", "locks", workspaceID+".lock.json"),
		),
		SkillsLocale: strings.TrimSpace(options.SkillsLocale),
		UserHome:     home,
	}
	for _, target := range []*string{
		&paths.SkillsConfiguredDir, &paths.SkillsStatePath,
		&paths.SkillsLockPath,
	} {
		*target, err = absoluteClean(*target)
		if err != nil {
			return SkillPaths{}, err
		}
	}
	return paths, nil
}

func firstPath(configured, fallback string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	return fallback
}

func absoluteClean(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("skill path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func realDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
