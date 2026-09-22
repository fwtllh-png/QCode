package skill

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	skillruntime "github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
)

const (
	skillsPerPage  = 20
	skillReadBytes = 32 << 10
)

type listInput struct {
	Cursor string `json:"cursor,omitempty"`
}

type readInput struct {
	Handle string `json:"handle"`
	Cursor string `json:"cursor,omitempty"`
}

type listTool struct {
	catalog *skillruntime.Catalog
}

type readTool struct {
	catalog *skillruntime.Catalog
}

type listedSkill struct {
	Handle         string              `json:"handle"`
	PackageHandle  string              `json:"package_handle"`
	ResourceHandle string              `json:"resource_handle"`
	Name           string              `json:"name"`
	Description    string              `json:"description"`
	Source         skillruntime.Source `json:"source"`
	Path           string              `json:"path"`
}

func RegisterDiscovery(
	registry *tool.Registry,
	catalog *skillruntime.Catalog,
) error {
	if registry == nil || catalog == nil {
		return errors.New("skill discovery tools require registry and catalog")
	}
	listExecutor, err := typed.Define(typed.Spec[listInput, tool.Result]{
		Descriptor:  listDescriptor(),
		Disposition: tool.DispositionAbortImmediately,
		Run: func(ctx context.Context, input listInput) (tool.Result, error) {
			return (&listTool{catalog: catalog}).run(ctx, input)
		},
		Encode: identityResult,
	})
	if err != nil {
		return err
	}
	readExecutor, err := typed.Define(typed.Spec[readInput, tool.Result]{
		Descriptor:  readDescriptor(),
		Disposition: tool.DispositionAbortImmediately,
		Run: func(ctx context.Context, input readInput) (tool.Result, error) {
			return (&readTool{catalog: catalog}).run(ctx, input)
		},
		Encode: identityResult,
	})
	if err != nil {
		return err
	}
	if err := registry.Register(listExecutor); err != nil {
		return err
	}
	return registry.Register(readExecutor)
}

func listDescriptor() tool.Descriptor {
	return tool.Descriptor{
		Name:        "skills_list",
		Description: "List authority-bound skills",
		Aliases:     []tool.Alias{{Name: "skills.list", Hidden: true}},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cursor": map[string]any{
					"type": "string", "maxLength": float64(256),
				},
			},
			"additionalProperties": false,
		},
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		RepeatPolicy: tool.RepeatExecute, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
	}
}

func readDescriptor() tool.Descriptor {
	properties := map[string]any{
		"handle": map[string]any{
			"type": "string", "minLength": float64(44), "maxLength": float64(44),
		},
	}
	properties["cursor"] = map[string]any{
		"type": "string", "maxLength": float64(256),
	}
	return tool.Descriptor{
		Name:        "skills_read",
		Description: "Read a skill by any advertised skill, package, or resource handle",
		Aliases:     []tool.Alias{{Name: "skills.read", Hidden: true}},
		InputSchema: map[string]any{
			"type": "object", "properties": properties,
			"required":             []string{"handle"},
			"additionalProperties": false,
		},
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "skill", Field: "handle", Access: tool.AccessRead,
		}}},
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		RepeatPolicy: tool.RepeatExecute, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
	}
}

func (t *listTool) run(
	ctx context.Context,
	input listInput,
) (tool.Result, error) {
	if input.Cursor == "" {
		if err := t.catalog.Refresh(ctx); err != nil {
			return tool.Result{}, err
		}
	}
	summaries, err := t.catalog.ListHandles(ctx)
	if err != nil {
		return tool.Result{}, err
	}
	visible := summaries[:0]
	for _, summary := range summaries {
		if summary.ModelInvocable {
			visible = append(visible, summary)
		}
	}
	summaries = visible
	digest := summaryPageDigest(summaries)
	start, err := decodePageCursor(input.Cursor, digest)
	if err != nil || start > len(summaries) {
		return tool.Result{}, errors.New("skills.list cursor is invalid or stale")
	}
	end := min(start+skillsPerPage, len(summaries))
	listed := make([]listedSkill, 0, end-start)
	for _, summary := range summaries[start:end] {
		listed = append(listed, listedSkill{
			Handle: summary.Handle, PackageHandle: summary.PackageHandle,
			ResourceHandle: summary.ResourceHandle, Name: summary.Name,
			Description: boundedDescription(summary.Description),
			Source:      summary.Source, Path: summary.Path,
		})
	}
	next := ""
	if end < len(summaries) {
		next = encodePageCursor(end, digest)
	}
	content, err := json.Marshal(map[string]any{
		"skills": listed, "next_cursor": next,
	})
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{
		Content: string(content),
		Metadata: map[string]any{
			"count": len(listed), "next_cursor": next,
			"catalog_digest": digest,
		},
	}, nil
}

