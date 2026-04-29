package brain

import (
	"context"
	"fmt"
)

// GetStats returns counts for the main entities in the system.
func (b *Brain) GetStats(ctx context.Context) (map[string]int64, error) {
	stats := make(map[string]int64)

	queries := map[string]string{
		"namespaces": "SELECT COUNT(*) FROM namespaces",
		"episodes":   "SELECT COUNT(*) FROM episodes WHERE deleted_at IS NULL",
		"facts":      "SELECT COUNT(*) FROM facts WHERE deleted_at IS NULL",
		"failures":   "SELECT COUNT(*) FROM failures WHERE deleted_at IS NULL",
	}

	for key, query := range queries {
		var count int64
		err := b.pool.QueryRow(ctx, query).Scan(&count)
		if err != nil {
			return nil, fmt.Errorf("stats: failed to count %s: %w", key, err)
		}
		stats[key] = count
	}

	return stats, nil
}
