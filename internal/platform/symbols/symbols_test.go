package symbols

import (
	"fmt"
	"strings"
	"testing"
)

func TestLanguageResolvesByExtension(t *testing.T) {
	for path, want := range map[string]string{
		"a/b/main.go": LanguageGo, "pkg/mod.PY": LanguagePython,
		"web/app.tsx": LanguageTypeScript, "web/app.mjs": LanguageJavaScript,
		"src/lib.rs": LanguageRust, "Main.java": LanguageJava,
		"src/lib.cpp": LanguageCPP, "src/lib.c": LanguageC, "api.rb": "ruby",
		"README.md": "", "Makefile": "", "archive.tar.gz": "",
	} {
		if got := Language(path); got != want {
			t.Errorf("Language(%q) = %q, want %q", path, got, want)
		}
	}
	// Every language is supported now: the question is only which tier answers.
	if !Supported("markdown") || !Supported(LanguageGo) || !Supported("ruby") {
		t.Fatal("Supported no longer answers for every language")
	}
	if found := Extract("markdown", []byte("# title\n")); found != nil {
		t.Fatalf("markdown headings yielded %#v", found)
	}
}

func TestExtractGo(t *testing.T) {
	source := `package store

import "fmt"

// Handler serves requests.
type Handler struct {
	name string
}

type (
	Reader interface{}
	writer struct{}
)

const Version = "1"

var (
	ErrMissing = fmt.Errorf("missing")
	internal   = 2
)

func New(name string) *Handler {
	return &Handler{name: name}
}

func (h *Handler) Serve(path string) error {
	// func Decoy() inside a comment must not be indexed.
	return nil
}

func (h Handler) name() string { return h.name }

func Generic[T any](value T) T { return value }
`
	assertSymbols(t, LanguageGo, source, []Symbol{
		{Name: "Handler", Kind: KindType, Line: 6, Exported: true},
		{Name: "Reader", Kind: KindType, Line: 11, Exported: true},
		{Name: "writer", Kind: KindType, Line: 12},
		{Name: "Version", Kind: KindConst, Line: 15, Exported: true},
		{Name: "ErrMissing", Kind: KindVar, Line: 18, Exported: true},
		{Name: "internal", Kind: KindVar, Line: 19},
		{Name: "New", Kind: KindFunction, Line: 22, Exported: true},
		{Name: "Serve", Kind: KindMethod, Container: "Handler", Line: 26, Exported: true},
		{Name: "name", Kind: KindMethod, Container: "Handler", Line: 31},
		{Name: "Generic", Kind: KindFunction, Line: 33, Exported: true},
	})
}

func TestExtractPython(t *testing.T) {
	source := `"""Module docstring.

def hidden(): pass
"""
import os

TIMEOUT = 30
_private = 1


class Service:
    """Serve requests."""

    def __init__(self, name):
        self.name = name

    async def handle(self, request):
        return None

    def _internal(self):
        pass


def helper(value):
    # def decoy(): pass
    return value
`
	assertSymbols(t, LanguagePython, source, []Symbol{
		{Name: "TIMEOUT", Kind: KindConst, Line: 7, Exported: true},
		{Name: "Service", Kind: KindClass, Line: 11, Exported: true},
		{Name: "__init__", Kind: KindMethod, Container: "Service", Line: 14},
		{Name: "handle", Kind: KindMethod, Container: "Service", Line: 17, Exported: true},
		{Name: "_internal", Kind: KindMethod, Container: "Service", Line: 20},
		{Name: "helper", Kind: KindFunction, Line: 24, Exported: true},
	})
}

func TestExtractTypeScript(t *testing.T) {
	source := `import { join } from "path";

export interface Options {
	root: string;
}

export type Handler = (value: string) => void;

export const MAX = 10;

export const build = async (options: Options) => {
	if (options.root) {
		return null;
	}
	return options;
};

function local(value: string) {
	return value;
}

export default class Server {
	private port: number;

	constructor(port: number) {
		this.port = port;
	}

	public async listen(): Promise<void> {
		for (const item of []) {
			console.log(item);
		}
	}

	#secret() {
		return 1;
	}
}
`
	assertSymbols(t, LanguageTypeScript, source, []Symbol{
		{Name: "Options", Kind: KindInterface, Line: 3, Exported: true},
		{Name: "Handler", Kind: KindType, Line: 7, Exported: true},
		{Name: "MAX", Kind: KindConst, Line: 9, Exported: true},
		{Name: "build", Kind: KindFunction, Line: 11, Exported: true},
		{Name: "local", Kind: KindFunction, Line: 18},
		{Name: "Server", Kind: KindClass, Line: 22, Exported: true},
		{Name: "constructor", Kind: KindMethod, Container: "Server", Line: 25, Exported: true},
		{Name: "listen", Kind: KindMethod, Container: "Server", Line: 29, Exported: true},
		{Name: "#secret", Kind: KindMethod, Container: "Server", Line: 35},
	})
}

