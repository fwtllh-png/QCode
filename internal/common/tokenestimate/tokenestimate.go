// Package tokenestimate defines the runtime's baseline token estimation.
//
// Every capacity decision that cannot wait for provider-reported usage starts
// from these estimates, so they must be honest for the writing systems the
// runtime is actually used with. Two constants carry that policy:
//
//   - Latin prose averages about four characters per token across the major
//     production tokenizers (cl100k/o200k, Anthropic, DeepSeek, GLM).
//   - Dense-script text — CJK ideographs, kana, hangul, and their punctuation —
//     averages close to one token per character on the same tokenizers; the
//     older "divide every rune by four" heuristic undercounted it by roughly
//     4x and let CJK sessions overflow their measured context.
//
// Over-estimating is the safe direction: it compacts earlier rather than
// overflowing. Residual per-provider error is corrected after the first
// provider usage observation by the engine's calibrated estimator, which
// scales these baselines by the observed ratio.
package tokenestimate

// DenseBytesPerToken is the UTF-8 byte-to-token ratio of dense-script text:
// common CJK characters occupy three UTF-8 bytes and estimate as one token.
// Byte↔token budget conversions that have no text at hand use it so a byte
// cap never admits more tokens than the same budget would allow for CJK
// content. Astral-plane characters (four bytes, one token) are rare enough
// that the calibration ratio, not this constant, absorbs them.
const DenseBytesPerToken = 3

// Text estimates the tokens in one piece of text: one token per dense-script
// rune, four characters per token otherwise. Empty text estimates as zero.
func Text(text string) uint64 {
	if text == "" {
		return 0
	}
	var dense, sparse uint64
	for _, char := range text {
		if isDenseScript(char) {
			dense++
		} else {
			sparse++
		}
	}
	return dense + (sparse+3)/4
}

// MaxTokensForBytes returns the largest token count that bytes of dense-script
// text can represent. Converting a byte ceiling into a token ceiling with it
// keeps the ceiling valid for every script.
func MaxTokensForBytes(bytes uint64) uint64 {
	if bytes == 0 {
		return 0
	}
	tokens := (bytes + DenseBytesPerToken - 1) / DenseBytesPerToken
	if tokens == 0 {
		return 1
	}
	return tokens
}

// BytesForTokens returns the byte budget that tokens can occupy at
// dense-script density, saturating instead of overflowing.
func BytesForTokens(tokens uint64) uint64 {
	maximum := uint64(^uint(0) >> 1)
	if tokens > maximum/DenseBytesPerToken {
		return maximum
	}
	return tokens * DenseBytesPerToken
}

// BudgetedByteLen returns the length in bytes of the longest prefix of text
// whose estimate stays within maxTokens. It is the text-aware replacement for
// multiplying a token budget by a fixed byte ratio: ASCII text keeps its
// four-characters-per-token headroom while CJK text is cut at the point its
// tokens actually reach the budget.
func BudgetedByteLen(text string, maxTokens uint64) int {
	if maxTokens == 0 {
		return 0
	}
	length := len(text)
	if Text(text) <= maxTokens {
		return length
	}
	var dense, sparse uint64
	for index, char := range text {
		if isDenseScript(char) {
			dense++
		} else {
			sparse++
		}
		if dense+(sparse+3)/4 > maxTokens {
			return index
		}
	}
	return length
}

// isDenseScript reports whether a rune belongs to a writing system whose
// production tokenizers emit roughly one token per character: CJK ideographs
// and their extensions, kana, hangul, CJK punctuation and fullwidth forms,
// and the CJK-related supplementary planes.
func isDenseScript(char rune) bool {
	switch {
	case char >= 0x1100 && char <= 0x11FF, // Hangul Jamo
		char >= 0x2E80 && char <= 0x2FDF,   // CJK radicals
		char >= 0x3000 && char <= 0x30FF,   // CJK punctuation, hiragana, katakana
		char >= 0x3130 && char <= 0x318F,   // Hangul compatibility jamo
		char >= 0x3400 && char <= 0x4DBF,   // CJK extension A
		char >= 0x4E00 && char <= 0x9FFF,   // CJK unified ideographs
		char >= 0xA960 && char <= 0xA97F,   // Hangul jamo extension A
		char >= 0xAC00 && char <= 0xD7AF,   // Hangul syllables
		char >= 0xF900 && char <= 0xFAFF,   // CJK compatibility ideographs
		char >= 0xFF00 && char <= 0xFFEF,   // fullwidth and halfwidth forms
		char >= 0x1B000 && char <= 0x1B2FF, // kana supplements
		char >= 0x20000 && char <= 0x3FFFF: // CJK extensions B through G
		return true
	default:
		return false
	}
}
