package brain

import (
	"context"
	"fmt"
	"time"

	"github.com/alash3al/stash/internal/models"
	"github.com/jackc/pgx/v5"
)

// SetContext updates the working context for a namespace.
func (b *Brain) SetContext(ctx context.Context, namespaceSlug, focus string, expiresAt time.Time) error {
	if err := validatePath(namespaceSlug); err != nil {
		return err
	}
	if err := validateContent(focus); err != nil {
		return err
	}
	nsID, err := b.resolveNamespaceID(ctx, namespaceSlug)
	if err != nil {
		return err
	}

	_, err = b.pool.Exec(ctx,
		`INSERT INTO contexts (namespace_id, focus, expires_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (namespace_id) DO UPDATE SET focus = $2, expires_at = $3, updated_at = now()`,
		nsID, focus, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("set context: %w", err)
	}
	return nil
}

// GetContext returns the working context for a namespace.
func (b *Brain) GetContext(ctx context.Context, namespaceSlug string) (*models.Context, error) {
	if err := validatePath(namespaceSlug); err != nil {
		return nil, err
	}
	nsID, err := b.resolveNamespaceID(ctx, namespaceSlug)
	if err != nil {
		return nil, err
	}

	var c models.Context
	err = b.pool.QueryRow(ctx,
		`SELECT namespace_id, focus, expires_at, created_at, updated_at
		 FROM contexts WHERE namespace_id = $1`,
		nsID,
	).Scan(&c.NamespaceID, &c.Focus, &c.ExpiresAt, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get context: %w", err)
	}
	// Populate persona from the namespace and prepend it to the focus
	if ns, err := b.GetNamespaceByID(ctx, nsID); err == nil {
		c.Persona = ns.Persona
		if c.Persona != "" {
			if c.Focus != "" {
				c.Focus = c.Persona + "\n\n" + c.Focus
			} else {
				c.Focus = c.Persona
			}
		}
	}

	return &c, nil
}

// ClearContext removes the working context for a namespace.
func (b *Brain) ClearContext(ctx context.Context, namespaceSlug string) error {
	if err := validatePath(namespaceSlug); err != nil {
		return err
	}
	nsID, err := b.resolveNamespaceID(ctx, namespaceSlug)
	if err != nil {
		return err
	}

	_, err = b.pool.Exec(ctx, "DELETE FROM contexts WHERE namespace_id = $1", nsID)
	if err != nil {
		return fmt.Errorf("clear context: %w", err)
	}
	return nil
}
