package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/alash3al/stash/internal/models"
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

	vec, err := b.embedder.Embed(ctx, content)
	if err != nil {
		return 0, fmt.Errorf("embed: %w", err)
	}

	var id int64
	err = b.pool.QueryRow(ctx,
		`INSERT INTO episodes (namespace_id, content, embedding, embedding_model, occurred_at)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		nsID, content, pgvector.NewVector(vec), b.embedder.Model(), occurred,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert episode: %w", err)
	}
	return id, nil
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
	err := b.pool.QueryRow(ctx,
		`SELECT id, namespace_id, content, embedding, embedding_model, occurred_at, created_at, deleted_at
		 FROM episodes WHERE id = $1`,
		episodeID,
	).Scan(&e.ID, &e.NamespaceID, &e.Content, &e.Embedding, &e.EmbeddingModel, &e.OccurredAt, &e.CreatedAt, &e.DeletedAt)
	if err != nil {
		return nil, ErrEpisodeNotFound
	}
	return &e, nil
}
