package agentcontext

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// Offsets address the complete, canonical textual projection of one message.
// Parents retain heading/list identities and literal labels across splitting.
type NarrativeSourceRange struct {
	MessageID string          `json:"message_id"`
	Digest    string          `json:"digest"`
	Bytes     int             `json:"bytes"`
	Start     int             `json:"start"`
	End       int             `json:"end"`
	Parents   []ReferenceItem `json:"parents,omitempty"`
}

type NarrativeCoverage struct {
	ID     string               `json:"id"`
	Digest string               `json:"digest"`
	Source NarrativeSourceRange `json:"source"`
}

func narrativeSourceExcerpts(id string, role provider.Role, turn uint64, body string, ceiling int) []NarrativeExcerpt {
	data := []byte(body)
	blocks := parser.DefaultBlockParsers()
	for i := range blocks {
		blocks[i].Value = &sourceBlockParser{BlockParser: blocks[i].Value.(parser.BlockParser)}
	}
	doc := parser.NewParser(parser.WithBlockParsers(blocks...)).Parse(text.NewReader(data))
	boundaries := map[int]bool{0: true, len(data): true}
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && node.Type() == ast.TypeBlock {
			if start := markdownBlockStart(node, data); start >= 0 {
				boundaries[start] = true
			}
		}
		return ast.WalkContinue, nil
	})
	positions := make([]int, 0, len(boundaries))
	for position := range boundaries {
		positions = append(positions, position)
	}
	sort.Ints(positions)
	structure := IndexConversationAnswer(id, id, turn, body)
	var excerpts []NarrativeExcerpt
	for i := 1; i < len(positions); i++ {
		start, end := positions[i-1], positions[i]
		for start < end {
			stop := end
			if ceiling > 0 && stop-start > ceiling {
				stop = start + len(utf8Prefix(body[start:end], ceiling))
				if stop == start {
					_, size := utf8.DecodeRuneInString(body[start:end])
					stop = start + size
				}
			}
			source := NarrativeSourceRange{MessageID: id, Digest: digestString(body), Bytes: len(body), Start: start, End: stop}
			for _, item := range structure.Items {
				if item.Start <= start && item.End >= stop {
					source.Parents = append(source.Parents, item)
				}
			}
			excerpts = append(excerpts, newNarrativeExcerpt(role, turn, body[start:stop], source))
			start = stop
		}
	}
	return excerpts
}

func newNarrativeExcerpt(role provider.Role, turn uint64, body string, source NarrativeSourceRange) NarrativeExcerpt {
	id := source.MessageID
	if source.Start != 0 || source.End != source.Bytes {
		id += fmt.Sprintf(":%d:%d", source.Start, source.End)
	}
	return NarrativeExcerpt{MessageID: id, Role: role, Turn: turn, Text: body, Digest: digestString(body), Source: &source}
}

func validateNarrativeRange(excerpt NarrativeExcerpt) error {
	if excerpt.Truncated {
		return errors.New("truncated narrative source is not complete")
	}
	if s := excerpt.Source; s != nil {
		if s.MessageID == "" || s.Digest == "" || s.Start < 0 || s.End > s.Bytes || s.End-s.Start != len(excerpt.Text) || s.End <= s.Start {
			return errors.New("narrative source range is invalid")
		}
		if newNarrativeExcerpt(excerpt.Role, excerpt.Turn, excerpt.Text, *s).MessageID != excerpt.MessageID {
			return errors.New("narrative range identity is invalid")
		}
	}
	return nil
}

// NarrativeInputPart never discards data: callers retain the remaining ranges.
func NarrativeInputPart(input NarrativeInputArtifact, excerpts []NarrativeExcerpt) NarrativeInputArtifact {
	input.CompleteSources = false
	input.Excerpts = cloneNarrativeExcerpts(excerpts)
	input.Digest = input.digest()
	return input
}

