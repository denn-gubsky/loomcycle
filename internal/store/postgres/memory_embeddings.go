package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Vector Memory — Postgres pgvector implementation.
//
// Wire format: pgvector accepts its native text representation
// `[1.0,2.0,3.0]::vector`. We encode/decode that ourselves rather
// than pulling in pgvector-go as a dependency — the format is
// trivial and the round-trip stays bytes-equal.
//
// Distance function: cosine (`<=>`). pgvector's cosine_distance is
// `1 - cos(theta)`, range [0, 2]. We convert to similarity via
// `1.0 - distance` and surface that as MemorySearchEntry.Score, in
// [0, 1] for non-negative real vectors.
//
// Dimension-mismatch handling: we pre-check by reading the first
// matching row's `dimension` column and comparing against the
// query vector's length BEFORE running the cosine query. This gives
// a typed ErrDimensionMismatch instead of pgvector's opaque
// "different vector dimensions" runtime error.

// SupportsVectors reports whether vector ops are available. True
// only when both Config.PgvectorEnabled was set AND the post-
// migration extension probe in Open() saw `vector` loaded.
func (s *Store) SupportsVectors() bool {
	return s.pgvectorEnabled
}

// MemoryEmbedSet upserts the embedding for one (scope, scope_id, key).
// The base memory row must exist — the FK enforces this; absent
// base row surfaces as a Postgres `foreign_key_violation`.
func (s *Store) MemoryEmbedSet(ctx context.Context, scope store.MemoryScope, scopeID, key string, e store.MemoryEmbedding) error {
	if !s.pgvectorEnabled {
		return store.ErrVectorUnsupported
	}
	if e.Dimension <= 0 {
		return errors.New("memory_embeddings: dimension must be > 0")
	}
	if len(e.Vector) != e.Dimension {
		return fmt.Errorf("memory_embeddings: vector length %d != declared dimension %d", len(e.Vector), e.Dimension)
	}
	vecText := encodePgvector(e.Vector)
	createdAt := e.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO memory_embeddings(scope, scope_id, key, provider, model, dimension, embedding, embed_text, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::vector, $8, $9)
		 ON CONFLICT (scope, scope_id, key) DO UPDATE SET
		    provider   = excluded.provider,
		    model      = excluded.model,
		    dimension  = excluded.dimension,
		    embedding  = excluded.embedding,
		    embed_text = excluded.embed_text,
		    created_at = excluded.created_at`,
		string(scope), scopeID, key, e.Provider, e.Model, e.Dimension, vecText, e.EmbedText, createdAt,
	)
	if err != nil {
		return fmt.Errorf("MemoryEmbedSet: %w", err)
	}
	return nil
}

// MemoryEmbedGet returns the stored embedding, decoding the
// pgvector text representation back to float32.
func (s *Store) MemoryEmbedGet(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEmbedding, error) {
	if !s.pgvectorEnabled {
		return store.MemoryEmbedding{}, store.ErrVectorUnsupported
	}
	var (
		provider, model, embedText, embeddingText string
		dimension                                 int
		createdAt                                 time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT provider, model, dimension, embedding::text, embed_text, created_at
		 FROM memory_embeddings
		 WHERE scope = $1 AND scope_id = $2 AND key = $3`,
		string(scope), scopeID, key,
	).Scan(&provider, &model, &dimension, &embeddingText, &embedText, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.MemoryEmbedding{}, &store.ErrNotFound{Kind: "memory_embedding", ID: key}
	}
	if err != nil {
		return store.MemoryEmbedding{}, fmt.Errorf("MemoryEmbedGet: %w", err)
	}
	vec, err := decodePgvector(embeddingText)
	if err != nil {
		return store.MemoryEmbedding{}, fmt.Errorf("MemoryEmbedGet: decode vector: %w", err)
	}
	return store.MemoryEmbedding{
		Provider:  provider,
		Model:     model,
		Dimension: dimension,
		Vector:    vec,
		EmbedText: embedText,
		CreatedAt: createdAt,
	}, nil
}

