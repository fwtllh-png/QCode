package tool

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// DefaultIdentityKeys are the public identity fields used when a descriptor
// has not declared IdentityKeys. Large bodies such as command, content, and
// patch are not in this list.
var DefaultIdentityKeys = []string{
	"path", "paths", "target", "file", "handle", "name", "query",
	"start_line", "max_lines",
}

var largeIdentityKeys = map[string]struct{}{
	"command": {},
	"content": {},
	"patch":   {},
}

// ResolvedIdentityKeys returns the compaction identity fields for a tool.
// Declared IdentityKeys win. Otherwise the default whitelist is unioned with
// required scalar InputSchema fields, and large bodies stay omitted unless
// the descriptor named them.
func ResolvedIdentityKeys(descriptor Descriptor) []string {
	if keys := normalizeIdentityKeys(descriptor.IdentityKeys); len(keys) > 0 {
		return keys
	}
	return undeclaredIdentityKeys(descriptor.InputSchema)
}

func undeclaredIdentityKeys(schema map[string]any) []string {
	seen := make(map[string]struct{}, len(DefaultIdentityKeys))
	keys := make([]string, 0, len(DefaultIdentityKeys))
	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if _, large := largeIdentityKeys[key]; large {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for _, key := range DefaultIdentityKeys {
		add(key)
	}
	for _, key := range requiredScalarFields(schema) {
		add(key)
	}
	return keys
}

func normalizeIdentityKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(keys))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

func requiredScalarFields(schema map[string]any) []string {
	properties, _ := schema["properties"].(map[string]any)
	if len(properties) == 0 {
		return nil
	}
	var keys []string
	for _, name := range collectRequiredNames(schema) {
		property, _ := properties[name].(map[string]any)
		if !scalarSchema(property) {
			continue
		}
		keys = append(keys, name)
	}
	return keys
}

func collectRequiredNames(schema map[string]any) []string {
	names := stringList(schema["required"])
	for _, key := range []string{"anyOf", "oneOf"} {
		alternatives, _ := schema[key].([]any)
		for _, alternative := range alternatives {
			object, _ := alternative.(map[string]any)
			names = append(names, stringList(object["required"])...)
		}
	}
	return names
}

func stringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			text, _ := item.(string)
			if strings.TrimSpace(text) != "" {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func scalarSchema(schema map[string]any) bool {
	switch schema["type"] {
	case "string", "number", "integer", "boolean":
		return true
	default:
		return false
	}
}

// CompactIdentityArguments keeps only the named identity fields. Declared
// large string fields are truncated to stringLimit UTF-8 bytes.
func CompactIdentityArguments(arguments string, keys []string, stringLimit int) string {
	if arguments == "" {
		return "{}"
	}
	if len(keys) == 0 {
		keys = DefaultIdentityKeys
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &object) != nil {
		return "{}"
	}
	kept := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		value, ok := object[key]
		if !ok {
			continue
		}
		if stringLimit > 0 {
			if _, large := largeIdentityKeys[key]; large {
				value = truncateJSONString(value, stringLimit)
			}
		}
		kept[key] = value
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func truncateJSONString(raw json.RawMessage, limit int) json.RawMessage {
	var text string
	if json.Unmarshal(raw, &text) != nil || len(text) <= limit {
		return raw
	}
	encoded, err := json.Marshal(truncateUTF8(text, limit) + "...")
	if err != nil {
		return raw
	}
	return encoded
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
