package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/alash3al/stash/internal/models"
	"github.com/alash3al/stash/internal/observability"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"
)

// Remember stores a new episode in the given namespace.
// If occurredAt is nil, the current time is used.
// Returns the episode ID on success.
func (b *Brain) Remember(ctx context.Context, namespaceSlug, content string, occurredAt *time.Time) (int64, error) {
	if err := validateContent(content); err != nil {
		return 0, err
	}
	if err := validatePath(namespaceSlug); err != nil {
		return 0, err
	}

	nsID, err := b.resolveNamespaceID(ctx, namespaceSlug)
	if err != nil {
		return 0, err
	}

	occurred := time.Now().UTC()
	if occurredAt != nil {
		occurred = *occurredAt
	}

	vec, embedErr := b.embedder.Embed(ctx, content)
	var embedding any
	var embeddingModel any
	if embedErr == nil {
		embedding = pgvector.NewVector(vec)
		embeddingModel = b.embedder.Model()
	} else {
		// Memory capture is the durable operation. A metered AI provider is an
		// enrichment dependency and must never be able to discard the write.
		observability.RecordDeferredEmbedding("episode")
	}

	var id int64
	err = b.pool.QueryRow(ctx,
		`INSERT INTO episodes (namespace_id, content, embedding, embedding_model, occurred_at)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		nsID, content, embedding, embeddingModel, occurred,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert episode: %w", err)
	}
	return id, nil
}

// ForgetEpisode soft-deletes the episode that best matches the query across the given namespaces.
// If namespaceSlugs is empty, searches all namespaces.
func (b *Brain) ForgetEpisode(ctx context.Context, namespaceSlugs []string, query string) error {
	if err := validateContent(query); err != nil {
		return err
	}
	for _, slug := range namespaceSlugs {
		if err := validatePath(slug); err != nil {
			return err
		}
	}

	nsIDs, err := b.resolveNamespaceIDs(ctx, namespaceSlugs)
	if err != nil {
		return err
	}

	vec, err := b.embedder.Embed(ctx, query)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}

	var id int64
	if len(nsIDs) == 1 {
		err = b.pool.QueryRow(ctx,
			`SELECT id FROM episodes
			 WHERE namespace_id = $1 AND deleted_at IS NULL AND embedding IS NOT NULL
			 ORDER BY embedding <=> $2 LIMIT 1`,
			nsIDs[0], pgvector.NewVector(vec),
		).Scan(&id)
	} else {
		err = b.pool.QueryRow(ctx,
			`SELECT id FROM episodes
			 WHERE namespace_id = ANY($1) AND deleted_at IS NULL AND embedding IS NOT NULL
			 ORDER BY embedding <=> $2 LIMIT 1`,
			nsIDs, pgvector.NewVector(vec),
		).Scan(&id)
	}
	if err != nil {
		return ErrEpisodeNotFound
	}

	_, err = b.pool.Exec(ctx,
		"UPDATE episodes SET deleted_at = now() WHERE id = $1", id,
	)
	if err != nil {
		return fmt.Errorf("soft delete episode: %w", err)
	}
	return nil
}

// PurgeEpisode hard-deletes an episode by ID.
func (b *Brain) PurgeEpisode(ctx context.Context, episodeID int64) error {
	tag, err := b.pool.Exec(ctx, "DELETE FROM episodes WHERE id = $1", episodeID)
	if err != nil {
		return fmt.Errorf("purge episode: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrEpisodeNotFound
	}
	return nil
}

// GetEpisode returns a single episode by ID.
func (b *Brain) GetEpisode(ctx context.Context, episodeID int64) (*models.Episode, error) {
	var e models.Episode
	var embeddingText pgtype.Text
	var embeddingModel pgtype.Text
	err := b.pool.QueryRow(ctx,
		`SELECT id, namespace_id, content, embedding::text, embedding_model, occurred_at, created_at, deleted_at
		 FROM episodes WHERE id = $1`,
		episodeID,
	).Scan(&e.ID, &e.NamespaceID, &e.Content, &embeddingText, &embeddingModel, &e.OccurredAt, &e.CreatedAt, &e.DeletedAt)
	if err != nil {
		return nil, ErrEpisodeNotFound
	}
	if embeddingText.Valid {
		if err := e.Embedding.Parse(embeddingText.String); err != nil {
			return nil, fmt.Errorf("parse episode embedding: %w", err)
		}
	}
	if embeddingModel.Valid {
		e.EmbeddingModel = embeddingModel.String
	}
	return &e, nil
}
