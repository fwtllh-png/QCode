package skill

import (
	"regexp"
	"strings"
)

const (
	DefaultMaxDepth     = 4
	DefaultMaxFileBytes = 1 << 20
	DefaultMaxSkills    = 1024
	DefaultMaxEntries   = 8192
	DefaultMaxResolved  = 256
	DefaultMaxLoadBytes = 4 << 20
)

type Source string

const (
	SourceWorkspace  Source = "workspace"
	SourceConfigured Source = "configured"
	SourceUser       Source = "user"
)

type Metadata struct {
	Name                   string            `json:"name"`
	Description            string            `json:"description"`
	LocalizedDescriptions  map[string]string `json:"localized_descriptions,omitempty"`
	DisableModelInvocation bool              `json:"disable_model_invocation,omitempty"`
	UserInvocable          *bool             `json:"user_invocable,omitempty"`
}

func (m Metadata) DescriptionFor(locale string) string {
	if value := localizedValue(m.LocalizedDescriptions, locale); value != "" {
		return value
	}
	return m.Description
}

type Summary struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	Source         Source `json:"source"`
	Path           string `json:"path"`
	Version        string `json:"version"`
	Compatibility  string `json:"compatibility,omitempty"`
	Digest         string `json:"digest"`
	Locked         bool   `json:"locked"`
	Handle         string `json:"handle"`
	PackageHandle  string `json:"package_handle"`
	ResourceHandle string `json:"resource_handle"`
	ModelInvocable bool   `json:"model_invocable"`
}

type Loaded struct {
	Summary
	Content      string            `json:"content"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
}

type Issue struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type Limits struct {
	MaxDepth     int
	MaxFileBytes int64
	MaxSkills    int
	MaxEntries   int
	MaxResolved  int
	MaxLoadBytes int64
}

func (l Limits) normalized() Limits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultMaxDepth
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	if l.MaxSkills <= 0 {
		l.MaxSkills = DefaultMaxSkills
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultMaxEntries
	}
	if l.MaxResolved <= 0 {
		l.MaxResolved = DefaultMaxResolved
	}
	if l.MaxLoadBytes <= 0 {
		l.MaxLoadBytes = DefaultMaxLoadBytes
	}
	return l
}

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	localePattern = regexp.MustCompile(`^[A-Za-z]{2,3}(?:[-_][A-Za-z0-9]{2,8})*$`)
)

func localizedValue(values map[string]string, locale string) string {
	locale = normalizeLocale(locale)
	if locale == "" {
		return ""
	}
	if value := values[locale]; value != "" {
		return value
	}
	if language, _, found := strings.Cut(locale, "-"); found {
		return values[language]
	}
	return ""
}

func normalizeLocale(locale string) string {
	locale = strings.TrimSpace(strings.ReplaceAll(locale, "_", "-"))
	if locale == "" || !localePattern.MatchString(locale) {
		return ""
	}
	parts := strings.Split(locale, "-")
	parts[0] = strings.ToLower(parts[0])
	for index := 1; index < len(parts); index++ {
		parts[index] = strings.ToUpper(parts[index])
	}
	return strings.Join(parts, "-")
}
