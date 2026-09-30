package symbols

import (
	"regexp"
	"strings"
)

// Every rule is anchored at the start of the trimmed line, so a declaration is
// only recognised where a declaration can appear. Names are captured with \w+,
// which is what keeps a call or an expression from matching.
//
// Each table recognises identity; the shared decorate step adds the signature
// and docstring, and every row reports ResolutionLexical.

var (
	goMethod     = regexp.MustCompile(`^func\s*\(\s*\w+\s+\*?(\w+)`)
	goMethodName = regexp.MustCompile(`\)\s*(\w+)\s*[\(\[]`)
	goFunction   = regexp.MustCompile(`^func\s+(\w+)`)
	goType       = regexp.MustCompile(`^type\s+(\w+)`)
	goValue      = regexp.MustCompile(`^(const|var)\s+(\w+)`)
	goGroupStart = regexp.MustCompile(`^(type|const|var)\s*\($`)
	goGroupEntry = regexp.MustCompile(`^(\w+)`)
)

func extractGo(lines []line, options Options) []Symbol {
	var found []Symbol
	group := ""
	for index, current := range lines {
		trimmed := strings.TrimSpace(current.Code)
		if trimmed == "" {
			continue
		}
		if group != "" {
			if trimmed == ")" {
				group = ""
				continue
			}
			if match := goGroupEntry.FindStringSubmatch(trimmed); match != nil {
				kind := KindConst
				switch group {
				case "type":
					kind = KindType
				case "var":
					kind = KindVar
				}
				found = append(found, decorate(lines, index, symbol(
					match[1], kind, "", current.Number, exportedByCase(match[1]),
				), options))
			}
			continue
		}
		if match := goGroupStart.FindStringSubmatch(trimmed); match != nil {
			group = match[1]
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "func ("):
			receiver := goMethod.FindStringSubmatch(trimmed)
			name := goMethodName.FindStringSubmatch(trimmed)
			if receiver == nil || name == nil {
				continue
			}
			found = append(found, decorate(lines, index, symbol(
				name[1], KindMethod, receiver[1], current.Number, exportedByCase(name[1]),
			), options))
		case strings.HasPrefix(trimmed, "func "):
			if match := goFunction.FindStringSubmatch(trimmed); match != nil {
				found = append(found, decorate(lines, index, symbol(
					match[1], KindFunction, "", current.Number, exportedByCase(match[1]),
				), options))
			}
		case strings.HasPrefix(trimmed, "type "):
			if match := goType.FindStringSubmatch(trimmed); match != nil {
				found = append(found, decorate(lines, index, symbol(
					match[1], KindType, "", current.Number, exportedByCase(match[1]),
				), options))
			}
		default:
			if match := goValue.FindStringSubmatch(trimmed); match != nil {
				kind := KindConst
				if match[1] == "var" {
					kind = KindVar
				}
				found = append(found, decorate(lines, index, symbol(
					match[2], kind, "", current.Number, exportedByCase(match[2]),
				), options))
			}
		}
	}
	return found
}

var (
	pythonDef      = regexp.MustCompile(`^(?:async\s+)?def\s+(\w+)`)
	pythonClass    = regexp.MustCompile(`^class\s+(\w+)`)
	pythonConstant = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)\s*(?::[^=]+)?=`)
)

// extractPython follows indentation instead of braces, which is what tells a
// method from a function. Its documentation is the docstring that opens the
// body, so the body's first statement replaces the `#` comments above when one
// is there.
func extractPython(lines []line, options Options) []Symbol {
	var (
		found   []Symbol
		classes []container
	)
	for index, current := range lines {
		trimmed := strings.TrimSpace(current.Code)
		if trimmed == "" {
			continue
		}
		for len(classes) != 0 && current.Indent <= classes[len(classes)-1].depth {
			classes = classes[:len(classes)-1]
		}
		enclosing := ""
		if len(classes) != 0 {
			enclosing = classes[len(classes)-1].name
		}
		switch {
		case pythonClass.MatchString(trimmed):
			match := pythonClass.FindStringSubmatch(trimmed)
			declared := decorate(lines, index, symbol(
				match[1], KindClass, enclosing, current.Number, exportedByUnderscore(match[1]),
			), options)
			declared.Docstring = orDocstring(
				pythonDocstring(lines, index, options), declared.Docstring,
			)
			found = append(found, declared)
			classes = append(classes, container{name: match[1], depth: current.Indent})
		case pythonDef.MatchString(trimmed):
			match := pythonDef.FindStringSubmatch(trimmed)
			kind := KindFunction
			if enclosing != "" {
				kind = KindMethod
			}
			declared := decorate(lines, index, symbol(
				match[1], kind, enclosing, current.Number, exportedByUnderscore(match[1]),
			), options)
			declared.Docstring = orDocstring(
				pythonDocstring(lines, index, options), declared.Docstring,
			)
			found = append(found, declared)
		case current.Indent == 0:
			if match := pythonConstant.FindStringSubmatch(trimmed); match != nil {
				found = append(found, decorate(lines, index, symbol(
					match[1], KindConst, "", current.Number, true,
				), options))
			}
		}
	}
	return found
}

