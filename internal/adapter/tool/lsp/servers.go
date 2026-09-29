package lsp

import (
	"errors"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ServerSpec describes a language server command selected from a source path.
type ServerSpec struct {
	Name   string
	Binary string
	Args   []string
}

var defaultServers = map[string]ServerSpec{
	".go":   {Name: "gopls", Binary: "gopls"},
	".c":    {Name: "clangd", Binary: "clangd"},
	".cc":   {Name: "clangd", Binary: "clangd"},
	".cpp":  {Name: "clangd", Binary: "clangd"},
	".cxx":  {Name: "clangd", Binary: "clangd"},
	".h":    {Name: "clangd", Binary: "clangd"},
	".hh":   {Name: "clangd", Binary: "clangd"},
	".hpp":  {Name: "clangd", Binary: "clangd"},
	".hxx":  {Name: "clangd", Binary: "clangd"},
	".rs":   {Name: "rust-analyzer", Binary: "rust-analyzer"},
	".py":   {Name: "pyright", Binary: "pyright-langserver", Args: []string{"--stdio"}},
	".pyi":  {Name: "pyright", Binary: "pyright-langserver", Args: []string{"--stdio"}},
	".js":   {Name: "typescript-language-server", Binary: "typescript-language-server", Args: []string{"--stdio"}},
	".jsx":  {Name: "typescript-language-server", Binary: "typescript-language-server", Args: []string{"--stdio"}},
	".ts":   {Name: "typescript-language-server", Binary: "typescript-language-server", Args: []string{"--stdio"}},
	".tsx":  {Name: "typescript-language-server", Binary: "typescript-language-server", Args: []string{"--stdio"}},
	".java": {Name: "jdtls", Binary: "jdtls"},
}

// ResolveServer returns an installed language server for path.
func ResolveServer(path string) (ServerSpec, error) {
	spec, ok := defaultServers[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return ServerSpec{}, errors.New("no language server is configured for " + filepath.Ext(path))
	}
	binary, err := exec.LookPath(spec.Binary)
	if err != nil {
		return ServerSpec{}, errors.New(spec.Binary + " is not available")
	}
	spec.Binary = binary
	spec.Args = append([]string(nil), spec.Args...)
	return spec, nil
}

// AvailableServers reports installed default language servers by stable name.
func AvailableServers() []string {
	seen := make(map[string]struct{})
	var result []string
	for _, spec := range defaultServers {
		if _, exists := seen[spec.Name]; exists {
			continue
		}
		if _, err := exec.LookPath(spec.Binary); err == nil {
			seen[spec.Name] = struct{}{}
			result = append(result, spec.Name)
		}
	}
	sort.Strings(result)
	return result
}

func (c Checker) forPaths(paths []string) (Checker, error) {
	if strings.TrimSpace(c.Binary) != "" {
		return c, nil
	}
	var selected ServerSpec
	for _, path := range paths {
		spec, err := ResolveServer(path)
		if err != nil {
			return Checker{}, err
		}
		if selected.Name != "" && selected.Name != spec.Name {
			return Checker{}, errors.New(
				"one LSP request cannot mix " + selected.Name + " and " + spec.Name,
			)
		}
		selected = spec
	}
	if selected.Name == "" {
		return Checker{}, errors.New("at least one source file is required")
	}
	c.Binary = selected.Binary
	c.Args = selected.Args
	return c, nil
}
