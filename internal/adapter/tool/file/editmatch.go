package file

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Fallback matching for exact edits.
//
// The byte-for-byte compare-and-swap remains the primary contract. When it
// misses, the edit gets two bounded recovery attempts that never widen what is
// written:
//
//  1. a one-shot line-number prefix strip, for old text pasted from rendered
//     numbered output ("  12:", "12|", "12→") instead of the file itself;
//  2. a normalized match: line-local one-to-one punctuation and space folds
//     plus surrounding-whitespace trimming locate the span in a folded view,
//     the span is projected back onto original byte offsets, and only those
//     bytes are replaced. Folded text is never written back, so untouched
//     content keeps its original punctuation and spacing byte for byte.
//
// Both attempts keep the CAS count semantics: the folded view must contain
// exactly the declared number of occurrences, or the original miss error is
// reported unchanged.

// Disproportion guard bounds for a normalized match, following the
// fuzzy-edit practice opencode established and minimax-code vendored: a
// projected span that is disproportionately larger than the declared old text
// is rejected even when it folds back cleanly, because it usually means the
// match anchored on the wrong region. The slack absorbs legitimate growth
// (multi-byte originals behind ASCII folds, CRLF endings); the ratio rejects
// runaway spans. Boundary tests lock both.
const (
	editDisproportionLineSlack = 3
	editDisproportionLineRatio = 2
	editDisproportionByteSlack = 500
	editDisproportionByteRatio = 4
)

// editFoldRune maps confusable punctuation to its ASCII counterpart. Every
// fold is exactly one rune to one rune: that invariant is what lets a match in
// the folded view be projected back onto original byte offsets without
// rewriting any other part of the file.
func editFoldRune(r rune) rune {
	switch {
	case r == '‘' || r == '’' || r == '‚' || r == '‛':
		return '\''
	case r == '“' || r == '”' || r == '„' || r == '‟':
		return '"'
	case r >= 0x2010 && r <= 0x2015, r == 0x2212: // hyphens, dashes, minus
		return '-'
	case r == 0x00A0, r >= 0x2000 && r <= 0x200A, r == 0x202F, r == 0x205F, r == 0x3000:
		return ' '
	}
	return r
}

// normalizedEdit is a folded view of edit text plus the mapping from folded
// byte offsets back to original byte offsets. originAtByte[i] holds the
// original offset of the rune starting at folded byte offset i, and
// endAtByte[i] the original offset where that rune ends; positions inside a
// multi-byte rune hold that rune's boundaries and are never read, because a
// substring match of valid UTF-8 always begins and ends on rune boundaries.
type normalizedEdit struct {
	text         string
	originAtByte []int
	endAtByte    []int
}

// normalizeEdit folds view line by line. Within a line, surrounding
// whitespace is trimmed and confusable punctuation is folded; line endings
// themselves are kept as '\n' separators so multi-line spans stay contiguous.
// Leading whitespace is trimmed for matching only: a projected span starts at
// the first kept rune of its first line, so the file's own indentation
// survives outside the replaced range.
func normalizeEdit(view string) normalizedEdit {
	type keptRune struct {
		value  rune
		offset int
	}
	var line []keptRune
	var folded []rune
	var starts []int
	var ends []int
	keep := func(r keptRune) {
		folded = append(folded, editFoldRune(r.value))
		starts = append(starts, r.offset)
		ends = append(ends, r.offset+utf8.RuneLen(r.value))
	}
	trimBounds := func() (int, int) {
		start, end := 0, len(line)
		for start < end && unicode.IsSpace(line[start].value) {
			start++
		}
		for end > start && unicode.IsSpace(line[end-1].value) {
			end--
		}
		return start, end
	}
	for offset, value := range view {
		if value == '\n' {
			start, end := trimBounds()
			for index := start; index < end; index++ {
				keep(line[index])
			}
			line = line[:0]
			folded = append(folded, '\n')
			starts = append(starts, offset)
			ends = append(ends, offset+1)
			continue
		}
		line = append(line, keptRune{value: value, offset: offset})
	}
	start, end := trimBounds()
	for index := start; index < end; index++ {
		keep(line[index])
	}
	line = nil

	text := string(folded)
	originAtByte := make([]int, len(text)+1)
	endAtByte := make([]int, len(text)+1)
	position := 0
	for index, value := range folded {
		width := utf8.RuneLen(value)
		for fill := 0; fill < width; fill++ {
			originAtByte[position+fill] = starts[index]
			endAtByte[position+fill] = ends[index]
		}
		position += width
	}
	originAtByte[len(text)] = len(view)
	endAtByte[len(text)] = len(view)
	return normalizedEdit{text: text, originAtByte: originAtByte, endAtByte: endAtByte}
}

