package result_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
)

func TestMCPAvailabilityErrorsRemainRecoverable(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		category string
	}{
		{"unavailable", mcp.ErrServerUnavailable, mcp.ErrorCategoryUnavailable},
		{"circuit_open", mcp.ErrCircuitOpen, mcp.ErrorCategoryCircuitOpen},
	} {
		for _, wrap := range []struct {
			name string
			wrap func(error) error
		}{
			{"direct", func(err error) error { return err }},
			{"wrapped", func(err error) error { return fmt.Errorf("remote call: %w", err) }},
			{"joined", func(err error) error { return errors.Join(errors.New("transport closed"), err) }},
		} {
			t.Run(test.name+"/"+wrap.name, func(t *testing.T) {
				err := wrap.wrap(test.err)
				if !errors.Is(err, test.err) {
					t.Fatal("MCP sentinel identity was lost")
				}
				content, ok := toolresult.RecoverableFailure(err)
				if !ok || content != err.Error() {
					t.Fatalf("recovery = %q, %v; want original error text", content, ok)
				}
				if category := toolresult.FailureCategory(err); category != test.category {
					t.Fatalf("category = %q, want %q", category, test.category)
				}
				result, ok := toolresult.RecoverResult(
					tool.NewRegistry(nil, nil), provider.ToolCall{Name: "remote"},
					tool.Result{Metadata: map[string]any{"mcp_server": "fixture"}}, err,
				)
				if !ok || !result.IsError || result.Content != err.Error() ||
					result.Metadata["error_category"] != test.category ||
					result.Metadata["mcp_server"] != "fixture" ||
					len(result.Metadata) != 2 {
					t.Fatalf("recovered result = %+v, ok=%v", result, ok)
				}
				if result.Outcome == nil || result.Outcome.Facts.Failure == nil ||
					result.Outcome.Facts.Failure.Category != test.category {
					t.Fatalf("failure outcome = %+v", result.Outcome)
				}
			})
		}
	}
}

type uncategorizedError struct{}

func (uncategorizedError) Error() string               { return "unclassified adapter error" }
func (uncategorizedError) RecoverableCategory() string { return "" }

func TestRecoveryRequiresAnExplicitNonemptyCategory(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("MCP server unavailable"),
		errors.New("MCP circuit breaker is open"),
		fmt.Errorf("remote call: %w", uncategorizedError{}),
	} {
		if content, ok := toolresult.RecoverableFailure(err); ok || content != "" {
			t.Fatalf("unclassified error %v recovered as %q", err, content)
		}
		if category := toolresult.FailureCategory(err); category != "" {
			t.Fatalf("unclassified error %v has category %q", err, category)
		}
	}
}