func SplitNarrativeExcerpt(excerpt NarrativeExcerpt) ([]NarrativeExcerpt, error) {
	if excerpt.Source == nil {
		return nil, errors.New("narrative source range is required for splitting")
	}
	middle := len(utf8Prefix(excerpt.Text, len(excerpt.Text)/2))
	if middle == 0 {
		return nil, errors.New("narrative source metadata exceeds request budget")
	}
	left, right := *excerpt.Source, *excerpt.Source
	left.End = left.Start + middle
	right.Start = left.End
	return []NarrativeExcerpt{newNarrativeExcerpt(excerpt.Role, excerpt.Turn, excerpt.Text[:middle], left), newNarrativeExcerpt(excerpt.Role, excerpt.Turn, excerpt.Text[middle:], right)}, nil
}

func narrativeCoverage(input NarrativeInputArtifact, items []NarrativeItem) ([]NarrativeCoverage, error) {
	covered := map[string]bool{}
	for _, item := range items {
		for _, id := range item.SourceMessageIDs {
			covered[id] = true
		}
	}
	var coverage []NarrativeCoverage
	for _, excerpt := range input.Excerpts {
		if !covered[excerpt.MessageID] {
			return nil, fmt.Errorf("narrative output leaves source range %q uncovered", excerpt.MessageID)
		}
		if excerpt.Source != nil {
			coverage = append(coverage, NarrativeCoverage{ID: excerpt.MessageID, Digest: excerpt.Digest, Source: *excerpt.Source})
		}
	}
	return coverage, nil
}

// MergeNarrativeParts verifies exact range coverage against the complete input.
// It does not treat citations as proof of semantic fidelity.
func MergeNarrativeParts(input NarrativeInputArtifact, parts []NarrativeArtifact, now time.Time) (NarrativeArtifact, error) {
	artifact := NarrativeArtifact{Version: NarrativeSchemaVersion, ThreadID: input.ThreadID, WindowID: input.SourceWindowID, AuthorityDigest: input.AuthorityDigest, InputDigest: input.Digest, RouteDigest: input.RouteDigest, CreatedAt: now, ExpiresAt: input.ExpiresAt}
	for _, part := range parts {
		if err := part.Validate(now); err != nil {
			return artifact, err
		}
		if part.ThreadID != input.ThreadID || part.RouteDigest != input.RouteDigest || part.AuthorityDigest != input.AuthorityDigest {
			return artifact, errors.New("narrative part identity mismatch")
		}
		artifact.Body.Items = append(artifact.Body.Items, part.Body.Items...)
		artifact.Coverage = append(artifact.Coverage, part.Coverage...)
	}
	for _, excerpt := range input.Excerpts {
		if excerpt.Source == nil {
			continue
		}
		s := excerpt.Source
		cursor := s.Start
		for _, coverage := range artifact.Coverage {
			c := coverage.Source
			if c.MessageID != s.MessageID || c.Start < s.Start || c.End > s.End {
				continue
			}
			if c.Digest != s.Digest || c.Start != cursor || coverage.Digest != digestString(excerpt.Text[c.Start-s.Start:c.End-s.Start]) {
				return artifact, errors.New("narrative range coverage mismatch")
			}
			cursor = c.End
		}
		if cursor != s.End {
			return artifact, errors.New("narrative range coverage is incomplete")
		}
	}
	for _, kind := range input.RequiredKinds {
		found := false
		for _, item := range artifact.Body.Items {
			if item.Kind == kind {
				found = true
				break
			}
		}
		if !found {
			return artifact, fmt.Errorf("narrative output is missing required %q context", kind)
		}
	}
	artifact.Digest = artifact.digest()
	return artifact, artifact.Validate(now)
}

