package agentcontext

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConversationDependencyRangesKeepPremisesWithoutSiblingDefinitions(t *testing.T) {
	for _, text := range []string{
		"# 报告\n共同前提\n\n1. 无关兄弟定义\n2. 目标条目\n\n共同结尾约束\n\n## 其他章节\n无关章节内容\n",
		"# 报告\n共同前提\n\n1. 父项前提\n   1. 无关兄弟定义\n   2. 目标条目\n2. 无关顶层定义\n\n共同结尾约束\n",
	} {
		source := IndexConversationAnswer("thread", "report", 1, text)
		before := append([]ReferenceItem(nil), source.Items...)
		var selected ReferenceItem
		for _, item := range source.Items {
			if item.Kind == "list_item" && strings.Contains(text[item.Start:item.End], "目标条目") && item.Start > selected.Start {
				selected = item
			}
		}
		if selected.ID == "" {
			t.Fatal("fixture has no target list item")
		}
		ranges := ConversationDependencyRanges(source, []string{selected.ID})
		var rendered strings.Builder
		covered := false
		previousEnd := -1
		for _, r := range ranges {
			if r.Start <= previousEnd || r.End <= r.Start || r.End > len(text) || !utf8.ValidString(text[r.Start:r.End]) {
				t.Fatalf("invalid disjoint source range: %+v", r)
			}
			covered = covered || r.Start <= selected.Start && r.End >= selected.End
			rendered.WriteString(text[r.Start:r.End])
			previousEnd = r.End
		}
		result := rendered.String()
		if !covered || !strings.Contains(result, "共同前提") || !strings.Contains(result, "共同结尾约束") || !strings.Contains(result, "2. 目标条目") {
			t.Fatalf("lost definition, numbering or common premises: %s", result)
		}
		if strings.Contains(text, "父项前提") && !strings.Contains(result, "父项前提") {
			t.Fatal("lost nested parent premise")
		}
		if strings.Contains(result, "无关") || !reflect.DeepEqual(source.Items, before) {
			t.Fatalf("copied siblings or changed source identity: %s", result)
		}
	}
}
