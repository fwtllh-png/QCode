// Package repoindex stores what the runtime knows about the files and
// declarations of a workspace, so finding code does not start from a full text
// scan every time.
//
// The index is a cache and never a source of truth: it is keyed by content
// digest, it can be dropped and rebuilt at any point, and every consumer must
// keep working when it is empty or unavailable. Rows are keyed by the canonical
// workspace root rather than a workspaces(id) reference, because a session
// without a persistent store keeps its index in an ephemeral database that has
// no workspace rows.
package repoindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
	"strings"
	"time"
)

// File is one indexed file. Size and Modified together are the cheap hint that a
// file has not changed; Digest is what actually decides it. Rank is the file's
// PageRank from the reference graph — zero until a graph build has run, which
// is the map's signal to fall back to declaration counts. EntryPoint records
// the entry-point classification so every consumer of the index answers it the
// same way.
type File struct {
	ReferenceSitesTruncated bool    `json:"reference_sites_truncated"`
	ScopeAware              bool    `json:"scope_aware"`
	PackageName             string  `json:"package_name,omitempty"`
	Path                    string  `json:"path"`
	Language                string  `json:"language,omitempty"`
	Size                    int64   `json:"size"`
	Digest                  string  `json:"digest"`
	SymbolCount             int     `json:"symbol_count"`
	Rank                    float64 `json:"rank,omitempty"`
	EntryPoint              bool    `json:"entry_point,omitempty"`
	Modified                time.Time
	IndexedAt               time.Time
}

// Symbol is one declaration the extractor found. Line is 1-based. Signature
// and Docstring are the bounded declaration text and comment block; Resolution
// says which extraction tier produced the row, and consumers pass it on rather
// than present a heuristic row as a rule-table one.
type Symbol struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Container  string `json:"container,omitempty"`
	Line       int    `json:"line"`
	Exported   bool   `json:"exported,omitempty"`
	Signature  string `json:"signature,omitempty"`
	Docstring  string `json:"docstring,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

// Reference is one identifier a file uses, with how often. The rows are the
// raw material for reference edges; they say which names appear where, not
// what declared them.
type Reference struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Record is a file together with the declarations it holds.
type Record struct {
	ReferenceSites []symbols.ReferenceSite
	File           File
	Symbols        []Symbol
	References     []Reference
	// Imports are the raw import specifiers the extractor read; the graph
	// build resolves them once the file set is complete.
	Imports []string
}

// Meta describes the state of one root's index.
type Meta struct {
	IndexerVersion int       `json:"indexer_version"`
	Source         string    `json:"source,omitempty"`
	FileCount      int       `json:"file_count"`
	SymbolCount    int       `json:"symbol_count"`
	Truncated      bool      `json:"truncated,omitempty"`
	RefreshedAt    time.Time `json:"refreshed_at"`
}

// Query selects symbols. An empty Name matches every symbol, which is how a
// caller lists the declarations of specific paths.
type Query struct {
	// Name matches case-insensitively: exactly when Exact is set, otherwise as a
	// substring.
	Name  string
	Exact bool
	// Kinds restricts the result to these symbol kinds when non-empty.
	Kinds []string
	// Paths restricts the result to these files when non-empty.
	Paths []string
	// Limit bounds the returned rows. Zero selects DefaultQueryLimit.
	Limit int
}

// DefaultQueryLimit bounds a query that names no limit of its own.
const DefaultQueryLimit = 200

// maxQueryLimit bounds what a caller can ask for, so one tool call cannot pull
// the whole symbol table into memory.
const maxQueryLimit = 2000

// Store reads and writes the index rows of a single workspace root.
type Store struct {
	db   *sql.DB
	root string
}

// NewStore returns a store for root. The database must already carry the index
// schema; callers get that by opening the state database.
func NewStore(db *sql.DB, root string) (*Store, error) {
	if db == nil {
		return nil, errors.New("repository index requires a database handle")
	}
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("repository index requires a workspace root")
	}
	return &Store{db: db, root: root}, nil
}

// Root is the workspace the store indexes.
func (s *Store) Root() string { return s.root }

// Meta returns the recorded index state, reporting found=false before the first
// refresh.
func (s *Store) Meta(ctx context.Context) (Meta, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT indexer_version, source, file_count, symbol_count, truncated, refreshed_at
		FROM repo_index_meta WHERE root_path = ?`, s.root)
	var (
		meta        Meta
		truncated   int
		refreshedAt string
	)
	err := row.Scan(
		&meta.IndexerVersion, &meta.Source, &meta.FileCount,
		&meta.SymbolCount, &truncated, &refreshedAt,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Meta{}, false, nil
	case err != nil:
		return Meta{}, false, fmt.Errorf("read repository index meta: %w", err)
	}
	meta.Truncated = truncated != 0
	if meta.RefreshedAt, err = time.Parse(time.RFC3339Nano, refreshedAt); err != nil {
		return Meta{}, false, fmt.Errorf("read repository index meta: %w", err)
	}
	return meta, true, nil
}

