// Package symbols extracts source declarations, documentation and file-level
// identifiers. Valid source in the supported grammar set uses syntax trees;
// malformed or unsupported input falls back to language-specific lexical rules
// or generic heuristics. Resolution travels with each row. Syntax extraction
// does not imply type checking, macro expansion or semantic reference resolution.
package symbols

import (
	"context"
	"strings"
	"unicode"
)

// Kinds a declaration can have. The vocabulary is shared across languages so a
// caller can filter without knowing which language produced a row: a Rust trait
// and a TypeScript interface are both KindInterface, a Go struct and a Rust enum
// are both KindType.
const (
	KindFunction  = "function"
	KindMethod    = "method"
	KindType      = "type"
	KindClass     = "class"
	KindInterface = "interface"
	KindConst     = "const"
	KindVar       = "var"
)

// Resolution labels how a row was produced, so a consumer can tell a
// hand-tuned rule table from the generic fallback. The values are ordered by
// trust: a helper that aggregates rows reports the weakest one it saw.
const (
	// ResolutionHeuristic is the generic engine: shared declaration shapes
	// applied to a language without a rule table. It is the floor every
	// language gets, never a claim about the language itself.
	ResolutionHeuristic = "heuristic"
	// ResolutionLexical is a per-language rule table.
	ResolutionLexical = "lexical"
	// ResolutionSyntax is a grammar-derived declaration, without type resolution.
	ResolutionSyntax = "syntax"
)

// Languages with a Tier 1 rule table. Languages known by name but absent from
// this list (ruby, shell, ...) are still indexed through the generic engine.
const (
	LanguageGo         = "go"
	LanguagePython     = "python"
	LanguageJavaScript = "javascript"
	LanguageTypeScript = "typescript"
	LanguageRust       = "rust"
	LanguageJava       = "java"
	LanguageC          = "c"
	LanguageCPP        = "cpp"
)

// LanguageGeneric names the tier every language falls back to. It is what the
// index records for a file whose extension and shebang say nothing.
const LanguageGeneric = "generic"

// Symbol is one declaration. Line is 1-based so it can be shown to a reader
// without adjustment. Signature and Docstring carry the declaration text and
// the comment block glued to it, each bounded by the caller's Options; a symbol
// whose extraction produced neither keeps them empty.
type Symbol struct {
	Name       string
	Kind       string
	Container  string
	Line       int
	Exported   bool
	Signature  string
	Docstring  string
	Resolution string
}

// Reference counts one identifier the file uses. It is file-level, not
// symbol-level, because a reference says where to look next, not what declared
// it; graphs built from these rows (proposal R2) join them against the symbol
// table themselves.
type Reference struct {
	Name  string
	Count int
}

// Result is everything one file yielded.
type Result struct {
	ReferenceSitesTruncated bool
	ScopeAware              bool
	PackageName             string
	ReferenceSites          []ReferenceSite
	// Resolution is syntax when imports and references came from the parsed tree.
	Resolution string
	Imports    []string
	Symbols    []Symbol
	References []Reference
}

// Options bound the detail one extraction records. Zero selects the defaults;
// the bounds keep a pathological file from turning the index into a copy of
// its own source. Callers expose them as configuration with provenance.
type Options struct {
	// SourcePath distinguishes grammar dialects such as TypeScript and TSX.
	SourcePath string
	// SignatureMaxBytes bounds the recorded declaration text.
	SignatureMaxBytes int
	// DocstringMaxBytes bounds the recorded comment block.
	DocstringMaxBytes int
	// ReferenceMaxCount independently bounds distinct identifier counts and
	// scoped reference occurrences per file; occurrence truncation is reported.
	ReferenceMaxCount int
}

// Defaults for options left unset.
const (
	DefaultSignatureMaxBytes = 512
	DefaultDocstringMaxBytes = 2048
	DefaultReferenceMaxCount = 4096
)

