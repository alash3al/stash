package reasoner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/alash3al/stash/internal/models"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

const systemPrompt = `You are a strict information extraction engine.

Rules:
- Extract ONLY what is explicitly stated in the provided text.
- You may use language understanding to interpret what is written.
- You may NOT infer, assume, generalize, or fill in gaps from your own knowledge.
- If a field cannot be found in the text, output null for that field.
- Never guess. Never approximate. Never complete missing information.
- When in doubt, output null.
- Output ONLY valid JSON matching the provided schema.
- No explanation. No preamble. No markdown. Just the JSON object.`

const retryWarning = "Your previous response was invalid or contained invented information. Follow the rules strictly. Output ONLY valid JSON matching the provided schema."

type OpenAI struct {
	client              openai.Client
	model               string
	maxTokens           int64
	rateLimitCooldown   time.Duration
	paymentCooldown     time.Duration
	serverErrorCooldown time.Duration
	now                 func() time.Time
	circuitMu           sync.Mutex
	circuitUntil        time.Time
	circuitStatus       int
}

// OpenAIConfig controls bounded provider retries and the process-local circuit
// breaker. The SDK already honors Retry-After and retries 429/5xx responses;
// the circuit prevents every remaining consolidation stage and namespace from
// immediately repeating an exhausted request budget.
type OpenAIConfig struct {
	MaxTokens           int64
	MaxRetries          int
	RateLimitCooldown   time.Duration
	PaymentCooldown     time.Duration
	ServerErrorCooldown time.Duration
}

// UnavailableError is returned while the provider circuit is open.
type UnavailableError struct {
	StatusCode int
	RetryAt    time.Time
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("reasoner provider unavailable (status %d) until %s", e.StatusCode, e.RetryAt.UTC().Format(time.RFC3339))
}

// IsUnavailable reports whether an error means the reasoner circuit is open.
func IsUnavailable(err error) bool {
	var unavailable *UnavailableError
	return errors.As(err, &unavailable)
}

func NewOpenAI(baseURL, apiKey, model string) (*OpenAI, error) {
	return NewOpenAIWithConfig(baseURL, apiKey, model, OpenAIConfig{})
}

func NewOpenAIWithConfig(baseURL, apiKey, model string, cfg OpenAIConfig) (*OpenAI, error) {
	if apiKey == "" {
		return nil, errors.New("reasoner: apiKey is required")
	}
	if model == "" {
		return nil, errors.New("reasoner: model is required")
	}

	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 4096
	}
	if cfg.MaxRetries < 0 {
		return nil, errors.New("reasoner: max retries cannot be negative")
	}
	if cfg.RateLimitCooldown <= 0 {
		cfg.RateLimitCooldown = 5 * time.Minute
	}
	if cfg.PaymentCooldown <= 0 {
		cfg.PaymentCooldown = time.Hour
	}
	if cfg.ServerErrorCooldown <= 0 {
		cfg.ServerErrorCooldown = time.Minute
	}

	client := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(cfg.MaxRetries),
	)

	return &OpenAI{
		client:              client,
		model:               model,
		maxTokens:           cfg.MaxTokens,
		rateLimitCooldown:   cfg.RateLimitCooldown,
		paymentCooldown:     cfg.PaymentCooldown,
		serverErrorCooldown: cfg.ServerErrorCooldown,
		now:                 time.Now,
	}, nil
}

// Availability returns an error without making a provider request when the
// circuit is open. It is intentionally exposed so consolidation can skip
// downstream LLM stages instead of generating a storm of identical failures.
func (o *OpenAI) Availability() error {
	o.circuitMu.Lock()
	defer o.circuitMu.Unlock()
	now := o.now()
	if o.circuitUntil.IsZero() || !now.Before(o.circuitUntil) {
		o.circuitUntil = time.Time{}
		o.circuitStatus = 0
		return nil
	}
	return &UnavailableError{StatusCode: o.circuitStatus, RetryAt: o.circuitUntil}
}

func (o *OpenAI) openCircuit(status int, cooldown time.Duration) error {
	o.circuitMu.Lock()
	defer o.circuitMu.Unlock()
	retryAt := o.now().Add(cooldown)
	if retryAt.After(o.circuitUntil) {
		o.circuitUntil = retryAt
		o.circuitStatus = status
	}
	return &UnavailableError{StatusCode: o.circuitStatus, RetryAt: o.circuitUntil}
}

