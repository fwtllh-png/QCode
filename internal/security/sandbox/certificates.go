package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ToolchainProbeTimeout bounds a local, noninteractive toolchain metadata query,
// using the same five-second budget as the sandbox capability probe.
const ToolchainProbeTimeout = 5 * time.Second

// ToolchainProbeMaxOutputBytes is the public safety ceiling for local metadata
// output. A directory query has one path; larger output is rejected, not parsed.
const ToolchainProbeMaxOutputBytes = 64 << 10

type toolchainProbeOutput struct{ buffer bytes.Buffer }

func (b *toolchainProbeOutput) Len() int       { return b.buffer.Len() }
func (b *toolchainProbeOutput) String() string { return b.buffer.String() }

func (b *toolchainProbeOutput) Write(data []byte) (int, error) {
	if len(data) > ToolchainProbeMaxOutputBytes-b.Len() {
		return 0, errors.New("toolchain metadata exceeds output limit")
	}
	return b.buffer.Write(data)
}

// DiscoverCertificateFiles is the environment-preparer entry for public CA
// files. BuildPolicy does not call it when contract=v1.
func DiscoverCertificateFiles(exposure *ToolchainExposure, workspace string) {
	discoverCertificateFiles(exposure, workspace)
}

// discoverCertificateFiles queries OpenSSL's documented OPENSSLDIR instead of
// inferring a trust store from an installation prefix or package manager.
func discoverCertificateFiles(exposure *ToolchainExposure, workspace string) {
	seen := make(map[string]bool)
	directories := append([]string(nil), exposure.BinDirs...)
	for _, root := range exposure.ReadRoots {
		directories = append(directories, filepath.Join(root, "bin"))
	}
	for _, directory := range directories {
		executable := filepath.Join(directory, "openssl")
		canonical, err := filepath.EvalSymlinks(executable)
		if err != nil || seen[canonical] ||
			validateInjectedRoot(filepath.Dir(canonical), workspace) != nil ||
			pathContains(workspace, canonical) {
			continue
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		seen[canonical] = true
		ctx, cancel := context.WithTimeout(context.Background(), ToolchainProbeTimeout)
		command := exec.CommandContext(ctx, canonical, "version", "-d")
		command.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
		command.WaitDelay = ToolchainProbeTimeout
		var output toolchainProbeOutput
		command.Stdout = &output
		err = command.Run()
		cancel()
		if err != nil {
			continue
		}
		path, err := opensslCertificateFile(output.String())
		if err == nil {
			_ = exposeCertificateFile(exposure, path, workspace)
		}
	}
}

func opensslCertificateFile(output string) (string, error) {
	label, value, ok := strings.Cut(strings.TrimSpace(output), ":")
	if !ok || label != "OPENSSLDIR" {
		return "", fmt.Errorf("OpenSSL did not report OPENSSLDIR")
	}
	directory, err := strconv.Unquote(strings.TrimSpace(value))
	if err != nil || !filepath.IsAbs(directory) {
		return "", fmt.Errorf("OpenSSL reported an invalid OPENSSLDIR")
	}
	// cert.pem is OpenSSL's documented default file under OPENSSLDIR.
	return filepath.Join(directory, "cert.pem"), nil
}

func configuredCertificateFiles(exposure *ToolchainExposure, workspace string) error {
	for _, name := range []string{
		"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	} {
		path := os.Getenv(name)
		if path == "" {
			continue
		}
		if err := exposeCertificateFile(exposure, path, workspace); err != nil {
			return fmt.Errorf("%s certificate dependency: %w", name, err)
		}
		exposure.Environment = append(exposure.Environment, name+"="+path)
	}
	slices.Sort(exposure.ReadFiles)
	slices.Sort(exposure.Environment)
	return nil
}

func exposeCertificateFile(exposure *ToolchainExposure, path, workspace string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("certificate path must be absolute")
	}
	lexical, canonical, err := canonicalHostReadFile(path)
	if err != nil {
		return err
	}
	for _, candidate := range []string{lexical, canonical} {
		if err := validateInjectedRoot(filepath.Dir(candidate), workspace); err != nil {
			return err
		}
		if pathContains(workspace, candidate) {
			return fmt.Errorf("workspace certificate injection is forbidden")
		}
	}
	// Keep the checked identities. Resolving the link again here could grant a
	// different target if the host changes it between validation and insertion.
	for _, candidate := range []string{lexical, canonical} {
		if !slices.Contains(exposure.ReadFiles, candidate) {
			exposure.ReadFiles = append(exposure.ReadFiles, candidate)
		}
	}
	return nil
}
