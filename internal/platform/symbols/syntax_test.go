package symbols

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestSyntaxLanguageCoverage(t *testing.T) {
	for _, tt := range []struct{ language, path, source, name, kind, container string }{
		{LanguageGo, "a.go", "package a\ntype Box[T any] struct{}\nfunc (b *Box[T])\nRead(\n value T,\n) T { return value }\n", "Read", KindMethod, "Box"},
		{LanguageJavaScript, "a.jsx", "export function\nrender() { return <div/>; }", "render", KindFunction, ""},
		{LanguageTypeScript, "a.ts", "export const identity = <T>(x: T): T => x;", "identity", KindFunction, ""},
		{LanguageTypeScript, "a.tsx", "export const View = (x: string) => <div>{x}</div>;", "View", KindFunction, ""},
		{LanguagePython, "a.py", "class Service:\n @staticmethod\n def serve(\n   value,\n ):\n  return value\n", "serve", KindMethod, "Service"},
		{LanguageRust, "a.rs", "pub struct Box<T>{value:T}\nimpl<T> Box<T> { pub fn read(\n &self,\n) -> &T { &self.value } }", "read", KindMethod, "Box"},
		{LanguageJava, "A.java", "class A {\n public <T> T read(\n T value\n) { return value; }\n}", "read", KindMethod, "A"},
		{LanguageC, "a.c", "int\nread_value(\n int x\n) { return x; }", "read_value", KindFunction, ""},
		{LanguageCPP, "a.cpp", "class A { public: int read(\nint x\n) { return x; } };", "read", KindMethod, "A"},
		{"csharp", "A.cs", "public class A { public int Read(\nint x\n) {return x;} }", "Read", KindMethod, "A"},
		{"ruby", "a.rb", "class A\n def read(\n x\n)\n x\n end\nend\n", "read", KindMethod, "A"},
		{"php", "a.php", "<?php class A { public function read(\n$x\n) { return $x; } }", "read", KindMethod, "A"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			result := ExtractResult(tt.language, []byte(tt.source), Options{SourcePath: tt.path})
			if result.Resolution != ResolutionSyntax {
				t.Fatalf("did not parse as syntax: %+v", result)
			}
			for _, s := range result.Symbols {
				if s.Name == tt.name {
					if s.Kind != tt.kind || s.Container != tt.container {
						t.Fatalf("symbol: %+v", s)
					}
					return
				}
			}
			t.Fatalf("missing %s: %+v", tt.name, result.Symbols)
		})
	}
}

func TestSyntaxIgnoresLiteralDeclarationsAndReferences(t *testing.T) {
	for _, tt := range []struct{ lang, source string }{
		{LanguageGo, "package main\nvar text = `\nfunc Fake() {}\n`\nfunc Real(){}\n"},
		{LanguagePython, "text = '''\ndef Fake(): pass\n'''\ndef Real(): pass\n"},
		{LanguageJavaScript, "const text = `\nfunction Fake() {}\n`;\nfunction Real() {}\n"},
		{LanguageRust, "const TEXT: &str = r#\"\nfn Fake() {}\n\"#;\nfn Real(){}\n"},
	} {
		t.Run(tt.lang, func(t *testing.T) {
			r := ExtractResult(tt.lang, []byte(tt.source), Options{})
			if r.Resolution != ResolutionSyntax {
				t.Fatalf("fallback: %+v", r)
			}
			for _, s := range r.Symbols {
				if s.Name == "Fake" {
					t.Fatalf("literal symbol: %+v", s)
				}
			}
			for _, ref := range r.References {
				if ref.Name == "Fake" {
					t.Fatalf("literal reference: %+v", ref)
				}
			}
		})
	}
}

