package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

func TestToolchainProbeOutputBoundaries(t *testing.T) {
	var output toolchainProbeOutput
	if n, err := output.Write(bytes.Repeat([]byte("x"), ToolchainProbeMaxOutputBytes)); err != nil || n != ToolchainProbeMaxOutputBytes {
		t.Fatalf("exact bound: n=%d err=%v", n, err)
	}
	if n, err := output.Write([]byte("x")); err == nil || n != 0 || output.Len() != ToolchainProbeMaxOutputBytes {
		t.Fatalf("overflow accepted: n=%d len=%d err=%v", n, output.Len(), err)
	}
}

func TestCertificateDiscoveryUsesToolchainReportedDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin, config, trust := filepath.Join(root, "tools", "bin"), filepath.Join(root, "custom-config"), filepath.Join(root, "public-trust")
	for _, path := range []string{bin, config, trust} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bundle := filepath.Join(trust, "roots.pem")
	if err := os.WriteFile(bundle, []byte("public certificate fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(config, "cert.pem")
	if err := os.Symlink(bundle, link); err != nil {
		t.Fatal(err)
	}
	program := "#!/bin/sh\n[ \"$1\" = version ] && [ \"$2\" = -d ] || exit 1\nprintf '%s\\n' 'OPENSSLDIR: " + strconv.Quote(config) + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "openssl"), []byte(program), 0o755); err != nil {
		t.Fatal(err)
	}
	exposure := ToolchainExposure{BinDirs: []string{bin}}
	discoverCertificateFiles(&exposure, filepath.Join(root, "workspace"))
	if !slices.Contains(exposure.ReadFiles, bundle) || !slices.Contains(exposure.ReadFiles, link) {
		t.Fatalf("reported certificate dependency missing: %+v", exposure)
	}
	if len(exposure.ReadRoots) != 0 || len(exposure.ReadFiles) != 2 {
		t.Fatalf("certificate discovery broadened access: %+v", exposure)
	}
	for _, output := range []string{"garbage", "OPENSSLDIR: relative", `OPENSSLDIR: "relative"`, "OPENSSLDIR: \"/a\"\nOTHER: \"/b\""} {
		if _, err := opensslCertificateFile(output); err == nil {
			t.Fatalf("accepted invalid metadata: %q", output)
		}
	}
}

func TestConfiguredCertificateFilesPreserveValidatedEnvironment(t *testing.T) {
	for _, name := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		t.Setenv(name, "")
	}
	root := t.TempDir()
	file := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(file, []byte("certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_EXTRA_CA_CERTS", file)
	var exposure ToolchainExposure
	if err := configuredCertificateFiles(&exposure, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(exposure.Environment, "NODE_EXTRA_CA_CERTS="+file) {
		t.Fatalf("certificate setting lost: %+v", exposure)
	}
	t.Setenv("SSL_CERT_FILE", filepath.Join(root, "missing"))
	if err := configuredCertificateFiles(&exposure, t.TempDir()); err == nil {
		t.Fatal("missing explicit certificate silently ignored")
	}
}

func TestCertificateExposureRejectsUntrustedPaths(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	for _, name := range []string{"workspace", "secrets", "public"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(root, name, "ca.pem")
		if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "public", "linked.pem")
	if err := os.Symlink(filepath.Join(root, "secrets", "ca.pem"), link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative.pem", workspace, filepath.Join(workspace, "ca.pem"), link, filepath.Join(root, "missing.pem")} {
		var exposure ToolchainExposure
		if err := exposeCertificateFile(&exposure, path, workspace); err == nil || len(exposure.ReadFiles) != 0 {
			t.Fatalf("invalid certificate path exposed: %s %+v %v", path, exposure, err)
		}
	}
}
