package tool

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// PageRequest selects one bounded window of a retained body. result_get and
// handle_read both page through ProjectPage, so their cursors mean the same
// thing: every offset is a byte offset into the body, and a returned
// continuation cursor always lies past the start of the page it came from.
type PageRequest struct {
	Mode      string
	StartLine int
	MaxLines  int
	Query     string
	Offset    int
	// Limit is the page's byte cap. A page always holds at least one whole
	// character, so caps below utf8.UTFMax are raised to it.
	Limit int
}

// Page is the projected window plus the cursor metadata to continue from.
type Page struct {
	Excerpt string
	More    bool
	Cursor  map[string]any
}

// ProjectPage projects the summary, head, tail, bytes, lines or query window
// of body.
func ProjectPage(body string, request PageRequest) (Page, error) {
	limit := max(request.Limit, utf8.UTFMax)
	cursor := map[string]any{}
	page := Page{Cursor: cursor}
	switch request.Mode {
	case "summary":
		page.Excerpt, page.More = summarizeResult(body, limit)
	case "head":
		page.Excerpt, page.More = boundedSlice(body, 0, limit)
		if page.More {
			cursor["next_offset"] = len(page.Excerpt)
		}
	case "tail":
		start := max(0, len(body)-limit)
		page.Excerpt = validSuffix(body, start)
		page.More = start > 0
		if page.More {
			cursor["previous_offset"] = len(body) - len(page.Excerpt)
		}
	case "bytes":
		start := validUTF8Start(body, request.Offset)
		page.Excerpt, page.More = boundedSlice(body, start, limit)
		cursor["offset"] = start
		if page.More {
			cursor["next_offset"] = start + len(page.Excerpt)
		}
	case "lines":
		startLine := request.StartLine
		if startLine == 0 {
			startLine = 1
		}
		var nextLine, nextOffset int
		page.Excerpt, page.More, nextLine, nextOffset = selectLines(body, startLine, request.MaxLines, limit)
		cursor["start_line"] = startLine
		if page.More {
			if nextLine > 0 {
				cursor["next_start_line"] = nextLine
			}
			if nextOffset > 0 {
				cursor["next_offset"] = nextOffset
			}
		}
	case "query":
		var nextOffset int
		page.Excerpt, page.More, nextOffset = queryLines(body, request.Query, request.Offset, request.MaxLines, limit)
		cursor["query"] = request.Query
		cursor["offset"] = request.Offset
		if page.More {
			cursor["next_offset"] = nextOffset
		}
	default:
		return Page{}, fmt.Errorf("unsupported retrieval mode %q", request.Mode)
	}
	return page, nil
}

func summarizeResult(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	if limit < 5 {
		excerpt, _ := boundedSlice(value, 0, limit)
		return excerpt, true
	}
	const separator = "\n...\n"
	if limit <= len(separator) {
		excerpt, _ := boundedSlice(value, 0, limit)
		return excerpt, true
	}
	headBytes := (limit - len(separator)) / 2
	tailBytes := limit - len(separator) - headBytes
	head, _ := boundedSlice(value, 0, headBytes)
	tail := validSuffix(value, len(value)-tailBytes)
	for len(head)+len(separator)+len(tail) > limit {
		tail = validSuffix(tail, 1)
	}
	return head + separator + tail, true
}

func countLines(value string) int {
	if value == "" {
		return 0
	}
	count := strings.Count(value, "\n")
	if !strings.HasSuffix(value, "\n") {
		count++
	}
	return count
}

func selectLines(value string, startLine, maxLines, limit int) (excerpt string, more bool, nextLine, nextOffset int) {
	lines := strings.SplitAfter(value, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if startLine > len(lines) {
		return "", false, 0, 0
	}
	startOffset := 0
	for _, line := range lines[:startLine-1] {
		startOffset += len(line)
	}
	lines = lines[startLine-1:]
	selectedLines := len(lines)
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		selectedLines = maxLines
	}
	selected := strings.Join(lines, "")
	excerpt, byteMore := boundedSlice(selected, 0, limit)
	if byteMore {
		return excerpt, true, 0, startOffset + len(excerpt)
	}
	nextLine = startLine + selectedLines
	if nextLine > countLines(value) {
		nextLine = 0
	}
	return excerpt, nextLine > 0, nextLine, 0
}

func queryLines(value, query string, offset, maxLines, limit int) (excerpt string, more bool, nextOffset int) {
	if offset >= len(value) {
		return "", false, 0
	}
	offset = validUTF8Start(value, offset)
	var builder strings.Builder
	cursor := offset
	matches := 0
	for _, line := range strings.SplitAfter(value[offset:], "\n") {
		lineStart := cursor
		cursor += len(line)
		if strings.Contains(line, query) {
			remaining := limit - builder.Len()
			part, lineMore := boundedSlice(line, 0, remaining)
			builder.WriteString(part)
			matches++
			if lineMore {
				return builder.String(), true, lineStart + len(part)
			}
			if maxLines > 0 && matches >= maxLines {
				return builder.String(), cursor < len(value), cursor
			}
			if builder.Len() >= limit {
				return builder.String(), cursor < len(value), cursor
			}
		}
	}
	return builder.String(), false, 0
}