// SetMeta records the index state after a refresh.
func (s *Store) SetMeta(ctx context.Context, meta Meta) error {
	truncated := 0
	if meta.Truncated {
		truncated = 1
	}
	if meta.RefreshedAt.IsZero() {
		meta.RefreshedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repo_index_meta(
			root_path, indexer_version, source, file_count, symbol_count, truncated, refreshed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(root_path) DO UPDATE SET
			indexer_version = excluded.indexer_version,
			source = excluded.source,
			file_count = excluded.file_count,
			symbol_count = excluded.symbol_count,
			truncated = excluded.truncated,
			refreshed_at = excluded.refreshed_at`,
		s.root, meta.IndexerVersion, meta.Source, meta.FileCount,
		meta.SymbolCount, truncated, timestamp(meta.RefreshedAt),
	)
	if err != nil {
		return fmt.Errorf("record repository index meta: %w", err)
	}
	return nil
}

// Files returns the indexed files by path, which is how an incremental refresh
// learns which digests it already holds. Each row carries its graph rank when
// one has been computed.
func (s *Store) Files(ctx context.Context) (map[string]File, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.path, f.language, f.size_bytes, f.modified_unix_nano, f.digest,
			f.symbol_count, f.indexed_at, f.entry_point,
			IFNULL(r.rank, 0), f.scope_aware, f.package_name, f.reference_sites_truncated
		FROM repo_index_files f
		LEFT JOIN repo_index_file_rank r
			ON r.root_path = f.root_path AND r.path = f.path
		WHERE f.root_path = ?`, s.root)
	if err != nil {
		return nil, fmt.Errorf("read repository index files: %w", err)
	}
	defer rows.Close()
	files := make(map[string]File)
	for rows.Next() {
		var (
			file       File
			modified   int64
			indexedAt  string
			entryPoint int
		)
		if err := rows.Scan(
			&file.Path, &file.Language, &file.Size, &modified,
			&file.Digest, &file.SymbolCount, &indexedAt, &entryPoint,
			&file.Rank, &file.ScopeAware, &file.PackageName, &file.ReferenceSitesTruncated,
		); err != nil {
			return nil, fmt.Errorf("read repository index files: %w", err)
		}
		if file.IndexedAt, err = time.Parse(time.RFC3339Nano, indexedAt); err != nil {
			return nil, fmt.Errorf("read repository index files: %w", err)
		}
		file.Modified = time.Unix(0, modified)
		file.EntryPoint = entryPoint != 0
		files[file.Path] = file
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read repository index files: %w", err)
	}
	return files, nil
}

// Apply writes a batch of records atomically, replacing the symbols of every
// path it carries. Callers pass batches rather than whole repositories so a
// large refresh never holds one long write transaction against the shared
// database.
func (s *Store) Apply(ctx context.Context, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, record := range records {
			if err := s.applyRecord(ctx, tx, record); err != nil {
				return err
			}
		}
		return nil
	})
}

