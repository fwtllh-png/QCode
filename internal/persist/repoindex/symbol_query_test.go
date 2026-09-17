package repoindex

import (
	"math"
	"testing"
)

func TestSymbolPageFiltersBeforeLimitAndCountsAllMatches(t *testing.T) {
	store := openStore(t)
	blockers := make([]Symbol, maxQueryLimit+1)
	for n := range blockers {
		blockers[n] = Symbol{Name: "Serve", Kind: "function", Line: n + 1}
	}
	apply(t, store,
		Record{File: File{Path: "a.go"}, Symbols: blockers},
		Record{File: File{Path: "z/api.go"}, Symbols: []Symbol{
			{Name: "Serve", Kind: "function", Line: 1, Exported: true},
			{Name: "Serve", Kind: "function", Line: 2, Exported: true},
			{Name: "Serve", Kind: "type", Line: 3, Exported: true},
			{Name: "Serve", Kind: "function", Line: 4},
		}},
	)
	for _, query := range []Query{
		{Name: "Serve", Exact: true, PathPrefix: "z/", Kinds: []string{"function"}, ExportedOnly: true, Limit: 1},
		{Name: "ser", Paths: []string{"z/api.go"}, Kinds: []string{"function"}, ExportedOnly: true, Limit: 1},
		{Name: "Serve", Exact: true, Kinds: []string{"function"}, ExportedOnly: true, Limit: 1},
	} {
		found, total, err := store.SymbolsWithTotal(t.Context(), query)
		if err != nil || total != 2 || len(found) != 1 || found[0].Path != "z/api.go" || found[0].Line != 1 {
			t.Fatalf("query=%+v found=%+v total=%d err=%v", query, found, total, err)
		}
	}
	for _, limit := range []int{maxQueryLimit - 1, maxQueryLimit, maxQueryLimit + 1, math.MaxInt} {
		found, total, err := store.SymbolsWithTotal(t.Context(), Query{Name: "Serve", Limit: limit})
		if err != nil || total != len(blockers)+4 || len(found) != min(limit, maxQueryLimit) {
			t.Fatalf("limit=%d returned=%d total=%d err=%v", limit, len(found), total, err)
		}
	}
	found, total, err := store.SymbolsWithTotal(t.Context(), Query{PathPrefix: "missing/"})
	if err != nil || len(found) != 0 || total != 0 {
		t.Fatalf("empty page=%+v total=%d err=%v", found, total, err)
	}
}

func TestSymbolPathPrefixIsLiteralAndCaseSensitive(t *testing.T) {
	store := openStore(t)
	for _, path := range []string{
		"src_%/api.go", "srcXY/api.go", "Src_%/api.go",
		`src\name/api.go`, "srcname/api.go", "源码/接口.go",
	} {
		apply(t, store, Record{
			File:    File{Path: path},
			Symbols: []Symbol{{Name: "Serve", Kind: "function", Line: 1}},
		})
	}
	for _, prefix := range []string{"src_%/", `src\name/`, "源码/"} {
		found, total, err := store.SymbolsWithTotal(t.Context(), Query{PathPrefix: prefix, Limit: 1})
		if err != nil || total != 1 || len(found) != 1 {
			t.Fatalf("prefix=%q found=%+v total=%d err=%v", prefix, found, total, err)
		}
		plain, err := store.Symbols(t.Context(), Query{Paths: []string{found[0].Path}, PathPrefix: prefix})
		if err != nil || len(plain) != 1 || plain[0] != found[0] {
			t.Fatalf("plain query=%+v err=%v", plain, err)
		}
	}
}