func (o *OpenAI) completion(ctx context.Context, msgs []openai.ChatCompletionMessageParamUnion) (*openai.ChatCompletion, error) {
	if err := o.Availability(); err != nil {
		return nil, err
	}
	resp, err := o.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:     o.model,
		Messages:  msgs,
		MaxTokens: openai.Int(o.maxTokens),
	})
	if err == nil {
		return resp, nil
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 429:
			return nil, o.openCircuit(apiErr.StatusCode, o.rateLimitCooldown)
		case 402:
			return nil, o.openCircuit(apiErr.StatusCode, o.paymentCooldown)
		default:
			if apiErr.StatusCode >= 500 {
				return nil, o.openCircuit(apiErr.StatusCode, o.serverErrorCooldown)
			}
		}
	}
	return nil, fmt.Errorf("chat.completions call failed: %w", err)
}

// --- JSON response types ---

type jsonFact struct {
	Entity   *string `json:"entity"`
	Property *string `json:"property"`
	Value    *string `json:"value"`
	Summary  *string `json:"summary"`
}

type jsonRelationship struct {
	From         string  `json:"from"`
	RelationType string  `json:"relation_type"`
	To           string  `json:"to"`
	Confidence   float32 `json:"confidence"`
}

type jsonPattern struct {
	Pattern     string  `json:"pattern"`
	Coherence   float32 `json:"coherence"`
	SourceFacts []int64 `json:"source_facts"`
	SourceRels  []int64 `json:"source_rels"`
}

type jsonContradiction struct {
	Classification string  `json:"classification"`
	Confidence     float32 `json:"confidence"`
	Explanation    string  `json:"explanation"`
}

type jsonCausalLink struct {
	CauseID    int64   `json:"cause_id"`
	EffectID   int64   `json:"effect_id"`
	Confidence float32 `json:"confidence"`
}

type jsonGoalProgress struct {
	GoalID     int64   `json:"goal_id"`
	Assessment string  `json:"assessment"`
	Note       string  `json:"note"`
	Confidence float32 `json:"confidence"`
}

type jsonFailurePattern struct {
	Type        string  `json:"type"`
	FailureID   int64   `json:"failure_id"`
	Evidence    string  `json:"evidence"`
	PatternFact string  `json:"pattern_fact"`
	Confidence  float32 `json:"confidence"`
}

type jsonHypothesisEvidence struct {
	HypothesisID  int64   `json:"hypothesis_id"`
	Verdict       string  `json:"verdict"`
	Confidence    float32 `json:"confidence"`
	Reasoning     string  `json:"reasoning"`
	NewConfidence float32 `json:"new_confidence"`
}

// --- ReasonStructured ---

