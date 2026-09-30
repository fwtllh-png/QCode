package symbols

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// This file carries the per-declaration detail every tier shares: the
// declaration's own text (signature), the comment block glued above it
// (docstring) and the file's identifier counts (references). All three are
// bounded by Options, so a pathological file cannot make the index a copy of
// its own source.

// decorate fills a declaration's Signature and Docstring from the lines around
// its declaration row. The extractor hands over the symbol it just recognised
// together with the row it recognised it on; decorate returns the completed
// symbol without touching identity fields, so the rule tables stay the only
// authority on what a declaration is.
func decorate(lines []line, at int, found Symbol, options Options) Symbol {
	found.Signature = signatureOf(lines, at, options)
	found.Docstring = docstringOf(lines, at, options)
	return found
}

// signatureOf assembles the declaration's text starting at its own row. A
// declaration is complete when its parentheses balance, whether it ends with a
// body marker (`{`, `;`) or simply at the end of a one-line form; a row with
// open parentheses continues onto the next line, which is how multi-line
// parameter lists keep their shape. Assembly stops at the byte bound either
// way, so the loop is bounded by SignatureMaxBytes and never by file length.
func signatureOf(lines []line, at int, options Options) string {
	current := ""
	for index := at; index < len(lines); index++ {
		if strings.TrimSpace(lines[index].Code) == "" {
			break
		}
		piece := strings.TrimSpace(lines[index].Text)
		if piece == "" {
			break
		}
		if current == "" {
			current = piece
		} else {
			current += " " + piece
		}
		if len(current) >= options.SignatureMaxBytes {
			current = boundText(current, options.SignatureMaxBytes)
			break
		}
		if strings.Count(current, "(") > strings.Count(current, ")") {
			continue
		}
		// Parentheses balance: the declaration text is complete. Trim the body
		// markers so the recorded text is the signature, not the first line of
		// the body; `{}` is an empty body, not part of the name either.
		current = strings.TrimSuffix(current, "{}")
		current = strings.TrimRight(current, "{;:")
		break
	}
	return strings.Join(strings.Fields(current), " ")
}

// docstringOf reads the comment block immediately above a declaration. A blank
// line ends the block, so a file banner two lines up is not mistaken for a
// docstring, and the byte bound ends one that runs long.
func docstringOf(lines []line, at int, options Options) string {
	collected := make([]string, 0, 8)
	total := 0
	for index := at - 1; index >= 0; index-- {
		text := strings.TrimSpace(lines[index].Text)
		content, comment := stripCommentMark(text)
		if !comment {
			break
		}
		if content == "" && len(collected) == 0 {
			// A bare marker line above the declaration still belongs to the
			// block, as the opening line of a comment that says nothing by
			// itself.
			collected = append(collected, "")
			continue
		}
		total += len(content) + 1
		if total > options.DocstringMaxBytes {
			break
		}
		collected = append(collected, content)
	}
	for left, right := 0, len(collected)-1; left < right; left, right = left+1, right-1 {
		collected[left], collected[right] = collected[right], collected[left]
	}
	return strings.TrimSpace(strings.Join(collected, "\n"))
}

// stripCommentMark removes one comment marker from a line, reporting whether
// the line is a comment at all. The forms are the shared ones: `//` (with its
// `///` and `//!` doc variants), `#` (only when followed by a space, so a
// JavaScript private field or a C preprocessor directive is not a comment),
// and the `/* ... */` block pair.
func stripCommentMark(text string) (string, bool) {
	switch {
	case strings.HasPrefix(text, "//"):
		return strings.TrimSpace(strings.TrimPrefix(text, "//")), true
	case text == "#" || strings.HasPrefix(text, "# "):
		return strings.TrimSpace(strings.TrimPrefix(text, "#")), true
	case strings.HasPrefix(text, "/*"):
		content := strings.TrimPrefix(text, "/*")
		content = strings.TrimSuffix(content, "*/")
		return strings.TrimSpace(content), true
	case strings.HasPrefix(text, "*"):
		return strings.TrimSpace(strings.TrimPrefix(text, "*")), true
	}
	return "", false
}