func (o Options) withDefaults() Options {
	if o.SignatureMaxBytes <= 0 {
		o.SignatureMaxBytes = DefaultSignatureMaxBytes
	}
	if o.DocstringMaxBytes <= 0 {
		o.DocstringMaxBytes = DefaultDocstringMaxBytes
	}
	if o.ReferenceMaxCount <= 0 {
		o.ReferenceMaxCount = DefaultReferenceMaxCount
	}
	return o
}

// maxLines bounds how much of a file is scanned. Generated files run to hundreds
// of thousands of lines and contribute little worth indexing; stopping keeps one
// pathological file from dominating a refresh.
const maxLines = 50000

// languagesByExtension maps a file extension to its language. Extensions absent
// here fall through DetectLanguage's shebang check and then to the generic
// engine, so an unlisted language yields heuristic rows rather than none.
var languagesByExtension = map[string]string{
	".go":   LanguageGo,
	".py":   LanguagePython,
	".pyi":  LanguagePython,
	".js":   LanguageJavaScript,
	".jsx":  LanguageJavaScript,
	".mjs":  LanguageJavaScript,
	".cjs":  LanguageJavaScript,
	".ts":   LanguageTypeScript,
	".tsx":  LanguageTypeScript,
	".mts":  LanguageTypeScript,
	".cts":  LanguageTypeScript,
	".rs":   LanguageRust,
	".java": LanguageJava,
	// C and C++ share one rule table; the names stay distinct so a query can
	// still tell which files are which.
	".c":   LanguageC,
	".h":   LanguageC,
	".cc":  LanguageCPP,
	".cp":  LanguageCPP,
	".cpp": LanguageCPP,
	".cxx": LanguageCPP,
	".c++": LanguageCPP,
	".hpp": LanguageCPP,
	".hh":  LanguageCPP,
	".hxx": LanguageCPP,
	".h++": LanguageCPP,
	".ipp": LanguageCPP,
	".tpp": LanguageCPP,
	// Known languages without a rule table yet. Recording the real name keeps
	// per-language queries meaningful; extraction falls to the generic engine
	// and the rows carry ResolutionHeuristic.
	".cs":      "csharp",
	".csx":     "csharp",
	".rb":      "ruby",
	".php":     "php",
	".kt":      "kotlin",
	".kts":     "kotlin",
	".swift":   "swift",
	".scala":   "scala",
	".sc":      "scala",
	".m":       "objective-c",
	".mm":      "objective-cpp",
	".sh":      "shell",
	".bash":    "shell",
	".zsh":     "shell",
	".fish":    "shell",
	".lua":     "lua",
	".pl":      "perl",
	".pm":      "perl",
	".r":       "r",
	".jl":      "julia",
	".ex":      "elixir",
	".exs":     "elixir",
	".erl":     "erlang",
	".hrl":     "erlang",
	".hs":      "haskell",
	".ml":      "ocaml",
	".mli":     "ocaml",
	".zig":     "zig",
	".d":       "d",
	".groovy":  "groovy",
	".gradle":  "groovy",
	".dart":    "dart",
	".vue":     "vue",
	".svelte":  "svelte",
	".el":      "emacs-lisp",
	".clj":     "clojure",
	".cljs":    "clojure",
	".rkt":     "racket",
	".scm":     "scheme",
	".f90":     "fortran",
	".f95":     "fortran",
	".pas":     "pascal",
	".asm":     "assembly",
	".s":       "assembly",
	".sql":     "sql",
	".proto":   "protobuf",
	".graphql": "graphql",
	".gql":     "graphql",
	".tf":      "terraform",
	".hcl":     "hcl",
	".vim":     "vimscript",
	".nim":     "nim",
	".v":       "verilog",
	".sv":      "systemverilog",
}

// extractors holds one Tier 1 scanner per rule-table language.
var extractors = map[string]func([]line, Options) []Symbol{
	LanguageGo:         extractGo,
	LanguagePython:     extractPython,
	LanguageJavaScript: extractBraces,
	LanguageTypeScript: extractBraces,
	LanguageRust:       extractRust,
	LanguageJava:       extractJava,
	LanguageC:          extractCPP,
	LanguageCPP:        extractCPP,
}