func TestSyntaxImports(t *testing.T) {
	for _, tt := range []struct {
		lang, source string
		want         []string
	}{
		{LanguageGo, "package a\nimport (alias `example.com/a`; _ \"example.com/b\")\nvar text=`import \"fake\"`\n", []string{"example.com/a", "example.com/b"}},
		{LanguageTypeScript, "import {\n x,\n y\n} from './a';\nexport {x} from './b';\nconst c = import('./c');\nconst d = require('./d');\nconst fake=\"import x from './fake'\";", []string{"./a", "./b", "./c", "./d"}},
		{LanguagePython, "import a as other, b\nfrom ..pkg import (\n x,\n y,\n)\n", []string{"a", "b", "..pkg"}},
		{LanguageRust, "use crate::foo::{A, nested::{B, C as D}, self};", []string{"crate::foo::A", "crate::foo::nested::B", "crate::foo::nested::C", "crate::foo"}},
		{LanguageJava, "import a.b.C;\nimport static a.b.D.call;\nclass A {}", []string{"a.b.C", "a.b.D.call"}},
		{LanguageCPP, "#include \"a.h\"\n#include <vector>\nint f() {return 1;}\n", []string{"a.h"}},
	} {
		t.Run(tt.lang, func(t *testing.T) {
			r := ExtractResult(tt.lang, []byte(tt.source), Options{})
			if r.Resolution != ResolutionSyntax || !reflect.DeepEqual(r.Imports, tt.want) {
				t.Fatalf("imports=%v resolution=%s want=%v", r.Imports, r.Resolution, tt.want)
			}
		})
	}
}

func TestSyntaxFallbackAndCancellation(t *testing.T) {
	r := ExtractResult(LanguageGo, []byte("package main\nfunc Broken( {\n"), Options{})
	if r.Resolution == ResolutionSyntax {
		t.Fatal("malformed source claimed syntax")
	}
	for _, s := range r.Symbols {
		if s.Resolution != ResolutionLexical {
			t.Fatalf("fallback: %+v", s)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = ExtractResultContext(ctx, LanguageGo, []byte("package main\nfunc Good(){}"), Options{})
	if len(r.Symbols) != 0 {
		t.Fatal("cancelled extraction returned symbols")
	}
}

func TestSyntaxBoundsAndConcurrentReuse(t *testing.T) {
	source := []byte("package p\n// 中文说明文档\nfunc 中文函数(参数 string) string { return 参数 }\n")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				r := ExtractResult(LanguageGo, source, Options{SignatureMaxBytes: 17, DocstringMaxBytes: 7, ReferenceMaxCount: 2})
				if r.Resolution != ResolutionSyntax || len(r.Symbols) != 1 {
					t.Errorf("result: %+v", r)
					return
				}
				s := r.Symbols[0]
				if len(s.Signature) > 17 || len(s.Docstring) > 7 || !utf8.ValidString(s.Signature) || !utf8.ValidString(s.Docstring) || len(r.References) > 2 {
					t.Errorf("bounds: %+v", r)
				}
			}
		}()
	}
	wg.Wait()
}

func TestSyntaxNoBodyInSignature(t *testing.T) {
	r := ExtractResult(LanguageGo, []byte("package p\n// Read reads.\nfunc Read(\n x interface{ Run() },\n) { panic(\"BODY\") }\n"), Options{})
	if len(r.Symbols) != 1 || strings.Contains(r.Symbols[0].Signature, "BODY") || !strings.Contains(r.Symbols[0].Signature, "interface{ Run() }") || r.Symbols[0].Docstring != "Read reads." {
		t.Fatalf("result: %+v", r)
	}
}

func BenchmarkSyntaxGeneratedGo(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			var source strings.Builder
			source.WriteString("package p\n")
			for i := 0; i < count; i++ {
				fmt.Fprintf(&source, "func F%d(x int) int { return x }\n", i)
			}
			data := []byte(source.String())
			ExtractResult(LanguageGo, data, Options{}) // warm lazy grammar loading and parser pool
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				r := ExtractResult(LanguageGo, data, Options{})
				if r.Resolution != ResolutionSyntax || len(r.Symbols) != count {
					b.Fatalf("symbols=%d resolution=%s", len(r.Symbols), r.Resolution)
				}
			}
		})
	}
}

func TestSyntaxNestedDefinitionsKeepTheirOwner(t *testing.T) {
	result := ExtractResult(LanguagePython, []byte("def outer():\n def inner():\n  return 1\n return inner()\n"), Options{})
	if result.Resolution != ResolutionSyntax || len(result.Symbols) != 2 || result.Symbols[1].Name != "inner" || result.Symbols[1].Container != "outer" {
		t.Fatalf("result=%+v", result)
	}
}

func TestSyntaxAnonymousFunctionDoesNotExportLocalVariables(t *testing.T) {
	r := ExtractResult(LanguageTypeScript, []byte("export default () => { const Local = 1; return Local; };"), Options{})
	if r.Resolution != ResolutionSyntax || len(r.Symbols) != 0 {
		t.Fatalf("result=%+v", r)
	}
}
