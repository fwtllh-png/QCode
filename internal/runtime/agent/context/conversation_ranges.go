package agentcontext

import "sort"

// ConversationDependencyRanges preserves each selected item in full and the
// surrounding prose of its ancestors. Sibling definitions are excluded; merely
// copying an ancestor's whole section would make focused recovery unbounded.
// Offsets always refer to the immutable original text.
func ConversationDependencyRanges(source ConversationSource, itemIDs []string) []ReferenceRange {
	items := make(map[string]ReferenceItem, len(source.Items))
	children := make(map[string][]ReferenceRange)
	for _, item := range source.Items {
		items[item.ID] = item
		children[item.ParentID] = append(children[item.ParentID], ReferenceRange{Start: item.Start, End: item.End})
	}
	var ranges []ReferenceRange
	ancestors := make(map[string]bool)
	for _, id := range itemIDs {
		item, ok := items[id]
		if !ok {
			continue
		}
		ranges = append(ranges, ReferenceRange{Start: item.Start, End: item.End})
		for item.ParentID != "" && !ancestors[item.ParentID] {
			item, ok = items[item.ParentID]
			if !ok {
				break
			}
			ancestors[item.ID] = true
			childRanges := mergeConversationRanges(children[item.ID])
			start := item.Start
			for _, child := range childRanges {
				if start < child.Start {
					ranges = append(ranges, ReferenceRange{Start: start, End: child.Start})
				}
				start = max(start, child.End)
			}
			if start < item.End {
				ranges = append(ranges, ReferenceRange{Start: start, End: item.End})
			}
		}
	}
	return mergeConversationRanges(ranges)
}

func mergeConversationRanges(ranges []ReferenceRange) []ReferenceRange {
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	var merged []ReferenceRange
	for _, r := range ranges {
		if len(merged) > 0 && r.Start <= merged[len(merged)-1].End {
			merged[len(merged)-1].End = max(merged[len(merged)-1].End, r.End)
		} else {
			merged = append(merged, r)
		}
	}
	return merged
}
