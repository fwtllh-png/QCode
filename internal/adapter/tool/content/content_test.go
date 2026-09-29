package content

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/orchestration/workspacebroker"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func TestContentToolsWithRealFilesAndFixtureDependencies(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "image.pgm", "P2\n1 1\n255\n0\n")
	writeFile(t, root, "audio.wav", "RIFF\x00\x00\x00\x00WAVE")
	writeFile(t, root, "input.md", "# Heading\n")
	writeFile(t, root, "valid.json", `{"ok":true}`)
	writeFile(t, root, "valid.csv", "name,value\nfixture,1\n")
	writeFile(t, root, "invalid.csv", "name,value\nfixture\n")

	ocr := writeExecutable(t, root, "ocr-fixture", `#!/bin/sh
test -f "$1" || exit 3
printf 'recognized image\n'
`)
	speech := writeExecutable(t, root, "speech-fixture", `#!/bin/sh
input="$1"
shift
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output_dir" ]; then
    shift
    output_dir="$1"
  fi
  shift
done
name=$(basename "$input")
name=${name%.*}
printf 'fixture transcript\n' > "$output_dir/$name.txt"
`)
	pandoc := writeExecutable(t, root, "pandoc-fixture", `#!/bin/sh
input="$1"
shift
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    shift
    output="$1"
  fi
  shift
done
printf '<h1>Converted</h1>\n' > "$output"
test -f "$input"
`)
	t.Setenv("QCODE_TESSERACT_BINARY", ocr)
	t.Setenv("QCODE_SPEECH_BINARY", speech)
	t.Setenv("QCODE_PANDOC_BINARY", pandoc)
	t.Setenv("QCODE_FFMPEG_BINARY", ocr)

	registry := tool.NewRegistry(nil, nil)
	broker, err := workspacebroker.New(
		root,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterWithBackendAndRuntime(
		registry, root, contentTestBackend{}, broker,
	); err != nil {
		t.Fatal(err)
	}
	probe := Probe()
	if !probe["ocr"] || !probe["speech"] || !probe["pandoc"] || !probe["ffmpeg"] {
		t.Fatalf("Probe under fixture env = %+v", probe)
	}
	result := execute(t, registry, "content_capabilities", `{}`)
	if result.IsError || !strings.Contains(result.Content, `"speech":true`) {
		t.Fatalf("capabilities = %+v", result)
	}
	var caps struct {
		Available map[string]bool `json:"available"`
	}
	if err := json.Unmarshal([]byte(result.Content), &caps); err != nil {
		t.Fatal(err)
	}
	for key, want := range probe {
		if caps.Available[key] != want {
			t.Fatalf("capabilities[%s]=%v probe=%v", key, caps.Available[key], want)
		}
	}
	if result := execute(t, registry, "image_ocr", `{"path":"image.pgm"}`); result.Content != "recognized image\n" {
		t.Fatalf("ocr = %+v", result)
	}
	if result := execute(t, registry, "speech_transcribe", `{"path":"audio.wav"}`); result.Content != "fixture transcript\n" {
		t.Fatalf("speech = %+v", result)
	}
	converted := execute(t, registry, "document_convert", `{"path":"input.md","output_path":"output.html","from":"markdown","to":"html"}`)
	if converted.IsError {
		t.Fatalf("convert = %+v", converted)
	}
	output, err := os.ReadFile(filepath.Join(root, "output.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "<h1>Converted</h1>\n" {
		t.Fatalf("output = %q", output)
	}
	if result := execute(t, registry, "data_validate", `{"path":"valid.json","format":"json"}`); result.IsError {
		t.Fatalf("json validation = %+v", result)
	}
	if result := execute(t, registry, "data_validate", `{"path":"valid.csv","format":"csv"}`); result.IsError ||
		result.Metadata["rows"] != 2 {
		t.Fatalf("csv validation = %+v", result)
	}
	if result := execute(t, registry, "data_validate", `{"path":"invalid.csv","format":"csv"}`); !result.IsError {
		t.Fatalf("invalid csv = %+v", result)
	}
}

type contentTestBackend struct{}

func (contentTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough", Available: true,
		Effective: securitymodel.Controls{FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

			FilesystemWrite: securitymodel.FilesystemWriteExactPaths, Network: securitymodel.NetworkDenied,
			ProcessTree: securitymodel.ProcessTreeGroupKill, CrossProcess: securitymodel.CrossProcessUnrestricted, Syscall: securitymodel.SyscallDenyDangerous, IPC: securitymodel.IPCUnrestricted, PathIdentity: securitymodel.PathIdentityDescriptorRelative, ArtifactOrigin: securitymodel.ArtifactOriginUnverifiedPath,
			DurableRecovery: securitymodel.DurableRecoveryMemoryOnly,
		},
	}
}

func (contentTestBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	return command, nil
}

func TestContentDependencyUnavailableIsStable(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "image.pgm", "P2\n1 1\n255\n0\n")
	t.Setenv("QCODE_TESSERACT_BINARY", filepath.Join(root, "missing"))
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, contentTestBackend{}); err != nil {
		t.Fatal(err)
	}
	var descriptor *tool.Descriptor
	for _, item := range registry.Descriptors(tool.VisibleModel) {
		if item.Name == "image_ocr" {
			copy := item
			descriptor = &copy
			break
		}
	}
	if descriptor == nil || descriptor.Availability != tool.AvailabilityUnavailable ||
		descriptor.UnavailableReason == "" {
		t.Fatalf("descriptor = %+v", descriptor)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	executor := &Tool{root: root, workspace: workspace, kind: "image_ocr"}
	if err := executor.bindContract(); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(
		t.Context(), json.RawMessage(`{"path":"image.pgm"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Metadata["error_category"] != "unavailable" ||
		result.Metadata["dependency"] != "ocr" {
		t.Fatalf("result = %+v", result)
	}
}

func execute(t *testing.T, registry *tool.Registry, name, arguments string) tool.Result {
	t.Helper()
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: name, Arguments: json.RawMessage(arguments),
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
