package repoindex

import (
	"path"
	"regexp"
	"strings"
)

// Import specifiers are recorded raw during a scan — the file set is still
// changing under a concurrent refresh — and resolved to repository paths later,
// when the graph build has every indexed file to resolve against. A specifier
// that names nothing inside the repository records no edge: the graph describes
// the inside of a workspace, and an external dependency is not a place to walk
// to.

var (
	goImportLine = regexp.MustCompile(`^(?:\w+(?:/[.\w-]+)*\s+)?"([^"]+)"$`)
	goSingle     = regexp.MustCompile(`^import\s+(?:\w+\s+)?"([^"]+)"`)
	// Relative module specifiers in TypeScript and JavaScript resolve against
	// the importing file; package specifiers (no ./ prefix) are external.
	scriptImportFrom = regexp.MustCompile(`(?:^|[,;=]\s*)(?:import\s+(?:type\s+)?[\w{},*\s]*?\s*from\s*|export\s+(?:type\s+)?[\w{},*\s]*?\s*from\s*|import\s*\(\s*|require\s*\(\s*)["']([^"']+)["']`)
	scriptSideEffect = regexp.MustCompile(`(?:^|[,;]\s*)import\s+["']([^"']+)["']`)
	// Python's forms: dotted absolute imports and relative ones counted by
	// leading dots.
	pythonImportFrom  = regexp.MustCompile(`^\s*from\s+(\.*)([\w.]*)\s+import\b`)
	pythonPlainImport = regexp.MustCompile(`^\s*import\s+([\w.,\s]+)`)
	// A Rust `use crate::...` path is the only intra-crate edge a lexical pass
	// can resolve: external crates come through Cargo.toml, which is the build
	// manifest's business.
	rustUse = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?use\s+(crate::[\w:]+)`)
	// Java imports name a class by package path.
	javaImport = regexp.MustCompile(`^\s*import\s+(?:static\s+)?([\w.]+)\s*;`)
	// A quoted C or C++ include is relative to the including file; angle
	// brackets search system paths and are external by definition.
	cInclude = regexp.MustCompile(`^\s*#\s*include\s+"([^"]+)"`)
)

// parseImports returns the raw import specifiers of one file for the graph
// build to resolve. Each language contributes only the specifiers a lexical
// pass can read honestly; anything else stays out of the graph rather than in
// it as a guess.
func parseImports(language string, data []byte) []string {
	switch language {
	case "go":
		return parseGoImports(data)
	case "javascript", "typescript":
		return parseScriptImports(data)
	case "python":
		return parsePythonImports(data)
	case "rust":
		return parseRustImports(data)
	case "java":
		return parseJavaImports(data)
	case "c", "cpp":
		return parseCIncludes(data)
	}
	return nil
}

func parseGoImports(data []byte) []string {
	var specs []string
	inGroup := false
	for _, raw := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(raw)
		switch {
		case trimmed == "import (":
			inGroup = true
		case inGroup && trimmed == ")":
			inGroup = false
		case inGroup:
			if match := goImportLine.FindStringSubmatch(trimmed); match != nil {
				specs = append(specs, match[1])
			}
		default:
			if match := goSingle.FindStringSubmatch(trimmed); match != nil {
				specs = append(specs, match[1])
			}
		}
	}
	return specs
}

func parseScriptImports(data []byte) []string {
	var specs []string
	for _, raw := range strings.Split(string(data), "\n") {
		for _, match := range scriptImportFrom.FindAllStringSubmatch(raw, -1) {
			specs = append(specs, match[1])
		}
		for _, match := range scriptSideEffect.FindAllStringSubmatch(raw, -1) {
			specs = append(specs, match[1])
		}
	}
	return specs
}

func parsePythonImports(data []byte) []string {
	var specs []string
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "from ") {
			if match := pythonImportFrom.FindStringSubmatch(raw); match != nil {
				specs = append(specs, match[1]+match[2])
				continue
			}
		}
		if match := pythonPlainImport.FindStringSubmatch(raw); match != nil {
			for _, dotted := range strings.Split(match[1], ",") {
				dotted = strings.TrimSpace(dotted)
				if dotted != "" {
					specs = append(specs, dotted)
				}
			}
		}
	}
	return specs
}