func TestExtractRust(t *testing.T) {
	source := `use std::io;

pub const LIMIT: usize = 4;

pub struct Store {
	name: String,
}

enum Mode {
	Fast,
}

pub trait Load {
	fn load(&self) -> io::Result<()>;
}

impl Store {
	pub fn new(name: String) -> Self {
		Self { name }
	}

	fn hidden(&self) {}
}

impl Load for Store {
	fn load(&self) -> io::Result<()> {
		Ok(())
	}
}

pub async fn serve() {}
`
	assertSymbols(t, LanguageRust, source, []Symbol{
		{Name: "LIMIT", Kind: KindConst, Line: 3, Exported: true},
		{Name: "Store", Kind: KindType, Line: 5, Exported: true},
		{Name: "Mode", Kind: KindType, Line: 9},
		{Name: "Load", Kind: KindInterface, Line: 13, Exported: true},
		{Name: "load", Kind: KindMethod, Container: "Load", Line: 14},
		{Name: "new", Kind: KindMethod, Container: "Store", Line: 18, Exported: true},
		{Name: "hidden", Kind: KindMethod, Container: "Store", Line: 22},
		{Name: "load", Kind: KindMethod, Container: "Store", Line: 26},
		{Name: "serve", Kind: KindFunction, Line: 31, Exported: true},
	})
}

func TestExtractJava(t *testing.T) {
	source := `package app;

public class Service implements Runnable
{
	private static final int LIMIT = 3;

	public Service(String name) {
		this.name = name;
	}

	@Override
	public void run() {
		if (LIMIT > 0) {
			return;
		}
	}

	private String describe(int value) {
		return "";
	}

	static int packagePrivate(int value) {
		return value;
	}
}

interface Loader {
	public String load();
}
`
	assertSymbols(t, LanguageJava, source, []Symbol{
		{Name: "Service", Kind: KindClass, Line: 3, Exported: true},
		{Name: "LIMIT", Kind: KindConst, Container: "Service", Line: 5},
		{Name: "Service", Kind: KindMethod, Container: "Service", Line: 7, Exported: true},
		{Name: "run", Kind: KindMethod, Container: "Service", Line: 12, Exported: true},
		{Name: "describe", Kind: KindMethod, Container: "Service", Line: 18},
		{Name: "packagePrivate", Kind: KindMethod, Container: "Service", Line: 22},
		{Name: "Loader", Kind: KindInterface, Line: 27},
		{Name: "load", Kind: KindMethod, Container: "Loader", Line: 28, Exported: true},
	})
}

func TestExtractExcludesDeclarationsInStrings(t *testing.T) {
	source := "package main\n\nvar template = `\nfunc Fake() {}\n`\n"
	found := Extract(LanguageGo, []byte(source))
	if len(found) != 1 || found[0].Name != "template" {
		t.Fatalf("symbols = %s", format(found))
	}
}

func TestLexicalFallbackStopsAtTheLineCeiling(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("package main\n")
	for index := 0; index < maxLines+10; index++ {
		fmt.Fprintf(&builder, "func Generated%d() {}\n", index)
	}
	found := extractors[LanguageGo](scan(LanguageGo, []byte(builder.String())), Options{}.withDefaults())
	if len(found) == 0 || len(found) >= maxLines+10 {
		t.Fatalf("symbols = %d, want a bounded count", len(found))
	}
	for _, symbol := range found {
		if symbol.Line > maxLines {
			t.Fatalf("symbol beyond the ceiling: %+v", symbol)
		}
	}
}

func assertSymbols(t *testing.T, language, source string, want []Symbol) {
	t.Helper()
	got := Extract(language, []byte(source))
	if len(got) != len(want) {
		t.Fatalf("symbols =\n%s\nwant\n%s", format(got), format(want))
	}
	for index := range want {
		// Identity fixtures exercise syntax extraction; signature and
		// docstring details have separate assertions. Lexical fallback is
		// covered by malformed-source and fragment tests.
		if got[index].Resolution != ResolutionSyntax {
			t.Fatalf("symbol %d resolution = %q, want %q", index,
				got[index].Resolution, ResolutionSyntax)
		}
		got[index].Signature = ""
		got[index].Docstring = ""
		got[index].Resolution = ""
		if got[index] != want[index] {
			t.Fatalf("symbol %d = %+v, want %+v\nall =\n%s", index, got[index], want[index], format(got))
		}
	}
}

func format(values []Symbol) string {
	var builder strings.Builder
	for _, value := range values {
		fmt.Fprintf(&builder, "  %+v\n", value)
	}
	return builder.String()
}
