package symbols

import "context"

// SemanticQuery identifies a symbol at a concrete document position. Line and
// Character are 1-based so tool callers see the same coordinates as editors
// and repository search results.
type SemanticQuery struct {
	Path      string
	Line      int
	Character int
}

// Location is a semantic definition or reference reported by a language
// provider. Coordinates are 1-based.
type Location struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

// SemanticResult carries provenance with every semantic answer. Source names
// the provider, Version is its reported version, and Confidence distinguishes
// compiler/language-server answers from lexical fallback.
type SemanticResult struct {
	Locations  []Location `json:"locations"`
	Source     string     `json:"source"`
	Version    string     `json:"version,omitempty"`
	Confidence string     `json:"confidence"`
}

// Provider resolves definitions and references using language semantics.
type Provider interface {
	Definition(context.Context, SemanticQuery) (SemanticResult, error)
	References(context.Context, SemanticQuery, bool) (SemanticResult, error)
}
