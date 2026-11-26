package brain

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// StatsTopNamespaces is how many namespaces the stats report lists, ranked by
// fact count.
const StatsTopNamespaces = 15

// ProviderInfo describes a model provider without carrying any credential. It
// holds the base-URL HOST only — never the full URL, which can embed userinfo
// or a key in its query string.
type ProviderInfo struct {
	Provider    string `json:"provider"`
	BaseURLHost string `json:"base_url_host"`
	Model       string `json:"model"`
	// CloudflareHost is true when the base-URL host is a Cloudflare-owned
	// domain (TOL-3934: Cloudflare is prohibited in the Tolowa stack). It is a
	// host-name check only and cannot see a Cloudflare proxy in front of an
	// unrelated hostname.
	CloudflareHost bool `json:"cloudflare_host"`
}

// providerByHostSuffix maps a host suffix to a friendly provider name. Order
// does not matter: suffixes are matched on a dot boundary.
var providerByHostSuffix = map[string]string{
	"deepinfra.com":     "deepinfra",
	"openai.com":        "openai",
	"openrouter.ai":     "openrouter",
	"cline.bot":         "cline",
	"anthropic.com":     "anthropic",
	"groq.com":          "groq",
	"together.xyz":      "together",
	"fireworks.ai":      "fireworks",
	"cloudflare.com":    "cloudflare",
	"cloudflare.net":    "cloudflare",
	"workers.dev":       "cloudflare",
	"pages.dev":         "cloudflare",
	"trycloudflare.com": "cloudflare",
}

var cloudflareSuffixes = []string{
	"cloudflare.com", "cloudflare.net", "workers.dev", "pages.dev", "trycloudflare.com",
}

func hostHasSuffix(host, suffix string) bool {
	return host == suffix || strings.HasSuffix(host, "."+suffix)
}

// NewProviderInfo derives a credential-free ProviderInfo from a base URL and
// model name. An unparseable URL yields an empty host rather than echoing the
// raw string back, since the raw string could contain a secret.
func NewProviderInfo(baseURL, model string) ProviderInfo {
	info := ProviderInfo{Model: model}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return info
	}
	host := strings.ToLower(u.Hostname())
	info.BaseURLHost = host
	if host == "" {
		return info
	}
	for _, suffix := range cloudflareSuffixes {
		if hostHasSuffix(host, suffix) {
			info.CloudflareHost = true
			break
		}
	}
	switch {
	case host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "host.docker.internal":
		info.Provider = "local"
	default:
		for suffix, name := range providerByHostSuffix {
			if hostHasSuffix(host, suffix) {
				info.Provider = name
				return info
			}
		}
		info.Provider = host
	}
	return info
}

// Stats is a read-only health snapshot of the store. It carries counts, ages
// and namespace slugs only — no row content and no credentials.
type Stats struct {
	GeneratedAt   time.Time          `json:"generated_at"`
	Counts        StatsCounts        `json:"counts"`
	TopNamespaces []NamespaceStats   `json:"top_namespaces_by_facts"`
	Consolidation ConsolidationStats `json:"consolidation_backlog"`
	Embedder      ProviderInfo       `json:"embedder"`
	Reasoner      ProviderInfo       `json:"reasoner"`
	Server        *ServerInfo        `json:"server,omitempty"`
}

// ServerInfo identifies the running build. It is filled in by the caller,
// which owns the build metadata.
type ServerInfo struct {
	Version string `json:"version"`
	GitSHA  string `json:"git_sha"`
}

type OpenResolved struct {
	Open     int64 `json:"open"`
	Resolved int64 `json:"resolved"`
}

type LiveExpired struct {
	Live    int64 `json:"live"`
	Expired int64 `json:"expired"`
}

// FactCounts separates facts a recall can return (live) from facts whose
// validity window has ended (expired) and facts soft-deleted, e.g. by decay.
type FactCounts struct {
	Live    int64 `json:"live"`
	Expired int64 `json:"expired"`
	Deleted int64 `json:"soft_deleted"`
}

type StatsCounts struct {
	Namespaces     int64        `json:"namespaces"`
	Episodes       int64        `json:"episodes"`
	Facts          FactCounts   `json:"facts"`
	Relationships  int64        `json:"relationships"`
	CausalLinks    int64        `json:"causal_links"`
	Contradictions OpenResolved `json:"contradictions"`
	Contexts       LiveExpired  `json:"contexts"`
}

type NamespaceStats struct {
	Namespace              string `json:"namespace"`
	Facts                  int64  `json:"facts"`
	LiveFacts              int64  `json:"live_facts"`
	UnconsolidatedEpisodes int64  `json:"unconsolidated_episodes"`
}

