// Package textdiff renders unified diffs and line statistics for text content.
//
// It exists so previews, receipts and hosts share one diff implementation that
// works without git: shelling out to `git diff` is unavailable in a workspace
// that is not a repository, and each caller counting lines for itself produced
// numbers that disagreed.
package textdiff

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// ErrBinary reports content a text diff cannot represent.
var ErrBinary = errors.New("binary content cannot be diffed")

// DefaultContext is the number of unchanged lines kept around each hunk.
const DefaultContext = 3

// maxCells bounds the LCS table after the common prefix and suffix are trimmed.
// Beyond it the changed middle is rendered as a delete-then-add block: still a
// correct diff, just not a minimal one. The bound keeps a pathological pair of
// files from allocating gigabytes.
const maxCells = 4 << 20

// Content is one side of a diff. Missing distinguishes an absent file (rendered
// as /dev/null) from an existing empty one.
type Content struct {
	Data    []byte
	Missing bool
}

// Text returns the content of an existing file.
func Text(data []byte) Content { return Content{Data: data} }

// Absent returns the content of a path that does not exist.
func Absent() Content { return Content{Missing: true} }

// Stats counts the lines a change added and removed.
type Stats struct {
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

// Unified renders a unified diff for path with contextLines of context, plus the
// line statistics. Identical sides yield an empty diff and zero stats, including
// when only one side is Missing: creating an empty file changes no line.
func Unified(path string, before, after Content, contextLines int) (string, Stats, error) {
	if err := rejectBinary(before, after); err != nil {
		return "", Stats{}, err
	}
	if contextLines < 0 {
		contextLines = DefaultContext
	}
	beforeLines, afterLines := splitLines(before.Data), splitLines(after.Data)
	script := diffLines(beforeLines, afterLines)
	stats := countStats(script)
	if stats.Added == 0 && stats.Removed == 0 {
		return "", stats, nil
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "--- %s\n", side("a/", path, before.Missing))
	fmt.Fprintf(&builder, "+++ %s\n", side("b/", path, after.Missing))
	for _, current := range group(script, contextLines) {
		writeHunk(&builder, current, beforeLines, afterLines)
	}
	return builder.String(), stats, nil
}

// Count returns only the line statistics, skipping diff rendering.
func Count(before, after Content) (Stats, error) {
	if err := rejectBinary(before, after); err != nil {
		return Stats{}, err
	}
	return countStats(diffLines(splitLines(before.Data), splitLines(after.Data))), nil
}

func rejectBinary(before, after Content) error {
	for _, content := range []Content{before, after} {
		if !content.Missing && bytes.IndexByte(content.Data, 0) >= 0 {
			return ErrBinary
		}
	}
	return nil
}

func side(prefix, path string, missing bool) string {
	if missing {
		return "/dev/null"
	}
	return prefix + path
}

// splitLines keeps each line's terminator, so a file that ends without a newline
// compares unequal to the same text that ends with one. Empty content has no
// lines at all.
func splitLines(data []byte) []string {
	text := string(data)
	var lines []string
	for text != "" {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			return append(lines, text)
		}
		lines = append(lines, text[:index+1])
		text = text[index+1:]
	}
	return lines
}

type opKind byte

const (
	opEqual  opKind = ' '
	opRemove opKind = '-'
	opAdd    opKind = '+'
)

// op is one line of the edit script. before and after are 0-based indices into
// the respective line slices, and -1 on the side where the line is absent.
type op struct {
	kind   opKind
	before int
	after  int
}

func countStats(script []op) Stats {
	var stats Stats
	for _, item := range script {
		switch item.kind {
		case opAdd:
			stats.Added++
		case opRemove:
			stats.Removed++
		}
	}
	return stats
}

// diffLines builds an edit script, trimming the common prefix and suffix before
// running the quadratic LCS over what is left.
func diffLines(before, after []string) []op {
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix &&
		before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}
	script := make([]op, 0, len(before)+len(after))
	for index := range prefix {
		script = append(script, op{kind: opEqual, before: index, after: index})
	}
	script = append(script, middle(
		before[prefix:len(before)-suffix], after[prefix:len(after)-suffix], prefix,
	)...)
	for index := range suffix {
		script = append(script, op{
			kind:   opEqual,
			before: len(before) - suffix + index,
			after:  len(after) - suffix + index,
		})
	}
	return script
}