func (o *OpenAI) ReasonStructured(ctx context.Context, texts []string) (*StructuredFact, error) {
	if len(texts) == 0 {
		return nil, errors.New("reasoner: texts must not be empty")
	}

	eventsList := strings.Join(texts, "\n- ")
	prompt := fmt.Sprintf(`Given these events, synthesize a single structured fact.

Events:
- %s

Output ONLY this exact JSON structure:
{"entity": "string or null", "property": "string or null", "value": "string or null", "summary": "string or null"}

Rules:
- The summary MUST synthesize the Events into a specific, detailed statement.
- Do NOT echo, quote, or closely paraphrase any single Event. Rewrite in your own words.
- Remove first-person markers ("I", "my", "we") — state facts about subjects, not the narrator.
- All values MUST come ONLY from the Events. Do not add outside details.
- If any field is not explicitly stated in the Events, use null.
- PRESERVE specifics: proper nouns, library names, version numbers, file paths, technical terms, and quantities.
- Do NOT replace specifics with vague words like "various", "components", "functionalities", "features", "certain".
- The entity should be the most specific subject (e.g., "pgxpool" not "the system").

BAD example (vague — DO NOT do this):
{"entity": "Stash", "property": "components", "value": "various functionalities", "summary": "Stash comprises various components for different functionalities."}

BAD example (verbatim copy — DO NOT do this):
{"entity": "I", "property": "currently_testing", "value": "the Stash memory system", "summary": "I am currently testing the Stash memory system."}

GOOD example (specific and synthesized):
{"entity": "Stash", "property": "database", "value": "PostgreSQL with pgvector", "summary": "Stash uses PostgreSQL with pgvector extension for vector storage and HNSW indexes."}`, eventsList)

	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(prompt),
	}

	var result *StructuredFact
	var valErr error

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := o.completion(ctx, msgs)
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, errors.New("reasoner: no response from LLM")
		}

		output := strings.TrimSpace(resp.Choices[0].Message.Content)
		raw := extractJSON(output)

		var jf jsonFact
		if err := json.Unmarshal([]byte(raw), &jf); err != nil {
			valErr = fmt.Errorf("parse json: %w", err)
			msgs = append(msgs, openai.SystemMessage(retryWarning))
			continue
		}

		result = &StructuredFact{
			Entity:   ptrStr(jf.Entity),
			Property: ptrStr(jf.Property),
			Value:    ptrStr(jf.Value),
			Summary:  ptrStr(jf.Summary),
		}

		if err := validateFactGrounding(result, texts); err != nil {
			valErr = fmt.Errorf("grounding validation: %w", err)
			msgs = append(msgs, openai.SystemMessage(retryWarning+" "+err.Error()))
			result = nil
			continue
		}

		valErr = nil
		break
	}

	if valErr != nil {
		return nil, valErr
	}

	return result, nil
}

// --- ReasonRelationships ---

func (o *OpenAI) ReasonRelationships(ctx context.Context, factContent string) ([]*StructuredRelationship, error) {
	if factContent == "" {
		return nil, errors.New("reasoner: factContent must not be empty")
	}

	prompt := fmt.Sprintf(`Given this fact, extract all directly stated relationships.

Fact: %s

Output ONLY a JSON array of objects:
[{"from": "subject entity", "relation_type": "lowercase_type", "to": "object entity", "confidence": 0.0}]

Rules:
- Only extract relationships DIRECTLY stated in the Fact above.
- Do NOT infer transitive or implied relationships (e.g., if "Alice works at TechCorp in Paris", do NOT output Alice --located_in--> Paris).
- "from" and "to" must be entity names mentioned verbatim in the Fact.
- relation_type must be a simple lowercase identifier (e.g., works_at, manages, knows).
- confidence must be between 0.7 and 1.0.
- If no relationships are explicitly stated, output: []`, factContent)

	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(prompt),
	}

	var result []*StructuredRelationship
	var valErr error

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := o.completion(ctx, msgs)
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, errors.New("reasoner: no response from LLM")
		}

		output := strings.TrimSpace(resp.Choices[0].Message.Content)
		raw := extractJSON(output)

		var jrels []jsonRelationship
		if err := json.Unmarshal([]byte(raw), &jrels); err != nil {
			valErr = fmt.Errorf("parse json: %w", err)
			msgs = append(msgs, openai.SystemMessage(retryWarning))
			continue
		}

		var validated []*StructuredRelationship
		groundingFailed := false
		for _, jr := range jrels {
			if jr.From == "" || jr.RelationType == "" || jr.To == "" {
				continue
			}
			if !stringsContains(factContent, jr.From) || !stringsContains(factContent, jr.To) {
				groundingFailed = true
				break
			}
			conf := jr.Confidence
			if conf < 0.7 {
				conf = 0.7
			}
			if conf > 1.0 {
				conf = 1.0
			}
			validated = append(validated, &StructuredRelationship{
				FromEntity:   jr.From,
				RelationType: jr.RelationType,
				ToEntity:     jr.To,
				Confidence:   conf,
			})
		}

		if groundingFailed {
			valErr = errors.New("relationship entity not found in fact content")
			msgs = append(msgs, openai.SystemMessage(retryWarning+" Entity names must appear verbatim in the fact."))
			continue
		}

		result = validated
		valErr = nil
		break
	}

	if valErr != nil {
		return nil, valErr
	}

	return result, nil
}

// --- ReasonPatterns ---

