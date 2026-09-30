package symbols

import "testing"

func TestCPPFallbackScopeAndVisibility(t *testing.T) {
	for _, tt := range []struct {
		name, source string
		want         []Symbol
	}{
		{"empty namespace", "namespace ns {}\nvoid g();\n", []Symbol{{Name: "g", Kind: KindFunction, Exported: true}}},
		{"namespace alias", "namespace alias = ns;\nvoid g();\n", []Symbol{{Name: "g", Kind: KindFunction, Exported: true}}},
		{"empty class", "class A {};\nvoid g();\n", []Symbol{{Name: "A", Kind: KindClass, Exported: true}, {Name: "g", Kind: KindFunction, Exported: true}}},
		{"forward class", "class A;\nvoid g();\n", []Symbol{{Name: "A", Kind: KindClass, Exported: true}, {Name: "g", Kind: KindFunction, Exported: true}}},
		{"access and namespace", "namespace ns {\nclass A {\nvoid hidden();\npublic:\nvoid visible();\n};\nvoid g();\n}\nvoid h();\n", []Symbol{{Name: "A", Kind: KindClass, Container: "ns", Exported: true}, {Name: "hidden", Kind: KindMethod, Container: "A"}, {Name: "visible", Kind: KindMethod, Container: "A", Exported: true}, {Name: "g", Kind: KindFunction, Container: "ns", Exported: true}, {Name: "h", Kind: KindFunction, Exported: true}}},
		{"allman", "struct A\n{\nvoid m();\n};\nint f()\n{\nreturn 0;\n}\n", []Symbol{{Name: "A", Kind: KindType, Exported: true}, {Name: "m", Kind: KindMethod, Container: "A", Exported: true}, {Name: "f", Kind: KindFunction, Exported: true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCPP(scan(LanguageCPP, []byte(tt.source)), Options{}.withDefaults())
			if len(got) != len(tt.want) {
				t.Fatalf("symbols = %+v", got)
			}
			for i, want := range tt.want {
				symbol := got[i]
				if symbol.Resolution != ResolutionLexical {
					t.Fatalf("unexpected tier: %+v", symbol)
				}
				symbol.Line, symbol.Signature, symbol.Docstring, symbol.Resolution = 0, "", "", ""
				if symbol != want {
					t.Fatalf("symbol = %+v, want %+v", symbol, want)
				}
			}
		})
	}
}
