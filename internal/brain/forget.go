package brain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// ForgetTarget is the row kind addressed by id / ids.
type ForgetTarget string

const (
	ForgetTargetEpisode ForgetTarget = "episode"
	ForgetTargetFact    ForgetTarget = "fact"
)

// ForgetInput selects how to resolve what to forget.
type ForgetInput struct {
	About      string
	TargetType ForgetTarget
	ID         int64
	IDs        []int64
}

// ForgetResult reports how many rows were soft-deleted.
type ForgetResult struct {
	EpisodesAffected int64 `json:"episodes_affected"`
	FactsAffected    int64 `json:"facts_affected"`
	RowsAffected     int64 `json:"rows_affected"`
}

func (r ForgetResult) add(o ForgetResult) ForgetResult {
	r.EpisodesAffected += o.EpisodesAffected
	r.FactsAffected += o.FactsAffected
	r.RowsAffected += o.RowsAffected
	return r
}

// Forget soft-deletes episodes and/or facts by semantic match or explicit id(s).
func (b *Brain) Forget(ctx context.Context, namespaceSlugs []string, in ForgetInput) (ForgetResult, error) {
	var zero ForgetResult

	switch {
	case in.About != "":
		return b.forgetByAbout(ctx, namespaceSlugs, in.About)
	case in.ID != 0 || len(in.IDs) > 0:
		if in.TargetType != ForgetTargetEpisode && in.TargetType != ForgetTargetFact {
			return zero, fmt.Errorf("brain: type must be %q or %q when using id or ids", ForgetTargetEpisode, ForgetTargetFact)
		}
		ids := in.IDs
		if in.ID != 0 {
			ids = append([]int64{in.ID}, ids...)
		}
		return b.forgetByIDs(ctx, in.TargetType, ids)
	default:
		return zero, ErrEmptyContent
	}
}

// ForgetEpisode soft-deletes the episode that best matches the query and expires orphan facts.
// If namespaceSlugs is empty, searches all namespaces.
func (b *Brain) ForgetEpisode(ctx context.Context, namespaceSlugs []string, query string) (ForgetResult, error) {
	return b.Forget(ctx, namespaceSlugs, ForgetInput{About: query})
}

func (b *Brain) forgetByAbout(ctx context.Context, namespaceSlugs []string, query string) (ForgetResult, error) {
	var result ForgetResult
	if err := validateContent(query); err != nil {
		return result, err
	}
	for _, slug := range namespaceSlugs {
		if err := validatePath(slug); err != nil {
			return result, err
		}
	}

	nsIDs, err := b.resolveNamespaceIDs(ctx, namespaceSlugs)
	if err != nil {
		return result, err
	}

	vec, err := b.embedder.Embed(ctx, query)
	if err != nil {
		return result, fmt.Errorf("embed: %w", err)
	}
	pgVec := pgvector.NewVector(vec)

	epID, epScore, epErr := b.findBestEpisode(ctx, nsIDs, pgVec)
	factID, factScore, factErr := b.findBestFact(ctx, nsIDs, pgVec)

	if epErr != nil && factErr != nil {
		return result, ErrMemoryNotFound
	}

	forgetEpisode := epErr == nil && (factErr != nil || epScore >= factScore)
	forgetFact := factErr == nil && (epErr != nil || factScore > epScore)
	// Equal scores: episode forget cascades to sourced facts; also forget the fact row directly.
	if epErr == nil && factErr == nil && epScore == factScore {
		forgetEpisode = true
		forgetFact = true
	}

	if forgetEpisode {
		part, err := b.forgetEpisodeByID(ctx, epID)
		if err != nil {
			return result, err
		}
		result = result.add(part)
	}

	if forgetFact {
		part, err := b.forgetFactByID(ctx, factID)
		if err != nil {
			if forgetEpisode && errors.Is(err, ErrFactNotFound) {
				// Already removed by episode cascade.
			} else {
				return result, err
			}
		} else {
			result = result.add(part)
		}
	}

	if result.RowsAffected == 0 {
		return result, ErrMemoryNotFound
	}
	return result, nil
}

func (b *Brain) forgetByIDs(ctx context.Context, target ForgetTarget, ids []int64) (ForgetResult, error) {
	var result ForgetResult
	for _, id := range ids {
		var part ForgetResult
		var err error
		switch target {
		case ForgetTargetEpisode:
			part, err = b.forgetEpisodeByID(ctx, id)
		case ForgetTargetFact:
			part, err = b.forgetFactByID(ctx, id)
		default:
			return result, fmt.Errorf("brain: unknown forget target %q", target)
		}
		if err != nil {
			return result, err
		}
		result = result.add(part)
	}
	return result, nil
}

