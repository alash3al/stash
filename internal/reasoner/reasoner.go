// Package reasoner synthesizes structured reasoning over text.
package reasoner

import (
	"context"

	"github.com/alash3al/stash/internal/models"
)

// StructuredFact represents an extracted fact with entity, property, and value.
type StructuredFact struct {
	Entity   string
	Property string
	Value    string
	Summary  string
}

// StructuredRelationship represents an extracted relationship between two entities.
type StructuredRelationship struct {
	FromEntity   string
	RelationType string
	ToEntity     string
	Confidence   float32
}

// StructuredPattern represents an abstract pattern derived from facts and relationships.
type StructuredPattern struct {
	Content        string
	CoherenceScore float32
	SourceFactIDs  []int64
	SourceRelIDs   []int64
}

// ContradictionClassification is the result of classifying a fact pair.
type ContradictionClassification string

const (
	ClassificationReplacement   ContradictionClassification = "replacement"
	ClassificationContradiction ContradictionClassification = "contradiction"
	ClassificationCompatible    ContradictionClassification = "compatible"
)

// ContradictionResult is the LLM output for classifying two facts about the same entity+property.
type ContradictionResult struct {
	Classification ContradictionClassification
	Confidence     float32
	Explanation    string
}

// StructuredCausalLink represents an extracted cause-effect relationship between two facts.
type StructuredCausalLink struct {
	CauseFactID  int64
	EffectFactID int64
	Confidence   float32
}

// GoalProgressAssessment is the LLM output for one goal against a batch of facts.
type GoalProgressAssessment struct {
	GoalID     int64
	Assessment string  // "progress", "suggested_complete", "contradicted", "irrelevant"
	Note       string
	Confidence float32
}

// FailurePatternResult covers repetition detection and pattern extraction.
type FailurePatternResult struct {
	Type        string  // "repetition" or "pattern"
	FailureID   int64   // For repetition: the original failure ID
	Evidence    string  // For repetition: what evidence suggests the repeat
	PatternFact string  // For pattern: the extracted higher-order fact content
	Confidence  float32
}

// HypothesisEvidenceResult is the LLM output for one hypothesis against a batch of facts.
type HypothesisEvidenceResult struct {
	HypothesisID  int64
	Verdict       string  // "supports", "weakens", "contradicts", "irrelevant"
	Confidence    float32
	Reasoning     string
	NewConfidence float32
}

// Reasoner synthesizes structured reasoning over text input.
type Reasoner interface {
	// ReasonStructured takes a list of text inputs and returns a structured fact.
	ReasonStructured(ctx context.Context, texts []string) (*StructuredFact, error)

	// ReasonRelationships takes a fact and extracts relationships between entities.
	ReasonRelationships(ctx context.Context, factContent string) ([]*StructuredRelationship, error)

	// ReasonPatterns takes facts and relationships and extracts abstract patterns.
	ReasonPatterns(ctx context.Context, facts []models.Fact, relationships []models.Relationship) ([]*StructuredPattern, error)

	// ReasonContradiction classifies whether a new fact replaces, contradicts, or is compatible with an old fact.
	ReasonContradiction(ctx context.Context, entity, property, oldValue, newValue string) (*ContradictionResult, error)

	// ReasonCausalLinks takes a batch of facts and extracts cause-effect pairs.
	ReasonCausalLinks(ctx context.Context, facts []models.Fact) ([]*StructuredCausalLink, error)

	// ReasonGoalProgress assesses whether recent facts indicate progress, completion, or contradiction of active goals.
	ReasonGoalProgress(ctx context.Context, goals []models.Goal, facts []models.Fact) ([]*GoalProgressAssessment, error)

	// ReasonFailurePatterns detects whether recent evidence repeats past failures, and extracts higher-order failure patterns.
	ReasonFailurePatterns(ctx context.Context, failures []models.Failure, evidence []string) ([]*FailurePatternResult, error)

	// ReasonHypothesisEvidence assesses whether new evidence supports, weakens, or contradicts open hypotheses.
	ReasonHypothesisEvidence(ctx context.Context, hypotheses []models.Hypothesis, facts []models.Fact) ([]*HypothesisEvidenceResult, error)

	// ReasonFailureCause identifies which fact likely caused a specific failure.
	// Returns the ID of the cause fact, the confidence score, and an error if reasoning fails.
	ReasonFailureCause(ctx context.Context, failure models.Failure, facts []models.Fact) (int64, float32, error)

	// Rerank optionally reorders a set of candidate facts for a query using
	// an LLM-based relevance reranker. Returns a reordered subset of the
	// provided `results` (preserving models.Fact structure).
	Rerank(ctx context.Context, query string, results []models.Fact) ([]models.Fact, error)
}