// replaceNormalized applies the folded fallback. It reports false without
// touching anything when the folded occurrence count differs from the declared
// one, when folding is the identity (the exact pass already rejected that
// predicate), or when a projected span fails the fidelity or disproportion
// guard.
func replaceNormalized(content, old, replacement string, occurrences int) ([]byte, bool) {
	normalizedContent := normalizeEdit(content)
	normalizedOld := normalizeEdit(old)
	if normalizedOld.text == "" || normalizedContent.text == "" {
		return nil, false
	}
	if normalizedContent.text == content && normalizedOld.text == old {
		return nil, false
	}
	if strings.Count(normalizedContent.text, normalizedOld.text) != occurrences {
		return nil, false
	}
	type span struct{ start, end int }
	var spans []span
	search := 0
	for {
		index := strings.Index(normalizedContent.text[search:], normalizedOld.text)
		if index < 0 {
			break
		}
		start := search + index
		end := start + len(normalizedOld.text)
		originStart := normalizedContent.originAtByte[start]
		// The span ends where the last matched rune ends in the original, so
		// trailing whitespace and the line terminator of the final matched
		// line stay in the file instead of being handed to the replacement.
		_, lastWidth := utf8.DecodeLastRuneInString(normalizedOld.text)
		originEnd := normalizedContent.endAtByte[end-lastWidth]
		if originEnd <= originStart {
			return nil, false
		}
		extracted := content[originStart:originEnd]
		// Fidelity guard: the projected bytes must fold back to exactly the
		// located text. A mapping error fails closed instead of editing the
		// wrong region.
		if normalizeEdit(extracted).text != normalizedOld.text {
			return nil, false
		}
		if disproportionateEditSpan(extracted, old) {
			return nil, false
		}
		spans = append(spans, span{start: originStart, end: originEnd})
		search = end
	}
	if len(spans) == 0 {
		return nil, false
	}
	// A span that carries CRLF endings hands its line endings to the
	// replacement, so the replacement adopts the file's convention instead of
	// introducing mixed endings.
	if strings.Contains(content[spans[0].start:spans[0].end], "\r\n") {
		replacement = strings.ReplaceAll(
			strings.ReplaceAll(replacement, "\r\n", "\n"), "\n", "\r\n",
		)
	}
	var builder strings.Builder
	cursor := 0
	for _, current := range spans {
		builder.WriteString(content[cursor:current.start])
		builder.WriteString(replacement)
		cursor = current.end
	}
	builder.WriteString(content[cursor:])
	return []byte(builder.String()), true
}

// disproportionateEditSpan rejects a projected span that grew far beyond the
// declared old text. Folded matching keeps line counts identical by
// construction, so the line guard is defense in depth against mapping bugs;
// the byte guard fires on real runaway anchors.
func disproportionateEditSpan(span, old string) bool {
	spanLines := strings.Count(span, "\n") + 1
	oldLines := strings.Count(old, "\n") + 1
	if spanLines >= max(oldLines+editDisproportionLineSlack, oldLines*editDisproportionLineRatio) {
		return true
	}
	if oldLines > 1 && len(span) > max(len(old)+editDisproportionByteSlack, len(old)*editDisproportionByteRatio) {
		return true
	}
	return false
}

// stripLineNumberPrefixes removes one numbered-output prefix ("  12:", "12|"
// or "12→") from every non-empty line of old. It succeeds only when at least
// two non-empty lines all carried such a prefix: a single "80: port" line is
// far more likely real content than a paste artifact, while numbered blocks
// come from copying rendered output wholesale.
func stripLineNumberPrefixes(old string) (string, bool) {
	nonEmpty := 0
	allStripped := true
	var builder strings.Builder
	for index, line := range strings.Split(old, "\n") {
		if index > 0 {
			builder.WriteByte('\n')
		}
		if strings.TrimSpace(line) == "" {
			builder.WriteString(line)
			continue
		}
		nonEmpty++
		stripped, ok := stripOneLineNumberPrefix(line)
		if !ok {
			allStripped = false
		}
		builder.WriteString(stripped)
	}
	if !allStripped || nonEmpty < 2 {
		return "", false
	}
	return builder.String(), true
}

func stripOneLineNumberPrefix(line string) (string, bool) {
	index := 0
	for index < len(line) && (line[index] == ' ' || line[index] == '\t') {
		index++
	}
	digits := index
	for index < len(line) && line[index] >= '0' && line[index] <= '9' {
		index++
	}
	if index == digits || index >= len(line) {
		return line, false
	}
	// Strip through the separator and nothing further: a following space or
	// tab can just as well be the line's real indentation, and the folded
	// fallback already tolerates surrounding-whitespace differences.
	switch line[index] {
	case ':', '|':
		return line[index+1:], true
	case 0xE2:
		if strings.HasPrefix(line[index:], "→") {
			return line[index+len("→"):], true
		}
	}
	return line, false
}
