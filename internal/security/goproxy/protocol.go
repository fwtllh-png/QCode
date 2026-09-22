package goproxy

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Protocol = "goproxy"

type RequestKind string

const (
	KindList   RequestKind = "list"
	KindLatest RequestKind = "latest"
	KindInfo   RequestKind = "info"
	KindMod    RequestKind = "mod"
	KindZip    RequestKind = "zip"
	KindSumdb  RequestKind = "sumdb"
)

type Request struct {
	Module  string
	Version string
	Kind    RequestKind
	Escaped string
}

func ParseRequest(rawPath string) (Request, error) {
	if strings.IndexByte(rawPath, '?') >= 0 || strings.IndexByte(rawPath, '#') >= 0 {
		return Request{}, fmt.Errorf("goproxy path must not include a query or fragment")
	}
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" || rawPath[0] != '/' {
		return Request{}, fmt.Errorf("goproxy path must be absolute")
	}
	if strings.Contains(rawPath, "//") || strings.Contains(rawPath, `\`) {
		return Request{}, fmt.Errorf("goproxy path is invalid")
	}
	cleaned := path.Clean(rawPath)
	if cleaned != rawPath {
		return Request{}, fmt.Errorf("goproxy path is invalid")
	}
	if strings.Contains(cleaned, "..") {
		return Request{}, fmt.Errorf("goproxy path is invalid")
	}
	rest := strings.TrimPrefix(cleaned, "/")
	if strings.HasPrefix(rest, "sumdb/") {
		// /sumdb/<name>/<path> is the checksum-database proxy route. The
		// payload is self-verifying, so the shape check only needs a
		// non-empty database name and a non-empty remainder.
		file := strings.TrimPrefix(rest, "sumdb/")
		name, remainder, ok := strings.Cut(file, "/")
		if !ok || name == "" || remainder == "" ||
			strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			return Request{}, fmt.Errorf("goproxy sumdb path is invalid")
		}
		return Request{Module: name, Kind: KindSumdb, Escaped: rest}, nil
	}
	switch {
	case strings.HasSuffix(rest, "/@v/list"):
		module, err := unescapeModule(strings.TrimSuffix(rest, "/@v/list"))
		if err != nil {
			return Request{}, err
		}
		return Request{Module: module, Kind: KindList, Escaped: rest}, nil
	case strings.HasSuffix(rest, "/@latest"):
		module, err := unescapeModule(strings.TrimSuffix(rest, "/@latest"))
		if err != nil {
			return Request{}, err
		}
		return Request{Module: module, Kind: KindLatest, Escaped: rest}, nil
	}
	const marker = "/@v/"
	index := strings.LastIndex(rest, marker)
	if index <= 0 {
		return Request{}, fmt.Errorf("goproxy path is not a module proxy request")
	}
	module, err := unescapeModule(rest[:index])
	if err != nil {
		return Request{}, err
	}
	file := rest[index+len(marker):]
	kind, version, err := parseVersionFile(file)
	if err != nil {
		return Request{}, err
	}
	return Request{
		Module: module, Version: version, Kind: kind, Escaped: rest,
	}, nil
}

func parseVersionFile(file string) (RequestKind, string, error) {
	switch {
	case strings.HasSuffix(file, ".info"):
		return KindInfo, strings.TrimSuffix(file, ".info"), validateVersion(strings.TrimSuffix(file, ".info"))
	case strings.HasSuffix(file, ".mod"):
		return KindMod, strings.TrimSuffix(file, ".mod"), validateVersion(strings.TrimSuffix(file, ".mod"))
	case strings.HasSuffix(file, ".zip"):
		return KindZip, strings.TrimSuffix(file, ".zip"), validateVersion(strings.TrimSuffix(file, ".zip"))
	default:
		return "", "", fmt.Errorf("goproxy version file is not info, mod, or zip")
	}
}

func validateVersion(version string) error {
	if version == "" || strings.ContainsAny(version, "/\\") || version == ".." ||
		strings.Contains(version, "..") {
		return fmt.Errorf("goproxy version is invalid")
	}
	if !utf8.ValidString(version) {
		return fmt.Errorf("goproxy version is invalid")
	}
	return nil
}

func unescapeModule(escaped string) (string, error) {
	if escaped == "" || strings.Contains(escaped, "\\") {
		return "", fmt.Errorf("goproxy module path is invalid")
	}
	var out strings.Builder
	for i := 0; i < len(escaped); i++ {
		if escaped[i] != '!' {
			out.WriteByte(escaped[i])
			continue
		}
		if i+1 >= len(escaped) {
			return "", fmt.Errorf("goproxy module path is invalid")
		}
		r := rune(escaped[i+1])
		if r < 'a' || r > 'z' {
			return "", fmt.Errorf("goproxy module path is invalid")
		}
		out.WriteRune(unicode.ToUpper(r))
		i++
	}
	module := out.String()
	if module == "" || strings.Contains(module, "..") || strings.HasPrefix(module, "/") {
		return "", fmt.Errorf("goproxy module path is invalid")
	}
	return module, nil
}

func MatchPrefix(module string, prefixes []string) bool {
	for _, prefix := range prefixes {
		prefix = strings.TrimSuffix(strings.TrimSpace(prefix), "/")
		if prefix == "" {
			continue
		}
		if prefix == "*" {
			return true
		}
		// Boundary match only: a prefix covers the module itself and its
		// subtree, never a sibling that merely shares leading characters
		// ("corp.io/team" must not authorize "corp.io/team-secrets").
		if module == prefix || strings.HasPrefix(module, prefix+"/") {
			return true
		}
	}
	return false
}
