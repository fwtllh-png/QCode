package symbols

import (
	"strings"
	"testing"
)

// These tests cover fallback languages, C/C++ structure and bounded detail.
// Syntax-supported languages report their grammar tier; generic languages
// retain their existing heuristic behavior.

func TestDetectLanguageFallsBackToShebang(t *testing.T) {
	for _, test := range []struct {
		path string
		data string
		want string
	}{
		{"script", "#!/usr/bin/env python3\nprint(1)\n", LanguagePython},
		{"script", "#!/usr/bin/python\nprint(1)\n", LanguagePython},
		{"script", "#!/bin/bash\necho hi\n", "shell"},
		{"script", "#!/usr/bin/env node\nconsole.log(1)\n", LanguageJavaScript},
		{"script", "#!/usr/bin/ruby\nputs 1\n", "ruby"},
		{"script", "#!/usr/bin/env perl\nprint 1;\n", "perl"},
		{"script", "#!/usr/bin/env unknown-lang\n", ""},
		{"main.go", "package main\n", LanguageGo},
		{"notes", "no shebang here\n", ""},
	} {
		if got := DetectLanguage(test.path, []byte(test.data)); got != test.want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestExtractGenericServesLanguagesWithoutARuleTable(t *testing.T) {
	kotlin := `package app

// Renders the scene once per frame.
class Renderer(private val scene: Scene) {
    fun draw(frame: Int) {
        check(frame > 0)
    }

    private fun release() {}
}

object Metrics {
    fun record(name: String) {}
}

fun main() {
}`
	found := Extract("kotlin", []byte(kotlin))
	want := []Symbol{
		{Name: "Renderer", Kind: KindClass, Line: 4, Signature: "class Renderer(private val scene: Scene)",
			Docstring: "Renders the scene once per frame.", Resolution: ResolutionHeuristic},
		{Name: "draw", Kind: KindMethod, Container: "Renderer", Line: 5,
			Signature: "fun draw(frame: Int)", Resolution: ResolutionHeuristic},
		{Name: "release", Kind: KindMethod, Container: "Renderer", Line: 9,
			Signature: "private fun release()", Resolution: ResolutionHeuristic},
		{Name: "Metrics", Kind: KindType, Line: 12, Signature: "object Metrics",
			Resolution: ResolutionHeuristic},
		{Name: "record", Kind: KindMethod, Container: "Metrics", Line: 13,
			Signature: "fun record(name: String)", Resolution: ResolutionHeuristic},
		{Name: "main", Kind: KindFunction, Line: 16, Signature: "fun main()",
			Resolution: ResolutionHeuristic},
	}
	assertFullSymbols(t, found, want)
}

func TestExtractGenericReadsIndentationLanguages(t *testing.T) {
	// A made-up language with Python shape: a rule table never existed, the
	// generic engine still gives the file a usable outline.
	source := `class Queue:

    def push(self, item):
        return None

def drain(queue):
    return queue
`
	found := Extract("unknown-indent", []byte(source))
	want := []Symbol{
		{Name: "Queue", Kind: KindClass, Line: 1, Signature: "class Queue",
			Resolution: ResolutionHeuristic},
		{Name: "push", Kind: KindMethod, Container: "Queue", Line: 3,
			Signature: "def push(self, item)", Resolution: ResolutionHeuristic},
		{Name: "drain", Kind: KindFunction, Line: 6,
			Signature: "def drain(queue)", Resolution: ResolutionHeuristic},
	}
	assertFullSymbols(t, found, want)
}

func TestExtractGenericSkipsCallsAndControlFlow(t *testing.T) {
	source := `if (ready()) {
	return;
}
process(queue);
total = compute(a, b) {
	cleanup();
}
`
	found := Extract("unknown", []byte(source))
	// `if (...)` and `process(...)` are statements, not declarations: only a
	// head whose line opens a body declares.
	if len(found) != 1 || found[0].Name != "compute" || found[0].Kind != KindFunction {
		t.Fatalf("symbols = %s", format(found))
	}
	if found[0].Signature != "total = compute(a, b)" {
		t.Fatalf("signature = %q", found[0].Signature)
	}
}

func TestExtractCPP(t *testing.T) {
	source := `#include "store.h"
#include <vector>

namespace inventory {

// Adds stock, never removes it.
void add(int sku) {
	helper(sku);
}

class Store final : public Base {
public:
	Store();
	~Store();
	int find(const char* name);
	virtual int reload(void) noexcept;
private:
	int hidden_();
	static void reset();
};

struct Config {
	bool verbose = false;
};

enum class Mode { Fast, Slow };

template <typename T>
T clamp(T value) {
	return value;
}

void api(int sku);
int assigned = lookup(sku);
}
`
	found := Extract(LanguageCPP, []byte(source))
	want := []Symbol{
		{Name: "add", Kind: KindFunction, Container: "inventory", Line: 7, Exported: true,
			Signature: "void add(int sku)", Docstring: "Adds stock, never removes it.",
			Resolution: ResolutionSyntax},
		{Name: "Store", Kind: KindClass, Container: "inventory", Line: 11, Exported: true,
			Signature: "class Store final : public Base", Resolution: ResolutionSyntax},
		{Name: "Store", Kind: KindMethod, Container: "Store", Line: 13, Exported: true,
			Signature: "Store()", Resolution: ResolutionSyntax},
		{Name: "~Store", Kind: KindMethod, Container: "Store", Line: 14, Exported: true,
			Signature: "~Store()", Resolution: ResolutionSyntax},
		{Name: "find", Kind: KindMethod, Container: "Store", Line: 15, Exported: true,
			Signature: "int find(const char* name)", Resolution: ResolutionSyntax},
		{Name: "reload", Kind: KindMethod, Container: "Store", Line: 16, Exported: true,
			Signature: "virtual int reload(void) noexcept", Resolution: ResolutionSyntax},
		{Name: "hidden_", Kind: KindMethod, Container: "Store", Line: 18, Exported: false,
			Signature: "int hidden_()", Resolution: ResolutionSyntax},
		{Name: "reset", Kind: KindMethod, Container: "Store", Line: 19, Exported: false,
			Signature: "static void reset()", Resolution: ResolutionSyntax},
		{Name: "Config", Kind: KindType, Container: "inventory", Line: 22, Exported: true,
			Signature: "struct Config", Resolution: ResolutionSyntax},
		{Name: "Mode", Kind: KindType, Container: "inventory", Line: 26, Exported: true,
			Signature: "enum class Mode", Resolution: ResolutionSyntax},
		{Name: "clamp", Kind: KindFunction, Container: "inventory", Line: 29, Exported: true,
			Signature: "T clamp(T value)", Resolution: ResolutionSyntax},
		{Name: "api", Kind: KindFunction, Container: "inventory", Line: 33, Exported: true,
			Signature: "void api(int sku)", Resolution: ResolutionSyntax},
	}
	assertFullSymbols(t, found, want)
	for _, symbol := range found {
		// A call used as a value reads as neither a declaration nor a
		// definition; a member field like `verbose` is not indexed either.
		if symbol.Name == "assigned" || symbol.Name == "lookup" ||
			symbol.Name == "helper" || symbol.Name == "verbose" {
			t.Fatalf("statement produced a row: %+v", symbol)
		}
	}
}

func TestExtractCPPAllmanBody(t *testing.T) {
	source := `int main(int argc, char** argv)
{
	return 0;
}
`
	found := Extract(LanguageC, []byte(source))
	if len(found) != 1 || found[0].Name != "main" || found[0].Kind != KindFunction {
		t.Fatalf("symbols = %s", format(found))
	}
	if found[0].Signature != "int main(int argc, char** argv)" {
		t.Fatalf("signature = %q", found[0].Signature)
	}
}

func TestSignatureSpansMultiLineDeclarations(t *testing.T) {
	source := `func Build(
	name string,
	opts ...Option,
) (*Result, error) {
	return nil, nil
}
`
	found := Extract(LanguageGo, []byte(source))
	if len(found) != 1 {
		t.Fatalf("symbols = %s", format(found))
	}
	want := "func Build( name string, opts ...Option, ) (*Result, error)"
	if found[0].Signature != want {
		t.Fatalf("signature = %q, want %q", found[0].Signature, want)
	}
}

func TestDocstringIsTheCommentBlockGluedToTheDeclaration(t *testing.T) {
	source := `// File banner, not attached to anything.

// Handler serves requests.
// It is a handler.
type Handler struct{}
`
	found := Extract(LanguageGo, []byte(source))
	if len(found) != 1 {
		t.Fatalf("symbols = %s", format(found))
	}
	if found[0].Docstring != "Handler serves requests.\nIt is a handler." {
		t.Fatalf("docstring = %q", found[0].Docstring)
	}
}

func TestPythonBodyDocstringWins(t *testing.T) {
	source := `# module level note

class Service:
    """Serve requests."""

    def handle(self):
        # comment above is not a docstring for handle
        return None
`
	found := Extract(LanguagePython, []byte(source))
	if len(found) != 2 {
		t.Fatalf("symbols = %s", format(found))
	}
	if found[0].Docstring != "Serve requests." {
		t.Fatalf("class docstring = %q", found[0].Docstring)
	}
	if found[1].Docstring != "" {
		t.Fatalf("method docstring = %q, want empty", found[1].Docstring)
	}
}

func TestReferencesCountIdentifiersAndStopWords(t *testing.T) {
	result := ExtractResult(LanguageGo, []byte(
		"package main\n\nfunc Use() error {\n\treturn Serve(Client)\n}\n"),
		Options{})
	byName := map[string]int{}
	for _, reference := range result.References {
		byName[reference.Name] = reference.Count
	}
	for _, name := range []string{"main", "Use", "Serve", "Client"} {
		if byName[name] == 0 {
			t.Errorf("reference %q missing", name)
		}
	}
	for _, name := range []string{"package", "func", "return", "if"} {
		if _, counted := byName[name]; counted {
			t.Errorf("stop word %q counted as a reference", name)
		}
	}
}

func TestReferencesRespectTheBound(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("package main\n")
	for index := 0; index < 32; index++ {
		builder.WriteString("var Marker")
		builder.WriteString(string(rune('A' + index%26)))
		builder.WriteString(" = 1\n")
	}
	result := ExtractResult(LanguageGo, []byte(builder.String()), Options{
		ReferenceMaxCount: 8,
	})
	if len(result.References) > 8 {
		t.Fatalf("references = %d, want at most 8", len(result.References))
	}
}

func assertFullSymbols(t *testing.T, got, want []Symbol) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("symbols =\n%s\nwant\n%s", format(got), format(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("symbol %d = %+v, want %+v\nall =\n%s",
				index, got[index], want[index], format(got))
		}
	}
}
