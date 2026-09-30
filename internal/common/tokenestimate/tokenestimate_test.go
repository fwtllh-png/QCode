package tokenestimate

import (
	"strings"
	"testing"
)

func TestTextMatchesLatinBaseline(t *testing.T) {
	// Four ASCII characters estimate as one token, matching the ratio the
	// calibrated estimator was tuned against.
	cases := map[string]uint64{
		"":               0,
		"a":              1,
		"abcd":           1,
		"abcde":          2,
		"hello world":    3,
		`{"path":"a/b"}`: 4,
	}
	for text, want := range cases {
		if got := Text(text); got != want {
			t.Errorf("Text(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestTextCountsDenseScriptsPerRune(t *testing.T) {
	// CJK ideographs, kana, hangul, and fullwidth punctuation estimate as one
	// token each instead of being divided by four.
	dense := map[string]string{
		"han ideograph":   "配置文件",
		"hiragana":        "こんにちは",
		"katakana":        "プロトコル",
		"hangul":          "설정 파일",
		"fullwidth punct": "：配置，正确。",
	}
	for name, text := range dense {
		runes := uint64(len([]rune(text)))
		if got := Text(text); got != runes {
			t.Errorf("%s: Text(%q) = %d, want %d (one per rune)", name, text, got, runes)
		}
	}
}

func TestTextMixesScripts(t *testing.T) {
	// Mixed content sums both weights: 4 CJK runes as 4 tokens plus the 13
	// ASCII characters (including spaces) as 4 tokens.
	if got := Text("配置 config 文件 file"); got != 8 {
		t.Fatalf("mixed text = %d, want 8", got)
	}
}

func TestTextHandlesAstralPlaneIdeographs(t *testing.T) {
	// CJK extension B lives on plane 2 and still counts per rune.
	text := "\U00020000\U00020001"
	if got := Text(text); got != 2 {
		t.Fatalf("astral ideographs = %d, want 2", got)
	}
}

func TestMaxTokensForBytesCoversDenseScript(t *testing.T) {
	// 30 bytes of UTF-8 CJK are 10 characters, so a byte-derived token
	// ceiling must cover 10 tokens, not the 7 that dividing by four gives.
	if got := MaxTokensForBytes(30); got != 10 {
		t.Fatalf("MaxTokensForBytes(30) = %d, want 10", got)
	}
	if got := MaxTokensForBytes(0); got != 0 {
		t.Fatalf("MaxTokensForBytes(0) = %d, want 0", got)
	}
	if got := MaxTokensForBytes(1); got != 1 {
		t.Fatalf("MaxTokensForBytes(1) = %d, want 1", got)
	}
}

func TestBytesForTokensSaturates(t *testing.T) {
	if got := BytesForTokens(10); got != 30 {
		t.Fatalf("BytesForTokens(10) = %d, want 30", got)
	}
	if got := BytesForTokens(^uint64(0)); got != ^uint64(0)>>1 {
		t.Fatalf("BytesForTokens(max) = %d, want int64 max", got)
	}
}

func TestBudgetedByteLenKeepsAsciiHeadroom(t *testing.T) {
	// ASCII text keeps roughly four bytes per token of headroom.
	if got := BudgetedByteLen("abcdefgh", 2); got != 8 {
		t.Fatalf("ascii budget = %d, want 8", got)
	}
	if got := BudgetedByteLen("abcdefghi", 2); got != 8 {
		t.Fatalf("ascii cut = %d, want 8", got)
	}
}

func TestBudgetedByteLenCutsDenseScriptAtTokenBudget(t *testing.T) {
	// Six CJK characters estimate as six tokens: a four-token budget cuts
	// after the fourth character, at byte 12.
	text := "配置文件内容"
	if got := BudgetedByteLen(text, 6); got != len(text) {
		t.Fatalf("within budget = %d, want %d", got, len(text))
	}
	if got := BudgetedByteLen(text, 4); got != 12 {
		t.Fatalf("dense cut = %d, want 12", got)
	}
}

func TestBudgetedByteLenZeroBudget(t *testing.T) {
	if got := BudgetedByteLen(strings.Repeat("x", 100), 0); got != 0 {
		t.Fatalf("zero budget = %d, want 0", got)
	}
}

func TestBudgetedByteLenStaysAValidPrefix(t *testing.T) {
	// The returned length must never split a multi-byte rune.
	text := "a配置文件内容"
	for budget := uint64(1); budget <= 8; budget++ {
		cut := BudgetedByteLen(text, budget)
		if cut > len(text) {
			t.Fatalf("budget %d: cut %d exceeds length %d", budget, cut, len(text))
		}
		if cut < len(text) && !strings.HasPrefix(text[:cut+1], text[:cut]) {
			t.Fatalf("budget %d: cut %d splits a rune", budget, cut)
		}
		if estimate := Text(text[:cut]); estimate > budget {
			t.Fatalf("budget %d: prefix estimates %d", budget, estimate)
		}
	}
}
