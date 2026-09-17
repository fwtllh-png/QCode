package search

import (
	"context"
	"strings"

	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
)

type impactStepEvidence struct {
	repoindex.ImpactStep
	Text           string `json:"text,omitempty"`
	EvidenceStatus string `json:"evidence_status"`
}

type relatedTestEvidence struct {
	Path       string               `json:"path"`
	Hops       int                  `json:"hops,omitempty"`
	Via        string               `json:"via,omitempty"`
	Resolution string               `json:"resolution"`
	Reason     string               `json:"reason"`
	Chain      []impactStepEvidence `json:"chain,omitempty"`
}

type impactEvidenceReader struct {
	tool        *symbolTool
	content     map[string]repowalk.Content
	unavailable map[string]string
}

func newImpactEvidenceReader(t *symbolTool) *impactEvidenceReader {
	return &impactEvidenceReader{tool: t, content: map[string]repowalk.Content{}, unavailable: map[string]string{}}
}
func (r *impactEvidenceReader) rows(ctx context.Context, tests []repoindex.RelatedTest) ([]relatedTestEvidence, error) {
	rows := make([]relatedTestEvidence, 0, len(tests))
	for _, test := range tests {
		row := relatedTestEvidence{Path: test.Path, Hops: test.Hops, Via: test.Via, Resolution: test.Resolution, Reason: test.Reason}
		for _, step := range test.Chain {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			out := impactStepEvidence{ImpactStep: step, EvidenceStatus: "not_recorded"}
			if evidence := step.Evidence; evidence != nil {
				path := evidence.Source
				if _, ok := r.content[path]; !ok && r.unavailable[path] == "" {
					content, reason, err := r.tool.readReferenceFile(ctx, path)
					if err != nil {
						return nil, err
					}
					if reason != string(repowalk.SkipNone) {
						r.unavailable[path] = reason
					} else {
						r.content[path] = content
					}
				}
				content := r.content[path]
				site := evidence.Site
				switch {
				case r.unavailable[path] != "":
					out.EvidenceStatus = r.unavailable[path]
				case content.Digest != evidence.SourceDigest:
					out.EvidenceStatus = "stale_digest"
				case site.StartByte < 0 || site.EndByte < site.StartByte || site.EndByte > len(content.Data):
					out.EvidenceStatus = "invalid_range"
				case string(content.Data[site.StartByte:site.EndByte]) != site.Name:
					out.EvidenceStatus = "invalid_occurrence"
				default:
					lines := strings.Split(string(content.Data), "\n")
					if site.Line < 1 || site.Line > len(lines) {
						out.EvidenceStatus = "invalid_line"
					} else {
						out.Text = lines[site.Line-1]
						out.EvidenceStatus = "verified_digest"
					}
				}
				if out.EvidenceStatus != "verified_digest" {
					out.Evidence = nil
				}
			}
			row.Chain = append(row.Chain, out)
		}
		rows = append(rows, row)
	}
	return rows, nil
}