// Delete removes paths and their symbols, which is how a refresh prunes files
// that disappeared from the workspace.
func (s *Store) Delete(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, path := range paths {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM repo_index_files WHERE root_path = ? AND path = ?`, s.root, path,
			); err != nil {
				return fmt.Errorf("delete repository index file: %w", err)
			}
		}
		return nil
	})
}

// Reset drops everything recorded for the root. A refresh calls it when the
// stored rows cannot be trusted: a changed indexer version, or a read that
// failed to make sense of them.
func (s *Store) Reset(ctx context.Context) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, statement := range []string{
			`DELETE FROM repo_index_symbols WHERE root_path = ?`,
			`DELETE FROM repo_index_references WHERE root_path = ?`,
			`DELETE FROM repo_index_reference_sites WHERE root_path = ?`,
			`DELETE FROM repo_index_imports WHERE root_path = ?`,
			`DELETE FROM repo_index_edges WHERE root_path = ?`,
			`DELETE FROM repo_index_file_rank WHERE root_path = ?`,
			`DELETE FROM repo_index_files WHERE root_path = ?`,
			`DELETE FROM repo_index_meta WHERE root_path = ?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, s.root); err != nil {
				return fmt.Errorf("reset repository index: %w", err)
			}
		}
		return nil
	})
}

// Symbols returns the declarations a query selects, ordered so that shorter
// names — the closer matches for a substring query — come first, then by path
// and line for a stable result.
func (s *Store) Symbols(ctx context.Context, query Query) ([]Symbol, error) {
	limit := query.Limit
	switch {
	case limit <= 0:
		limit = DefaultQueryLimit
	case limit > maxQueryLimit:
		limit = maxQueryLimit
	}
	statement := strings.Builder{}
	statement.WriteString(`
		SELECT path, name, kind, container, line, exported,
			signature, docstring, resolution
		FROM repo_index_symbols WHERE root_path = ?`)
	arguments := []any{s.root}
	if name := strings.TrimSpace(query.Name); name != "" {
		if query.Exact {
			statement.WriteString(" AND name = ? COLLATE NOCASE")
			arguments = append(arguments, name)
		} else {
			statement.WriteString(" AND name LIKE ? ESCAPE '\\'")
			arguments = append(arguments, "%"+escapeLike(name)+"%")
		}
	}
	if len(query.Kinds) != 0 {
		statement.WriteString(" AND kind IN (" + placeholders(len(query.Kinds)) + ")")
		for _, kind := range query.Kinds {
			arguments = append(arguments, kind)
		}
	}
	if len(query.Paths) != 0 {
		statement.WriteString(" AND path IN (" + placeholders(len(query.Paths)) + ")")
		for _, path := range query.Paths {
			arguments = append(arguments, path)
		}
	}
	statement.WriteString(" ORDER BY length(name), name, path, line LIMIT ?")
	arguments = append(arguments, limit)

	rows, err := s.db.QueryContext(ctx, statement.String(), arguments...)
	if err != nil {
		return nil, fmt.Errorf("query repository index symbols: %w", err)
	}
	defer rows.Close()
	symbols := make([]Symbol, 0, min(limit, 64))
	for rows.Next() {
		var (
			symbol   Symbol
			exported int
		)
		if err := rows.Scan(
			&symbol.Path, &symbol.Name, &symbol.Kind,
			&symbol.Container, &symbol.Line, &exported,
			&symbol.Signature, &symbol.Docstring, &symbol.Resolution,
		); err != nil {
			return nil, fmt.Errorf("query repository index symbols: %w", err)
		}
		symbol.Exported = exported != 0
		symbols = append(symbols, symbol)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query repository index symbols: %w", err)
	}
	return symbols, nil
}

// Paths returns the indexed paths, restricted to a language when one is given
// and always sorted. Consumers that scan file contents use it to stay on the
// files the index already vetted.
func (s *Store) Paths(ctx context.Context, language string) ([]string, error) {
	statement := `SELECT path FROM repo_index_files WHERE root_path = ?`
	arguments := []any{s.root}
	if language = strings.TrimSpace(language); language != "" {
		statement += ` AND language = ?`
		arguments = append(arguments, language)
	}
	statement += ` ORDER BY path`
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query repository index paths: %w", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("query repository index paths: %w", err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query repository index paths: %w", err)
	}
	return paths, nil
}

