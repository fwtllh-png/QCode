// Package workspacequery provides read-only workspace queries for the
// application: browsing, search, resources and Git inspection. Mutations are
// owned by the Runtime's GitControl and guarded tools.
package workspacequery

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

const (
	defaultLimit       = 200
	maxLimit           = 1000
	maxQueryBytes      = 256
	maxResourceBytes   = 1 << 20
	maxImageBytes      = 5 << 20
	maxSearchFileBytes = 512 << 10
	maxSearchReadBytes = 8 << 20
	maxPreviewBytes    = 300
)

type Entry struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size,omitempty"`
}

type BrowseResult struct {
	Path    string  `json:"path"`
	Entries []Entry `json:"entries"`
	More    bool    `json:"more"`
}

type SearchMatch struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Preview string `json:"preview"`
}

type SearchResult struct {
	Query   string        `json:"query"`
	Matches []SearchMatch `json:"matches"`
	More    bool          `json:"more"`
}

type Resource struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Digest  string `json:"digest"`
	Bytes   int    `json:"bytes"`
}

type ImageResource struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Bytes     int    `json:"bytes"`
	MediaType string `json:"media_type"`
	Data      []byte `json:"-"`
}

type Service struct {
	workspace *sandbox.Workspace
	walker    *repowalk.Walker
}

func New(
	root string,
	backend sandbox.Backend,
) (*Service, error) {
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		return nil, err
	}
	walker, err := repowalk.New(workspace.Root(), backend)
	if err != nil {
		return nil, err
	}
	return &Service{
		workspace: workspace,
		walker:    walker,
	}, nil
}

func (s *Service) Browse(
	ctx context.Context,
	directory string,
	limit int,
) (BrowseResult, error) {
	limit, err := normalizeLimit(limit)
	if err != nil {
		return BrowseResult{}, err
	}
	directory, err = normalizeDirectory(directory)
	if err != nil {
		return BrowseResult{}, err
	}
	if _, err := s.workspace.ResolveDirectory(directory); err != nil {
		return BrowseResult{}, err
	}
	listing, err := s.list(ctx)
	if err != nil {
		return BrowseResult{}, err
	}
	prefix := ""
	if directory != "." {
		prefix = directory + "/"
	}
	entries := make(map[string]Entry)
	for _, file := range listing.Files {
		if !strings.HasPrefix(file.Path, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(file.Path, prefix)
		if remainder == "" {
			continue
		}
		name, rest, _ := strings.Cut(remainder, "/")
		entryPath := path.Join(prefix, name)
		if rest != "" {
			entries[entryPath] = Entry{Path: entryPath, Kind: "directory"}
			continue
		}
		entries[entryPath] = Entry{Path: entryPath, Kind: "file", Size: file.Size}
	}
	values := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		values = append(values, entry)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Kind != values[j].Kind {
			return values[i].Kind == "directory"
		}
		return values[i].Path < values[j].Path
	})
	more := len(values) > limit
	if more {
		values = values[:limit]
	}
	return BrowseResult{Path: directory, Entries: values, More: more}, nil
}

func (s *Service) Search(
	ctx context.Context,
	query string,
	limit int,
) (SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchResult{}, errors.New("workspace search query is required")
	}
	if len(query) > maxQueryBytes {
		return SearchResult{}, errors.New("workspace search query is too long")
	}
	limit, err := normalizeLimit(limit)
	if err != nil {
		return SearchResult{}, err
	}
	listing, err := s.list(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	needle := strings.ToLower(query)
	result := SearchResult{
		Query: query, Matches: make([]SearchMatch, 0, min(limit, 32)),
	}
	var readBytes int64
	for _, entry := range listing.Files {
		if err := ctx.Err(); err != nil {
			return SearchResult{}, err
		}
		if readBytes >= maxSearchReadBytes {
			result.More = true
			break
		}
		content, skipped, err := s.walker.Read(entry, maxSearchFileBytes)
		if err != nil {
			return SearchResult{}, err
		}
		if skipped != repowalk.SkipNone {
			continue
		}
		readBytes += int64(len(content.Data))
		scanner := bufio.NewScanner(strings.NewReader(string(content.Data)))
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			column := strings.Index(strings.ToLower(text), needle)
			if column < 0 {
				continue
			}
			result.Matches = append(result.Matches, SearchMatch{
				Path: entry.Path, Line: line, Column: column + 1,
				Preview: truncate(text, maxPreviewBytes),
			})
			if len(result.Matches) == limit {
				result.More = true
				return result, nil
			}
		}
		if err := scanner.Err(); err != nil {
			return SearchResult{}, err
		}
	}
	return result, nil
}

