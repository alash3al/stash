package brain

import (
	"context"
	"fmt"

	"github.com/alash3al/stash/internal/embedder"
	"github.com/pgvector/pgvector-go"
)

// BackfillMissingEmbeddings enriches writes that were preserved while the
// embedding provider was unavailable. It is bounded, ordered, and stops after
// the first provider failure so an outage cannot create a retry storm.
func (b *Brain) BackfillMissingEmbeddings(ctx context.Context) (int, error) {
	if err := embedder.Availability(b.embedder); err != nil {
		return 0, err
	}

	type candidate struct {
		table   string
		id      int64
		content string
	}
	rows, err := b.pool.Query(ctx, `
		SELECT memory_table, id, content FROM (
			SELECT 'episodes'::text AS memory_table, id, content, created_at
			FROM episodes WHERE embedding IS NULL AND deleted_at IS NULL
			UNION ALL
			SELECT 'facts'::text AS memory_table, id, content, created_at
			FROM facts WHERE embedding IS NULL AND deleted_at IS NULL
		) missing
		ORDER BY created_at, id LIMIT $1`, b.config.EmbeddingBackfillBatch)
	if err != nil {
		return 0, fmt.Errorf("query missing embeddings: %w", err)
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.table, &c.id, &c.content); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan missing embedding: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("missing embedding rows: %w", err)
	}
	rows.Close()

	updated := 0
	for _, c := range candidates {
		vec, err := b.embedder.Embed(ctx, c.content)
		if err != nil {
			return updated, fmt.Errorf("backfill %s %d: %w", c.table, c.id, err)
		}
		query := "UPDATE episodes SET embedding = $1, embedding_model = $2 WHERE id = $3 AND embedding IS NULL"
		if c.table == "facts" {
			query = "UPDATE facts SET embedding = $1, embedding_model = $2, updated_at = now() WHERE id = $3 AND embedding IS NULL"
		}
		tag, err := b.pool.Exec(ctx, query, pgvector.NewVector(vec), b.embedder.Model(), c.id)
		if err != nil {
			return updated, fmt.Errorf("update %s embedding %d: %w", c.table, c.id, err)
		}
		updated += int(tag.RowsAffected())
	}
	return updated, nil
}