func (s *Store) applyRecord(ctx context.Context, tx *sql.Tx, record Record) error {
	file := record.File
	if strings.TrimSpace(file.Path) == "" {
		return errors.New("indexed file requires a path")
	}
	entryPoint := 0
	if file.EntryPoint {
		entryPoint = 1
	}
	if file.IndexedAt.IsZero() {
		file.IndexedAt = time.Now()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO repo_index_files(
			root_path, path, language, size_bytes, modified_unix_nano,
			digest, symbol_count, indexed_at, entry_point, scope_aware, package_name, reference_sites_truncated
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(root_path, path) DO UPDATE SET
			language = excluded.language,
			size_bytes = excluded.size_bytes,
			modified_unix_nano = excluded.modified_unix_nano,
			digest = excluded.digest,
			symbol_count = excluded.symbol_count,
			indexed_at = excluded.indexed_at,
			entry_point = excluded.entry_point, scope_aware=excluded.scope_aware, package_name=excluded.package_name, reference_sites_truncated=excluded.reference_sites_truncated`,
		s.root, file.Path, file.Language, file.Size, file.Modified.UnixNano(),
		file.Digest, len(record.Symbols), timestamp(file.IndexedAt), entryPoint, file.ScopeAware, file.PackageName, file.ReferenceSitesTruncated,
	); err != nil {
		return fmt.Errorf("write repository index file: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM repo_index_symbols WHERE root_path = ? AND path = ?`, s.root, file.Path,
	); err != nil {
		return fmt.Errorf("replace repository index symbols: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM repo_index_references WHERE root_path = ? AND path = ?`, s.root, file.Path,
	); err != nil {
		return fmt.Errorf("replace repository index references: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM repo_index_imports WHERE root_path = ? AND path = ?`, s.root, file.Path,
	); err != nil {
		return fmt.Errorf("replace repository index imports: %w", err)
	}
	for _, symbol := range record.Symbols {
		exported := 0
		if symbol.Exported {
			exported = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO repo_index_symbols(
				root_path, path, name, kind, container, line, exported,
				signature, docstring, resolution
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.root, file.Path, symbol.Name, symbol.Kind,
			symbol.Container, symbol.Line, exported,
			symbol.Signature, symbol.Docstring, symbol.Resolution,
		); err != nil {
			return fmt.Errorf("write repository index symbol: %w", err)
		}
	}
	for _, reference := range record.References {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO repo_index_references(
				root_path, path, name, use_count
			) VALUES (?, ?, ?, ?)`,
			s.root, file.Path, reference.Name, reference.Count,
		); err != nil {
			return fmt.Errorf("write repository index reference: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM repo_index_reference_sites WHERE root_path=? AND path=?`, s.root, file.Path); err != nil {
		return err
	}
	for position, site := range record.ReferenceSites {
		if _, err := tx.ExecContext(ctx, `INSERT INTO repo_index_reference_sites(root_path,path,position,name,target,module,kind,scope,start_byte,end_byte,line) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, s.root, file.Path, position, site.Name, site.Target, site.Module, site.Kind, site.Scope, site.StartByte, site.EndByte, site.Line); err != nil {
			return fmt.Errorf("write reference site: %w", err)
		}
	}
	for position, spec := range record.Imports {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO repo_index_imports(
				root_path, path, position, spec
			) VALUES (?, ?, ?, ?)`,
			s.root, file.Path, position, spec,
		); err != nil {
			return fmt.Errorf("write repository index import: %w", err)
		}
	}
	return nil
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, beginErr := s.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return fmt.Errorf("begin repository index transaction: %w", beginErr)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit repository index transaction: %w", err)
	}
	return nil
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// escapeLike keeps a query's wildcards from reaching SQL, so searching for "a_b"
// does not also match "axb".
func escapeLike(value string) string {
	replaced := strings.ReplaceAll(value, `\`, `\\`)
	replaced = strings.ReplaceAll(replaced, "%", `\%`)
	return strings.ReplaceAll(replaced, "_", `\_`)
}

func timestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

// Imports returns the raw import specifiers recorded per file, in the order
// the extractor read them. The graph build resolves them against the file set.
func (s *Store) Imports(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, spec FROM repo_index_imports WHERE root_path = ?
		ORDER BY path, position`, s.root)
	if err != nil {
		return nil, fmt.Errorf("read repository index imports: %w", err)
	}
	defer rows.Close()
	specs := make(map[string][]string)
	for rows.Next() {
		var path, spec string
		if err := rows.Scan(&path, &spec); err != nil {
			return nil, fmt.Errorf("read repository index imports: %w", err)
		}
		specs[path] = append(specs[path], spec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read repository index imports: %w", err)
	}
	return specs, nil
}

// ReplaceEdges swaps the graph rows for the root. The graph is wholly derived
// from symbols, references and imports, so replacing beats merging.
func (s *Store) ReplaceEdges(ctx context.Context, edges []graphEdge) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM repo_index_edges WHERE root_path = ?`, s.root,
		); err != nil {
			return fmt.Errorf("replace repository index edges: %w", err)
		}
		for _, edge := range edges {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO repo_index_edges(
					root_path, src_path, dst_path, kind, weight
				) VALUES (?, ?, ?, ?, ?)`,
				s.root, edge.Src, edge.Dst, edge.Kind, edge.Weight,
			); err != nil {
				return fmt.Errorf("write repository index edge: %w", err)
			}
		}
		return nil
	})
}

