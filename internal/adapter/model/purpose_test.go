package model

import (
	"strings"
	"testing"
)

func TestParsePurposeNamesTheValueItRejected(t *testing.T) {
	if _, err := ParsePurpose("act"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePurpose("plan"); err == nil {
		t.Fatal("removed plan purpose was accepted")
	}
	_, err := ParsePurpose("planning")
	if err == nil || !strings.Contains(err.Error(), `"planning"`) {
		t.Fatalf("ParsePurpose() error = %v, want the rejected value quoted", err)
	}
}
