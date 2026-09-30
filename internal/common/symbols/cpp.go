package symbols

import (
	"regexp"
	"strings"

	sitter "github.com/odvcencio/gotreesitter"
)

// C/C++ grammar-specific scope rules and the lexical fallback live together.
// The fallback recognizes declaration shapes only; template instantiation,
// overload resolution and preprocessor evaluation require compiler semantics.

var (
	// cppTemplateHead is stripped before a declaration is matched, so
	// `template<typename T> T max(T a, T b)` reads as the `T max(...)` inside
	// it. The parameters of the template itself stay in the signature.
	cppTemplateHead = regexp.MustCompile(`^template\s*<[^>]*>\s*`)
	// cppTypeHead reads class, struct and union heads. An attribute may sit
	// between the keyword and the name; inheritance and `final` sit after the
	// name and are the signature's business.
	cppTypeHead = regexp.MustCompile(
		`^(?:class|struct|union)\s+(?:__attribute__\s*\([^)]*\)\s+)?` +
			`(?:alignas\s*\([^)]*\)\s+)?([A-Za-z_]\w*)`,
	)
	cppEnumHead = regexp.MustCompile(`^enum(?:\s+(?:class|struct))?\s+([A-Za-z_]\w*)`)
	// cppNamespaceHead names a namespace by its full path; nested paths read
	// as one container, which is what a reader navigates by.
	cppNamespaceHead = regexp.MustCompile(`^namespace\s+([A-Za-z_][\w:]*)`)
	// cppTailModifiers are the words a member head may end with after its
	// parameter list: `) const`, `) const noexcept override`. Stripping them
	// lets an Allman head end at its `)` where the body brace opens below.
	cppTailModifiers = regexp.MustCompile(
		`((?:\s+(?:const|noexcept|override|final))+)$`,
	)
	// cppSpecialMember suppresses the `= default` / `= delete` tail so the
	// member still reports as a declaration.
	cppSpecialMember = regexp.MustCompile(`=\s*(?:default|delete|0)\s*$`)
	// cppAssignment matches an assignment that is not also a comparison, so a
	// call used as a value (`int x = compute();`) does not read as a
	// declaration.
	cppAssignment = regexp.MustCompile(`[^=!<>+\-*/%&|^]=[^=]`)
	// cppAccessLabel switches the exported view inside a class-like container.
	cppAccessLabel = regexp.MustCompile(`^(public|private|protected)\s*:`)
)

// cppContainer is one class, struct or namespace being walked through. A
// class-like container carries the exported view of its members: a struct
// starts public, a class starts private, and either switches at its next
// access label. A namespace leaves its members visible.
type cppContainer struct {
	name      string
	classLike bool
	exported  bool
	depth     int
	opened    bool
}

// extractCPP walks braces for containers and access labels for visibility.
// Exported answers "declared where a reader can reach it": a file-scope or
// namespace-scope function reports true, a class member follows its labels.
// Declarations are indexed alongside definitions — a header's member
// prototypes and the .cpp definitions are two rows, and a search that finds
// both errs on the side of finding.
func extractCPP(lines []line, options Options) []Symbol {
	var found []Symbol
	var containers []cppContainer
	depth := 0
	for index, source := range lines {
		if index > 0 {
			for _, brace := range lines[index-1].Code {
				switch brace {
				case '{':
					depth++
					for position := range containers {
						if depth > containers[position].depth {
							containers[position].opened = true
						}
					}
				case '}':
					depth = max(0, depth-1)
					for len(containers) > 0 {
						last := containers[len(containers)-1]
						if !last.opened || depth > last.depth {
							break
						}
						containers = containers[:len(containers)-1]
					}
				}
			}
		}
		trimmed := strings.TrimSpace(source.Code)
		if trimmed == "" {
			continue
		}
		// Preprocessor lines carry no declarations; the scan leaves them as
		// code, so they are excluded here by their shape.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if match := cppAccessLabel.FindStringSubmatch(trimmed); match != nil {
			for position := len(containers) - 1; position >= 0; position-- {
				if containers[position].classLike {
					containers[position].exported = match[1] == "public"
					break
				}
			}
			continue
		}
		declaration := cppTemplateHead.ReplaceAllLiteralString(trimmed, "")
		name, kind, isNamespace, isClassLike, exportedDefault :=
			cppTypeDeclaration(declaration)
		if isNamespace {
			// A namespace alias does not open a scope.
			if strings.Contains(declaration, "=") {
				continue
			}
			containers = append(containers, cppContainer{
				name: name, depth: depth,
			})
			continue
		}
		containerName, classOwner := cppInnermost(containers)
		if name != "" {
			memberExported := true
			if classOwner != nil {
				memberExported = classOwner.exported
			}
			found = append(found, decorate(lines, index, Symbol{
				Name: name, Kind: kind, Container: containerName,
				Line: source.Number, Exported: memberExported,
				Resolution: ResolutionLexical,
			}, options))
			if isClassLike && (strings.Contains(declaration, "{") || !strings.HasSuffix(declaration, ";")) {
				containers = append(containers, cppContainer{
					name: name, classLike: true,
					exported: exportedDefault, depth: depth,
				})
			}
			continue
		}
		if member, ok := cppFunctionHead(
			lines, index, declaration, classOwner != nil,
		); ok {
			// A namespace is a place, not a type: a function inside one stays
			// a function that names its container, while a class or struct
			// makes its members methods.
			memberKind := KindFunction
			memberExported := true
			if classOwner != nil {
				memberKind = KindMethod
				memberExported = classOwner.exported
			}
			found = append(found, decorate(lines, index, Symbol{
				Name: member, Kind: memberKind, Container: containerName,
				Line: source.Number, Exported: memberExported,
				Resolution: ResolutionLexical,
			}, options))
		}
	}
	return found
}