// ReplaceRanks records the file ranks of one graph build.
func (s *Store) ReplaceRanks(ctx context.Context, ranks map[string]float64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM repo_index_file_rank WHERE root_path = ?`, s.root,
		); err != nil {
			return fmt.Errorf("replace repository index ranks: %w", err)
		}
		for path, rank := range ranks {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO repo_index_file_rank(root_path, path, rank)
				VALUES (?, ?, ?)`,
				s.root, path, rank,
			); err != nil {
				return fmt.Errorf("write repository index rank: %w", err)
			}
		}
		return nil
	})
}

// Edges returns the stored graph rows for the root, sorted for determinism.
// Impact analysis (proposal R4) consumes the same rows the ranks were built
// from.
func (s *Store) Edges(ctx context.Context) ([]graphEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT src_path, dst_path, kind, weight
		FROM repo_index_edges WHERE root_path = ?
		ORDER BY src_path, dst_path, kind`, s.root)
	if err != nil {
		return nil, fmt.Errorf("read repository index edges: %w", err)
	}
	defer rows.Close()
	var edges []graphEdge
	for rows.Next() {
		var edge graphEdge
		if err := rows.Scan(&edge.Src, &edge.Dst, &edge.Kind, &edge.Weight); err != nil {
			return nil, fmt.Errorf("read repository index edges: %w", err)
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read repository index edges: %w", err)
	}
	return edges, nil
}

// referenceEdges joins the identifier counts against the symbol table: a file
// that uses a name declared elsewhere in the repository references the
// declaring file, with the use count as the edge weight. The join runs where
// both tables and their name indexes live.
func (s *Store) referenceEdges(ctx context.Context) ([]graphEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.path AS src, s.path AS dst, SUM(r.use_count) AS weight
		FROM repo_index_references r
		JOIN repo_index_symbols s
			ON s.root_path = r.root_path AND s.name = r.name
		WHERE r.root_path = ? AND r.path <> s.path
 AND NOT EXISTS (SELECT 1 FROM repo_index_files f WHERE f.root_path=r.root_path AND f.path=r.path AND f.scope_aware=1)
		GROUP BY r.path, s.path
		ORDER BY r.path, s.path`, s.root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []graphEdge
	for rows.Next() {
		var edge graphEdge
		edge.Kind = EdgeReference
		if err := rows.Scan(&edge.Src, &edge.Dst, &edge.Weight); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}