// pythonDocstring reads the string block that opens a Python declaration's
// body, which is where that language keeps its documentation. It is the body's
// first statement or it is not a docstring.
func pythonDocstring(lines []line, at int, options Options) string {
	for index := at + 1; index < len(lines); index++ {
		text := strings.TrimSpace(lines[index].Text)
		if text == "" {
			continue
		}
		if !isDocstringStart(text) {
			return ""
		}
		marker := docstringMarker(text)
		collected := []string{strings.TrimSuffix(
			strings.TrimPrefix(text, marker), marker,
		)}
		if closesOnSameLine(text, marker) {
			return boundDocstring(collected, options)
		}
		total := 0
		for rest := index + 1; rest < len(lines); rest++ {
			body := strings.TrimSpace(lines[rest].Text)
			total += len(body) + 1
			if total > options.DocstringMaxBytes {
				break
			}
			closed := strings.HasSuffix(body, marker)
			collected = append(collected, strings.TrimSuffix(body, marker))
			if closed {
				break
			}
		}
		return boundDocstring(collected, options)
	}
	return ""
}

func boundDocstring(collected []string, options Options) string {
	joined := strings.Join(collected, "\n")
	if len(joined) > options.DocstringMaxBytes {
		joined = boundText(joined, options.DocstringMaxBytes)
	}
	return strings.TrimSpace(joined)
}

// identifierPattern is the shape of a name worth counting.
var identifierPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// stopWords are the high-frequency reserved words shared across languages.
// Counting them as references would make every file reference "if" and
// "return"; the table is an engineering choice, not a language fact, and a
// name it wrongly suppresses only narrows a later reference graph built from
// these counts.
var stopWords = map[string]struct{}{
	"if": {}, "else": {}, "elif": {}, "elsif": {}, "for": {}, "foreach": {},
	"while": {}, "do": {}, "done": {}, "then": {}, "switch": {}, "case": {},
	"default": {}, "match": {}, "catch": {}, "except": {}, "finally": {},
	"try": {}, "return": {}, "yield": {}, "break": {}, "continue": {},
	"goto": {}, "and": {}, "or": {}, "not": {}, "in": {}, "is": {}, "as": {},
	"with": {}, "await": {}, "async": {}, "defer": {}, "go": {}, "func": {},
	"function": {}, "fn": {}, "def": {}, "fun": {}, "sub": {}, "proc": {},
	"class": {}, "struct": {}, "enum": {}, "interface": {}, "union": {},
	"type": {}, "trait": {}, "object": {}, "record": {}, "namespace": {},
	"module": {}, "package": {}, "import": {}, "export": {}, "from": {},
	"include": {}, "require": {}, "use": {}, "let": {}, "var": {}, "const": {},
	"val": {}, "new": {}, "delete": {}, "sizeof": {}, "typeof": {},
	"instanceof": {}, "this": {}, "self": {}, "super": {}, "null": {},
	"nil": {}, "none": {}, "true": {}, "false": {}, "pub": {}, "private": {},
	"protected": {}, "public": {}, "internal": {}, "static": {}, "virtual": {},
	"override": {}, "final": {}, "abstract": {}, "sealed": {}, "operator": {},
	"template": {}, "typename": {}, "using": {}, "extern": {}, "inline": {},
	"constexpr": {}, "explicit": {}, "friend": {}, "throw": {},
	"throws": {}, "raise": {}, "assert": {}, "unsafe": {}, "where": {},
	"select": {}, "loop": {}, "when": {}, "unless": {}, "until": {},
	"extension": {}, "impl": {}, "mut": {}, "box": {}, "ref": {},
}

// referencesOf counts the identifiers a file's code uses. The counts are the
// raw material for reference edges (proposal R2): which names appear together
// in a file, regardless of what declared them. Distinct names stop being
// recorded once the bound is reached, so a generated file full of unique
// identifiers contributes a bounded set rather than an unbounded one.
func referencesOf(lines []line, options Options) []Reference {
	counts := make(referenceCounts)
	for _, current := range lines {
		if current.Code == "" {
			continue
		}
		for _, name := range identifierPattern.FindAllString(current.Code, -1) {
			counts.add(name, options.ReferenceMaxCount)
		}
	}
	return counts.references()
}

type referenceCounts map[string]int

func (counts referenceCounts) add(name string, limit int) {
	if _, stop := stopWords[name]; stop || name == "_" {
		return
	}
	if _, exists := counts[name]; exists || len(counts) < limit {
		counts[name]++
	}
}

func (counts referenceCounts) references() []Reference {
	if len(counts) == 0 {
		return nil
	}
	references := make([]Reference, 0, len(counts))
	for name, count := range counts {
		references = append(references, Reference{Name: name, Count: count})
	}
	// A deterministic order: the most-used names first, ties by name.
	sort.Slice(references, func(i, j int) bool {
		if references[i].Count != references[j].Count {
			return references[i].Count > references[j].Count
		}
		return references[i].Name < references[j].Name
	})
	return references
}

// boundText applies the same byte and UTF-8 boundary contract to every tier.
func boundText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}