func parseRustImports(data []byte) []string {
	var specs []string
	for _, raw := range strings.Split(string(data), "\n") {
		if match := rustUse.FindStringSubmatch(raw); match != nil {
			specs = append(specs, match[1])
		}
	}
	return specs
}

func parseJavaImports(data []byte) []string {
	var specs []string
	for _, raw := range strings.Split(string(data), "\n") {
		if match := javaImport.FindStringSubmatch(raw); match != nil {
			specs = append(specs, match[1])
		}
	}
	return specs
}

func parseCIncludes(data []byte) []string {
	var specs []string
	for _, raw := range strings.Split(string(data), "\n") {
		if match := cInclude.FindStringSubmatch(raw); match != nil {
			specs = append(specs, match[1])
		}
	}
	return specs
}

// resolveImport maps one raw specifier to repository paths, reported through
// the emit callback. Resolution is per language and only ever names files the
// index holds: the callback checks membership, so this function can suggest
// candidates without knowing the file set.
func resolveImport(language, spec, fromPath string, emit func(candidate string)) {
	switch language {
	case "go":
		// A Go import names a package directory; the graph reaches every
		// indexed file of that package. The module prefix is stripped when
		// the caller supplied one through the spec's leading segments —
		// resolution tries the suffixes of the import path until one lands
		// inside the repository.
		emit(spec)
		segments := strings.Split(spec, "/")
		for cut := 1; cut < len(segments); cut++ {
			emit(strings.Join(segments[cut:], "/"))
		}
	case "javascript", "typescript":
		if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
			return
		}
		base := path.Join(path.Dir(fromPath), spec)
		for _, suffix := range []string{"", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
			emit(base + suffix)
		}
		for _, index := range []string{".ts", ".js", ".tsx", ".jsx"} {
			emit(base + "/index" + index)
		}
	case "python":
		if strings.HasPrefix(spec, ".") {
			// Relative: dots climb from the importing file's directory, the
			// remainder is the module path below it.
			ups := 0
			rest := spec
			for strings.HasPrefix(rest, ".") {
				ups++
				rest = rest[1:]
			}
			base := path.Dir(fromPath)
			for ; ups > 1; ups-- {
				base = path.Dir(base)
			}
			emitPythonModule(base, rest, emit)
			return
		}
		// Absolute against the repository root: try each suffix so a package
		// mounted under a top-level directory still resolves.
		segments := strings.Split(spec, ".")
		for cut := 0; cut < len(segments); cut++ {
			emitPythonModule("", strings.Join(segments[cut:], "."), emit)
		}
	case "rust":
		if !strings.HasPrefix(spec, "crate::") {
			return
		}
		// A use path can end in a symbol (including a grouped-use member),
		// rather than a module. Offer module prefixes; only indexed files
		// become edges, and no compiler-level binding is implied.
		parts := strings.Split(strings.TrimPrefix(spec, "crate::"), "::")
		for end := len(parts); end > 0; end-- {
			rest := strings.Join(parts[:end], "/")
			emit("src/" + rest + ".rs")
			emit("src/" + rest + "/mod.rs")
		}
	case "java":
		emit(strings.ReplaceAll(spec, ".", "/") + ".java")
		// A static import names a member of the class; the candidate without
		// the last segment reaches the class file itself.
		if segments := strings.Split(spec, "."); len(segments) > 1 {
			emit(strings.ReplaceAll(strings.Join(segments[:len(segments)-1], "."), ".", "/") + ".java")
		}
	case "c", "cpp":
		// A quoted include resolves against the including file's directory;
		// the exact relative spelling is the only honest candidate.
		emit(path.Join(path.Dir(fromPath), spec))
	}
}

// emitPythonModule offers the two forms a module takes on disk: a file and a
// package directory's __init__.
func emitPythonModule(base, dotted string, emit func(candidate string)) {
	if dotted == "" {
		emit(base)
		return
	}
	slashed := strings.ReplaceAll(dotted, ".", "/")
	if base == "" || base == "." {
		emit(slashed + ".py")
		emit(slashed + "/__init__.py")
		return
	}
	emit(base + "/" + slashed + ".py")
	emit(base + "/" + slashed + "/__init__.py")
}
