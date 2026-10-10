package guardian

import (
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"
)

// ContentCoverage describes code dependencies, not command safety. Only a
// closed set of literal POSIX shell operations can establish coverage here.
// Unknown executables/build systems require a future trusted dependency
// collector; a caller-supplied list of hashes cannot declare them complete.
type ContentCoverage struct {
	Required []string
	Missing  []string
}

func AnalyzeContent(command, cwd string, bodies map[string][]byte) ContentCoverage {
	var result ContentCoverage
	required, visiting, visited, writes := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	missing := func(reason string) { result.Missing = append(result.Missing, reason) }
	var inspect func(string)
	var script func(string)
	script = func(name string) {
		if filepath.IsAbs(name) || !strings.HasPrefix(name, "./") {
			missing("script does not name a relative snapshot path")
			return
		}
		if filepath.Clean(name[2:]) != name[2:] || strings.HasPrefix(name[2:], "../") {
			// Cleaning a/../b before resolving a can hide a symlink and
			// inspect different bytes from those the shell will execute.
			missing("script path contains unresolved traversal")
			return
		}
		path := filepath.Clean(filepath.Join(cwd, name))
		if !filepath.IsLocal(path) {
			missing("script is outside the execution snapshot")
			return
		}
		required[path] = true
		if visiting[path] {
			missing("recursive script dependency")
			return
		}
		if visited[path] {
			return
		}
		body, found := bodies[path]
		if !found {
			missing("script content is unavailable: " + path)
			return
		}
		// The interpreter is an OS-provided shell, not a mutable PATH program.
		if !utf8.Valid(body) || !strings.HasPrefix(string(body), "#!/bin/sh\n") {
			missing("script interpreter or format is not covered: " + path)
			return
		}
		visiting[path] = true
		inspect(string(body))
		delete(visiting, path)
		visited[path] = true
	}
	inspect = func(text string) {
		file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(text), "")
		if err != nil {
			missing("shell content cannot be parsed")
			return
		}
		syntax.Walk(file, func(node syntax.Node) bool {
			switch value := node.(type) {
			case *syntax.FuncDecl, *syntax.IfClause, *syntax.WhileClause, *syntax.ForClause, *syntax.CaseClause, *syntax.Subshell, *syntax.Block:
				missing("dynamic shell control flow is not covered")
				return false
			case *syntax.Redirect:
				name, ok := literalContentWord(value.Word)
				if !ok {
					missing("dynamic redirection is not covered")
					return false
				}
				if value.Op != syntax.RdrOut && value.Op != syntax.AppOut && value.Op != syntax.ClbOut {
					missing("input or descriptor redirection is not covered")
					return false
				}
				name = strings.TrimPrefix(name, "./")
				if filepath.IsAbs(name) || filepath.Clean(name) != name || !filepath.IsLocal(name) {
					missing("output path is not a canonical snapshot path")
					return false
				}
				path := filepath.Clean(filepath.Join(cwd, name))
				if !filepath.IsLocal(path) {
					missing("output path escapes the snapshot")
					return false
				}
				writes[path] = true
			case *syntax.CallExpr:
				if len(value.Assigns) != 0 || len(value.Args) == 0 {
					missing("shell environment changes are not covered")
					return false
				}
				var args []string
				for _, word := range value.Args {
					arg, ok := literalContentWord(word)
					if !ok {
						missing("dynamic command arguments are not covered")
						return false
					}
					args = append(args, arg)
				}
				switch args[0] {
				case "printf", "echo", ":", "true", "false":
					// Shell builtins with no external code dependencies. This
					// says nothing about the risk of their arguments or writes.
				default:
					if strings.HasPrefix(args[0], "./") {
						script(args[0])
					} else {
						missing("executable dependencies are not covered: " + args[0])
					}
				}
				return false
			}
			return true
		})
	}
	inspect(command)
	for path := range required {
		result.Required = append(result.Required, path)
		if writes[path] {
			missing("command rewrites execution content: " + path)
		}
	}
	slices.Sort(result.Required)
	slices.Sort(result.Missing)
	result.Missing = slices.Compact(result.Missing)
	return result
}

func literalContentWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	var result strings.Builder
	var appendParts func([]syntax.WordPart) bool
	appendParts = func(parts []syntax.WordPart) bool {
		for _, part := range parts {
			switch value := part.(type) {
			case *syntax.Lit:
				// Escaping, globbing and tilde expansion are intentionally
				// unresolved. Reject rather than guess the executed path.
				if strings.ContainsAny(value.Value, "\\*?[]~") {
					return false
				}
				result.WriteString(value.Value)
			case *syntax.SglQuoted:
				result.WriteString(value.Value)
			case *syntax.DblQuoted:
				if !appendParts(value.Parts) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !appendParts(word.Parts) {
		return "", false
	}
	return result.String(), true
}