// ConsolidationStats describes episodes the consolidator has not yet read: an
// episode is unconsolidated when its id is above its namespace's
// consolidation_progress.last_episode_id.
type ConsolidationStats struct {
	UnconsolidatedEpisodes int64 `json:"unconsolidated_episodes"`
	// UnembeddedEpisodes is the subset with no embedding yet. The consolidator
	// will not read past one of these, so a non-zero value with a stale oldest
	// age usually means the embedder is failing.
	UnembeddedEpisodes   int64      `json:"unembedded_episodes"`
	OldestEpisodeAt      *time.Time `json:"oldest_unconsolidated_at"`
	OldestEpisodeAgeSecs *int64     `json:"oldest_unconsolidated_age_seconds"`
	// NamespacesNeverConsolidated counts namespaces with no completed
	// consolidation run, so an empty namespace can be told apart from an
	// unconsolidated one.
	NamespacesNeverConsolidated int64 `json:"namespaces_never_consolidated"`
}

const statsCountsSQL = `
SELECT
  (SELECT count(*) FROM namespaces),
  (SELECT count(*) FROM episodes WHERE deleted_at IS NULL),
  (SELECT count(*) FROM facts WHERE deleted_at IS NULL AND (valid_until IS NULL OR valid_until > now())),
  (SELECT count(*) FROM facts WHERE deleted_at IS NULL AND valid_until IS NOT NULL AND valid_until <= now()),
  (SELECT count(*) FROM facts WHERE deleted_at IS NOT NULL),
  (SELECT count(*) FROM relationships WHERE deleted_at IS NULL),
  (SELECT count(*) FROM causal_links WHERE deleted_at IS NULL),
  (SELECT count(*) FROM contradictions WHERE NOT resolved),
  (SELECT count(*) FROM contradictions WHERE resolved),
  (SELECT count(*) FROM contexts WHERE expires_at > now()),
  (SELECT count(*) FROM contexts WHERE expires_at <= now())`

const statsBacklogSQL = `
SELECT
  count(*),
  count(*) FILTER (WHERE e.embedding IS NULL),
  min(e.created_at),
  (SELECT count(*) FROM namespaces n
     LEFT JOIN consolidation_progress p ON p.namespace_id = n.id
     WHERE p.last_run IS NULL)
FROM episodes e
LEFT JOIN consolidation_progress p ON p.namespace_id = e.namespace_id
WHERE e.deleted_at IS NULL AND e.id > COALESCE(p.last_episode_id, 0)`

const statsTopNamespacesSQL = `
WITH top AS (
  SELECT n.id, n.slug,
         count(*) AS facts,
         count(*) FILTER (WHERE f.valid_until IS NULL OR f.valid_until > now()) AS live_facts
  FROM namespaces n
  JOIN facts f ON f.namespace_id = n.id AND f.deleted_at IS NULL
  GROUP BY n.id, n.slug
  ORDER BY facts DESC, n.slug
  LIMIT $1
)
SELECT t.slug, t.facts, t.live_facts,
  (SELECT count(*) FROM episodes e
     LEFT JOIN consolidation_progress p ON p.namespace_id = e.namespace_id
     WHERE e.namespace_id = t.id AND e.deleted_at IS NULL
       AND e.id > COALESCE(p.last_episode_id, 0))
FROM top t
ORDER BY t.facts DESC, t.slug`

// Stats returns a read-only health snapshot. Everything runs in one
// read-only, repeatable-read transaction so the numbers are mutually
// consistent and the database itself refuses any write.
func (b *Brain) Stats(ctx context.Context) (*Stats, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("stats: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	s := &Stats{
		GeneratedAt:   time.Now().UTC(),
		TopNamespaces: []NamespaceStats{},
		Embedder:      b.config.EmbedderInfo,
		Reasoner:      b.config.ReasonerInfo,
	}

	c := &s.Counts
	if err := tx.QueryRow(ctx, statsCountsSQL).Scan(
		&c.Namespaces, &c.Episodes,
		&c.Facts.Live, &c.Facts.Expired, &c.Facts.Deleted,
		&c.Relationships, &c.CausalLinks,
		&c.Contradictions.Open, &c.Contradictions.Resolved,
		&c.Contexts.Live, &c.Contexts.Expired,
	); err != nil {
		return nil, fmt.Errorf("stats: counts: %w", err)
	}

	var oldest *time.Time
	cs := &s.Consolidation
	if err := tx.QueryRow(ctx, statsBacklogSQL).Scan(
		&cs.UnconsolidatedEpisodes, &cs.UnembeddedEpisodes, &oldest, &cs.NamespacesNeverConsolidated,
	); err != nil {
		return nil, fmt.Errorf("stats: backlog: %w", err)
	}
	if oldest != nil {
		at := oldest.UTC()
		age := int64(s.GeneratedAt.Sub(at).Seconds())
		if age < 0 {
			age = 0
		}
		cs.OldestEpisodeAt = &at
		cs.OldestEpisodeAgeSecs = &age
	}

	rows, err := tx.Query(ctx, statsTopNamespacesSQL, StatsTopNamespaces)
	if err != nil {
		return nil, fmt.Errorf("stats: top namespaces: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ns NamespaceStats
		if err := rows.Scan(&ns.Namespace, &ns.Facts, &ns.LiveFacts, &ns.UnconsolidatedEpisodes); err != nil {
			return nil, fmt.Errorf("stats: scan top namespace: %w", err)
		}
		s.TopNamespaces = append(s.TopNamespaces, ns)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats: top namespaces: %w", err)
	}
	return s, nil
}
