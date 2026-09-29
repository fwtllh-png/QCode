//go:build darwin

package sandbox

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResolveMachOLibraryUsesLoaderExecutableAndRPath(t *testing.T) {
	root := t.TempDir()
	executableDir := filepath.Join(root, "tool", "bin")
	loaderDir := filepath.Join(root, "tool", "lib")
	dependencyDir := filepath.Join(root, "dependency", "lib")
	for _, directory := range []string{
		executableDir,
		loaderDir,
		dependencyDir,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	loader := filepath.Join(loaderDir, "libtool.dylib")
	for _, path := range []string{
		loader,
		filepath.Join(loaderDir, "loader.dylib"),
		filepath.Join(executableDir, "executable.dylib"),
		filepath.Join(dependencyDir, "rpath.dylib"),
	} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(dependencyDir)
	tests := []struct {
		name    string
		library string
		rpaths  []string
		want    string
	}{
		{
			name:    "loader path",
			library: "@loader_path/loader.dylib",
			want:    filepath.Join(loaderDir, "loader.dylib"),
		},
		{
			name:    "executable path",
			library: "@executable_path/executable.dylib",
			want:    filepath.Join(executableDir, "executable.dylib"),
		},
		{
			name:    "rpath",
			library: "@rpath/rpath.dylib",
			rpaths: []string{
				filepath.Join(root, "missing"),
				dependencyDir,
			},
			want: filepath.Join(dependencyDir, "rpath.dylib"),
		},
		{
			name:    "relative rpath cannot fall back to process cwd",
			library: "@rpath/rpath.dylib",
			rpaths:  []string{"relative"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := resolveMachOLibrary(
				test.library,
				loader,
				executableDir,
				test.rpaths,
			)
			if got != test.want {
				t.Fatalf("resolveMachOLibrary() = %q, want %q", got, test.want)
			}
		})
	}
}

// Minimal Mach-O images make dependency binding independent of installed tools.
func writeDependencyImage(t *testing.T, path string, libraries ...string) {
	t.Helper()
	var commands bytes.Buffer
	for _, library := range libraries {
		size := uint32((24 + len(library) + 1 + 7) &^ 7)
		for _, value := range []uint32{uint32(macho.LoadCmdDylib), size, 24, 0, 0, 0} {
			if err := binary.Write(&commands, binary.LittleEndian, value); err != nil {
				t.Fatal(err)
			}
		}
		commands.WriteString(library)
		commands.Write(make([]byte, int(size)-24-len(library)))
	}
	var image bytes.Buffer
	for _, value := range []uint32{macho.Magic64, uint32(macho.CpuArm64), 0, uint32(macho.TypeExec), uint32(len(libraries)), uint32(commands.Len()), 0, 0} {
		if err := binary.Write(&image, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	image.Write(commands.Bytes())
	if err := os.WriteFile(path, image.Bytes(), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestPATHBindsTransitiveLibraryFilesWithoutAdjacentState(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin, libraryDir := filepath.Join(root, "bin"), filepath.Join(root, "libraries")
	for _, dir := range []string{bin, libraryDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	first, second := filepath.Join(libraryDir, "first.dylib"), filepath.Join(libraryDir, "second.dylib")
	alias := filepath.Join(libraryDir, "alias.dylib")
	aliasDir := filepath.Join(root, "library-alias")
	if err := os.Symlink(libraryDir, aliasDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(libraryDir, ".netrc")
	writeDependencyImage(t, private)
	adjacent := filepath.Join(libraryDir, "private.conf")
	writeDependencyImage(t, filepath.Join(bin, "unrecognized-program"), filepath.Join(aliasDir, "alias.dylib"), private, adjacent)
	writeDependencyImage(t, first, "@loader_path/second.dylib")
	writeDependencyImage(t, second, "@loader_path/first.dylib") // A cycle must terminate.
	if err := os.WriteFile(adjacent, []byte("not a dependency"), 0o600); err != nil {
		t.Fatal(err)
	}
	var exposure ToolchainExposure
	exposePATHExecutables(&exposure, bin, filepath.Join(root, "workspace"), map[string]bool{})
	for _, file := range []string{alias, filepath.Join(aliasDir, "alias.dylib"), first, second} {
		if !slices.Contains(exposure.ReadFiles, file) {
			t.Fatalf("missing dependency %s: %+v", file, exposure)
		}
	}
	if slices.Contains(exposure.ReadFiles, adjacent) || slices.Contains(exposure.ReadFiles, private) || len(exposure.ReadRoots) != 0 {
		t.Fatalf("dependency binding exposed adjacent state: %+v", exposure)
	}
}
