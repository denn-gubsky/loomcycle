package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC BE — the per-session embedding index backing History op=related
// (semantic "related chats"). See internal/store/sqlite/session_embeddings.go
// for the design rationale; the Postgres adapter mirrors it. The vector is a
// plain TEXT column (store.EncodeVector) — deliberately NOT pgvector: the
// per-chat index is small, ranks in Go, and so runs on any Postgres (no
// extension) and on the SQLite adapter identically.

// sessionSimilarCandidateCap bounds how many in-scope embedded chats a single
// SessionEmbedSearch loads before ranking (see the SQLite twin).
const sessionSimilarCandidateCap = 2000

// SessionEmbedUpsert writes/replaces one chat's embedding, copying the owning
// tenant/user/agent from the authoritative sessions row (INSERT ... SELECT) so
// the denormalised owner columns can't drift from — or be spoofed apart from —
// the session. A missing session inserts zero rows → *store.ErrNotFound.
func (s *Store) SessionEmbedUpsert(ctx context.Context, sessionID string, e store.SessionEmbedding) error {
	if e.Dimension <= 0 {
		return errors.New("session_embeddings: dimension must be > 0")
	}
	if len(e.Vector) != e.Dimension {
		return fmt.Errorf("session_embeddings: vector length %d != declared dimension %d", len(e.Vector), e.Dimension)
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO session_embeddings (session_id, tenant_id, user_id, agent, provider, model, dimension, vector, updated_at)
		SELECT s.id, s.tenant_id, s.user_id, s.agent, $1, $2, $3, $4, $5
		FROM sessions s WHERE s.id = $6
		ON CONFLICT (session_id) DO UPDATE SET
			tenant_id  = excluded.tenant_id,
			user_id    = excluded.user_id,
			agent      = excluded.agent,
			provider   = excluded.provider,
			model      = excluded.model,
			dimension  = excluded.dimension,
			vector     = excluded.vector,
			updated_at = excluded.updated_at`,
		e.Provider, e.Model, e.Dimension, store.EncodeVector(e.Vector), time.Now().UnixNano(), sessionID,
	)
	if err != nil {
		return fmt.Errorf("SessionEmbedUpsert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Kind: "session", ID: sessionID}
	}
	return nil
}

// SessionEmbedSearch ranks embedded chats by cosine similarity to query,
// applying the SessionFilter owner/tenant fold on the denormalised
// session_embeddings columns (the join-free tenant seam) and the archived
// filter on the sessions row. Mirrors ListSessions' rollup. Ranking is in Go.
func (s *Store) SessionEmbedSearch(ctx context.Context, f store.SessionFilter, query []float32, limit int) ([]store.SessionSimilar, error) {
	if len(query) == 0 {
		return nil, errors.New("SessionEmbedSearch: query vector is empty")
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 500 {
		limit = 500
	}

	var conds []string
	var args []any
	i := 1
	if f.TenantID != "" {
		conds = append(conds, fmt.Sprintf("se.tenant_id = $%d", i))
		args = append(args, f.TenantID)
		i++
	}
	if f.UserID != "" {
		conds = append(conds, fmt.Sprintf("se.user_id = $%d", i))
		args = append(args, f.UserID)
		i++
	}
	if f.AgentName != "" {
		conds = append(conds, fmt.Sprintf("se.agent = $%d", i))
		args = append(args, f.AgentName)
		i++
	}
	if !f.IncludeArchived {
		conds = append(conds, "s.archived_at IS NULL")
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	// Mirrors ListSessions' rollup but INNER JOINs session_embeddings so only
	// embedded chats are candidates, carrying se.vector for the in-Go ranking.
	// GROUP BY the two PKs (s.id, se.session_id) so every s.* / se.* column is
	// functional-dependency-valid.
	q := `SELECT
			s.id, s.tenant_id, s.agent, s.user_id, s.created_at,
			s.title, s.description, s.tags, s.pinned, s.archived_at, s.summary,
			COUNT(r.id)::BIGINT AS run_count,
			COALESCE(SUM(r.input_tokens), 0)::BIGINT AS in_tok,
			COALESCE(SUM(r.output_tokens), 0)::BIGINT AS out_tok,
			COALESCE(SUM(r.cost), 0)::DOUBLE PRECISION AS cost,
			COALESCE(MAX(CASE WHEN r.completed_at IS NOT NULL AND r.completed_at > r.started_at
			                  THEN r.completed_at ELSE r.started_at END), s.created_at) AS last_activity,
			CASE WHEN MAX(CASE WHEN r.status = 'running' THEN 1 ELSE 0 END) = 1 THEN 'running'
			     ELSE COALESCE((SELECT r2.status FROM runs r2 WHERE r2.session_id = s.id
			                    ORDER BY r2.started_at DESC LIMIT 1), '') END AS status,
			se.vector, se.dimension
		FROM session_embeddings se
		JOIN sessions s ON s.id = se.session_id
		LEFT JOIN runs r ON r.session_id = s.id
		` + where + `
		GROUP BY s.id, se.session_id
		ORDER BY se.updated_at DESC
		LIMIT $` + fmt.Sprintf("%d", i)
	args = append(args, sessionSimilarCandidateCap)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("SessionEmbedSearch: %w", err)
	}
	defer rows.Close()

	out := make([]store.SessionSimilar, 0, limit)
	for rows.Next() {
		var (
			ss                       store.SessionSummary
			userID                   *string
			title, description, tags *string
			summary                  *string
			pinned                   bool
			archivedNs               *int64
			runCount                 int64
			createdAt, lastActivity  time.Time
			statusStr, vecText       string
			dim                      int
		)
		if err := rows.Scan(
			&ss.SessionID, &ss.TenantID, &ss.Agent, &userID, &createdAt,
			&title, &description, &tags, &pinned, &archivedNs, &summary,
			&runCount, &ss.InputTokens, &ss.OutputTokens, &ss.Cost, &lastActivity, &statusStr,
			&vecText, &dim,
		); err != nil {
			return nil, fmt.Errorf("SessionEmbedSearch scan: %w", err)
		}
		// Skip rows embedded under a different model (dimension mismatch); in
		// practice all rows share the one configured embedder.
		if dim != len(query) {
			continue
		}
		vec, derr := store.DecodeVector(vecText)
		if derr != nil {
			return nil, fmt.Errorf("SessionEmbedSearch decode vector: %w", derr)
		}
		ss.CreatedAt = createdAt
		ss.LastActivity = lastActivity
		ss.RunCount = int(runCount)
		if userID != nil {
			ss.UserID = *userID
		}
		if title != nil {
			ss.Title = *title
		}
		if description != nil {
			ss.Description = *description
		}
		if summary != nil {
			ss.Summary = *summary
		}
		if tags != nil {
			decoded, terr := store.DecodeTags(*tags)
			if terr != nil {
				return nil, fmt.Errorf("SessionEmbedSearch decode tags: %w", terr)
			}
			ss.Tags = decoded
		}
		ss.Pinned = pinned
		ss.Archived = archivedNs != nil
		if statusStr != "" {
			ss.Status = store.RunStatus(statusStr)
		}
		out = append(out, store.SessionSimilar{SessionSummary: ss, Score: store.CosineSimilarity(query, vec)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SessionEmbedSearch iter: %w", err)
	}

	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].LastActivity.After(out[b].LastActivity)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
