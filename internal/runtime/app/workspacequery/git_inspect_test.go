package workspacequery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitInspectSeparatesIndexWorktreeAndUntracked(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "initial\n")
	write("remove.txt", "remove\n")
	write("image.bin", "\x00initial")
	write(".gitignore", ".secret\nignored.txt\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "initial")
	write("a.txt", "initial\nstaged\n")
	runGit(t, root, "add", "a.txt")
	write("a.txt", "initial\nstaged\nworking\n")
	write("new\tfile.txt", "hello\n")
	write("image.bin", "\x00modified")
	write(".secret", "do not expose\n")
	write("ignored.txt", "do not count\n")
	runGit(t, root, "add", "-f", ".secret")
	if err := os.Remove(filepath.Join(root, "remove.txt")); err != nil {
		t.Fatal(err)
	}
	service, err := New(root, queryTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	overview, err := service.GitOverview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]GitChange)
	for _, file := range overview.Files {
		files[file.Path] = file
	}
	if len(files) != 4 || !files["new\tfile.txt"].Untracked ||
		files["new\tfile.txt"].Unstaged == nil || files["new\tfile.txt"].Unstaged.Added != 1 ||
		files["a.txt"].Staged == nil || files["a.txt"].Staged.Added != 1 ||
		files["a.txt"].Unstaged == nil || files["a.txt"].Unstaged.Added != 1 ||
		files["image.bin"].Unstaged == nil || !files["image.bin"].Unstaged.Binary ||
		files["remove.txt"].Unstaged == nil || files["remove.txt"].Unstaged.Removed != 1 {
		t.Fatalf("unexpected Git files: %+v", files)
	}
	for _, staged := range []bool{false, true} {
		patch, err := service.GitPatch(t.Context(), "a.txt", staged)
		if err != nil {
			t.Fatal(err)
		}
		want := "+working"
		if staged {
			want = "+staged"
		}
		if !strings.Contains(patch.Diff, want) {
			t.Fatalf("staged=%t diff=%q", staged, patch.Diff)
		}
	}
	patch, err := service.GitPatch(t.Context(), "new\tfile.txt", false)
	if err != nil || !strings.Contains(patch.Diff, "+hello") {
		t.Fatalf("untracked patch=%+v err=%v", patch, err)
	}
	for _, name := range []string{".secret", "ignored.txt", "../outside", ":(glob)*", "not-present.txt"} {
		if _, err := service.GitPatch(t.Context(), name, false); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected inaccessible path %q, got %v", name, err)
		}
	}
}

func TestGitInspectCountsNewFilesBeforeAndAfterStaging(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	cases := []struct {
		name    string
		content string
		stat    GitChangeStat
	}{
		{"empty.txt", "", GitChangeStat{}},
		{"lines.txt", "first\n\nlast\n", GitChangeStat{Added: 3}},
		{"no-eol.txt", "first\nlast", GitChangeStat{Added: 2}},
		{"crlf.txt", "first\r\nlast\r\n", GitChangeStat{Added: 2}},
		{"blank.txt", "\n", GitChangeStat{Added: 1}},
		{"你好\tfile\nname.txt", "你好\n世界", GitChangeStat{Added: 2}},
		{" literal\\name.txt ", "literal\n", GitChangeStat{Added: 1}},
		{"binary.dat", "\x00one\ntwo\n", GitChangeStat{Binary: true}},
		{"limit.txt", strings.Repeat("x", GitInspectMaxBytes), GitChangeStat{Added: 1}},
	}
	for _, tc := range cases {
		if err := os.WriteFile(filepath.Join(root, tc.name), []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service, err := New(root, queryTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	for _, staged := range []bool{false, true} {
		if staged {
			runGit(t, root, "add", ".")
		}
		before, err := service.gitInspect(t.Context(), "status", "--porcelain=v1", "-z")
		if err != nil {
			t.Fatal(err)
		}
		overview, err := service.GitOverview(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(overview.Files) != len(cases) {
			t.Fatalf("unexpected files: %+v", overview.Files)
		}
		files := make(map[string]GitChange)
		for _, file := range overview.Files {
			files[file.Path] = file
		}
		for _, tc := range cases {
			file := files[tc.name]
			stat := file.Unstaged
			if staged {
				stat = file.Staged
			}
			if file.Untracked == staged || stat == nil || *stat != tc.stat {
				t.Errorf("staged=%t path=%q file=%+v stat=%+v want=%+v", staged, tc.name, file, stat, tc.stat)
			}
			if staged && file.Unstaged != nil || !staged && file.Staged != nil {
				t.Errorf("file counted in both scopes: %+v", file)
			}
		}
		if after, err := service.gitInspect(t.Context(), "status", "--porcelain=v1", "-z"); err != nil || after != before {
			t.Fatalf("GitOverview changed the index or worktree: err=%v", err)
		}
	}
}

func TestGitInspectParsesUnusualNamesAndRejectsIncompleteRecords(t *testing.T) {
	changes, err := parseGitChanges(" M dir/a\tb\nc.txt\x00?? dir/你好.txt\x00", "dir/")
	if err != nil || len(changes) != 2 || changes[0].Path != "a\tb\nc.txt" || changes[1].Path != "你好.txt" {
		t.Fatalf("changes=%+v err=%v", changes, err)
	}
	stats, err := parseGitNumstat("12\t3\ta\tb\nc.txt\x00-\t-\timage.png\x00")
	if err != nil || stats["a\tb\nc.txt"].Added != 12 || !stats["image.png"].Binary {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	for _, invalid := range []string{"1\t2\tpath", "1\x00", "-\t2\tpath\x00", "-1\t2\tpath\x00"} {
		if _, err := parseGitNumstat(invalid); err == nil {
			t.Fatalf("accepted invalid numstat %q", invalid)
		}
	}
	if _, err := parseGitChanges(" M a", ""); err == nil {
		t.Fatal("accepted incomplete status")
	}
}

func TestGitInspectUnbornAndNestedWorkspace(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	if err := os.WriteFile(filepath.Join(root, "nested", "untracked.txt"), []byte("one\ntwo"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := New(filepath.Join(root, "nested"), queryTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.GitOverview(t.Context())
	if err != nil || len(result.Files) != 2 || result.Files[0].Path != "new.txt" ||
		result.Files[0].Staged == nil || result.Files[0].Staged.Added != 1 ||
		result.Files[1].Path != "untracked.txt" || result.Files[1].Unstaged == nil ||
		result.Files[1].Unstaged.Added != 2 {
		t.Fatalf("overview=%+v err=%v", result, err)
	}
	patch, err := service.GitPatch(t.Context(), "new.txt", true)
	if err != nil || !strings.Contains(patch.Diff, "+new") {
		t.Fatalf("patch=%+v err=%v", patch, err)
	}
}

func TestGitInspectRefusesOversizedAndSymlinkContent(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("x", GitInspectMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("outside content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "encoding.txt"), []byte{0xff, '\n'}, 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := New(root, queryTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	overview, err := service.GitOverview(t.Context())
	if err != nil || len(overview.Files) != 4 {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}
	for _, file := range overview.Files {
		if !file.Untracked || file.Unstaged != nil {
			t.Fatalf("unreadable file must have unavailable stats: %+v", file)
		}
	}
	for _, name := range []string{"large.txt", "link", "hardlink", "encoding.txt"} {
		if _, err := service.GitPatch(t.Context(), name, false); err == nil {
			t.Fatalf("exposed %q", name)
		}
	}
}
