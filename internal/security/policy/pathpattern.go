package policy

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// PathPattern is a compiled path rule. A segment may use "*", "?", and
// "[...]" within one path segment, "\" escapes the next character, and a
// whole-segment "**" matches zero or more segments. A pattern matches a path
// when it matches the path or any of its ancestors, so a directory pattern
// covers its subtree.
type PathPattern struct {
	absolute bool
	segments []string
}

const pathWildcards = `*?[\{}`

// IsPathPattern reports whether a rule resource names filesystem locations.
// Absolute URLs and scheme-qualified identifiers are matched literally.
func IsPathPattern(resource string) bool {
	if parsed, err := url.Parse(resource); err == nil && parsed.IsAbs() && parsed.Host != "" {
		return false
	}
	return strings.Contains(resource, "/") || strings.Contains(resource, `\`) ||
		resource == "." || !strings.Contains(resource, ":")
}

func CompilePathPattern(pattern string) (PathPattern, error) {
	if strings.TrimSpace(pattern) == "" {
		return PathPattern{}, errors.New("path pattern is empty")
	}
	compiled := PathPattern{absolute: strings.HasPrefix(pattern, "/")}
	for _, segment := range strings.Split(strings.Trim(pattern, "/"), "/") {
		switch {
		case segment == "" || segment == ".":
			continue
		case segment == "**":
		default:
			if _, err := path.Match(segment, ""); err != nil {
				return PathPattern{}, fmt.Errorf("%q: %w", segment, err)
			}
			escaped, inClass, star := false, false, false
			for _, char := range segment {
				if escaped {
					escaped, star = false, false
					continue
				}
				if char == '\\' {
					escaped, star = true, false
					continue
				}
				if char == '[' {
					inClass = true
				}
				if char == ']' {
					inClass = false
				}
				if !inClass && (char == '{' || char == '}') {
					return PathPattern{}, errors.New("brace alternation is unsupported")
				}
				if !inClass && char == '*' && star {
					return PathPattern{}, fmt.Errorf("%q: ** must be a whole path segment", segment)
				}
				star = !inClass && char == '*'
			}
		}
		compiled.segments = append(compiled.segments, segment)
	}
	return compiled, nil
}

// EscapePathPattern quotes a literal location so wildcards in it match only
// themselves.
func EscapePathPattern(literal string) string {
	var b strings.Builder
	for _, char := range literal {
		if strings.ContainsRune(pathWildcards, char) {
			b.WriteByte('\\')
		}
		b.WriteRune(char)
	}
	return b.String()
}

func (p PathPattern) Match(value string) bool {
	return p.match(value, false)
}

// IntersectsTree checks the complete authority of a directory: a denied
// pattern may cover the root, an ancestor, or a possible descendant. It does
// not scan current files, because the operation can create new descendants.
func (p PathPattern) IntersectsTree(value string) bool {
	return p.match(value, true)
}

func (p PathPattern) match(value string, tree bool) bool {
	value = filepath.ToSlash(filepath.Clean(value))
	if strings.HasPrefix(value, "/") != p.absolute {
		return false
	}
	var segments []string
	for _, segment := range strings.Split(strings.Trim(value, "/"), "/") {
		if segment != "" && segment != "." {
			segments = append(segments, segment)
		}
	}
	return matchPrefix(p.segments, segments, tree)
}

// matchPrefix reports whether pattern matches some leading run of segments.
func matchPrefix(pattern, segments []string, tree bool) bool {
	if len(pattern) == 0 {
		return true
	}
	if tree && len(segments) == 0 {
		return true
	}
	if pattern[0] == "**" {
		for skip := 0; skip <= len(segments); skip++ {
			if matchPrefix(pattern[1:], segments[skip:], tree) {
				return true
			}
		}
		return false
	}
	if len(segments) == 0 {
		return false
	}
	matched, err := path.Match(pattern[0], segments[0])
	return err == nil && matched && matchPrefix(pattern[1:], segments[1:], tree)
}

func validateRuleResource(resource string) error {
	if resource == "" || resource == "*" || !IsPathPattern(resource) {
		return nil
	}
	_, err := CompilePathPattern(resource)
	return err
}