func (o *OpenAI) ReasonPatterns(ctx context.Context, facts []models.Fact, relationships []models.Relationship) ([]*StructuredPattern, error) {
	if len(facts) == 0 {
		return nil, nil
	}

	var factLines []string
	for _, f := range facts {
		factLines = append(factLines, fmt.Sprintf("[Fact %d] %s (confidence: %.2f)", f.ID, f.Content, f.Confidence))
	}

	var relLines []string
	for _, r := range relationships {
		relLines = append(relLines, fmt.Sprintf("[Rel %d] %s --%s--> %s (confidence: %.2f)", r.ID, r.FromEntity, r.RelationType, r.ToEntity, r.Confidence))
	}

	prompt := fmt.Sprintf(`Given these facts and relationships, extract patterns.

Facts:
%s

Relationships:
%s

Output ONLY a JSON array of objects:
[{"pattern": "string", "coherence": 0.0, "source_facts": [1,2], "source_rels": [3,4]}]

Rules:
- A pattern MUST have at least 2 items in source_facts OR at least 2 items in source_rels. Single-source patterns are NOT allowed.
- source_facts IDs must be from the Facts list above.
- source_rels IDs must be from the Relationships list above.
- Do not generalize beyond the evidence. If no pattern meets this threshold, output: []
- The pattern statement must be directly supported by the referenced sources.`, strings.Join(factLines, "\n"), strings.Join(relLines, "\n"))

	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(prompt),
	}

	factIDs := make(map[int64]bool)
	for _, f := range facts {
		factIDs[f.ID] = true
	}
	relIDs := make(map[int64]bool)
	for _, r := range relationships {
		relIDs[r.ID] = true
	}

	var result []*StructuredPattern
	var valErr error

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := o.completion(ctx, msgs)
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, errors.New("reasoner: no response from LLM")
		}

		output := strings.TrimSpace(resp.Choices[0].Message.Content)
		raw := extractJSON(output)

		var jpats []jsonPattern
		if err := json.Unmarshal([]byte(raw), &jpats); err != nil {
			valErr = fmt.Errorf("parse json: %w", err)
			msgs = append(msgs, openai.SystemMessage(retryWarning))
			continue
		}

		var validated []*StructuredPattern
		validationFailed := false

		for _, jp := range jpats {
			if jp.Pattern == "" {
				continue
			}

			validFacts, badFacts := filterIDs(jp.SourceFacts, factIDs)
			validRels, badRels := filterIDs(jp.SourceRels, relIDs)

			if badFacts || badRels {
				validationFailed = true
				break
			}

			if len(validFacts)+len(validRels) < 2 {
				continue
			}

			coherence := jp.Coherence
			if coherence <= 0 || coherence > 1.0 {
				coherence = 0.5
			}

			validated = append(validated, &StructuredPattern{
				Content:        jp.Pattern,
				CoherenceScore: coherence,
				SourceFactIDs:  validFacts,
				SourceRelIDs:   validRels,
			})
		}

		if validationFailed {
			valErr = errors.New("pattern references non-existent source IDs")
			msgs = append(msgs, openai.SystemMessage(retryWarning+" Source IDs must be from the provided lists only."))
			continue
		}

		result = validated
		valErr = nil
		break
	}

	if valErr != nil {
		return nil, valErr
	}

	return result, nil
}

// --- ReasonContradiction ---