// MemoryEmbedSearch runs Top-K cosine similarity. Pre-checks the
// scope's stored dimension against the query vector dimension — a
// mismatch returns ErrDimensionMismatch rather than letting pgvector
// raise its own runtime error.
//
// Empty (scope, scope_id) returns ([], nil) — explicit non-error so
// "no rows yet" doesn't pollute the agent's error path.
func (s *Store) MemoryEmbedSearch(ctx context.Context, scope store.MemoryScope, scopeID, keyPrefix string, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	if !s.pgvectorEnabled {
		return nil, store.ErrVectorUnsupported
	}
	if len(query) == 0 {
		return nil, errors.New("MemoryEmbedSearch: query vector is empty")
	}
	if topK <= 0 {
		topK = 10
	}
	// Defensive upper cap. Agents may request at most 50 per the
	// RFC; the tool layer surfaces 50 as its hard max. We accept 51
	// internally so the tool layer's "topK+1" truncation probe at
	// the boundary still returns the extra row used to detect
	// overflow.
	if topK > 51 {
		topK = 51
	}

	// Dimension pre-check. Read one row's dimension under (scope,
	// scope_id). If none exists, return empty results (the "empty
	// scope" non-error path). If exists, compare against the query.
	var storedDim int
	err := s.pool.QueryRow(ctx,
		`SELECT dimension FROM memory_embeddings
		 WHERE scope = $1 AND scope_id = $2
		 LIMIT 1`,
		string(scope), scopeID,
	).Scan(&storedDim)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("MemoryEmbedSearch dim probe: %w", err)
	}
	if storedDim != len(query) {
		return nil, &store.MemoryError{
			Code: store.ErrDimensionMismatch.Code,
			Msg:  fmt.Sprintf("memory: query embedding dimension %d does not match stored rows' dimension %d — run /v1/_memory/reembed to migrate", len(query), storedDim),
		}
	}

	queryText := encodePgvector(query)

	// Filter expired base rows in the WHERE via a JOIN. The base
	// memory row's expires_at column is the source of truth; the
	// CASCADE handles deletes but not TTL-expired-not-yet-swept.
	// We JOIN + filter so stale rows don't leak into search results.
	prefixCondition := ""
	args := []any{string(scope), scopeID, queryText, topK}
	if keyPrefix != "" {
		prefixCondition = " AND me.key LIKE $5"
		args = append(args, keyPrefix+"%")
	}
	sql := `SELECT me.key, m.value, m.expires_at, m.created_at, m.updated_at,
	               1.0 - (me.embedding <=> $3::vector) AS score,
	               me.provider, me.model
	         FROM memory_embeddings me
	         JOIN memory m
	            ON me.scope    = m.scope
	           AND me.scope_id = m.scope_id
	           AND me.key      = m.key
	         WHERE me.scope = $1
	           AND me.scope_id = $2
	           AND (m.expires_at IS NULL OR m.expires_at > now())` + prefixCondition + `
	         ORDER BY me.embedding <=> $3::vector
	         LIMIT $4`
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("MemoryEmbedSearch query: %w", err)
	}
	defer rows.Close()

	out := []store.MemorySearchEntry{}
	for rows.Next() {
		var (
			key                string
			valueBytes         []byte
			expiresAt          *time.Time
			createdAt          time.Time
			updatedAt          time.Time
			score              float64
			provider, modelStr string
		)
		if err := rows.Scan(&key, &valueBytes, &expiresAt, &createdAt, &updatedAt, &score, &provider, &modelStr); err != nil {
			return nil, fmt.Errorf("MemoryEmbedSearch scan: %w", err)
		}
		entry := store.MemorySearchEntry{
			MemoryEntry: store.MemoryEntry{
				Key:       key,
				Value:     valueBytes,
				CreatedAt: createdAt,
				UpdatedAt: updatedAt,
			},
			Score: score,
		}
		if expiresAt != nil {
			entry.ExpiresAt = *expiresAt
		}
		entry.EmbeddedWith.Provider = provider
		entry.EmbeddedWith.Model = modelStr
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("MemoryEmbedSearch iter: %w", err)
	}
	return out, nil
}

