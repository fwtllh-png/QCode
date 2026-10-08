package agentcontext

import (
	"fmt"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// IndexConversationAnswer parses Markdown structure only. It does not infer
// tasks, progress, verification, or execution authority from answer wording.
func IndexConversationAnswer(threadID, turnID string, turn uint64, answer string) ConversationSource {
	source := ConversationSource{ThreadID: threadID, TurnID: turnID, Turn: turn,
		MessageID: turnID + ":final", Text: answer}
	source.ContentDigest = digestString(source.Text)
	source.ID = source.identity()
	data := []byte(source.Text)
	blocks := parser.DefaultBlockParsers()
	for i := range blocks {
		blocks[i].Value = &sourceBlockParser{BlockParser: blocks[i].Value.(parser.BlockParser)}
	}
	markdown := parser.NewParser(parser.WithBlockParsers(blocks...), parser.WithInlineParsers(parser.DefaultInlineParsers()...), parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...))
	document := markdown.Parse(text.NewReader(data))
	parents := make(map[ast.Node]string)
	incomplete := false
	ordinals := make(map[*ast.List]int)
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		kind, label := "", ""
		switch value := node.(type) {
		case *ast.Heading:
			kind, label = "heading", string(value.Text(data))
			if source.Title == "" {
				source.Title = label
			}
		case *ast.ListItem:
			kind = "list_item"
			list := value.Parent().(*ast.List)
			ordinal := list.Start + ordinals[list]
			ordinals[list]++
			if list.IsOrdered() {
				label = fmt.Sprint(ordinal)
			} else {
				label = string(list.Marker)
			}
		default:
			return ast.WalkContinue, nil
		}
		source.Structured = true
		start := markdownBlockStart(node, data)
		if start < 0 {
			incomplete = true
			return ast.WalkContinue, nil
		}
		end := markdownBlockEnd(node, data)
		if kind == "heading" {
			// A heading references its complete section, including code and lists.
			heading := node.(*ast.Heading)
			end = len(data)
			for sibling := node.NextSibling(); sibling != nil; sibling = sibling.NextSibling() {
				if next, ok := sibling.(*ast.Heading); ok && next.Level <= heading.Level {
					end = markdownBlockStart(next, data)
					break
				}
			}
		}
		if end <= start || end > len(data) {
			incomplete = true
			return ast.WalkContinue, nil
		}
		if list, ok := node.Parent().(*ast.List); ok && list.IsOrdered() {
			// AST determines that this is an ordered item. Retain its literal source
			// marker rather than renumbering it when Markdown renderer normalizes lists.
			prefix := strings.TrimLeft(string(data[start:end]), " \t>")
			digits := 0
			for digits < len(prefix) && prefix[digits] >= '0' && prefix[digits] <= '9' {
				digits++
			}
			if digits > 0 && digits < len(prefix) && prefix[digits] == list.Marker {
				label = prefix[:digits]
			}
		}
		item := ReferenceItem{Kind: kind, Label: label, Start: start, End: end}
		for parent := node.Parent(); parent != nil; parent = parent.Parent() {
			if id := parents[parent]; id != "" {
				item.ParentID = id
				break
			}
		}
		item.ID = referenceItemID(source.ID, item)
		parents[node] = item.ID
		source.Items = append(source.Items, item)
		source.Structured = true
		return ast.WalkContinue, nil
	})
	// Headings are sibling blocks in CommonMark. Add section ancestry by
	// containment after the AST walk, keeping closer list parents intact.
	for i := range source.Items {
		item := &source.Items[i]
		if item.ParentID != "" {
			continue
		}
		closest := -1
		for j, parent := range source.Items {
			if parent.Kind == "heading" && parent.Start < item.Start && parent.End >= item.End && (closest < 0 || parent.Start > source.Items[closest].Start) {
				closest = j
			}
		}
		if closest >= 0 {
			item.ParentID = source.Items[closest].ID
		}
	}
	// Unsupported or ambiguous source ranges retain a complete original block.
	candidate := &ConversationState{Sources: map[string]ConversationSource{source.ID: source}}
	if err := candidate.Validate(); err != nil || incomplete {
		source.Items = nil
	}
	if len(source.Items) == 0 && len(data) != 0 {
		item := ReferenceItem{Kind: "text", Start: 0, End: len(data)}
		item.ID = referenceItemID(source.ID, item)
		source.Items = []ReferenceItem{item}
	}
	return source
}

func markdownBlockStart(node ast.Node, source []byte) int {
	start := -1
	if value, ok := node.AttributeString("qcode_source_start"); ok {
		start = value.(int)
	}
	_ = ast.Walk(node, func(child ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && child.Type() == ast.TypeBlock && child.Lines().Len() > 0 {
			segment := child.Lines().At(0)
			if start < 0 || segment.Start < start {
				start = segment.Start
			}
		}
		return ast.WalkContinue, nil
	})
	if start < 0 {
		return -1
	}
	for start > 0 && source[start-1] != '\n' {
		start--
	}
	return start
}

func markdownBlockEnd(node ast.Node, source []byte) int {
	for current := node; current != nil; current = current.Parent() {
		for next := current.NextSibling(); next != nil; next = next.NextSibling() {
			if start := markdownBlockStart(next, source); start >= 0 {
				return start
			}
		}
	}
	return len(source)
}

// Record block openings while the parser still knows delimiters. AST Lines
// alone excludes fences and empty items, so it cannot delimit source ranges.
type sourceBlockParser struct{ parser.BlockParser }

func (p *sourceBlockParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, segment := reader.PeekLine()
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node != nil {
		node.SetAttributeString("qcode_source_start", segment.Start)
	}
	return node, state
}