func (o *OpenAI) ReasonContradiction(ctx context.Context, entity, property, oldValue, newValue string) (*ContradictionResult, error) {
	if entity == "" || property == "" || oldValue == "" || newValue == "" {
		return nil, errors.New("reasoner: entity, property, oldValue, and newValue are required")
	}

	prompt := fmt.Sprintf(`Given two facts about the same entity and property, classify their relationship.

Entity: %s
Property: %s
Old value: %s
New value: %s

Output ONLY this exact JSON structure:
{"classification": "replacement|contradiction|compatible", "confidence": 0.0, "explanation": "string"}

Rules:
- "replacement": The new value replaces the old one (e.g., changed address, updated title, new phone number). The old value is no longer true.
- "contradiction": Both values cannot be true simultaneously, but it is unclear which is correct. Requires human resolution.
- "compatible": Both values can be true at the same time (e.g., multiple roles, parallel attributes). No conflict.
- confidence must be between 0.0 and 1.0.
- explanation must be a brief sentence justifying the classification.`, entity, property, oldValue, newValue)

	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(prompt),
	}

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := o.completion(ctx, msgs)
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, errors.New("reasoner: no response from LLM")
		}

		output := strings.TrimSpace(resp.Choices[0].Message.Content)
		raw := extractJSON(output)

		var jc jsonContradiction
		if err := json.Unmarshal([]byte(raw), &jc); err != nil {
			msgs = append(msgs, openai.SystemMessage(retryWarning))
			continue
		}

		class := ContradictionClassification(jc.Classification)
		switch class {
		case ClassificationReplacement, ClassificationContradiction, ClassificationCompatible:
		default:
			msgs = append(msgs, openai.SystemMessage(retryWarning+" Classification must be one of: replacement, contradiction, compatible."))
			continue
		}

		conf := jc.Confidence
		if conf < 0 {
			conf = 0
		}
		if conf > 1 {
			conf = 1
		}

		return &ContradictionResult{
			Classification: class,
			Confidence:     conf,
			Explanation:    jc.Explanation,
		}, nil
	}

	return nil, errors.New("reasoner: failed to get valid contradiction classification after retries")
}

// --- ReasonCausalLinks ---

func (o *OpenAI) ReasonCausalLinks(ctx context.Context, facts []models.Fact) ([]*StructuredCausalLink, error) {
	if len(facts) < 2 {
		return nil, nil
	}

	var factLines []string
	for _, f := range facts {
		factLines = append(factLines, fmt.Sprintf("[Fact %d] %s", f.ID, f.Content))
	}

	prompt := fmt.Sprintf(`Given these facts, identify cause-effect relationships between them.

Facts:
%s

Output ONLY a JSON array of objects:
[{"cause_id": 1, "effect_id": 2, "confidence": 0.9}]

Rules:
- cause_id and effect_id must be fact IDs from the list above.
- Only identify relationships where one fact DIRECTLY caused or led to another.
- Do NOT infer transitive or indirect causation.
- A fact can be both a cause and an effect of different facts.
- cause_id and effect_id must be different (no self-loops).
- confidence must be between 0.5 and 1.0.
- If no causal relationships are evident, output: []`, strings.Join(factLines, "\n"))

	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(prompt),
	}

	factIDs := make(map[int64]bool)
	for _, f := range facts {
		factIDs[f.ID] = true
	}

	var result []*StructuredCausalLink
	var valErr error

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := o.completion(ctx, msgs)
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, errors.New("reasoner: no response from LLM")
		}

		output := strings.TrimSpace(resp.Choices[0].Message.Content)
		raw := extractJSON(output)

		var jlinks []jsonCausalLink
		if err := json.Unmarshal([]byte(raw), &jlinks); err != nil {
			valErr = fmt.Errorf("parse json: %w", err)
			msgs = append(msgs, openai.SystemMessage(retryWarning))
			continue
		}

		var validated []*StructuredCausalLink
		validationFailed := false

		for _, jl := range jlinks {
			if !factIDs[jl.CauseID] || !factIDs[jl.EffectID] {
				validationFailed = true
				break
			}
			if jl.CauseID == jl.EffectID {
				continue
			}

			conf := jl.Confidence
			if conf < 0.5 {
				conf = 0.5
			}
			if conf > 1.0 {
				conf = 1.0
			}

			validated = append(validated, &StructuredCausalLink{
				CauseFactID:  jl.CauseID,
				EffectFactID: jl.EffectID,
				Confidence:   conf,
			})
		}

		if validationFailed {
			valErr = errors.New("causal link references non-existent fact ID")
			msgs = append(msgs, openai.SystemMessage(retryWarning+" Fact IDs must be from the provided list only."))
			continue
		}

		result = validated
		valErr = nil
		break
	}

	if valErr != nil {
		return nil, valErr
	}

	return result, nil
}

// --- ReasonGoalProgress ---