func (s *Service) Resource(ctx context.Context, name string) (Resource, error) {
	name, err := normalizeFile(name)
	if err != nil {
		return Resource{}, err
	}
	entry, err := s.fileEntry(ctx, name)
	if err != nil {
		return Resource{}, err
	}
	content, skipped, err := s.walker.Read(entry, maxResourceBytes)
	if err != nil {
		return Resource{}, err
	}
	switch skipped {
	case repowalk.SkipNone:
		return Resource{
			Path: content.Path, Content: string(content.Data),
			Digest: strings.TrimPrefix(content.Digest, "sha256:"),
			Bytes:  len(content.Data),
		}, nil
	case repowalk.SkipLarge:
		return Resource{}, errors.New("workspace resource exceeds byte limit")
	case repowalk.SkipBinary, repowalk.SkipEncoding:
		return Resource{}, errors.New("workspace resource is not UTF-8 text")
	default:
		return Resource{}, fs.ErrNotExist
	}
}

func (s *Service) Image(ctx context.Context, name string) (ImageResource, error) {
	name, err := normalizeFile(name)
	if err != nil {
		return ImageResource{}, err
	}
	entry, err := s.fileEntry(ctx, name)
	if err != nil {
		return ImageResource{}, err
	}
	if entry.Size > maxImageBytes {
		return ImageResource{}, errors.New("workspace image exceeds byte limit")
	}
	file, err := s.workspace.OpenFile(entry.Path)
	if err != nil {
		return ImageResource{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return ImageResource{}, readErr
	}
	if closeErr != nil {
		return ImageResource{}, closeErr
	}
	if len(data) > maxImageBytes {
		return ImageResource{}, errors.New("workspace image exceeds byte limit")
	}
	mediaType := imageMediaType(data)
	if mediaType == "" {
		return ImageResource{}, errors.New("workspace resource is not a supported image")
	}
	return ImageResource{
		Path: entry.Path, Digest: strings.TrimPrefix(repowalk.Digest(data), "sha256:"),
		Bytes: len(data), MediaType: mediaType, Data: data,
	}, nil
}

func (s *Service) fileEntry(ctx context.Context, name string) (repowalk.Entry, error) {
	listing, err := s.list(ctx)
	if err != nil {
		return repowalk.Entry{}, err
	}
	for _, entry := range listing.Files {
		if entry.Path == name {
			return entry, nil
		}
	}
	return repowalk.Entry{}, fs.ErrNotExist
}

func imageMediaType(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case len(data) >= 6 &&
		(bytes.Equal(data[:6], []byte("GIF87a")) ||
			bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif"
	case len(data) >= 12 &&
		string(data[:4]) == "RIFF" &&
		string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return ""
	}
}

func (s *Service) list(ctx context.Context) (repowalk.Listing, error) {
	listing, err := s.walker.List(ctx)
	if err != nil {
		return repowalk.Listing{}, err
	}
	if listing.Source != repowalk.SourceGit {
		return repowalk.Listing{}, errors.New(
			"workspace query requires git-backed enumeration",
		)
	}
	return listing, nil
}

func normalizeLimit(value int) (int, error) {
	if value == 0 {
		return defaultLimit, nil
	}
	if value < 1 || value > maxLimit {
		return 0, errors.New("limit must be between 1 and 1000")
	}
	return value, nil
}

func normalizeDirectory(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return ".", nil
	}
	clean := path.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return "", errors.New("workspace directory escapes the root")
	}
	return clean, nil
}

func normalizeFile(value string) (string, error) {
	value, err := normalizeDirectory(value)
	if err != nil {
		return "", err
	}
	if value == "." || repowalk.Skippable(value) {
		return "", fs.ErrNotExist
	}
	return value, nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}