func middle(before, after []string, offset int) []op {
	switch {
	case len(before) == 0 && len(after) == 0:
		return nil
	case len(before) == 0 || len(after) == 0 ||
		(len(before)+1)*(len(after)+1) > maxCells:
		return replaceAll(before, after, offset)
	default:
		return lcsScript(before, after, offset)
	}
}

func replaceAll(before, after []string, offset int) []op {
	script := make([]op, 0, len(before)+len(after))
	for index := range before {
		script = append(script, op{kind: opRemove, before: offset + index, after: -1})
	}
	for index := range after {
		script = append(script, op{kind: opAdd, before: -1, after: offset + index})
	}
	return script
}

// lcsScript walks a longest-common-subsequence table, emitting removals before
// additions so a replaced line reads as -old followed by +new.
func lcsScript(before, after []string, offset int) []op {
	columns := len(after) + 1
	table := make([]int32, (len(before)+1)*columns)
	for row := len(before) - 1; row >= 0; row-- {
		for column := len(after) - 1; column >= 0; column-- {
			if before[row] == after[column] {
				table[row*columns+column] = table[(row+1)*columns+column+1] + 1
				continue
			}
			table[row*columns+column] = max(
				table[(row+1)*columns+column], table[row*columns+column+1],
			)
		}
	}
	script := make([]op, 0, len(before)+len(after))
	row, column := 0, 0
	for row < len(before) && column < len(after) {
		switch {
		case before[row] == after[column]:
			script = append(script, op{
				kind: opEqual, before: offset + row, after: offset + column,
			})
			row, column = row+1, column+1
		case table[(row+1)*columns+column] >= table[row*columns+column+1]:
			script = append(script, op{kind: opRemove, before: offset + row, after: -1})
			row++
		default:
			script = append(script, op{kind: opAdd, before: -1, after: offset + column})
			column++
		}
	}
	for ; row < len(before); row++ {
		script = append(script, op{kind: opRemove, before: offset + row, after: -1})
	}
	for ; column < len(after); column++ {
		script = append(script, op{kind: opAdd, before: -1, after: offset + column})
	}
	return script
}

// group slices the edit script into hunks, keeping contextLines unchanged lines
// on each side and merging hunks that would otherwise share context.
func group(script []op, contextLines int) [][]op {
	var hunks [][]op
	index := 0
	for index < len(script) {
		if script[index].kind == opEqual {
			index++
			continue
		}
		start := max(index-contextLines, 0)
		end := index
		for end < len(script) {
			if script[end].kind != opEqual {
				end++
				continue
			}
			run := end
			for run < len(script) && script[run].kind == opEqual {
				run++
			}
			// A run of unchanged lines longer than twice the context ends the
			// hunk; a shorter one is cheaper to keep inside it.
			if run-end > 2*contextLines || run == len(script) {
				end = min(end+contextLines, len(script))
				break
			}
			end = run
		}
		hunks = append(hunks, script[start:end])
		index = end
	}
	return hunks
}

func writeHunk(builder *strings.Builder, ops []op, beforeLines, afterLines []string) {
	beforeStart, beforeCount := 0, 0
	afterStart, afterCount := 0, 0
	for _, item := range ops {
		if item.before >= 0 {
			if beforeCount == 0 {
				beforeStart = item.before + 1
			}
			beforeCount++
		}
		if item.after >= 0 {
			if afterCount == 0 {
				afterStart = item.after + 1
			}
			afterCount++
		}
	}
	fmt.Fprintf(
		builder, "@@ -%s +%s @@\n",
		formatRange(beforeStart, beforeCount), formatRange(afterStart, afterCount),
	)
	for _, item := range ops {
		if item.kind == opAdd {
			writeLine(builder, opAdd, afterLines[item.after])
			continue
		}
		writeLine(builder, item.kind, beforeLines[item.before])
	}
}

func writeLine(builder *strings.Builder, kind opKind, line string) {
	builder.WriteByte(byte(kind))
	if text, found := strings.CutSuffix(line, "\n"); found {
		builder.WriteString(text)
		builder.WriteByte('\n')
		return
	}
	builder.WriteString(line)
	builder.WriteString("\n\\ No newline at end of file\n")
}

func formatRange(start, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprintf("%d", start)
	default:
		return fmt.Sprintf("%d,%d", start, count)
	}
}