// Language returns the language of path, or an empty string when the extension
// is unknown. An empty answer does not mean the file is skipped: it means
// DetectLanguage still has a shebang to check and the generic engine to fall
// back on.
func Language(path string) string {
	lowered := strings.ToLower(path)
	if index := strings.LastIndexByte(lowered, '.'); index >= 0 {
		return languagesByExtension[lowered[index:]]
	}
	return ""
}

// DetectLanguage names the language of a file from its path first and its
// shebang second. An empty result means "not identified", which callers record
// as LanguageGeneric.
func DetectLanguage(path string, data []byte) string {
	if language := Language(path); language != "" {
		return language
	}
	return languageOfShebang(data)
}

// languageOfShebang reads the first line for an interpreter. Only the forms a
// declaration index can use are recognised; an unknown interpreter keeps its
// empty answer rather than being guessed at.
func languageOfShebang(data []byte) string {
	first := data
	if index := strings.IndexByte(string(data), '\n'); index >= 0 {
		first = data[:index]
	}
	line := strings.TrimSpace(string(first))
	if !strings.HasPrefix(line, "#!") {
		return ""
	}
	line = strings.TrimPrefix(line, "#!")
	line = strings.TrimSpace(line)
	// Strip the direct path form (/usr/bin/python3) to its last segment, and
	// the env form (env -S python3) to its last non-flag word.
	fields := strings.FieldsFunc(line, func(r rune) bool {
		return r == '/' || r == ' ' || r == '\t'
	})
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	switch {
	case strings.HasPrefix(last, "python"):
		return LanguagePython
	case strings.HasPrefix(last, "node"):
		return LanguageJavaScript
	case strings.HasPrefix(last, "ruby"):
		return "ruby"
	case strings.HasPrefix(last, "perl"):
		return "perl"
	case last == "sh" || last == "bash" || last == "zsh" || last == "dash" ||
		last == "fish" || last == "ksh":
		return "shell"
	}
	return ""
}

// Supported reports whether declarations can be extracted for a language.
// Every language is supported: the question is only which tier answers, and
// the resolution on each row says so.
func Supported(language string) bool {
	return true
}

// Extract returns the declarations of data, in the order they appear. It keeps
// the Options-free call sites of earlier milestones; ExtractResult is the full
// form. A language without a rule table yields the generic engine's heuristic
// rows rather than none.
func Extract(language string, data []byte) []Symbol {
	return ExtractResult(language, data, Options{}).Symbols
}

// ExtractResult returns the declarations and file-level references of data.
func ExtractResult(language string, data []byte, options Options) Result {
	return ExtractResultContext(context.Background(), language, data, options)
}

// ExtractResultContext propagates indexing cancellation into syntax parsing.
func ExtractResultContext(ctx context.Context, language string, data []byte, options Options) Result {
	options = options.withDefaults()
	if ctx.Err() != nil {
		return Result{}
	}
	if result, ok := extractSyntax(ctx, language, data, options); ok {
		return result
	}
	if ctx.Err() != nil {
		return Result{}
	}
	lines := scan(language, data)
	result := Result{Resolution: ResolutionHeuristic}
	if extractor, found := extractors[language]; found {
		result.Resolution = ResolutionLexical
		result.Symbols = extractor(lines, options)
	} else {
		result.Symbols = extractGeneric(lines, options)
	}
	result.References = referencesOf(lines, options)
	return result
}

// line is one source line with its comment and string noise removed.
type line struct {
	// Number is the 1-based position in the original file.
	Number int
	// Code is the line with comment-only content blanked out. Trailing comments
	// are kept: cutting them would need to know which quotes are strings, and
	// getting that wrong corrupts the declaration itself.
	Code string
	// Text is the original line, kept so a docstring can be read out of the
	// comment a declaration is glued to.
	Text string
	// Indent is the count of leading spaces, with a tab counting as one.
	Indent int
}

