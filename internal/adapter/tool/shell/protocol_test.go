package shell

import (
	"strings"
	"testing"
	"time"
)

// The declared process deadline is a documented contract with one working-day
// ceiling: values beyond it are rejected rather than occupying a session
// indefinitely.
func TestProcessTimeoutCeiling(t *testing.T) {
	day := int64(24 * time.Hour / time.Millisecond)
	if _, err := processTimeout(day); err != nil {
		t.Fatalf("24h timeout rejected: %v", err)
	}
	if _, err := processTimeout(day + 1); err == nil ||
		!strings.Contains(err.Error(), "timeout exceeds") {
		t.Fatalf("over-ceiling timeout = %v, want ceiling error", err)
	}
	if _, err := processTimeout(-1); err == nil {
		t.Fatal("negative timeout accepted")
	}
}
