// Package prompt assembles instructions, repository context and other model
// input sections with explicit budgets and receipts. Context remains the source
// of facts, while contextview selects the visible history and accounts for input
// capacity. Prompt does not own authoritative turn or session state.
package prompt
