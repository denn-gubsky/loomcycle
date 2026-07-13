package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC BE — the per-session embedding index backing History op=related
// (semantic "related chats").
//
// Unlike memory_embeddings (which refuses on the default pure-Go SQLite build
// because it needs the sqlite-vec C extension), this index needs NO extension:
// the per-chat vector set is small, so the vector is a plain TEXT column
// (store.EncodeVector) and cosine ranking runs in Go over the folded candidate
// set. That keeps History op=related working on the default single-binary
// deployment — the common case — and lets the same real search run in CI on
// both backends (the pgvector round-trip contract path is opt-in / not run in
// default CI).

// sessionSimilarCandidateCap bounds how many in-scope embedded chats a single
// SessionEmbedSearch loads before ranking, so a large tenant can't blow memory
// on a `tenant`/`global` search. The most-recently-embedded chats win the cap
// (ORDER BY updated_at DESC). The index fills only as chats are recapped /
// annotated, so in practice the candidate set is far below this.
const sessionSimilarCandidateCap = 2000

// SessionEmbedUpsert writes/replaces one chat's embedding. The owning
// tenant_id/user_id/agent are copied from the authoritative sessions row (via
// INSERT ... SELECT) so the denormalised owner columns the search folds on can
// never drift from — or be spoofed apart from — the session. A missing session
// inserts zero rows → *store.ErrNotFound.
func (s *Store) SessionEmbedUpsert(ctx context.Context, sessionID string, e store.SessionEmbedding) error {
	if e.Dimension <= 0 {
		return errors.New("session_embeddings: dimension must be > 0")
	}
	if len(e.Vector) != e.Dimension {
		return fmt.Errorf("session_embeddings: vector length %d != declared dimension %d", len(e.Vector), e.Dimension)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO session_embeddings (session_id, tenant_id, user_id, agent, provider, model, dimension, vector, updated_at)
		SELECT s.id, s.tenant_id, s.user_id, s.agent, ?, ?, ?, ?, ?
		FROM sessions s WHERE s.id = ?
		ON CONFLICT(session_id) DO UPDATE SET
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
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("SessionEmbedUpsert rows: %w", err)
	}
	if n == 0 {
		return &store.ErrNotFound{Kind: "session", ID: sessionID}
	}
	return nil
}

// SessionEmbedSearch ranks embedded chats by cosine similarity to query. The
// owner/tenant fold is applied on the denormalised session_embeddings columns
// (the join-free tenant seam — cross-tenant rows never leave the DB); the
// archived filter is applied on the sessions row (it changes after upsert, so
// it is NOT denormalised). Ranking is in Go over the folded, recency-capped
// candidate set.
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
	if f.TenantID != "" {
		conds = append(conds, "se.tenant_id = ?")
		args = append(args, f.TenantID)
	}
	if f.UserID != "" {
		conds = append(conds, "se.user_id = ?")
		args = append(args, f.UserID)
	}
	if f.AgentName != "" {
		conds = append(conds, "se.agent = ?")
		args = append(args, f.AgentName)
	}
	if !f.IncludeArchived {
		conds = append(conds, "s.archived_at IS NULL")
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	// Mirrors ListSessions' rollup (run_count / tokens / cost / last_activity /
	// derived status) but INNER JOINs session_embeddings so only embedded chats
	// are candidates, and carries se.vector for the in-Go ranking. GROUP BY the
	// two PKs (s.id, se.session_id) so every s.* / se.* column is
	// functional-dependency-valid on Postgres.
	q := `SELECT
			s.id, s.tenant_id, s.agent, s.user_id, s.created_at,
			s.title, s.description, s.tags, s.pinned, s.archived_at, s.summary,
			COUNT(r.id) AS run_count,
			COALESCE(SUM(r.input_tokens), 0) AS in_tok,
			COALESCE(SUM(r.output_tokens), 0) AS out_tok,
			COALESCE(SUM(r.cost), 0) AS cost,
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
		LIMIT ?`
	args = append(args, sessionSimilarCandidateCap)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("SessionEmbedSearch: %w", err)
	}
	defer rows.Close()

	out := make([]store.SessionSimilar, 0, limit)
	for rows.Next() {
		var ss store.SessionSummary
		var userID, title, description, tags, summary sql.NullString
		var createdNs, lastActNs int64
		var archivedNs sql.NullInt64
		var pinnedInt int64
		var statusStr, vecText string
		var dim int
		if err := rows.Scan(
			&ss.SessionID, &ss.TenantID, &ss.Agent, &userID, &createdNs,
			&title, &description, &tags, &pinnedInt, &archivedNs, &summary,
			&ss.RunCount, &ss.InputTokens, &ss.OutputTokens, &ss.Cost, &lastActNs, &statusStr,
			&vecText, &dim,
		); err != nil {
			return nil, fmt.Errorf("SessionEmbedSearch scan: %w", err)
		}
		// Skip rows embedded under a different model (dimension mismatch) — they
		// can't be compared to this query vector. In practice all rows share the
		// one configured embedder, so this only trips after an embedder change.
		if dim != len(query) {
			continue
		}
		vec, derr := store.DecodeVector(vecText)
		if derr != nil {
			return nil, fmt.Errorf("SessionEmbedSearch decode vector: %w", derr)
		}
		ss.CreatedAt = time.Unix(0, createdNs)
		ss.LastActivity = time.Unix(0, lastActNs)
		if userID.Valid {
			ss.UserID = userID.String
		}
		if title.Valid {
			ss.Title = title.String
		}
		if description.Valid {
			ss.Description = description.String
		}
		if summary.Valid {
			ss.Summary = summary.String
		}
		if tags.Valid {
			decoded, terr := store.DecodeTags(tags.String)
			if terr != nil {
				return nil, fmt.Errorf("SessionEmbedSearch decode tags: %w", terr)
			}
			ss.Tags = decoded
		}
		ss.Pinned = pinnedInt != 0
		ss.Archived = archivedNs.Valid
		if statusStr != "" {
			ss.Status = store.RunStatus(statusStr)
		}
		out = append(out, store.SessionSimilar{SessionSummary: ss, Score: store.CosineSimilarity(query, vec)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SessionEmbedSearch iter: %w", err)
	}

	rankSessionSimilar(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// rankSessionSimilar sorts results by descending cosine score, tie-breaking by
// most-recent activity so ordering is deterministic across backends.
func rankSessionSimilar(rs []store.SessionSimilar) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Score != rs[j].Score {
			return rs[i].Score > rs[j].Score
		}
		return rs[i].LastActivity.After(rs[j].LastActivity)
	})
}
