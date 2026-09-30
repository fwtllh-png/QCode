package symbols

import (
	"regexp"
	"strings"
)

// The generic engine is Tier 0: the declarations every brace-and-name language
// shares, applied to any language without a rule table. It is a floor, not a
// claim — its rows carry ResolutionHeuristic so a consumer can weigh a guessed
// Kotlin method against a hand-tuned Go one.

// genericType matches the type keywords that recur across languages, with the
// visibility words that may sit in front of them. The set is the shared
// vocabulary of mainstream languages; a language that spells a type
// declaration differently simply reports none from this rule, which is the
// honest answer for a heuristic tier.
var genericType = regexp.MustCompile(
	`^(?:public\s+|private\s+|protected\s+|internal\s+|open\s+|abstract\s+|` +
		`final\s+|sealed\s+|data\s+|static\s+|partial\s+|export\s+)*` +
		`(class|interface|struct|enum|union|trait|object|record|module)\s+` +
		`([A-Za-z_]\w*)`,
)

// genericTypeKinds maps the matched keyword to the shared kind vocabulary.
var genericTypeKinds = map[string]string{
	"class":     KindClass,
	"interface": KindInterface,
	"trait":     KindInterface,
	"struct":    KindType,
	"enum":      KindType,
	"union":     KindType,
	"object":    KindType,
	"record":    KindType,
	"module":    KindType,
}

// genericCall finds a name immediately followed by an opening parenthesis —
// the head of a call or a definition alike; the caller's context decides which.
var genericCall = regexp.MustCompile(`([A-Za-z_]\w*)\s*\(`)

// callHeadKeywords are the words whose `name(` shape is a statement, not a
// declaration head: control flow, and the guards and assertions that read like
// one at a glance.
var callHeadKeywords = map[string]struct{}{
	"if": {}, "for": {}, "while": {}, "switch": {}, "catch": {}, "return": {},
	"do": {}, "else": {}, "with": {}, "await": {}, "yield": {}, "new": {},
	"typeof": {}, "delete": {}, "void": {}, "case": {}, "throw": {}, "try": {},
	"except": {}, "select": {}, "when": {}, "unless": {}, "until": {},
	"loop": {}, "match": {}, "guard": {}, "defer": {}, "go": {}, "panic": {},
	"assert": {}, "check": {}, "require": {}, "ensure": {}, "echo": {},
	"print": {}, "println": {}, "format": {}, "include": {}, "import": {},
	"macro_rules": {}, "static_assert": {}, "sizeof": {}, "offsetof": {},
	"alignof": {}, "decltype": {}, "attribute": {}, "asm": {}, "volatile": {},
	"lock": {}, "sync": {}, "unsafe": {}, "test": {}, "bench": {},
}

// lastCallHead returns the name of the last `name(` on the line, which for a
// definition is the defined name — return types, receivers and annotations sit
// to its left. A line whose first head is a control keyword is a statement
// (`if (ready()) {`), not a declaration, and reports as absent however many
// further heads it carries.
func lastCallHead(trimmed string) (string, bool) {
	matches := genericCall.FindAllStringSubmatch(trimmed, -1)
	if len(matches) == 0 {
		return "", false
	}
	if _, control := callHeadKeywords[matches[0][1]]; control {
		return "", false
	}
	for index := len(matches) - 1; index >= 0; index-- {
		name := matches[index][1]
		if _, suppress := callHeadKeywords[name]; suppress {
			continue
		}
		return name, true
	}
	return "", false
}

// extractGeneric reads declarations from any brace-or-colon language. A
// definition is a `name(...)` head whose line opens a block — `{` for brace
// languages (or `{}` for an empty body), `:` for indentation languages; a
// type is a shared keyword followed by a name. Everything else — visibility,
// returns, generics — is detail the signature keeps and the identity ignores.
//
// Containers follow the file's own shape: a type that opens a brace body is
// tracked by braces, a type that ends in `:` is tracked by indentation. The
// two never mix within one container, because a declaration opens its body
// one way or the other.
func extractGeneric(lines []line, options Options) []Symbol {
	var found []Symbol
	current := scope{}
	var indented []container
	for index, source := range lines {
		trimmed := strings.TrimSpace(source.Code)
		if trimmed == "" {
			current.advance(source.Code)
			continue
		}
		for len(indented) != 0 && source.Indent <= indented[len(indented)-1].depth {
			indented = indented[:len(indented)-1]
		}
		holder := current.current()
		if holder == "" && len(indented) != 0 {
			holder = indented[len(indented)-1].name
		}
		if match := genericType.FindStringSubmatch(trimmed); match != nil {
			if kind, known := genericTypeKinds[match[1]]; known {
				found = append(found, decorate(lines, index, Symbol{
					Name: match[2], Kind: kind, Container: holder,
					Line: source.Number, Resolution: ResolutionHeuristic,
				}, options))
				if strings.HasSuffix(trimmed, ":") {
					indented = append(indented, container{
						name: match[2], depth: source.Indent,
					})
				} else {
					current.enter(match[2])
				}
			}
			current.advance(source.Code)
			continue
		}
		if name, ok := lastCallHead(trimmed); ok {
			opensBody := strings.HasSuffix(trimmed, "{") ||
				strings.HasSuffix(trimmed, "{}") ||
				strings.HasSuffix(trimmed, ":")
			if opensBody {
				kind := KindFunction
				if holder != "" {
					kind = KindMethod
				}
				found = append(found, decorate(lines, index, Symbol{
					Name: name, Kind: kind, Container: holder,
					Line: source.Number, Resolution: ResolutionHeuristic,
				}, options))
			}
		}
		current.advance(source.Code)
	}
	return found
}
