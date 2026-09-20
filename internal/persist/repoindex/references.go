package repoindex

import (
	"context"
	"fmt"
	"path"
	"sort"

	"github.com/fwtllh-png/QCode/internal/platform/symbols"
)

// ReferenceRelation is a candidate backed by an occurrence in a specific
// source digest. It never represents inferred receiver types or dynamic calls.
type ReferenceRelation struct {
	Source       string                `json:"source"`
	SourceDigest string                `json:"source_digest"`
	Destination  string                `json:"destination"`
	Site         symbols.ReferenceSite `json:"site"`
}

func (s *Store) ReferenceSites(ctx context.Context) (map[string][]symbols.ReferenceSite, error) {
	return s.referenceSites(ctx, nil)
}

// ReferenceSitesFor reads the sites of the given sources only, in the same
// deterministic order as the full read.
func (s *Store) ReferenceSitesFor(
	ctx context.Context,
	paths []string,
) (map[string][]symbols.ReferenceSite, error) {
	only := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		only[path] = struct{}{}
	}
	return s.referenceSites(ctx, only)
}

func (s *Store) referenceSites(
	ctx context.Context,
	only map[string]struct{},
) (map[string][]symbols.ReferenceSite, error) {
	query := `SELECT path,name,target,module,kind,scope,start_byte,end_byte,line FROM repo_index_reference_sites WHERE root_path=?`
	args := []any{s.root}
	if len(only) != 0 {
		paths := make([]string, 0, len(only))
		for path := range only {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		query += ` AND path IN (` + placeholders(len(paths)) + `)`
		args = append(args, stringsToAny(paths)...)
	}
	query += ` ORDER BY path,position`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string][]symbols.ReferenceSite{}
	for rows.Next() {
		var file string
		var site symbols.ReferenceSite
		if err := rows.Scan(&file, &site.Name, &site.Target, &site.Module, &site.Kind, &site.Scope, &site.StartByte, &site.EndByte, &site.Line); err != nil {
			return nil, err
		}
		result[file] = append(result[file], site)
	}
	return result, rows.Err()
}

// ReferenceRelations uses package boundaries and explicit import bindings to
// narrow declarations. Module path resolution retains the index's existing
// candidate semantics; aliases configured by build systems are not guessed.
func (s *Store) ReferenceRelations(ctx context.Context) ([]ReferenceRelation, error) {
	return s.referenceRelations(ctx, nil)
}

// ReferenceRelationsFor resolves only the given sources' sites; a nil set
// resolves every scoped file, which is the full rebuild's shape.
func (s *Store) ReferenceRelationsFor(
	ctx context.Context,
	only map[string]struct{},
) ([]ReferenceRelation, error) {
	return s.referenceRelations(ctx, only)
}