// cppTypeDeclaration classifies a declaration head as a type or namespace
// opener. name is empty when the line opens neither.
func cppTypeDeclaration(declaration string) (
	name, kind string, isNamespace, isClassLike bool, exportedDefault bool,
) {
	if match := cppNamespaceHead.FindStringSubmatch(declaration); match != nil {
		return match[1], "", true, false, false
	}
	if match := cppTypeHead.FindStringSubmatch(declaration); match != nil {
		switch {
		case strings.HasPrefix(declaration, "class"):
			return match[1], KindClass, false, true, false
		case strings.HasPrefix(declaration, "struct"),
			strings.HasPrefix(declaration, "union"):
			return match[1], KindType, false, true, true
		}
	}
	if match := cppEnumHead.FindStringSubmatch(declaration); match != nil {
		return match[1], KindType, false, false, true
	}
	return "", "", false, false, false
}

// cppInnermost reports the name of the innermost container — namespace or
// class — plus the nearest class-like container, which is the one a member's
// kind and visibility belong to. A nil owner means the position leaves
// visibility on and a function stays a function.
func cppInnermost(containers []cppContainer) (string, *cppContainer) {
	var owner *cppContainer
	for position := len(containers) - 1; position >= 0; position-- {
		if containers[position].classLike {
			owner = &containers[position]
			break
		}
	}
	if len(containers) == 0 {
		return "", owner
	}
	return containers[len(containers)-1].name, owner
}

// cppFunctionHead decides that a line declares a function. The head is a
// name with a balanced parameter list that opens a body on this line, ends a
// declaration (`;`, the header-file form), or ends at `)` with the brace
// opening below (Allman). classContext relaxes the return-type requirement
// for constructors and destructors, which have none. A destructor's `~` is
// part of its name.
func cppFunctionHead(
	lines []line, index int, declaration string, classContext bool,
) (string, bool) {
	cleaned := cppSpecialMember.ReplaceAllLiteralString(declaration, "")
	if cppAssignment.MatchString(cleaned) {
		return "", false
	}
	if !strings.HasSuffix(cleaned, "{") && !strings.HasSuffix(cleaned, ";") {
		withoutTail := strings.TrimSpace(
			cppTailModifiers.ReplaceAllLiteralString(cleaned, ""),
		)
		if !strings.HasSuffix(withoutTail, ")") {
			return "", false
		}
		opensBody := false
		for rest := index + 1; rest < len(lines); rest++ {
			following := strings.TrimSpace(lines[rest].Code)
			if following == "" {
				continue
			}
			opensBody = strings.HasPrefix(following, "{")
			break
		}
		if !opensBody {
			return "", false
		}
	}
	location := genericCall.FindStringSubmatchIndex(cleaned)
	if location == nil {
		return "", false
	}
	name := cleaned[location[2]:location[3]]
	if _, control := callHeadKeywords[name]; control {
		return "", false
	}
	head := strings.TrimSpace(cleaned[:location[2]])
	// Outside a class a bare `name(...)` is a call statement; a declaration
	// carries a return type to the name's left. Inside a class the bare form
	// is a constructor or destructor, which is exactly the rule that lets it
	// through.
	if head == "" && !classContext {
		return "", false
	}
	if strings.Contains(cleaned, "~"+name+"(") {
		name = "~" + name
	}
	return name, true
}

// cppTypeScopes records declared class scopes before classifying qualified
// definitions. A namespace qualifier alone never proves a member function.
func (w *syntaxWalker) cppTypeScopes(root *sitter.Node) map[string]bool {
	types := map[string]bool{}
	var visit func(*sitter.Node, string)
	visit = func(n *sitter.Node, owner string) {
		if n == nil || w.ctx.Err() != nil || n.IsError() || n.IsMissing() || (n.Parent() != nil && w.damaged[n]) {
			return
		}
		switch w.kind(n) {
		case "namespace_definition", "class_specifier", "struct_specifier", "union_specifier":
			name := w.text(w.field(n, "name"))
			if name != "" {
				if owner != "" {
					name = owner + "::" + name
				}
				if w.kind(n) != "namespace_definition" {
					types[name] = true
				}
				owner = name
			}
		}
		for child := range syntaxChildren(n) {
			visit(child, owner)
		}
	}
	for root := range w.reliableRoots(root) {
		visit(root, "")
	}
	return types
}

func (w *syntaxWalker) cppTopLevel(n *sitter.Node) bool {
	if n == nil {
		return false
	}
	kind := w.kind(n)
	if strings.HasPrefix(kind, "preproc_") {
		return true
	}
	switch kind {
	case ";", "comment", "declaration", "function_definition", "class_specifier", "struct_specifier", "union_specifier", "enum_specifier", "namespace_definition", "namespace_alias_definition", "template_declaration", "template_instantiation", "using_declaration", "alias_declaration", "linkage_specification", "static_assert_declaration", "type_definition", "expression_statement", "attributed_statement":
		return true
	}
	return false
}

func cppQualifiedScope(owner, name string) string {
	if owner == "" || strings.HasPrefix(name, "::") {
		return strings.TrimPrefix(name, "::")
	}
	return owner + "::" + name
}

func (w *syntaxWalker) cppMethodScope(owner, qualifier string) bool {
	if strings.HasPrefix(qualifier, "::") {
		return w.cppTypes[strings.TrimPrefix(qualifier, "::")]
	}
	for owner != "" {
		if w.cppTypes[cppQualifiedScope(owner, qualifier)] {
			return true
		}
		at := strings.LastIndex(owner, "::")
		if at < 0 {
			break
		}
		owner = owner[:at]
	}
	return w.cppTypes[qualifier]
}
