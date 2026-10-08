package agentcontext

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// B2 verifies complete, contiguous ranges, including the former lost tail.
func TestContextContinuityP3NarrativeRangeCoverage(t *testing.T) {
	data, err := os.ReadFile("testdata/context_continuity_long_report_zh.txt")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSpace(string(data))
	for _, ceiling := range []int{0, 127} {
		t.Run(string(rune('a'+ceiling)), func(t *testing.T) {
			limits := NarrativeLimits{}
			limits.ExcerptMaxBytes = ceiling
			input, err := BuildNarrativeInput("continuity-thread", "window", "truth", "route", []provider.Message{messageAt(provider.RoleAssistant, body, 1)}, limits, time.Now().UTC(), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			var rebuilt strings.Builder
			cursor := 0
			for _, excerpt := range input.Excerpts {
				if excerpt.Truncated || !utf8.ValidString(excerpt.Text) || excerpt.Source == nil || excerpt.Source.Start != cursor || excerpt.Source.End-excerpt.Source.Start != len(excerpt.Text) {
					t.Fatalf("invalid range: %+v", excerpt)
				}
				rebuilt.WriteString(excerpt.Text)
				cursor = excerpt.Source.End
			}
			if rebuilt.String() != body || cursor != len(body) {
				t.Fatal("source bytes were dropped or duplicated")
			}
			for _, definition := range []string{"重试时丢弃原始请求参数，导致第二次请求缺少过滤条件。", "分页游标未随结果推进，下一页会重复返回上一页的数据。"} {
				if !strings.Contains(rebuilt.String(), definition) {
					t.Fatal("lost tail definition")
				}
			}
		})
	}
}
