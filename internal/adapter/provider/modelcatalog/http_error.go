package modelcatalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
)

func responseError(operation string, response *http.Response, apiKey string) error {
	summary := fmt.Sprintf("%s HTTP %d", operation, response.StatusCode)
	body, err := io.ReadAll(io.LimitReader(response.Body, httpclient.MaxErrorBodyBytes+1))
	if err != nil || len(body) > httpclient.MaxErrorBodyBytes {
		return fmt.Errorf("%s", summary)
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	// Only display a complete structured message. Raw or truncated bodies may
	// contain credentials that cannot be reliably redacted.
	if json.Unmarshal(body, &payload) != nil {
		return fmt.Errorf("%s", summary)
	}
	message := telemetry.NewRedactor(strings.TrimSpace(apiKey)).Redact(payload.Error.Message)
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return fmt.Errorf("%s", summary)
	}
	return fmt.Errorf("%s: %s", summary, message)
}
