package symbols

import (
	"context"
	"iter"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	sitter "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// Only grammars with extraction rules and fixtures are enabled. Loading a
// grammar alone does not establish that its declaration shapes are supported.
var syntaxLanguages = map[string]string{
	LanguageGo: "go", LanguagePython: "python", LanguageJavaScript: "javascript",
	LanguageTypeScript: "typescript", LanguageRust: "rust", LanguageJava: "java",
	LanguageC: "c", LanguageCPP: "cpp", "csharp": "c_sharp", "ruby": "ruby", "php": "php",
}

var syntaxPools sync.Map // grammar name -> *sync.Pool; parsers are exclusively checked out

func syntaxParser(entry *grammars.LangEntry) (*sitter.Parser, func()) {
	candidate := &sync.Pool{New: func() any { return sitter.NewParser(entry.Language()) }}
	value, _ := syntaxPools.LoadOrStore(entry.Name, candidate)
	pool := value.(*sync.Pool)
	parser := pool.Get().(*sitter.Parser)
	return parser, func() { parser.SetCancellationFlag(nil); pool.Put(parser) }
}

func extractSyntax(ctx context.Context, language string, data []byte, options Options) (result Result, ok bool) {
	grammar, supported := syntaxLanguages[language]
	if !supported || ctx.Err() != nil {
		return Result{}, false
	}
	if language == LanguageTypeScript && strings.HasSuffix(strings.ToLower(options.SourcePath), ".tsx") {
		grammar = "tsx"
	}
	entry := grammars.DetectLanguageByName(grammar)
	if entry == nil {
		return Result{}, false
	}
	parser, release := syntaxParser(entry)
	defer release()
	var cancelled uint32
	stop := context.AfterFunc(ctx, func() { atomic.StoreUint32(&cancelled, 1) })
	defer stop()
	parser.SetCancellationFlag(&cancelled)
	var tree *sitter.Tree
	var err error
	if entry.TokenSourceFactory != nil {
		tree, err = parser.ParseWithTokenSourceStrict(data, entry.TokenSourceFactory(data, entry.Language()))
	} else {
		tree, err = parser.ParseStrict(data)
	}
	if tree != nil {
		defer tree.Release()
	}
	if err != nil || tree == nil || tree.RootNode() == nil || ctx.Err() != nil {
		return Result{}, false
	}
	// An erroneous tree can contain recovery-created declarations. Keep the
	// established lexical fallback, and never label those rows as syntax facts.
	if tree.RootNode().HasError() {
		return Result{}, false
	}
	walker := syntaxWalker{ctx: ctx, tree: sitter.Bind(tree), language: language, options: options, counts: map[string]int{}, docs: map[*sitter.Node]string{}}
	walker.walk(tree.RootNode(), syntaxScope{})
	if ctx.Err() != nil {
		return Result{}, false
	}
	result = walker.result
	result.Resolution = ResolutionSyntax
	switch language {
	case LanguageGo:
		result.ReferenceSites, result.PackageName, result.ScopeAware, result.ReferenceSitesTruncated = goReferenceSites(ctx, data, options.ReferenceMaxCount)
	case LanguageJavaScript, LanguageTypeScript:
		result.ReferenceSites, result.ReferenceSitesTruncated = walker.scriptReferenceSites(tree.RootNode())
		result.ScopeAware = true
	}
	if ctx.Err() != nil {
		return Result{}, false
	}
	for name, count := range walker.counts {
		result.References = append(result.References, Reference{Name: name, Count: count})
	}
	sort.Slice(result.References, func(i, j int) bool {
		a, b := result.References[i], result.References[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Name < b.Name
	})
	sort.SliceStable(result.Symbols, func(i, j int) bool { return result.Symbols[i].Line < result.Symbols[j].Line })
	return result, true
}

type syntaxScope struct {
	name                      string
	class, function, exported bool
}
type syntaxWalker struct {
	ctx      context.Context
	tree     *sitter.BoundTree
	language string
	options  Options
	result   Result
	counts   map[string]int
	docs     map[*sitter.Node]string
}

func (w *syntaxWalker) field(n *sitter.Node, name string) *sitter.Node {
	return w.tree.ChildByField(n, name)
}
func (w *syntaxWalker) text(n *sitter.Node) string { return w.tree.NodeText(n) }
func (w *syntaxWalker) kind(n *sitter.Node) string { return w.tree.NodeType(n) }

func (w *syntaxWalker) walk(n *sitter.Node, scope syntaxScope) {
	if n == nil || w.ctx.Err() != nil {
		return
	}
	typ := w.kind(n)
	// Literal text and comments are never references or declarations. String
	// interpolation is deliberately excluded from the approximate reference graph.
	if strings.Contains(typ, "comment") || typ == "string" || strings.Contains(typ, "string_literal") || typ == "template_string" {
		return
	}
	w.imports(n)
	if typ == "identifier" || typ == "type_identifier" || typ == "field_identifier" || typ == "property_identifier" || typ == "package_identifier" {
		name := w.text(n)
		if _, skip := stopWords[name]; !skip && name != "_" {
			if _, exists := w.counts[name]; exists || len(w.counts) < w.options.ReferenceMaxCount {
				w.counts[name]++
			}
		}
	}
	name, kind := w.declaration(n, scope)
	childScope := scope
	switch typ {
	case "arrow_function", "function_expression", "lambda", "lambda_expression", "closure_expression", "anonymous_function_creation_expression":
		childScope = syntaxScope{name: scope.name, function: true}
	}
	w.attachDocs(n)
	if typ == "impl_item" {
		childScope = syntaxScope{name: w.typeName(w.field(n, "type")), class: true}
	}
	if typ == "namespace_definition" || typ == "module" {
		if owner := w.text(w.field(n, "name")); owner != "" {
			childScope = syntaxScope{name: owner, exported: true}
		}
	}
	if name != "" {
		exported := w.exported(n, name, scope)
		w.add(n, name, kind, scope.name, exported)
		if kind == KindClass || kind == KindType || kind == KindInterface {
			childScope = syntaxScope{name: name, class: true, exported: w.language != LanguageCPP || typ != "class_specifier"}
		} else if kind == KindFunction || kind == KindMethod {
			childScope = syntaxScope{name: name, function: true}
		}
	}
	for child := range syntaxChildren(n) {
		if w.kind(child) == "access_specifier" {
			childScope.exported = w.text(child) == "public"
			continue
		}
		w.walk(child, childScope)
	}
}

func (w *syntaxWalker) declaration(n *sitter.Node, scope syntaxScope) (string, string) {
	typ := w.kind(n)
	name := w.text(w.field(n, "name"))
	switch typ {
	case "class_declaration", "class_definition", "class", "class_specifier":
		return name, KindClass
	case "interface_declaration", "trait_item", "trait_declaration":
		return name, KindInterface
	case "type_spec", "type_alias", "type_alias_declaration", "struct_item", "enum_item", "struct_specifier", "union_specifier", "enum_specifier", "struct_declaration", "enum_declaration", "record_declaration":
		return name, KindType
	case "function_declaration", "function_definition", "function_item", "function_signature_item", "method_definition", "method_declaration", "constructor_declaration", "method", "singleton_method":
		if name == "" {
			name = w.declaratorName(w.field(n, "declarator"))
		}
		kind := KindFunction
		if scope.class || typ == "method_declaration" || typ == "method_definition" || typ == "constructor_declaration" || typ == "method" || typ == "singleton_method" {
			kind = KindMethod
		}
		return name, kind
	case "declaration", "field_declaration":
		if (w.language == LanguageC || w.language == LanguageCPP) && !scope.function {
			decl := w.field(n, "declarator")
			if w.kind(decl) == "function_declarator" {
				kind := KindFunction
				if scope.class {
					kind = KindMethod
				}
				return w.declaratorName(decl), kind
			}
		}
	case "const_item", "static_item":
		if !scope.function {
			if typ == "const_item" {
				return name, KindConst
			}
			return name, KindVar
		}
	case "const_spec", "var_spec":
		if scope.function {
			return "", ""
		}
		kind := KindVar
		if typ == "const_spec" {
			kind = KindConst
		}
		// Go permits multiple names in one spec; each direct identifier before
		// the type/value fields is a declaration, not an initializer reference.
		for c := range syntaxChildren(n) {
			if w.kind(c) != "identifier" {
				break
			}
			if w.text(c) != "_" {
				w.add(n, w.text(c), kind, scope.name, w.exported(n, w.text(c), scope))
			}
		}
	case "variable_declarator":
		if scope.class && !scope.function && (w.language == LanguageJava || w.language == "csharp") {
			owner := n.Parent()
			if w.kind(owner) == "variable_declaration" {
				owner = owner.Parent()
			}
			kind := KindVar
			if w.hasModifier(owner, "final") || w.hasModifier(owner, "const") {
				kind = KindConst
			}
			w.add(n, name, kind, scope.name, w.exported(owner, name, scope))
			return "", ""
		}
		if !scope.function && (w.language == LanguageJavaScript || w.language == LanguageTypeScript) && w.kind(w.field(n, "name")) == "identifier" {
			value := w.field(n, "value")
			kind := KindVar
			if w.kind(value) == "arrow_function" || w.kind(value) == "function_expression" {
				kind = KindFunction
			} else if strings.HasPrefix(w.text(n.Parent()), "const ") {
				kind = KindConst
			}
			return name, kind
		}
	case "assignment":
		if w.language == LanguagePython && !scope.function && !scope.class {
			left := w.field(n, "left")
			name = w.text(left)
			if w.kind(left) == "identifier" && name == strings.ToUpper(name) && !strings.HasPrefix(name, "_") {
				return name, KindConst
			}
		}
	}
	return "", ""
}

func (w *syntaxWalker) declaratorName(n *sitter.Node) string {
	for n != nil {
		switch w.kind(n) {
		case "identifier", "field_identifier", "type_identifier", "qualified_identifier", "destructor_name", "operator_name":
			return w.text(n)
		}
		n = w.field(n, "declarator")
	}
	return ""
}
func (w *syntaxWalker) typeName(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	if w.kind(n) == "type_identifier" {
		return w.text(n)
	}
	for child := range syntaxChildren(n) {
		if name := w.typeName(child); name != "" {
			return name
		}
	}
	return ""
}

func (w *syntaxWalker) exported(n *sitter.Node, name string, scope syntaxScope) bool {
	switch w.language {
	case LanguageGo:
		return exportedByCase(name)
	case LanguagePython, "ruby":
		return !strings.HasPrefix(name, "_")
	case LanguageJavaScript, LanguageTypeScript:
		if scope.class {
			return !strings.HasPrefix(name, "#") && !w.hasModifier(n, "private") && !w.hasModifier(n, "protected")
		}
		for p := n.Parent(); p != nil; p = p.Parent() {
			if w.kind(p) == "export_statement" {
				return true
			}
			if w.kind(p) == "program" {
				break
			}
		}
		return false
	case LanguageRust:
		return w.hasModifier(n, "pub")
	case LanguageJava, "csharp":
		return w.hasModifier(n, "public")
	case "php":
		return !w.hasModifier(n, "private") && !w.hasModifier(n, "protected")
	case LanguageC, LanguageCPP:
		if scope.class {
			return scope.exported
		}
		return true
	}
	return false
}
func (w *syntaxWalker) hasModifier(n *sitter.Node, modifier string) bool {
	for c := range syntaxChildren(n) {
		if strings.Contains(w.kind(c), "modifier") {
			for _, word := range strings.Fields(w.text(c)) {
				if word == modifier {
					return true
				}
			}
		}
	}
	return false
}

func (w *syntaxWalker) add(n *sitter.Node, name, kind, container string, exported bool) {
	if name == "" || name == "_" {
		return
	}
	if w.language == LanguageGo && kind == KindMethod {
		container = w.typeName(w.field(n, "receiver"))
	}
	if (w.language == LanguageC || w.language == LanguageCPP) && strings.Contains(name, "::") {
		at := strings.LastIndex(name, "::")
		container = name[:at]
		name = name[at+2:]
		kind = KindMethod
	}
	start, end := n.StartByte(), n.EndByte()
	if body := w.field(n, "body"); body != nil {
		end = body.StartByte()
	}
	if value := w.field(n, "value"); w.kind(value) == "arrow_function" || w.kind(value) == "function_expression" {
		if body := w.field(value, "body"); body != nil {
			end = body.StartByte()
		}
	}
	signature := strings.TrimSpace(string(w.tree.Source()[start:end]))
	signature = strings.TrimRight(signature, ";:")
	doc := w.docs[n]
	if p := n.Parent(); p != nil && (w.kind(p) == "type_declaration" || w.kind(p) == "var_declaration" || w.kind(p) == "const_declaration" || w.kind(p) == "export_statement" || w.kind(p) == "lexical_declaration") {
		if doc == "" {
			doc = w.docs[p]
		}
		if p.NamedChildCount() == 1 || w.kind(p) == "export_statement" {
			start = p.StartByte()
			signature = strings.TrimSpace(string(w.tree.Source()[start:end]))
		}
	}

	if w.language == LanguagePython {
		if body := w.field(n, "body"); body != nil && body.NamedChildCount() > 0 {
			first := body.NamedChild(0)
			for i := 0; i < body.NamedChildCount(); i++ {
				first = body.NamedChild(i)
				if !strings.Contains(w.kind(first), "comment") {
					break
				}
			}
			if w.kind(first) == "expression_statement" && first.NamedChildCount() > 0 && w.kind(first.NamedChild(0)) == "string" {
				first = first.NamedChild(0)
			}
			if w.kind(first) == "string" {
				doc = strings.Trim(w.text(first), "\"'")
			}
		}
	}
	line := int(n.StartPoint().Row) + 1
	if w.language == LanguageJava {
		if named := w.field(n, "name"); named != nil {
			line = int(named.StartPoint().Row) + 1
		}
	}
	w.result.Symbols = append(w.result.Symbols, Symbol{Name: name, Kind: kind, Container: container, Line: line, Exported: exported, Signature: syntaxBound(strings.Join(strings.Fields(signature), " "), w.options.SignatureMaxBytes), Docstring: syntaxBound(strings.TrimSpace(doc), w.options.DocstringMaxBytes), Resolution: ResolutionSyntax})
}
func cleanSyntaxComment(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if s, ok := stripCommentMark(line); ok {
			line = s
		}
		line = strings.TrimSuffix(line, "*/")
		lines = append(lines, strings.TrimSpace(line))
	}
	return strings.Join(lines, "\n")
}
func syntaxBound(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

// Keep import extraction in the same tree traversal as declarations. These
// are specifiers, not a claim of compiler-level module or symbol resolution.
func (w *syntaxWalker) imports(n *sitter.Node) {
	add := func(n *sitter.Node) {
		if n != nil {
			value := w.text(n)
			if decoded, err := strconv.Unquote(value); err == nil {
				value = decoded
			} else {
				value = strings.Trim(value, "\"'`")
			}
			if value != "" {
				w.result.Imports = append(w.result.Imports, value)
			}
		}
	}
	switch w.language {
	case LanguageGo:
		if w.kind(n) == "import_spec" {
			add(w.field(n, "path"))
		}
	case LanguageJavaScript, LanguageTypeScript:
		if w.kind(n) == "import_statement" || w.kind(n) == "export_statement" {
			add(w.field(n, "source"))
		}
		if w.kind(n) == "call_expression" {
			fn := w.text(w.field(n, "function"))
			if fn == "require" || fn == "import" {
				args := w.field(n, "arguments")
				if args != nil && args.NamedChildCount() == 1 && w.kind(args.NamedChild(0)) == "string" {
					add(args.NamedChild(0))
				}
			}
		}
	case LanguagePython:
		if w.kind(n) == "import_from_statement" {
			add(w.field(n, "module_name"))
		}
		if w.kind(n) == "import_statement" {
			for c := range syntaxChildren(n) {
				if w.kind(c) == "aliased_import" {
					c = w.field(c, "name")
				}
				add(c)
			}
		}
	case LanguageJava:
		if w.kind(n) == "import_declaration" {
			for c := range syntaxChildren(n) {
				if w.kind(c) == "scoped_identifier" || w.kind(c) == "identifier" {
					add(c)
				}
			}
		}
	case LanguageC, LanguageCPP:
		if w.kind(n) == "preproc_include" {
			p := w.field(n, "path")
			if w.kind(p) == "string_literal" {
				add(p)
			}
		}
	case LanguageRust:
		if w.kind(n) == "use_declaration" {
			w.rustImports(w.field(n, "argument"), "")
		}
	}
}
func (w *syntaxWalker) rustImports(n *sitter.Node, prefix string) {
	if n == nil {
		return
	}
	switch w.kind(n) {
	case "scoped_use_list":
		path := w.text(w.field(n, "path"))
		if prefix != "" {
			path = prefix + "::" + path
		}
		w.rustImports(w.field(n, "list"), path)
	case "use_list":
		for child := range syntaxChildren(n) {
			w.rustImports(child, prefix)
		}
	case "use_as_clause":
		w.rustImports(w.field(n, "path"), prefix)
	default:
		path := w.text(n)
		if prefix != "" {
			if path == "self" {
				path = prefix
			} else {
				path = prefix + "::" + path
			}
		}
		if strings.HasPrefix(path, "crate::") {
			w.result.Imports = append(w.result.Imports, path)
		}
	}
}

// Compute adjacency once per sibling list; rescanning it for every declaration
// makes generated files quadratic in their number of declarations.
func (w *syntaxWalker) attachDocs(n *sitter.Node) {
	var comments []string
	var previousEnd uint32
	for c := range syntaxChildren(n) {
		if c.StartPoint().Row > previousEnd+1 {
			comments = nil
		}
		if strings.Contains(w.kind(c), "comment") {
			comments = append(comments, cleanSyntaxComment(w.text(c)))
		} else {
			if len(comments) > 0 {
				w.docs[c] = strings.Join(comments, "\n")
			}
			comments = nil
		}
		previousEnd = c.EndPoint().Row
	}
}

// NamedChild(i) scans preceding siblings in this runtime. Walk direct children
// once instead, to keep wide source files linear in their syntax-node count.
func syntaxChildren(n *sitter.Node) iter.Seq[*sitter.Node] {
	return func(yield func(*sitter.Node) bool) {
		if n == nil {
			return
		}
		for i := 0; i < n.ChildCount(); i++ {
			c := n.Child(i)
			if c != nil && c.IsNamed() && !yield(c) {
				return
			}
		}
	}
}
