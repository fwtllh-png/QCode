package symbols

import (
	"strings"
	"testing"
)

func TestScopedReferencesKeepOccurrences(t *testing.T) {
	source := []byte("package p\nfunc Use(){ Run(); { Run:=func(){}; Run() }; Run() }\n")
	r := ExtractResult(LanguageGo, source, Options{})
	if !r.ScopeAware || len(r.ReferenceSites) != 2 {
		t.Fatalf("sites=%+v", r.ReferenceSites)
	}
	for _, site := range r.ReferenceSites {
		if site.Target != "Run" || site.Kind != ReferencePackage || string(source[site.StartByte:site.EndByte]) != "Run" || site.Scope == 0 {
			t.Fatalf("site=%+v", site)
		}
	}
}

func TestScriptBindingBoundaries(t *testing.T) {
	for name, body := range map[string]string{
		"catch":       "try {} catch(run) {run();}",
		"for-of":      "for(const run of callbacks){run();}",
		"destructure": "const {run: local}=object; local();",
		"property":    "const object={run:1}; object.run;",
	} {
		t.Run(name, func(t *testing.T) {
			r := ExtractResult(LanguageTypeScript, []byte("import {run} from './engine'; "+body), Options{})
			if !r.ScopeAware {
				t.Fatal("no scoped analysis")
			}
			for _, site := range r.ReferenceSites {
				if site.Kind == ReferenceImport {
					t.Fatalf("false import site=%+v", site)
				}
			}
		})
	}
}

func TestScriptTypeParametersDoNotLeak(t *testing.T) {
	r := ExtractResult(LanguageTypeScript, []byte("import {T} from './engine'; type Box<T> = {value:T}; T();"), Options{})
	count := 0
	for _, site := range r.ReferenceSites {
		if site.Kind == ReferenceImport {
			count++
		}
	}
	if !r.ScopeAware || count != 1 {
		t.Fatalf("sites=%+v", r.ReferenceSites)
	}
}

func TestScopedReferenceLimit(t *testing.T) {
	r := ExtractResult(LanguageGo, []byte("package p\nfunc Use(){"+strings.Repeat("Run();", 20)+"}"), Options{ReferenceMaxCount: 3})
	if !r.ScopeAware || len(r.ReferenceSites) != 3 || !r.ReferenceSitesTruncated {
		t.Fatalf("sites=%+v", r.ReferenceSites)
	}
}

func TestGoMethodNameIsNotAUse(t *testing.T) {
	r := ExtractResult(LanguageGo, []byte("package p\ntype T struct{}\nfunc (t T) Run(){}\n"), Options{})
	if !r.ScopeAware {
		t.Fatal("no scopes")
	}
	for _, site := range r.ReferenceSites {
		if site.Name == "Run" {
			t.Fatalf("declaration treated as use: %+v", site)
		}
	}
}
