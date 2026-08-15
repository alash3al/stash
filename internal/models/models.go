// Package models defines domain structs for pgx scanning.
// Every field tag matches the PostgreSQL column name exactly.
package models

import (
	"time"

	"github.com/pgvector/pgvector-go"
)

// Namespace owns memory.
type Namespace struct {
	ID          int64     `db:"id"`
	Slug        string    `db:"slug"`
	Name        string    `db:"name"`
	Description string    `db:"description"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

// Episode is an immutable, append-only raw event.
type Episode struct {
	ID             int64           `db:"id"`
	NamespaceID    int64           `db:"namespace_id"`
	Content        string          `db:"content"`
	Embedding      pgvector.Vector `db:"embedding"`
	EmbeddingModel string          `db:"embedding_model"`
	OccurredAt     time.Time       `db:"occurred_at"`
	CreatedAt      time.Time       `db:"created_at"`
	DeletedAt      *time.Time      `db:"deleted_at"`
}

// Fact is a belief derived from episodes.
type Fact struct {
	ID             int64           `db:"id"`
	NamespaceID    int64           `db:"namespace_id"`
	Content        string          `db:"content"`
	Embedding      pgvector.Vector `db:"embedding"`
	EmbeddingModel string          `db:"embedding_model"`
	Confidence     float32         `db:"confidence"`
	Entity         *string         `db:"entity"`
	Property       *string         `db:"property"`
	Value          *string         `db:"value"`
	ValidFrom      *time.Time      `db:"valid_from"`
	ValidUntil     *time.Time      `db:"valid_until"`
	CreatedAt      time.Time       `db:"created_at"`
	UpdatedAt      time.Time       `db:"updated_at"`
	DeletedAt      *time.Time      `db:"deleted_at"`
}

// Contradiction records a conflict between two facts about the same entity and property.
type Contradiction struct {
	ID          int64      `db:"id"`
	NamespaceID int64      `db:"namespace_id"`
	OldFactID   int64      `db:"old_fact_id"`
	NewFactID   int64      `db:"new_fact_id"`
	Entity      string     `db:"entity"`
	Property    string     `db:"property"`
	OldValue    string     `db:"old_value"`
	NewValue    string     `db:"new_value"`
	Confidence  float32    `db:"confidence"`
	Method      string     `db:"method"`
	Resolved    bool       `db:"resolved"`
	Resolution  *string    `db:"resolution"`
	ResolvedAt  *time.Time `db:"resolved_at"`
	CreatedAt   time.Time  `db:"created_at"`
}

// FactSource links a fact to the episodes that support it.
type FactSource struct {
	FactID    int64 `db:"fact_id"`
	EpisodeID int64 `db:"episode_id"`
}

// Relationship is an extracted entity edge.
type Relationship struct {
	ID           int64      `db:"id"`
	NamespaceID  int64      `db:"namespace_id"`
	FromEntity   string     `db:"from_entity"`
	RelationType string     `db:"relation_type"`
	ToEntity     string     `db:"to_entity"`
	Confidence   float32    `db:"confidence"`
	SourceFactID *int64     `db:"source_fact_id"`
	CreatedAt    time.Time  `db:"created_at"`
	DeletedAt    *time.Time `db:"deleted_at"`
}

// Pattern is an abstraction over facts and relationships.
type Pattern struct {
	ID             int64      `db:"id"`
	NamespaceID    int64      `db:"namespace_id"`
	Content        string     `db:"content"`
	Confidence     float32    `db:"confidence"`
	SourceFactIDs  []int64    `db:"source_fact_ids"`
	SourceRelIDs   []int64    `db:"source_rel_ids"`
	CoherenceScore float32    `db:"coherence_score"`
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
	DeletedAt      *time.Time `db:"deleted_at"`
}

// Context is the active working state for a namespace.
type Context struct {
	NamespaceID int64     `db:"namespace_id"`
	Focus       string    `db:"focus"`
	ExpiresAt   time.Time `db:"expires_at"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

// CausalLink records a cause-effect relationship between two facts.
type CausalLink struct {
	ID           int64      `db:"id"`
	NamespaceID  int64      `db:"namespace_id"`
	CauseFactID  int64      `db:"cause_fact_id"`
	EffectFactID int64      `db:"effect_fact_id"`
	Confidence   float32    `db:"confidence"`
	Method       string     `db:"method"`
	CreatedAt    time.Time  `db:"created_at"`
	DeletedAt    *time.Time `db:"deleted_at"`
}

// Hypothesis is a belief held with uncertainty plus a plan to verify it.
type Hypothesis struct {
	ID               int64      `db:"id"`
	NamespaceID      int64      `db:"namespace_id"`
	Content          string     `db:"content"`
	Confidence       float32    `db:"confidence"`
	Status           string     `db:"status"`
	VerificationPlan string     `db:"verification_plan"`
	Method           string     `db:"method"`
	ConfirmedFactID  *int64     `db:"confirmed_fact_id"`
	RejectionReason  *string    `db:"rejection_reason"`
	SourceFactIDs    []int64    `db:"source_fact_ids"`
	TestedAt         *time.Time `db:"tested_at"`
	ConfirmedAt      *time.Time `db:"confirmed_at"`
	RejectedAt       *time.Time `db:"rejected_at"`
	CreatedAt        time.Time  `db:"created_at"`
	UpdatedAt        time.Time  `db:"updated_at"`
	DeletedAt        *time.Time `db:"deleted_at"`
}

// Goal is an intended outcome that persists across sessions.
type Goal struct {
	ID          int64      `db:"id"`
	NamespaceID int64      `db:"namespace_id"`
	ParentID    *int64     `db:"parent_id"`
	Content     string     `db:"content"`
	Status      string     `db:"status"`
	Priority    int        `db:"priority"`
	Notes       string     `db:"notes"`
	CompletedAt *time.Time `db:"completed_at"`
	AbandonedAt *time.Time `db:"abandoned_at"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
	DeletedAt   *time.Time `db:"deleted_at"`
}

// Failure records what didn't work, why, and what to do instead.
type Failure struct {
	ID          int64      `db:"id"`
	NamespaceID int64      `db:"namespace_id"`
	GoalID      *int64     `db:"goal_id"`
	Content     string     `db:"content"`
	Reason      string     `db:"reason"`
	Lesson      string     `db:"lesson"`
	CreatedAt   time.Time  `db:"created_at"`
	DeletedAt   *time.Time `db:"deleted_at"`
}

// ConsolidationProgress tracks per-stage checkpoint per namespace.
type ConsolidationProgress struct {
	NamespaceID            int64      `db:"namespace_id"`
	LastEpisodeID          int64      `db:"last_episode_id"`
	LastFactID             int64      `db:"last_fact_id"`
	LastRelationshipID     int64      `db:"last_relationship_id"`
	LastPatternFactID      int64      `db:"last_pattern_fact_id"`
	LastPatternRelID       int64      `db:"last_pattern_rel_id"`
	LastGoalProgressFactID int64      `db:"last_goal_progress_fact_id"`
	LastFailureID          int64      `db:"last_failure_id"`
	LastFailureEpisodeID   int64      `db:"last_failure_episode_id"`
	LastHypothesisFactID   int64      `db:"last_hypothesis_fact_id"`
	LastCausalFactID       int64      `db:"last_causal_fact_id"`
	LastDecayRun           *time.Time `db:"last_decay_run"`
	LastRun                *time.Time `db:"last_run"`
	UpdatedAt              time.Time  `db:"updated_at"`
}

// Setting is a key-value store for operational state.
type Setting struct {
	Key       string    `db:"key"`
	Value     string    `db:"value"`
	UpdatedAt time.Time `db:"updated_at"`
}

// EmbeddingCache stores computed embeddings by text hash and model.
type EmbeddingCache struct {
	TextHash  string          `db:"text_hash"`
	Model     string          `db:"model"`
	Text      string          `db:"text"`
	Embedding pgvector.Vector `db:"embedding"`
	CreatedAt time.Time       `db:"created_at"`
}

// RecallResult carries a row from recall queries.
type RecallResult struct {
	ID             int64           `db:"id"`
	NamespaceID    int64           `db:"namespace_id"`
	Content        string          `db:"content"`
	Embedding      pgvector.Vector `db:"embedding"`
	EmbeddingModel string          `db:"embedding_model"`
	OccurredAt     time.Time       `db:"occurred_at"`
	CreatedAt      time.Time       `db:"created_at"`
	Score          float32         `db:"score"`
}