var (
	braceFunction  = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*(\w+)`)
	braceClass     = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+(\w+)`)
	braceInterface = regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?interface\s+(\w+)`)
	braceTypeAlias = regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?type\s+(\w+)\s*[=<]`)
	braceEnum      = regexp.MustCompile(`^(?:export\s+)?(?:const\s+)?enum\s+(\w+)`)
	braceCallable  = regexp.MustCompile(
		`^(?:export\s+)?(?:declare\s+)?(?:const|let|var)\s+(\w+)\s*(?::[^=]+)?=\s*` +
			`(?:async\s+)?(?:function|\(|[\w$]+\s*=>)`,
	)
	braceValue  = regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?(const|let|var)\s+(\w+)`)
	braceMethod = regexp.MustCompile(
		`^(?:public\s+|private\s+|protected\s+|static\s+|readonly\s+|abstract\s+|` +
			`async\s+|get\s+|set\s+|\*\s*)*([\w$#]+)\s*(?:<[^>]*>)?\s*\(`,
	)
)

// controlKeywords are the statements whose `keyword (` shape a method rule would
// otherwise read as a declaration.
var controlKeywords = map[string]struct{}{
	"if": {}, "for": {}, "while": {}, "switch": {}, "catch": {}, "return": {},
	"do": {}, "else": {}, "with": {}, "await": {}, "yield": {}, "function": {},
	"new": {}, "typeof": {}, "delete": {}, "void": {}, "case": {}, "throw": {},
	"super": {}, "import": {}, "require": {},
}

// extractBraces reads JavaScript and TypeScript, which share every declaration
// form this package recognises.
func extractBraces(lines []line, options Options) []Symbol {
	var found []Symbol
	current := scope{}
	for index, source := range lines {
		trimmed := strings.TrimSpace(source.Code)
		if trimmed == "" {
			current.advance(source.Code)
			continue
		}
		exported := strings.HasPrefix(trimmed, "export")
		switch {
		case braceClass.MatchString(trimmed):
			match := braceClass.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindClass, current.current(), source.Number, exported,
			), options))
			// Enter before applying the line's braces so the container's depth is
			// the one its body opens from, whichever line the brace sits on.
			current.enter(match[1])
			current.advance(source.Code)
			continue
		case braceInterface.MatchString(trimmed):
			match := braceInterface.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindInterface, current.current(), source.Number, exported,
			), options))
		case braceEnum.MatchString(trimmed):
			match := braceEnum.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindType, "", source.Number, exported,
			), options))
		case braceTypeAlias.MatchString(trimmed):
			match := braceTypeAlias.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindType, "", source.Number, exported,
			), options))
		case braceFunction.MatchString(trimmed):
			match := braceFunction.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindFunction, "", source.Number, exported,
			), options))
		case braceCallable.MatchString(trimmed):
			match := braceCallable.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindFunction, "", source.Number, exported,
			), options))
		case current.current() != "" && declaresMember(trimmed):
			match := braceMethod.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindMethod, current.current(), source.Number,
				!strings.Contains(trimmed, "private ") && exportedByUnderscore(match[1]),
			), options))
		default:
			if match := braceValue.FindStringSubmatch(trimmed); match != nil {
				kind := KindVar
				if match[1] == "const" {
					kind = KindConst
				}
				found = append(found, decorate(lines, index, symbol(
					match[2], kind, "", source.Number, exported,
				), options))
			}
		}
		current.advance(source.Code)
	}
	return found
}

// declaresMember reports a class member rather than a call. A member's signature
// opens a body or ends a declaration, and its name is never a control keyword.
func declaresMember(trimmed string) bool {
	match := braceMethod.FindStringSubmatch(trimmed)
	if match == nil {
		return false
	}
	if _, control := controlKeywords[match[1]]; control {
		return false
	}
	return strings.HasSuffix(trimmed, "{") || strings.HasSuffix(trimmed, ";")
}

var (
	rustVisibility = `(?:pub(?:\([^)]*\))?\s+)?`
	rustFunction   = regexp.MustCompile(`^` + rustVisibility +
		`(?:default\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+(\w+)`)
	rustStruct = regexp.MustCompile(`^` + rustVisibility + `struct\s+(\w+)`)
	rustEnum   = regexp.MustCompile(`^` + rustVisibility + `enum\s+(\w+)`)
	rustTrait  = regexp.MustCompile(`^` + rustVisibility + `(?:unsafe\s+)?trait\s+(\w+)`)
	rustType   = regexp.MustCompile(`^` + rustVisibility + `type\s+(\w+)`)
	rustValue  = regexp.MustCompile(`^` + rustVisibility + `(const|static)\s+(?:mut\s+)?(\w+)`)
	rustImpl   = regexp.MustCompile(
		`^impl(?:\s*<[^>]*>)?\s+(?:[\w:]+(?:<[^>]*>)?\s+for\s+)?([\w:]+)`,
	)
)