func (o *OpenAI) ReasonGoalProgress(ctx context.Context, goals []models.Goal, facts []models.Fact) ([]*GoalProgressAssessment, error) {
	if len(goals) == 0 || len(facts) == 0 {
		return nil, nil
	}

	var goalLines []string
	for _, g := range goals {
		goalLines = append(goalLines, fmt.Sprintf("[Goal %d] %s", g.ID, g.Content))
	}

	var factLines []string
	for _, f := range facts {
		factLines = append(factLines, fmt.Sprintf("[Fact %d] %s", f.ID, f.Content))
	}

	prompt := fmt.Sprintf(`Given these active goals and recent facts, assess whether each fact indicates progress toward, completion of, or contradiction of any goal.

Active Goals:
%s

Recent Facts:
%s

Output ONLY a JSON array of objects:
[{"goal_id": 1, "assessment": "progress|suggested_complete|contradicted|irrelevant", "note": "brief explanation", "confidence": 0.9}]

Rules:
- goal_id must be a goal ID from the list above.
- Use "progress" if the fact indicates partial progress toward the goal.
- Use "suggested_complete" only if the fact strongly indicates the goal is fully achieved.
- Use "contradicted" if the fact contradicts or undermines the goal.
- Use "irrelevant" if the fact has no bearing on the goal.
- Only output assessments for goals where the facts are relevant. If no goals are affected, output: []
- confidence must be between 0.5 and 1.0.`, strings.Join(goalLines, "\n"), strings.Join(factLines, "\n"))

	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(prompt),
	}

	goalIDs := make(map[int64]bool)
	for _, g := range goals {
		goalIDs[g.ID] = true
	}

	var result []*GoalProgressAssessment
	var valErr error

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := o.completion(ctx, msgs)
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, errors.New("reasoner: no response from LLM")
		}

		output := strings.TrimSpace(resp.Choices[0].Message.Content)
		raw := extractJSON(output)

		var jassess []jsonGoalProgress
		if err := json.Unmarshal([]byte(raw), &jassess); err != nil {
			valErr = fmt.Errorf("parse json: %w", err)
			msgs = append(msgs, openai.SystemMessage(retryWarning))
			continue
		}

		var validated []*GoalProgressAssessment
		validationFailed := false

		for _, ja := range jassess {
			if !goalIDs[ja.GoalID] {
				validationFailed = true
				break
			}

			assess := ja.Assessment
			switch assess {
			case "progress", "suggested_complete", "contradicted", "irrelevant":
			default:
				assess = "irrelevant"
			}

			if assess == "irrelevant" {
				continue
			}

			conf := ja.Confidence
			if conf < 0.5 {
				conf = 0.5
			}
			if conf > 1.0 {
				conf = 1.0
			}

			validated = append(validated, &GoalProgressAssessment{
				GoalID:     ja.GoalID,
				Assessment: assess,
				Note:       ja.Note,
				Confidence: conf,
			})
		}

		if validationFailed {
			valErr = errors.New("goal progress assessment references non-existent goal ID")
			msgs = append(msgs, openai.SystemMessage(retryWarning+" Goal IDs must be from the provided list only."))
			continue
		}

		result = validated
		valErr = nil
		break
	}

	if valErr != nil {
		return nil, valErr
	}

	return result, nil
}

// --- ReasonFailurePatterns ---

