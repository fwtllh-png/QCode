package goproxy

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const CredentialKindHost = "host"

// SplitProxyList splits GOPROXY on the official comma separator and the
// documented |direct fallback form.
func SplitProxyList(raw string) []string {
	raw = strings.ReplaceAll(raw, "|", ",")
	var items []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}

// InspectHost builds a session GOPROXY binding from the host GOPROXY value.
// It does not carry userinfo. Prefix "*" means the host proxy already covers
// every module the host Go toolchain sends there.
func InspectHost(goproxyValue string) (Binding, bool) {
	first, ok := firstProxyURL(goproxyValue)
	if !ok {
		return Binding{}, false
	}
	origin := url.URL{Scheme: first.Scheme, Host: first.Host}
	return Binding{
		Upstream: origin.String(),
		Prefixes: []string{"*"},
		Credential: CredentialRef{
			Kind: CredentialKindHost,
			Name: first.Hostname(),
		},
	}, true
}

// LookupHostSecret reads the host GOPROXY userinfo or the host netrc for
// that proxy. The secret stays in the caller; it is never a Binding field.
func LookupHostSecret(goproxyValue, netrcPath string) (string, bool) {
	first, ok := firstProxyURL(goproxyValue)
	if !ok {
		return "", false
	}
	if first.User != nil {
		password, _ := first.User.Password()
		user := first.User.Username()
		switch {
		case user != "" && password != "":
			return user + ":" + password, true
		case password != "":
			return password, true
		case user != "":
			return user, true
		}
	}
	return lookupNetrc(netrcPath, first.Hostname())
}

func DefaultNetrcPath() string {
	if path := strings.TrimSpace(os.Getenv("NETRC")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".netrc")
}

func firstProxyURL(raw string) (*url.URL, bool) {
	for _, item := range SplitProxyList(raw) {
		if item == "off" || item == "direct" {
			return nil, false
		}
		parsed, err := url.Parse(item)
		if err != nil || parsed.Hostname() == "" {
			return nil, false
		}
		if parsed.Scheme != "https" && parsed.Scheme != "http" {
			return nil, false
		}
		return parsed, true
	}
	return nil, false
}

func lookupNetrc(path, host string) (string, bool) {
	path = strings.TrimSpace(path)
	host = strings.TrimSpace(host)
	if path == "" || host == "" {
		return "", false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return parseNetrc(string(body), host)
}

func parseNetrc(body, host string) (string, bool) {
	tokens := strings.Fields(body)
	var machine, login, password string
	inMacro := false
	apply := func() (string, bool) {
		if machine != host && machine != "default" {
			return "", false
		}
		if login == "" && password == "" {
			return "", false
		}
		if login != "" && password != "" {
			return login + ":" + password, true
		}
		if password != "" {
			return password, true
		}
		return login, true
	}
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if inMacro {
			if token == "" || token == "macdef" {
				inMacro = false
			}
			continue
		}
		switch token {
		case "macdef":
			inMacro = true
		case "machine":
			if secret, ok := apply(); ok && machine == host {
				return secret, true
			}
			machine, login, password = nextNetrcArg(tokens, &i), "", ""
		case "default":
			if secret, ok := apply(); ok && machine == host {
				return secret, true
			}
			machine, login, password = "default", "", ""
		case "login":
			login = nextNetrcArg(tokens, &i)
		case "password":
			password = nextNetrcArg(tokens, &i)
		}
	}
	if secret, ok := apply(); ok {
		return secret, true
	}
	return "", false
}

func nextNetrcArg(tokens []string, index *int) string {
	if *index+1 >= len(tokens) {
		return ""
	}
	*index++
	return tokens[*index]
}