func (t *readTool) run(
	ctx context.Context,
	input readInput,
) (tool.Result, error) {
	summary, err := t.catalog.SummaryForHandle(ctx, input.Handle)
	if err != nil {
		return tool.Result{}, recoverableSkillHandleError(err)
	}
	plan, err := t.catalog.LoadHandle(ctx, input.Handle)
	if err != nil {
		return tool.Result{}, recoverableSkillHandleError(err)
	}
	content := renderLoadedPlan(plan)
	digest := contentDigest(content)
	start, err := decodePageCursor(input.Cursor, digest)
	if err != nil || start > len(content) || !isUTF8Boundary(content, start) {
		return tool.Result{}, errors.New("skills.read cursor is invalid or stale")
	}
	end := min(start+skillReadBytes, len(content))
	for end < len(content) && !isUTF8Boundary(content, end) {
		end--
	}
	next := ""
	if end < len(content) {
		next = encodePageCursor(end, digest)
	}
	resolved := make([]skillruntime.ResolvedSkill, 0, len(plan))
	for _, item := range plan {
		resolved = append(resolved, skillruntime.ResolvedSkill{
			Name: item.Name, Version: item.Version, Source: item.Source,
			Path:   item.Path,
			Digest: item.Digest, Dependencies: item.Dependencies,
			Locked: item.Locked,
		})
	}
	return tool.Result{
		Content: content[start:end],
		Metadata: map[string]any{
			"name": summary.Name, "handle": summary.Handle,
			"path":            summary.Path,
			"package_handle":  summary.PackageHandle,
			"resource_handle": summary.ResourceHandle,
			"content_digest":  digest, "next_cursor": next,
			"resolved_skills": resolved,
		},
	}, nil
}

func renderLoadedPlan(plan []skillruntime.Loaded) string {
	if len(plan) == 0 {
		return ""
	}
	sections := make([]string, 0, len(plan))
	for index, item := range plan {
		role := "dependency"
		if index == len(plan)-1 {
			role = "root"
		}
		header := fmt.Sprintf("# Skill %s: %s@%s\nsource_path=%q",
			role, item.Name, item.Version, item.Path)
		if item.Source == skillruntime.SourceBuiltin {
			header += "\nEmbedded, self-contained skill; source_path is not a filesystem path."
		} else {
			header += fmt.Sprintf("\nresource_base=%q\n", filepath.Dir(item.Path)) +
				"Resolve this skill's relative references, scripts, and assets against resource_base. " +
				"Use file_read for resources inside the workspace or shell_read for permitted external paths; " +
				"normal policy and sandbox checks still apply."
		}
		sections = append(sections, header+"\n\n"+item.Content)
	}
	return strings.Join(sections, "\n\n")
}

func summaryPageDigest(values []skillruntime.Summary) string {
	var builder strings.Builder
	for _, value := range values {
		builder.WriteString(value.Handle)
		builder.WriteByte('\n')
	}
	return contentDigest(builder.String())
}

func contentDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func encodePageCursor(offset int, digest string) string {
	value := strconv.Itoa(offset) + ":" + digest
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodePageCursor(value, digest string) (int, error) {
	if value == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, err
	}
	offsetText, currentDigest, ok := strings.Cut(string(raw), ":")
	if !ok || currentDigest != digest {
		return 0, errors.New("cursor digest differs")
	}
	offset, err := strconv.Atoi(offsetText)
	if err != nil || offset < 0 {
		return 0, errors.New("cursor offset is invalid")
	}
	return offset, nil
}

func boundedDescription(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= 160 {
		return value
	}
	return string(runes[:157]) + "..."
}

func isUTF8Boundary(value string, index int) bool {
	return index >= 0 && index <= len(value) &&
		(index == len(value) || index == 0 || value[index]&0xc0 != 0x80)
}

func identityResult(value tool.Result) (tool.Result, error) {
	return value, nil
}
