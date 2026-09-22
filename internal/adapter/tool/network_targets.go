package tool

import (
	"errors"
	"strings"
)

// DeclaredNetworkTarget is one exact destination a process tool may request.
type DeclaredNetworkTarget struct {
	Host         string   `json:"host"`
	Protocol     string   `json:"protocol"`
	Port         uint16   `json:"port"`
	Methods      []string `json:"methods"`
	AllowPrivate bool     `json:"allow_private"`
}

// NetworkTargetsInputSchema returns the shared guarded-process destination schema.
func NetworkTargetsInputSchema() map[string]any {
	return map[string]any{
		"type": "array",
		"description": "Exact outbound HTTP(S) destinations only. Do not add " +
			"localhost or port 0 for a local listener; omit this field and use " +
			"allow_loopback instead.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host": map[string]any{
					"type":        "string",
					"description": "DNS host without scheme or port.",
				},
				"protocol": map[string]any{
					"type": "string",
					"enum": []string{"http", "https"},
				},
				"port": map[string]any{
					"type": "integer", "minimum": 1, "maximum": 65535,
					"description": "Exact outbound port. Port 0 is an ephemeral local " +
						"listener, not a network destination; use allow_loopback instead.",
				},
				"methods": map[string]any{
					"type":     "array",
					"items":    map[string]any{"type": "string"},
					"minItems": 1,
					"maxItems": 16,
					"description": "Planned request methods. Enforced per method for " +
						"plaintext HTTP only; for HTTPS the managed proxy controls the " +
						"tunnel endpoint (CONNECT) and cannot see or restrict methods " +
						"inside TLS, so any declared method grants the endpoint.",
				},
				"allow_private": map[string]any{
					"type":        "boolean",
					"description": "Required for private or local resolved IPs.",
				},
			},
			"required": []string{
				"host", "protocol", "port", "methods", "allow_private",
			},
			"additionalProperties": false,
		},
		"maxItems": 32,
	}
}

// ValidateDeclaredNetworkTargets rejects destinations the Guard cannot bind.
// Methods are names only: for HTTPS the managed proxy terminates policy at
// the tunnel endpoint and never inspects TLS, so no protocol/method pairing
// is enforced here. Plaintext HTTP forward requests are matched per method
// by the session gate.
func ValidateDeclaredNetworkTargets(targets []DeclaredNetworkTarget) error {
	if len(targets) > 32 {
		return errors.New("network_targets exceeds 32 entries")
	}
	for _, target := range targets {
		if strings.TrimSpace(target.Host) == "" ||
			(target.Protocol != "http" && target.Protocol != "https") ||
			target.Port == 0 {
			return errors.New("network target requires host, http/https protocol, and port")
		}
		if len(target.Methods) == 0 || len(target.Methods) > 16 {
			return errors.New("network target requires 1-16 methods")
		}
		for _, method := range target.Methods {
			if strings.ToUpper(strings.TrimSpace(method)) == "" {
				return errors.New("network target method is empty")
			}
		}
	}
	return nil
}
