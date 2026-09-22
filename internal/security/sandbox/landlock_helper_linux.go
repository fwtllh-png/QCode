//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

func init() {
	if len(os.Args) < 2 || os.Args[1] != landlockHelperArgument {
		return
	}
	if err := runLandlockHelper(os.Args[2:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "internal Landlock helper: %v\n", err)
		os.Exit(126)
	}
	os.Exit(126)
}

// prepareLandlockInvocation encodes filesystem rules only. The helper never
// delivers a Process Session network channel; v1 network grants must fail
// closed in the caller when the managed proxy is unavailable.
func prepareLandlockInvocation(
	policy Policy,
	helperPath string,
	requestRoot string,
	executable string,
	arguments []string,
	environment []string,
	workspaceReadOnly bool,
	additionalReadPaths []string,
	workspaceWritePaths []string,
) (string, string, error) {
	if helperPath == "" {
		return "", "", errors.New("Linux strong sandbox requires an injected helper executable")
	}
	if requestRoot == "" {
		return "", "", errors.New("Linux strong sandbox requires a private request root")
	}
	helper, err := resolveExecutableLiteral(helperPath, environment)
	if err != nil {
		return "", "", fmt.Errorf("resolve Landlock helper: %w", err)
	}
	target, err := resolveExecutableLiteral(executable, environment)
	if err != nil {
		return "", "", fmt.Errorf("resolve Landlock target: %w", err)
	}
	readOnly := append(append([]string{}, policy.RuntimeReadRoots...), policy.HostReadRoots...)
	readOnly = append(readOnly, policy.HostReadFiles...)
	readOnly = append(readOnly, additionalReadPaths...)
	readOnly = append(readOnly, target, helper)
	if workspaceReadOnly {
		readOnly = append(readOnly, policy.WorkspaceRoot)
	}
	slices.Sort(readOnly)
	readOnly = slices.Compact(readOnly)
	readWrite := []string{policy.PrivateTemp}
	if !workspaceReadOnly {
		readWrite = append(readWrite, policy.WorkspaceRoot)
	}
	readWrite = append(readWrite, workspaceWritePaths...)
	readWrite = append(readWrite, policy.HostWriteRoots...)
	slices.Sort(readWrite)
	request := landlockRequest{
		SchemaVersion: landlockSchemaVersion,
		PolicyID:      policy.ID,
		SyscallPolicy: policySyscallMode(policy),
		ReadOnly:      readOnly,
		ReadWrite:     readWrite,
		Executable:    target,
		Arguments:     append([]string{target}, arguments...),
	}
	encoded, err := encodeLandlockRequest(request)
	if err != nil {
		return "", "", err
	}
	requestDirectory, err := os.MkdirTemp(requestRoot, "request-")
	if err != nil {
		return "", "", fmt.Errorf("create Landlock helper request directory: %w", err)
	}
	if err := os.Chmod(requestDirectory, 0o700); err != nil {
		_ = os.RemoveAll(requestDirectory)
		return "", "", err
	}
	path := requestDirectory + "/policy.json"
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.RemoveAll(requestDirectory)
		return "", "", fmt.Errorf("create Landlock helper request: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.RemoveAll(requestDirectory)
	}
	if err := file.Chmod(0o600); err != nil {
		cleanup()
		return "", "", err
	}
	if _, err := file.Write(encoded); err != nil {
		cleanup()
		return "", "", err
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return "", "", err
	}
	if err := file.Close(); err != nil {
		_ = os.RemoveAll(requestDirectory)
		return "", "", err
	}
	return helper, path, nil
}

func createLandlockRequestRoot() (string, error) {
	root, err := os.MkdirTemp("", "qcode-landlock-requests-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return "", err
	}
	canonical, err := canonicalDirectory(root)
	if err != nil {
		_ = os.RemoveAll(root)
		return "", err
	}
	return canonical, nil
}

func runLandlockHelper(arguments []string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	requestPath, expectedPolicyID, err := parseLandlockHelperArguments(arguments)
	if err != nil {
		return err
	}
	fd, err := unix.Open(requestPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("open helper request")
	}
	file := os.NewFile(uintptr(fd), requestPath)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return errors.New("stat helper request")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 ||
		stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("unsafe helper request file")
	}
	if err := os.Remove(requestPath); err != nil {
		return errors.New("unlink helper request")
	}
	request, err := decodeLandlockRequest(file)
	if err != nil {
		return err
	}
	if request.PolicyID != expectedPolicyID {
		return errors.New("helper policy identity mismatch")
	}
	var rules []landlock.Rule
	for _, path := range request.ReadOnly {
		canonical, err := canonicalExisting(path)
		if err != nil || canonical != path {
			return errors.New("Landlock read root is not canonical")
		}
		info, err := os.Stat(path)
		if err != nil {
			return errors.New("Landlock read root is unavailable")
		}
		if info.IsDir() {
			rules = append(rules, landlock.RODirs(path))
		} else if info.Mode().IsRegular() || info.Mode()&os.ModeDevice != 0 {
			rules = append(rules, landlock.ROFiles(path))
		} else {
			return errors.New("Landlock read root has an unsupported type")
		}
	}
	for _, path := range request.ReadWrite {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Landlock write root is unavailable")
		}
		if info.IsDir() {
			canonical, err := canonicalDirectory(path)
			if err != nil || canonical != path {
				return errors.New("Landlock write root is unavailable")
			}
			rules = append(rules, landlock.RWDirs(path).WithRefer())
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path || !info.Mode().IsRegular() {
			return errors.New("Landlock write file is unavailable")
		}
		rules = append(rules, landlock.RWFiles(path))
	}
	if err := landlock.V3.RestrictPaths(rules...); err != nil {
		return fmt.Errorf("apply Landlock ABI v3 policy: %w", err)
	}
	if err := applyLinuxSyscallPolicy(request.SyscallPolicy); err != nil {
		return err
	}
	if err := os.Setenv("QCODE_LANDLOCK_ACTIVE", "1"); err != nil {
		return errors.New("set Landlock activation marker")
	}
	if err := os.Setenv("QCODE_NO_NEW_PRIVS_ACTIVE", "1"); err != nil {
		return errors.New("set no-new-privs activation marker")
	}
	if err := os.Setenv("QCODE_SECCOMP_ACTIVE", request.SyscallPolicy); err != nil {
		return errors.New("set seccomp activation marker")
	}
	if err := syscall.Exec(request.Executable, request.Arguments, os.Environ()); err != nil {
		return fmt.Errorf("exec sandbox target: %w", err)
	}
	return errors.New("exec sandbox target unexpectedly returned")
}