func (o *OpenAI) ReasonFailurePatterns(ctx context.Context, failures []models.Failure, evidence []string) ([]*FailurePatternResult, error) {
	if len(failures) == 0 || len(evidence) == 0 {
		return nil, nil
	}

	var failureLines []string
	for _, f := range failures {
		failureLines = append(failureLines, fmt.Sprintf("[Failure %d] What: %s | Why: %s | Lesson: %s", f.ID, f.Content, f.Reason, f.Lesson))
	}

	prompt := fmt.Sprintf(`Given these past failures and recent evidence, detect whether any failure is being repeated, and extract higher-order failure patterns.

Past Failures:
%s

Recent Evidence:
- %s

Output ONLY a JSON array of objects:
[{"type": "repetition", "failure_id": 1, "evidence": "what evidence suggests the repeat", "pattern_fact": "", "confidence": 0.9}]

Rules for repetition detection:
- failure_id must be a failure ID from the list above.
- Only flag a repetition if the recent evidence clearly matches the same type of mistake.
- The evidence field must explain what in the recent text matches the failure pattern.

Rules for pattern extraction (higher-order):
- If you notice a recurring theme across multiple failures, output a pattern object:
  {"type": "pattern", "failure_id": 0, "evidence": "", "pattern_fact": "agent consistently fails at X type of task", "confidence": 0.8}
- Pattern facts must be general enough to cover multiple failures but specific enough to be actionable.
- If no repetitions or patterns are found, output: []
- confidence must be between 0.5 and 1.0.`, strings.Join(failureLines, "\n"), strings.Join(evidence, "\n- "))

	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(prompt),
	}

	failureIDs := make(map[int64]bool)
	for _, f := range failures {
		failureIDs[f.ID] = true
	}

	var result []*FailurePatternResult
	var valErr error

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := o.completion(ctx, msgs)
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, errors.New("reasoner: no response from LLM")
		}

		output := strings.TrimSpace(resp.Choices[0].Message.Content)
		raw := extractJSON(output)

		var jpatterns []jsonFailurePattern
		if err := json.Unmarshal([]byte(raw), &jpatterns); err != nil {
			valErr = fmt.Errorf("parse json: %w", err)
			msgs = append(msgs, openai.SystemMessage(retryWarning))
			continue
		}

		var validated []*FailurePatternResult
		validationFailed := false

		for _, jp := range jpatterns {
			switch jp.Type {
			case "repetition":
				if !failureIDs[jp.FailureID] {
					validationFailed = true
					break
				}
				if jp.Evidence == "" {
					continue
				}
				conf := jp.Confidence
				if conf < 0.5 {
					conf = 0.5
				}
				if conf > 1.0 {
					conf = 1.0
				}
				validated = append(validated, &FailurePatternResult{
					Type:       "repetition",
					FailureID:  jp.FailureID,
					Evidence:   jp.Evidence,
					Confidence: conf,
				})
			case "pattern":
				if jp.PatternFact == "" {
					continue
				}
				conf := jp.Confidence
				if conf < 0.5 {
					conf = 0.5
				}
				if conf > 1.0 {
					conf = 1.0
				}
				validated = append(validated, &FailurePatternResult{
					Type:        "pattern",
					PatternFact: jp.PatternFact,
					Confidence:  conf,
				})
			default:
				continue
			}
		}

		if validationFailed {
			valErr = errors.New("failure pattern references non-existent failure ID")
			msgs = append(msgs, openai.SystemMessage(retryWarning+" Failure IDs must be from the provided list only."))
			continue
		}

		result = validated
		valErr = nil
		break
	}

	if valErr != nil {
		return nil, valErr
	}

	return result, nil
}

// --- ReasonHypothesisEvidence ---