func (b *Brain) findBestEpisode(ctx context.Context, nsIDs []int64, vec pgvector.Vector) (int64, float32, error) {
	var id int64
	var score float32
	var err error
	if len(nsIDs) == 1 {
		err = b.pool.QueryRow(ctx,
			`SELECT id, 1 - (embedding <=> $2) AS score FROM episodes
			 WHERE namespace_id = $1 AND deleted_at IS NULL AND embedding IS NOT NULL
			 ORDER BY embedding <=> $2 LIMIT 1`,
			nsIDs[0], vec,
		).Scan(&id, &score)
	} else {
		err = b.pool.QueryRow(ctx,
			`SELECT id, 1 - (embedding <=> $2) AS score FROM episodes
			 WHERE namespace_id = ANY($1) AND deleted_at IS NULL AND embedding IS NOT NULL
			 ORDER BY embedding <=> $2 LIMIT 1`,
			nsIDs, vec,
		).Scan(&id, &score)
	}
	if err != nil {
		return 0, 0, ErrEpisodeNotFound
	}
	return id, score, nil
}

func (b *Brain) findBestFact(ctx context.Context, nsIDs []int64, vec pgvector.Vector) (int64, float32, error) {
	var id int64
	var score float32
	var err error
	if len(nsIDs) == 1 {
		err = b.pool.QueryRow(ctx,
			`SELECT id, 1 - (embedding <=> $2) AS score FROM facts
			 WHERE namespace_id = $1 AND deleted_at IS NULL AND embedding IS NOT NULL
			 ORDER BY embedding <=> $2 LIMIT 1`,
			nsIDs[0], vec,
		).Scan(&id, &score)
	} else {
		err = b.pool.QueryRow(ctx,
			`SELECT id, 1 - (embedding <=> $2) AS score FROM facts
			 WHERE namespace_id = ANY($1) AND deleted_at IS NULL AND embedding IS NOT NULL
			 ORDER BY embedding <=> $2 LIMIT 1`,
			nsIDs, vec,
		).Scan(&id, &score)
	}
	if err != nil {
		return 0, 0, ErrFactNotFound
	}
	return id, score, nil
}

func (b *Brain) forgetEpisodeByID(ctx context.Context, episodeID int64) (ForgetResult, error) {
	var result ForgetResult
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		"UPDATE episodes SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL",
		episodeID,
	)
	if err != nil {
		return result, fmt.Errorf("soft delete episode: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return result, ErrEpisodeNotFound
	}
	result.EpisodesAffected = tag.RowsAffected()

	factsExpired, err := b.expireFactsForForgottenEpisode(ctx, tx, episodeID)
	if err != nil {
		return result, err
	}
	result.FactsAffected = factsExpired
	result.RowsAffected = result.EpisodesAffected + result.FactsAffected

	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

func (b *Brain) forgetFactByID(ctx context.Context, factID int64) (ForgetResult, error) {
	var result ForgetResult
	tag, err := b.pool.Exec(ctx,
		"UPDATE facts SET deleted_at = now(), embedding = NULL WHERE id = $1 AND deleted_at IS NULL",
		factID,
	)
	if err != nil {
		return result, fmt.Errorf("soft delete fact: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return result, ErrFactNotFound
	}
	result.FactsAffected = tag.RowsAffected()
	result.RowsAffected = result.FactsAffected
	return result, nil
}

// expireFactsForForgottenEpisode removes fact_sources for episodeID and soft-deletes facts
// that no longer have any live (non-deleted) episode sources.
func (b *Brain) expireFactsForForgottenEpisode(ctx context.Context, tx pgx.Tx, episodeID int64) (int64, error) {
	rows, err := tx.Query(ctx,
		"SELECT fact_id FROM fact_sources WHERE episode_id = $1",
		episodeID,
	)
	if err != nil {
		return 0, fmt.Errorf("list fact sources: %w", err)
	}
	defer rows.Close()

	var factIDs []int64
	for rows.Next() {
		var factID int64
		if err := rows.Scan(&factID); err != nil {
			return 0, fmt.Errorf("scan fact_id: %w", err)
		}
		factIDs = append(factIDs, factID)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if _, err := tx.Exec(ctx, "DELETE FROM fact_sources WHERE episode_id = $1", episodeID); err != nil {
		return 0, fmt.Errorf("delete fact_sources: %w", err)
	}

	var expired int64
	for _, factID := range factIDs {
		var liveSources int64
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM fact_sources fs
			 INNER JOIN episodes e ON e.id = fs.episode_id
			 WHERE fs.fact_id = $1 AND e.deleted_at IS NULL`,
			factID,
		).Scan(&liveSources); err != nil {
			return expired, fmt.Errorf("count live sources: %w", err)
		}
		if liveSources > 0 {
			continue
		}

		tag, err := tx.Exec(ctx,
			"UPDATE facts SET deleted_at = now(), embedding = NULL WHERE id = $1 AND deleted_at IS NULL",
			factID,
		)
		if err != nil {
			return expired, fmt.Errorf("expire orphan fact: %w", err)
		}
		expired += tag.RowsAffected()
	}
	return expired, nil
}

// ParseForgetIDs parses a comma-separated list of decimal ids.
func ParseForgetIDs(raw string) ([]int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid id %q: %w", p, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
