package prompt_test

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/builtin"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/platform/process"
)

// TestDumpDescriptorSizes prints the rendered description and schema sizes of
// the default catalog. It asserts nothing: it is the measurement harness for
// keeping model-visible tool contracts lean (behavioral guidance belongs in
// the stable prompt partitions, not in per-tool descriptions). Run with
// QCODE_DUMP_SIZES=1 to see the table.
func TestDumpDescriptorSizes(t *testing.T) {
	if os.Getenv("QCODE_DUMP_SIZES") == "" {
		t.Skip("size dump disabled")
	}
	root := t.TempDir()
	store := contentstore.NewMemory(contentstore.Options{})
	manager := process.NewSessionManager(0)
	t.Cleanup(func() {
		manager.CloseAll()
		_ = store.Close(context.Background())
	})
	registry, _, err := builtin.NewWithDependencies(
		root, catalogBenchmarkBackend{}, store, manager,
	)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		name   string
		desc   int
		schema int
	}
	var rows []row
	var totalDesc, totalSchema int
	for _, descriptor := range registry.Descriptors(tool.VisibleModel) {
		schema, _ := json.Marshal(descriptor.InputSchema)
		rows = append(rows, row{descriptor.Name, len(descriptor.Description), len(schema)})
		totalDesc += len(descriptor.Description)
		totalSchema += len(schema)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].desc > rows[j].desc })
	for _, r := range rows {
		if r.desc > 200 {
			t.Logf("%-24s desc=%5dB schema=%5dB", r.name, r.desc, r.schema)
		}
	}
	t.Logf("TOTAL desc=%dB schema=%dB tools=%d", totalDesc, totalSchema, len(rows))
}
