package symbols

import (
	"reflect"
	"testing"
	"unicode/utf8"
)

func TestFallbackReferenceBoundRetainsCounts(t *testing.T) {
	r := ExtractResult("unknown", []byte("alpha beta gamma delta\nalpha gamma beta\n"), Options{ReferenceMaxCount: 2})
	want := []Reference{{Name: "alpha", Count: 2}, {Name: "beta", Count: 2}}
	if !reflect.DeepEqual(r.References, want) {
		t.Fatalf("references = %+v, want %+v", r.References, want)
	}
}

func TestFallbackDetailsPreserveUTF8(t *testing.T) {
	for _, limit := range []int{1, 10, 11, 12, 13, 14, 20} {
		options := Options{SignatureMaxBytes: limit, DocstringMaxBytes: limit}.withDefaults()
		lines := scan(LanguageGo, []byte("// 中文说明\nfunc Real(参数 string) {}\n"))
		found := extractGo(lines, options)
		if len(found) != 1 {
			t.Fatalf("symbols = %+v", found)
		}
		for _, text := range []string{found[0].Signature, found[0].Docstring, boundDocstring([]string{"中文说明"}, options)} {
			if len(text) > limit || !utf8.ValidString(text) {
				t.Fatalf("limit=%d text=%q", limit, text)
			}
		}
	}
}