func validateCompleteNarrativeSources(input NarrativeInputArtifact) error {
	if !input.CompleteSources {
		return nil
	}
	type sourceBody struct {
		body   strings.Builder
		size   int
		digest string
	}
	sources := map[string]*sourceBody{}
	for _, excerpt := range input.Excerpts {
		s := excerpt.Source
		if s == nil {
			return errors.New("complete narrative input has no source range")
		}
		body := sources[s.MessageID]
		if body == nil {
			body = &sourceBody{size: s.Bytes, digest: s.Digest}
			sources[s.MessageID] = body
		}
		if body.body.Len() != s.Start || body.size != s.Bytes || body.digest != s.Digest {
			return errors.New("narrative source ranges overlap or have gaps")
		}
		body.body.WriteString(excerpt.Text)
	}
	for _, body := range sources {
		if body.body.Len() != body.size || digestString(body.body.String()) != body.digest {
			return errors.New("narrative source is incomplete or changed")
		}
	}
	return nil
}

func validateNarrativeArtifactCoverage(artifact NarrativeArtifact) error {
	if len(artifact.Coverage) == 0 {
		return nil
	}
	covered := map[string]string{}
	for _, c := range artifact.Coverage {
		s := c.Source
		id := s.MessageID
		if s.Start != 0 || s.End != s.Bytes {
			id += fmt.Sprintf(":%d:%d", s.Start, s.End)
		}
		if c.ID != id || c.Digest == "" || s.Digest == "" || s.Start < 0 || s.End <= s.Start || s.End > s.Bytes || covered[c.ID] != "" {
			return errors.New("narrative coverage metadata is invalid")
		}
		covered[c.ID] = c.Digest
	}
	used := map[string]bool{}
	for _, item := range artifact.Body.Items {
		var digests []string
		for _, id := range item.SourceMessageIDs {
			digest, ok := covered[id]
			if !ok {
				return errors.New("narrative item has no covered source")
			}
			digests = append(digests, digest)
			used[id] = true
		}
		if item.SourceDigest != digestString(strings.Join(digests, "\x00")) {
			return errors.New("narrative item source digest differs from coverage")
		}
	}
	if len(used) != len(covered) {
		return errors.New("narrative coverage includes an uncited range")
	}
	return nil
}

// MergeNarrativeRepresentation appends disjoint source representations only.
// Authority, Plan, and conversation selection are deliberately absent here.
func MergeNarrativeRepresentation(previous *NarrativeArtifact, next NarrativeArtifact) NarrativeArtifact {
	if previous != nil && previous.ThreadID == next.ThreadID && previous.RouteDigest == next.RouteDigest {
		// Explicit compaction may regenerate a source. Replace only items
		// derived from that source; never concatenate duplicate coverage.
		replaced := map[string]bool{}
		for _, c := range next.Coverage {
			replaced[c.Source.MessageID] = true
		}
		oldSources := map[string]string{}
		for _, c := range previous.Coverage {
			oldSources[c.ID] = c.Source.MessageID
		}
		retained := map[string]bool{}
		var items []NarrativeItem
		for _, item := range previous.Body.Items {
			keep := true
			for _, id := range item.SourceMessageIDs {
				if replaced[oldSources[id]] {
					keep = false
					break
				}
			}
			if keep {
				items = append(items, item)
				for _, id := range item.SourceMessageIDs {
					retained[id] = true
				}
			}
		}
		var coverage []NarrativeCoverage
		for _, c := range previous.Coverage {
			if retained[c.ID] {
				coverage = append(coverage, c)
			}
		}
		next.Body.Items = append(items, next.Body.Items...)
		next.Coverage = append(coverage, next.Coverage...)
	}
	next.Digest = next.digest()
	return next
}

func cloneNarrativeExcerpts(excerpts []NarrativeExcerpt) []NarrativeExcerpt {
	cloned := append([]NarrativeExcerpt(nil), excerpts...)
	for i := range cloned {
		if source := cloned[i].Source; source != nil {
			copy := *source
			copy.Parents = append([]ReferenceItem(nil), source.Parents...)
			cloned[i].Source = &copy
		}
	}
	return cloned
}
