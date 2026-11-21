package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/alash3al/stash/internal/models"
	"github.com/pgvector/pgvector-go"
)

func (b *Brain) consolidateFailurePatterns(ctx context.Context, nsID int64, cp *models.ConsolidationProgress) (repeats, patterns, llmCalls int, errs []string) {
	rows, err := b.pool.Query(ctx,
		`SELECT id, namespace_id, goal_id, content, reason, lesson, created_at, deleted_at
		 FROM failures WHERE namespace_id = $1 AND deleted_at IS NULL AND id > $2
		 ORDER BY id LIMIT $3`,
		nsID, cp.LastFailureID, b.consolidationBatchLimit(50),
	)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch failures: %v", err))
		return
	}
	defer rows.Close()

	var failures []models.Failure
	var maxFailureID int64
	for rows.Next() {
		var f models.Failure
		if err := rows.Scan(&f.ID, &f.NamespaceID, &f.GoalID, &f.Content, &f.Reason, &f.Lesson, &f.CreatedAt, &f.DeletedAt); err != nil {
			errs = append(errs, fmt.Sprintf("scan failure: %v", err))
			continue
		}
		failures = append(failures, f)
		if f.ID > maxFailureID {
			maxFailureID = f.ID
		}
	}
	if err := rows.Err(); err != nil {
		errs = append(errs, fmt.Sprintf("failure rows: %v", err))
		return
	}

	if len(failures) == 0 {
		return
	}

	epSQL, epArgs, err := b.queries.FetchEpisodes(nsID, cp.LastFailureEpisodeID, b.consolidationBatchLimit(30))
	if err != nil {
		errs = append(errs, fmt.Sprintf("build fetch episodes for failures: %v", err))
		return
	}

	epRows, err := b.pool.Query(ctx, epSQL, epArgs...)
	if err != nil {
		errs = append(errs, fmt.Sprintf("fetch episodes for failures: %v", err))
		return
	}
	defer epRows.Close()

	var episodeTexts []string
	var maxEpisodeID int64
	for epRows.Next() {
		var e models.Episode
		// See consolidateEpisodesToFacts: NULL embedding/embedding_model must
		// not poison the rows iterator, so scan through pointers.
		var emb *pgvector.Vector
		var embModel *string
		if err := epRows.Scan(&e.ID, &e.NamespaceID, &e.Content, &emb, &embModel, &e.OccurredAt, &e.CreatedAt); err != nil {
			errs = append(errs, fmt.Sprintf("scan episode for failures: %v", err))
			continue
		}
		if emb != nil {
			e.Embedding = *emb
		}
		if embModel != nil {
			e.EmbeddingModel = *embModel
		}
		episodeTexts = append(episodeTexts, e.Content)
		if e.ID > maxEpisodeID {
			maxEpisodeID = e.ID
		}
	}
	if err := epRows.Err(); err != nil {
		errs = append(errs, fmt.Sprintf("episode rows for failures: %v", err))
		return
	}

	if len(episodeTexts) == 0 {
		if maxFailureID > cp.LastFailureID {
			cp.LastFailureID = maxFailureID
		}
		return
	}

	llmCalls++
	results, err := b.reasoner.ReasonFailurePatterns(ctx, failures, episodeTexts)
	if err != nil {
		errs = append(errs, fmt.Sprintf("reason failure patterns: %v", err))
		return
	}

	for _, r := range results {
		switch r.Type {
		case "repetition":
			content := fmt.Sprintf("REPEAT FAILURE [failure #%d]: %s", r.FailureID, r.Evidence)
			// Embed like any other episode so it is semantically recallable.
			// Fall back to NULL only if embedding fails — the scan paths now
			// tolerate NULL instead of aborting the namespace read.
			var embArg any
			embModel := ""
			if vec, embErr := b.embedder.Embed(ctx, content); embErr == nil {
				embArg = pgvector.NewVector(vec)
				embModel = b.embedder.Model()
			} else {
				errs = append(errs, fmt.Sprintf("embed repeat failure episode: %v", embErr))
			}
			_, err := b.pool.Exec(ctx,
				`INSERT INTO episodes (namespace_id, content, embedding, embedding_model, occurred_at)
				 VALUES ($1, $2, $3, $4, $5)`,
				nsID, content, embArg, embModel, time.Now().UTC(),
			)
			if err != nil {
				errs = append(errs, fmt.Sprintf("insert repeat failure episode: %v", err))
				continue
			}
			repeats++

		case "pattern":
			if r.PatternFact == "" {
				continue
			}
			vec, embErr := b.embedder.Embed(ctx, r.PatternFact)
			if embErr != nil {
				errs = append(errs, fmt.Sprintf("embed failure pattern fact: %v", embErr))
				continue
			}
			_, err := b.pool.Exec(ctx,
				`INSERT INTO facts (namespace_id, content, embedding, embedding_model, confidence, valid_from)
				 VALUES ($1, $2, $3, $4, $5, $6)`,
				nsID, r.PatternFact, pgvector.NewVector(vec), b.embedder.Model(), r.Confidence, time.Now().UTC(),
			)
			if err != nil {
				errs = append(errs, fmt.Sprintf("insert failure pattern fact: %v", err))
				continue
			}
			patterns++
		}
	}

	if len(errs) == 0 {
		if maxFailureID > cp.LastFailureID {
			cp.LastFailureID = maxFailureID
		}
		if maxEpisodeID > cp.LastFailureEpisodeID {
			cp.LastFailureEpisodeID = maxEpisodeID
		}
	}

	return
}