// scan splits data into lines and blanks the ones that carry no code, so a
// declaration written inside a comment or a docstring is not reported.
func scan(language string, data []byte) []line {
	raw := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(raw) > maxLines {
		raw = raw[:maxLines]
	}
	lines := make([]line, 0, len(raw))
	inBlockComment := false
	docstring := ""
	for index, text := range raw {
		trimmed := strings.TrimSpace(text)
		code := text
		switch {
		case language == LanguagePython && docstring != "":
			code = ""
			if strings.Contains(trimmed, docstring) {
				docstring = ""
			}
		case language == LanguagePython && isDocstringStart(trimmed):
			// A docstring opening on its own line hides everything until it closes.
			code = ""
			if marker := docstringMarker(trimmed); !closesOnSameLine(trimmed, marker) {
				docstring = marker
			}
		case inBlockComment:
			code = ""
			if strings.Contains(text, "*/") {
				inBlockComment = false
				if after := text[strings.Index(text, "*/")+2:]; strings.TrimSpace(after) != "" {
					code = after
				}
			}
		case strings.HasPrefix(trimmed, "//"):
			code = ""
		case language == LanguagePython && strings.HasPrefix(trimmed, "#"):
			// Only Python comments start with #. Treating it as a comment
			// everywhere would hide a JavaScript private member.
			code = ""
		case strings.HasPrefix(trimmed, "/*"):
			code = ""
			if !strings.Contains(trimmed, "*/") {
				inBlockComment = true
			}
		}
		lines = append(lines, line{
			Number: index + 1, Code: code, Text: text, Indent: indentOf(code),
		})
	}
	return lines
}

func isDocstringStart(trimmed string) bool {
	return strings.HasPrefix(trimmed, `"""`) || strings.HasPrefix(trimmed, "'''") ||
		strings.HasPrefix(trimmed, `r"""`) || strings.HasPrefix(trimmed, `f"""`)
}

func docstringMarker(trimmed string) string {
	if strings.Contains(trimmed, `"""`) {
		return `"""`
	}
	return "'''"
}

func closesOnSameLine(trimmed, marker string) bool {
	body := trimmed[strings.Index(trimmed, marker)+len(marker):]
	return strings.Contains(body, marker)
}

func indentOf(text string) int {
	count := 0
	for _, symbol := range text {
		switch symbol {
		case ' ', '\t':
			count++
		default:
			return count
		}
	}
	return count
}

// exportedByCase reports the Go and Java convention that a capitalised name is
// visible outside its package.
func exportedByCase(name string) bool {
	for _, symbol := range name {
		return unicode.IsUpper(symbol)
	}
	return false
}

// container tracks the declaration a brace language is currently inside. opened
// distinguishes a body that has started from one whose brace is still on a later
// line, so a type declared in the Allman style keeps its members.
type container struct {
	name   string
	depth  int
	opened bool
}

// scope follows brace nesting so a method can report the type that holds it.
type scope struct {
	depth      int
	containers []container
}

// enter records a container whose body opens at the current depth.
func (s *scope) enter(name string) {
	s.containers = append(s.containers, container{name: name, depth: s.depth})
}

// current is the innermost container, or an empty string at top level.
func (s *scope) current() string {
	if len(s.containers) == 0 {
		return ""
	}
	return s.containers[len(s.containers)-1].name
}

// advance applies one line's braces and drops containers whose body ended.
func (s *scope) advance(code string) {
	s.depth += strings.Count(code, "{") - strings.Count(code, "}")
	if s.depth < 0 {
		s.depth = 0
	}
	for index := range s.containers {
		if s.depth > s.containers[index].depth {
			s.containers[index].opened = true
		}
	}
	for len(s.containers) != 0 {
		innermost := s.containers[len(s.containers)-1]
		if !innermost.opened || s.depth > innermost.depth {
			break
		}
		s.containers = s.containers[:len(s.containers)-1]
	}
}
