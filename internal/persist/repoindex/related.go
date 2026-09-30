package repoindex

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/common/symbols"
)

// TestMapper adapts the index for a caller that wants the mapping alone, with
// the route each answer came by. An index that cannot answer becomes an error,
// which is what a verify gate's affected scope needs to report itself
// unavailable instead of passing on no evidence. A nil index reports itself
// the same way.
type TestMapper struct {
	Index *Index
}

func (m TestMapper) RelatedTests(
	ctx context.Context, paths []string,
) (map[string][]RelatedTest, error) {
	related, snapshot, err := m.Index.RelatedTests(ctx, paths)
	if err != nil {
		return nil, err
	}
	if !snapshot.Ready() {
		message := "the repository index is " + snapshot.Status
		if snapshot.Detail != "" {
			message += ": " + snapshot.Detail
		}
		return nil, errors.New(message)
	}
	return related, nil
}

// Paths projects the mapping to bare test paths, for callers that only need
// where to look.
func Paths(mapping map[string][]RelatedTest) map[string][]string {
	projection := make(map[string][]string, len(mapping))
	for source, tests := range mapping {
		paths := make([]string, 0, len(tests))
		for _, test := range tests {
			paths = append(paths, test.Path)
		}
		projection[source] = paths
	}
	return projection
}

// IsTestPath reports whether a path is a test file by the conventions of its
// language.
func IsTestPath(path string) bool {
	_, name := splitPath(path)
	switch symbols.Language(path) {
	case symbols.LanguageGo:
		return strings.HasSuffix(name, "_test.go")
	case symbols.LanguagePython:
		return strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test.py")
	case symbols.LanguageJavaScript, symbols.LanguageTypeScript:
		base := trimExtension(name)
		return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".spec")
	case symbols.LanguageRust:
		return strings.HasPrefix(path, "tests/") || strings.Contains(path, "/tests/")
	case symbols.LanguageJava:
		return strings.HasSuffix(trimExtension(name), "Test")
	default:
		return false
	}
}

// Convention reports whether this package knows how a language names its tests.
// The verify gate needs the distinction to refuse a scope it cannot map instead
// of reporting an empty pass.
func Convention(path string) bool {
	switch symbols.Language(path) {
	case symbols.LanguageGo, symbols.LanguagePython,
		symbols.LanguageJavaScript, symbols.LanguageTypeScript, symbols.LanguageJava:
		return true
	default:
		return false
	}
}

func relatedTests(path string, indexed map[string]struct{}, directories map[string][]string) []string {
	if !Convention(path) {
		return nil
	}
	if IsTestPath(path) {
		if _, found := indexed[path]; found {
			return []string{path}
		}
		return []string{}
	}
	directory, name := splitPath(path)
	base := trimExtension(name)
	found := make(map[string]struct{})
	for _, candidate := range candidates(path, directory, base, name) {
		if _, exists := indexed[candidate]; exists {
			found[candidate] = struct{}{}
		}
	}
	if symbols.Language(path) == symbols.LanguageGo {
		// Go tests are package scoped: every test in the directory can exercise the
		// file, and naming alone cannot say which one does.
		for _, sibling := range directories[directory] {
			if strings.HasSuffix(sibling, "_test.go") {
				found[join(directory, sibling)] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(found))
	for candidate := range found {
		result = append(result, candidate)
	}
	sort.Strings(result)
	return result
}

// candidates lists the paths a project of this language would give the tests of
// one source file.
func candidates(path, directory, base, name string) []string {
	switch symbols.Language(path) {
	case symbols.LanguageGo:
		return []string{join(directory, base+"_test.go")}
	case symbols.LanguagePython:
		return []string{
			join(directory, "test_"+name),
			join(directory, base+"_test.py"),
			join(join(directory, "tests"), "test_"+name),
			join("tests", "test_"+name),
		}
	case symbols.LanguageJavaScript, symbols.LanguageTypeScript:
		extension := name[len(base):]
		return []string{
			join(directory, base+".test"+extension),
			join(directory, base+".spec"+extension),
			join(join(directory, "__tests__"), base+".test"+extension),
			join(join(directory, "__tests__"), name),
		}
	case symbols.LanguageJava:
		test := join(directory, base+"Test.java")
		if index := strings.Index(directory, "src/main/java/"); index >= 0 {
			main := directory[:index] + "src/test/java/" + directory[index+len("src/main/java/"):]
			return []string{join(main, base+"Test.java"), test}
		}
		return []string{test}
	default:
		return nil
	}
}

func trimExtension(name string) string {
	if index := strings.LastIndexByte(name, '.'); index > 0 {
		return name[:index]
	}
	return name
}

func splitPath(path string) (directory, name string) {
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		return path[:index], path[index+1:]
	}
	return "", path
}

func join(directory, name string) string {
	if directory == "" {
		return name
	}
	return directory + "/" + name
}