func (s *Store) referenceRelations(
	ctx context.Context,
	only map[string]struct{},
) ([]ReferenceRelation, error) {
	files, err := s.Files(ctx)
	if err != nil {
		return nil, err
	}
	sites, err := s.referenceSites(ctx, only)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path,name,exported FROM repo_index_symbols WHERE root_path=? AND container=''`, s.root)
	if err != nil {
		return nil, err
	}
	declarations := map[string]map[string]bool{}
	for rows.Next() {
		var file, name string
		var exported bool
		if err := rows.Scan(&file, &name, &exported); err != nil {
			rows.Close()
			return nil, err
		}
		if declarations[file] == nil {
			declarations[file] = map[string]bool{}
		}
		declarations[file][name] = exported
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	byDirectory := map[string][]string{}
	byPackage := map[string][]string{}
	for name, file := range files {
		if file.Language == "go" {
			byDirectory[path.Dir(name)] = append(byDirectory[path.Dir(name)], name)
			key := path.Dir(name) + "\x00" + file.PackageName
			byPackage[key] = append(byPackage[key], name)
		}
	}
	importedCandidates := map[string]map[string]bool{}
	var result []ReferenceRelation
	for _, source := range sortedFileKeys(files) {
		if len(only) != 0 {
			if _, keep := only[source]; !keep {
				continue
			}
		}
		file := files[source]
		for _, site := range sites[source] {
			candidates := map[string]bool{}
			switch site.Kind {
			case symbols.ReferencePackage:
				if file.Language != "go" {
					continue
				}
				for _, destination := range byPackage[path.Dir(source)+"\x00"+file.PackageName] {
					candidates[destination] = true
				}
			case symbols.ReferenceImport:
				key := file.Language + "\x00" + path.Dir(source) + "\x00" + site.Module
				if cached, ok := importedCandidates[key]; ok {
					candidates = cached
				} else {
					resolveImport(file.Language, site.Module, source, func(candidate string) {
						if _, ok := files[candidate]; ok {
							candidates[candidate] = true
							return
						}
						if file.Language == "go" {
							for _, destination := range byDirectory[candidate] {
								if !isExternalGoTest(files[destination].PackageName) {
									candidates[destination] = true
								}
							}
						}
					})
					importedCandidates[key] = candidates
				}
			default:
				continue
			}
			for destination := range candidates {
				if destination == source {
					continue
				}
				names := declarations[destination]
				exported, exists := names[site.Target]
				if site.Kind == symbols.ReferenceImport && site.Target == "*" {
					for _, value := range names {
						if value {
							exists, exported = true, true
							break
						}
					}
				}
				if !exists || (site.Kind == symbols.ReferenceImport && !exported) {
					continue
				}
				result = append(result, ReferenceRelation{Source: source, SourceDigest: file.Digest, Destination: destination, Site: site})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Site.StartByte != b.Site.StartByte {
			return a.Site.StartByte < b.Site.StartByte
		}
		return a.Destination < b.Destination
	})
	return result, nil
}
func isExternalGoTest(name string) bool { return len(name) > 5 && name[len(name)-5:] == "_test" }

func (s *Store) scopedReferenceEdges(ctx context.Context) ([]graphEdge, error) {
	return s.scopedReferenceEdgesFrom(ctx, nil)
}

// scopedReferenceEdgesFrom restricts the scoped resolution to the given
// sources; a nil set means every file.
func (s *Store) scopedReferenceEdgesFrom(
	ctx context.Context,
	only map[string]struct{},
) ([]graphEdge, error) {
	relations, err := s.referenceRelations(ctx, only)
	if err != nil {
		return nil, fmt.Errorf("resolve scoped references: %w", err)
	}
	type key struct{ source, destination, kind string }
	counts := map[key]float64{}
	for _, r := range relations {
		counts[key{r.Source, r.Destination, r.Site.Kind}]++
	}
	var edges []graphEdge
	for k, count := range counts {
		edges = append(edges, graphEdge{Src: k.source, Dst: k.destination, Kind: k.kind, Weight: count})
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		if a.Dst != b.Dst {
			return a.Dst < b.Dst
		}
		return a.Kind < b.Kind
	})
	return edges, nil
}

// ReferenceEvidence reads the derived evidence under the refresh lock so the
// file digests, relations and declarations belong to one index generation.
func (i *Index) ReferenceEvidence(ctx context.Context, name string) (map[string]File, []ReferenceRelation, []Symbol, Snapshot, error) {
	snapshot, err := i.Ensure(ctx)
	if err != nil || !snapshot.Ready() {
		return nil, nil, nil, snapshot, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	snapshot = i.snapshot
	if !snapshot.Ready() {
		return nil, nil, nil, snapshot, nil
	}
	files, err := i.store.Files(ctx)
	if err != nil {
		return nil, nil, nil, snapshot, err
	}
	relations, err := i.store.ReferenceRelations(ctx)
	if err != nil {
		return nil, nil, nil, snapshot, err
	}
	declarations, err := i.store.Symbols(ctx, Query{Name: name, Exact: true, Limit: max(1, snapshot.Meta.SymbolCount)})
	return files, relations, declarations, snapshot, err
}