func (o *OpenAI) ReasonHypothesisEvidence(ctx context.Context, hypotheses []models.Hypothesis, facts []models.Fact) ([]*HypothesisEvidenceResult, error) {
	if len(hypotheses) == 0 || len(facts) == 0 {
		return nil, nil
	}

	var hypLines []string
	for _, h := range hypotheses {
		hypLines = append(hypLines, fmt.Sprintf("[Hypothesis %d] (status: %s, confidence: %.2f) %s", h.ID, h.Status, h.Confidence, h.Content))
	}

	var factLines []string
	for _, f := range facts {
		factLines = append(factLines, fmt.Sprintf("[Fact %d] %s", f.ID, f.Content))
	}

	prompt := fmt.Sprintf(`Given these open hypotheses and recent facts, assess whether each fact supports, weakens, or contradicts any hypothesis.

Open Hypotheses:
%s

Recent Facts:
%s

Output ONLY a JSON array of objects:
[{"hypothesis_id": 1, "verdict": "supports|weakens|contradicts|irrelevant", "confidence": 0.9, "reasoning": "brief explanation", "new_confidence": 0.7}]

Rules:
- hypothesis_id must be a hypothesis ID from the list above.
- Use "supports" if the fact provides evidence confirming or reinforcing the hypothesis.
- Use "weakens" if the fact provides evidence against but does not conclusively disprove the hypothesis.
- Use "contradicts" if the fact conclusively disproves the hypothesis.
- Use "irrelevant" if the fact has no bearing on the hypothesis.
- new_confidence is the suggested updated confidence for the hypothesis based on this evidence.
  - If supports: new_confidence should be higher than current confidence.
  - If weakens: new_confidence should be lower than current confidence.
  - If contradicts: new_confidence should be significantly lower (below 0.3).
- Only output assessments for hypotheses where the facts are relevant. If none are affected, output: []
- confidence must be between 0.5 and 1.0.
- new_confidence must be between 0.0 and 1.0.`, strings.Join(hypLines, "\n"), strings.Join(factLines, "\n"))

	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(prompt),
	}

	hypIDs := make(map[int64]bool)
	for _, h := range hypotheses {
		hypIDs[h.ID] = true
	}

	var result []*HypothesisEvidenceResult
	var valErr error

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := o.completion(ctx, msgs)
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, errors.New("reasoner: no response from LLM")
		}

		output := strings.TrimSpace(resp.Choices[0].Message.Content)
		raw := extractJSON(output)

		var jevidence []jsonHypothesisEvidence
		if err := json.Unmarshal([]byte(raw), &jevidence); err != nil {
			valErr = fmt.Errorf("parse json: %w", err)
			msgs = append(msgs, openai.SystemMessage(retryWarning))
			continue
		}

		var validated []*HypothesisEvidenceResult
		validationFailed := false

		for _, je := range jevidence {
			if !hypIDs[je.HypothesisID] {
				validationFailed = true
				break
			}

			verdict := je.Verdict
			switch verdict {
			case "supports", "weakens", "contradicts", "irrelevant":
			default:
				verdict = "irrelevant"
			}

			if verdict == "irrelevant" {
				continue
			}

			conf := je.Confidence
			if conf < 0.5 {
				conf = 0.5
			}
			if conf > 1.0 {
				conf = 1.0
			}

			newConf := je.NewConfidence
			if newConf < 0 {
				newConf = 0
			}
			if newConf > 1 {
				newConf = 1
			}

			validated = append(validated, &HypothesisEvidenceResult{
				HypothesisID:  je.HypothesisID,
				Verdict:       verdict,
				Confidence:    conf,
				Reasoning:     je.Reasoning,
				NewConfidence: newConf,
			})
		}

		if validationFailed {
			valErr = errors.New("hypothesis evidence references non-existent hypothesis ID")
			msgs = append(msgs, openai.SystemMessage(retryWarning+" Hypothesis IDs must be from the provided list only."))
			continue
		}

		result = validated
		valErr = nil
		break
	}

	if valErr != nil {
		return nil, valErr
	}

	return result, nil
}

// --- Helpers ---

func extractJSON(s string) string {
	s = strings.TrimSpace(s)

	trimmed := strings.TrimPrefix(s, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)

	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return trimmed
	}

	return s
}

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func validateFactGrounding(sf *StructuredFact, texts []string) error {
	if sf.Summary == "" {
		return nil
	}

	// Anti-verbatim check: summary must not be a near-verbatim copy of any single source text
	summaryLower := strings.ToLower(sf.Summary)
	summaryWords := tokenize(summaryLower)
	if len(summaryWords) == 0 {
		return nil
	}

	for _, text := range texts {
		textLower := strings.ToLower(text)
		if strings.Contains(textLower, summaryLower) {
			return fmt.Errorf("summary is a verbatim copy of source text")
		}
		textWords := tokenize(textLower)
		if len(textWords) == 0 {
			continue
		}
		matches := 0
		for w := range summaryWords {
			if textWords[w] {
				matches++
			}
		}
		if float32(matches)/float32(len(summaryWords)) > 0.80 {
			return fmt.Errorf("summary is too close to a verbatim copy of source text")
		}
	}

	return nil
}

func tokenize(s string) map[string]bool {
	words := make(map[string]bool)
	var buf strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			buf.WriteRune(unicode.ToLower(r))
		} else {
			if buf.Len() >= 3 {
				words[buf.String()] = true
			}
			buf.Reset()
		}
	}
	if buf.Len() >= 3 {
		words[buf.String()] = true
	}
	return words
}

func stringsContains(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func filterIDs(ids []int64, valid map[int64]bool) (filtered []int64, hasInvalid bool) {
	for _, id := range ids {
		if valid[id] {
			filtered = append(filtered, id)
		} else {
			hasInvalid = true
		}
	}
	return
}
