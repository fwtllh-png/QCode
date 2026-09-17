package symbols

import (
	sitter "github.com/odvcencio/gotreesitter"
	"strings"
)

type scriptBinding struct {
	module, target string
	namespace      bool
}
type scriptScope struct {
	parent   *scriptScope
	start    int
	function bool
	bindings map[string]scriptBinding
}

func (s *scriptScope) lookup(name string) (scriptBinding, bool) {
	for ; s != nil; s = s.parent {
		if b, ok := s.bindings[name]; ok {
			return b, true
		}
	}
	return scriptBinding{}, false
}

func (w *syntaxWalker) scriptReferenceSites(root *sitter.Node) ([]ReferenceSite, bool) {
	scopes := map[*sitter.Node]*scriptScope{}
	declarations := map[*sitter.Node]bool{}
	suppressed := map[*sitter.Node]bool{}
	bind := func(s *scriptScope, n *sitter.Node, b scriptBinding) {
		if n != nil {
			s.bindings[w.text(n)] = b
			declarations[n] = true
		}
	}
	var pattern func(*scriptScope, *sitter.Node)
	pattern = func(s *scriptScope, n *sitter.Node) {
		switch w.kind(n) {
		case "identifier", "type_identifier", "shorthand_property_identifier_pattern":
			bind(s, n, scriptBinding{})
		case "pair_pattern":
			pattern(s, w.field(n, "value"))
		case "assignment_pattern", "object_assignment_pattern":
			pattern(s, w.field(n, "left"))
		case "array_pattern", "object_pattern", "rest_pattern":
			for c := range syntaxChildren(n) {
				pattern(s, c)
			}
		}
	}
	var collect func(*sitter.Node, *scriptScope)
	collect = func(n *sitter.Node, s *scriptScope) {
		if n == nil || w.ctx.Err() != nil {
			return
		}
		typ := w.kind(n)
		if typ == "import_statement" {
			module := strings.Trim(w.text(w.field(n, "source")), "\"'")
			var imported func(*sitter.Node)
			imported = func(c *sitter.Node) {
				suppressed[c] = true
				switch w.kind(c) {
				case "import_specifier":
					original := w.field(c, "name")
					local := w.field(c, "alias")
					if local == nil {
						local = original
					}
					bind(s, local, scriptBinding{module: module, target: w.text(original)})
					return
				case "namespace_import":
					for id := range syntaxChildren(c) {
						if w.kind(id) == "identifier" {
							bind(s, id, scriptBinding{module: module, target: "*", namespace: true})
						}
					}
					return
				case "import_clause":
					for child := range syntaxChildren(c) {
						if w.kind(child) == "identifier" {
							bind(s, child, scriptBinding{module: module, target: "*"})
						}
					}
				}
				for child := range syntaxChildren(c) {
					imported(child)
				}
			}
			imported(n)
			return
		}
		switch typ {
		case "function_declaration", "generator_function_declaration", "class_declaration", "interface_declaration", "type_alias_declaration", "enum_declaration":
			pattern(s, w.field(n, "name"))
		}
		switch typ {
		case "function_declaration", "generator_function_declaration", "function_expression", "generator_function", "arrow_function", "method_definition":
			s = &scriptScope{parent: s, start: int(n.StartByte()), function: true, bindings: map[string]scriptBinding{}}
			if typ == "function_expression" {
				pattern(s, w.field(n, "name"))
			}
		case "statement_block", "class_body", "catch_clause", "for_statement", "for_in_statement", "switch_body", "class_declaration", "interface_declaration", "type_alias_declaration", "function_type", "constructor_type", "method_signature", "call_signature", "construct_signature":
			s = &scriptScope{parent: s, start: int(n.StartByte()), bindings: map[string]scriptBinding{}}
		}
		scopes[n] = s
		switch typ {
		case "variable_declarator":
			owner := s
			if strings.HasPrefix(strings.TrimSpace(w.text(n.Parent())), "var ") {
				for owner.parent != nil && !owner.function {
					owner = owner.parent
				}
			}
			pattern(owner, w.field(n, "name"))
		case "required_parameter", "optional_parameter":
			name := w.field(n, "pattern")
			if name == nil {
				name = w.field(n, "name")
			}
			pattern(s, name)
		case "formal_parameters":
			for c := range syntaxChildren(n) {
				pattern(s, c)
			}
		case "arrow_function":
			pattern(s, w.field(n, "parameter"))
		case "catch_clause":
			pattern(s, w.field(n, "parameter"))
		case "for_in_statement":
			if kind := w.text(w.field(n, "kind")); kind == "let" || kind == "const" || kind == "var" {
				owner := s
				if kind == "var" {
					for owner.parent != nil && !owner.function {
						owner = owner.parent
					}
				}
				pattern(owner, w.field(n, "left"))
			}
		case "type_parameter":
			pattern(s, w.field(n, "name"))
		}
		for c := range syntaxChildren(n) {
			collect(c, s)
		}
	}
	global := &scriptScope{function: true, bindings: map[string]scriptBinding{}}
	collect(root, global)
	var sites []ReferenceSite
	truncated := false
	add := func(n *sitter.Node, s *scriptScope, b scriptBinding, kind string) {
		if len(sites) >= w.options.ReferenceMaxCount {
			truncated = true
			return
		}
		sites = append(sites, ReferenceSite{Name: w.text(n), Target: b.target, Module: b.module, Kind: kind, Scope: s.start, StartByte: int(n.StartByte()), EndByte: int(n.EndByte()), Line: int(n.StartPoint().Row) + 1})
	}
	var visit func(*sitter.Node)
	visit = func(n *sitter.Node) {
		if n == nil || w.ctx.Err() != nil || suppressed[n] {
			return
		}
		typ := w.kind(n)
		if strings.Contains(typ, "comment") || typ == "string" || typ == "template_string" {
			return
		}
		s := scopes[n]
		if s == nil {
			return
		}
		if typ == "member_expression" {
			object, property := w.field(n, "object"), w.field(n, "property")
			if w.kind(object) == "identifier" && w.kind(property) == "property_identifier" {
				if b, ok := s.lookup(w.text(object)); ok && b.module != "" {
					if b.namespace {
						b.target = w.text(property)
					}
					add(property, s, b, ReferenceImport)
					return
				}
			}
		}
		if (typ == "identifier" || typ == "type_identifier" || typ == "shorthand_property_identifier") && !declarations[n] {
			if b, ok := s.lookup(w.text(n)); ok {
				if b.module != "" {
					add(n, s, b, ReferenceImport)
				}
			} else {
				add(n, s, scriptBinding{target: w.text(n)}, ReferenceUnresolved)
			}
		}
		for c := range syntaxChildren(n) {
			visit(c)
		}
	}
	visit(root)
	return sites, truncated
}