func extractRust(lines []line, options Options) []Symbol {
	var found []Symbol
	current := scope{}
	for index, source := range lines {
		trimmed := strings.TrimSpace(source.Code)
		if trimmed == "" {
			current.advance(source.Code)
			continue
		}
		public := strings.HasPrefix(trimmed, "pub")
		switch {
		case rustImpl.MatchString(trimmed):
			match := rustImpl.FindStringSubmatch(trimmed)
			current.enter(lastSegment(match[1]))
			current.advance(source.Code)
			continue
		case rustFunction.MatchString(trimmed):
			match := rustFunction.FindStringSubmatch(trimmed)
			kind := KindFunction
			if current.current() != "" {
				kind = KindMethod
			}
			found = append(found, decorate(lines, index, symbol(
				match[1], kind, current.current(), source.Number, public,
			), options))
		case rustStruct.MatchString(trimmed):
			match := rustStruct.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindType, "", source.Number, public,
			), options))
		case rustEnum.MatchString(trimmed):
			match := rustEnum.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindType, "", source.Number, public,
			), options))
		case rustTrait.MatchString(trimmed):
			match := rustTrait.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindInterface, "", source.Number, public,
			), options))
			current.enter(match[1])
			current.advance(source.Code)
			continue
		case rustType.MatchString(trimmed):
			match := rustType.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[1], KindType, "", source.Number, public,
			), options))
		case rustValue.MatchString(trimmed):
			match := rustValue.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[2], KindConst, "", source.Number, public,
			), options))
		}
		current.advance(source.Code)
	}
	return found
}

var (
	javaModifiers = `(?:@\w+(?:\([^)]*\))?\s+)*(?:(public|protected|private)\s+)?` +
		`(?:static\s+)?(?:final\s+)?(?:abstract\s+)?(?:sealed\s+)?(?:non-sealed\s+)?`
	javaType   = regexp.MustCompile(`^` + javaModifiers + `(class|interface|enum|record)\s+(\w+)`)
	javaMethod = regexp.MustCompile(`^(?:@\w+(?:\([^)]*\))?\s+)*(public|protected|private)\s+` +
		`(?:static\s+)?(?:final\s+)?(?:synchronized\s+)?(?:abstract\s+)?(?:native\s+)?` +
		`(?:default\s+)?(?:<[^>]+>\s+)?[\w<>\[\],.?\s]*?(\w+)\s*\([^;{]*\)`)
	javaField = regexp.MustCompile(`^(?:(public|protected|private)\s+)?static\s+final\s+` +
		`[\w<>\[\],.]+\s+(\w+)\s*[=;]`)
)

// extractJava requires an explicit access modifier on a method, which leaves
// package-private methods out. Recognising them needs a real parser: without
// one, `Foo bar(baz)` is indistinguishable from a call.
func extractJava(lines []line, options Options) []Symbol {
	var found []Symbol
	current := scope{}
	for index, source := range lines {
		trimmed := strings.TrimSpace(source.Code)
		if trimmed == "" {
			current.advance(source.Code)
			continue
		}
		switch {
		case javaType.MatchString(trimmed):
			match := javaType.FindStringSubmatch(trimmed)
			kind := KindClass
			switch match[2] {
			case "interface":
				kind = KindInterface
			case "enum":
				kind = KindType
			}
			found = append(found, decorate(lines, index, symbol(
				match[3], kind, current.current(), source.Number, match[1] == "public",
			), options))
			current.enter(match[3])
			current.advance(source.Code)
			continue
		case javaField.MatchString(trimmed):
			match := javaField.FindStringSubmatch(trimmed)
			found = append(found, decorate(lines, index, symbol(
				match[2], KindConst, current.current(), source.Number, match[1] == "public",
			), options))
		case javaMethod.MatchString(trimmed) &&
			(strings.HasSuffix(trimmed, "{") || strings.HasSuffix(trimmed, ";")):
			match := javaMethod.FindStringSubmatch(trimmed)
			if _, control := controlKeywords[match[2]]; control {
				break
			}
			found = append(found, decorate(lines, index, symbol(
				match[2], KindMethod, current.current(), source.Number, match[1] == "public",
			), options))
		}
		current.advance(source.Code)
	}
	return found
}

func symbol(name, kind, container string, number int, exported bool) Symbol {
	return Symbol{
		Name: name, Kind: kind, Container: container, Line: number, Exported: exported,
		Resolution: ResolutionLexical,
	}
}

// orDocstring prefers the body docstring over the comments above, which is the
// Python convention; other languages never call it.
func orDocstring(preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}

// exportedByUnderscore reports the Python and JavaScript convention that a
// leading underscore marks a name as internal.
func exportedByUnderscore(name string) bool {
	return !strings.HasPrefix(name, "_") && !strings.HasPrefix(name, "#")
}

func lastSegment(value string) string {
	if index := strings.LastIndex(value, "::"); index >= 0 {
		return value[index+2:]
	}
	return value
}
