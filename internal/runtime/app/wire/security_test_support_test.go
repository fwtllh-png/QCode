package wire

import (
	"testing"
)

func withNonDurableTestJournal(t *testing.T, options ExecOptions) ExecOptions {
	t.Helper()
	durable := false
	options.ConfigOverrides.JournalDurable = &durable
	return options
}