// MemoryEmbedListByModel returns base-memory rows whose embedding
// is under a DIFFERENT (provider, model) than the supplied current
// pair. Drives the reembed admin endpoint (PR 4).
func (s *Store) MemoryEmbedListByModel(ctx context.Context, scope store.MemoryScope, scopeID, currentProvider, currentModel string, limit int) ([]store.MemoryEntry, error) {
	if !s.pgvectorEnabled {
		return nil, store.ErrVectorUnsupported
	}
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx,
		`SELECT me.key, m.value, m.expires_at, m.created_at, m.updated_at
		 FROM memory_embeddings me
		 JOIN memory m
		    ON me.scope = m.scope AND me.scope_id = m.scope_id AND me.key = m.key
		 WHERE me.scope = $1 AND me.scope_id = $2
		   AND (me.provider <> $3 OR me.model <> $4)
		   AND (m.expires_at IS NULL OR m.expires_at > now())
		 ORDER BY me.key
		 LIMIT $5`,
		string(scope), scopeID, currentProvider, currentModel, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("MemoryEmbedListByModel: %w", err)
	}
	defer rows.Close()

	out := []store.MemoryEntry{}
	for rows.Next() {
		var (
			key                  string
			valueBytes           []byte
			expiresAt            *time.Time
			createdAt, updatedAt time.Time
		)
		if err := rows.Scan(&key, &valueBytes, &expiresAt, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("MemoryEmbedListByModel scan: %w", err)
		}
		e := store.MemoryEntry{
			Key:       key,
			Value:     valueBytes,
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
		}
		if expiresAt != nil {
			e.ExpiresAt = *expiresAt
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MemoryEmbedStats returns per-(provider, model) row counts +
// total embedding bytes for the scope. Embedding bytes estimated
// as dimension * 4 (float32) per row; not exact (pgvector's on-disk
// size includes some metadata) but close enough for operator
// dashboards.
func (s *Store) MemoryEmbedStats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	if !s.pgvectorEnabled {
		return store.MemoryEmbedStats{}, store.ErrVectorUnsupported
	}
	rows, err := s.pool.Query(ctx,
		`SELECT provider, model, dimension, COUNT(*)
		 FROM memory_embeddings
		 WHERE scope = $1
		 GROUP BY provider, model, dimension
		 ORDER BY provider, model`,
		string(scope),
	)
	if err != nil {
		return store.MemoryEmbedStats{}, fmt.Errorf("MemoryEmbedStats: %w", err)
	}
	defer rows.Close()

	out := store.MemoryEmbedStats{Scope: scope, Models: []store.MemoryEmbedModelStats{}}
	for rows.Next() {
		var (
			provider, model string
			dimension       int
			rowCount        int
		)
		if err := rows.Scan(&provider, &model, &dimension, &rowCount); err != nil {
			return store.MemoryEmbedStats{}, fmt.Errorf("MemoryEmbedStats scan: %w", err)
		}
		out.Models = append(out.Models, store.MemoryEmbedModelStats{
			Provider:  provider,
			Model:     model,
			Dimension: dimension,
			RowCount:  rowCount,
		})
		out.TotalEmbeddingBytes += int64(rowCount) * int64(dimension) * 4
	}
	return out, rows.Err()
}

// encodePgvector formats a float32 slice as pgvector's text shape
// "[1.0,2.0,3.0]". Uses strconv.FormatFloat with -1 precision so
// the round-trip is byte-stable for the bit-pattern (matches what
// pgvector's text-output uses).
func encodePgvector(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.Grow(len(v) * 12)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// decodePgvector parses pgvector's text representation "[1,2,3]"
// back to []float32. Tolerates whitespace between elements.
func decodePgvector(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("decodePgvector: missing brackets in %q", s)
	}
	body := s[1 : len(s)-1]
	if body == "" {
		return []float32{}, nil
	}
	parts := strings.Split(body, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("decodePgvector: parse %q: %w", p, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}
