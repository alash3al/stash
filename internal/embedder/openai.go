package embedder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// OpenAI uses the OpenAI-compatible SDK to generate embeddings.
// Works with any OpenAI-compatible endpoint: api.openai.com,
// openrouter.ai, local Ollama, Together, vLLM, etc.
// The model string is passed as-is to the API — no stripping or
// transformation. Use the format your endpoint expects:
//
//	OpenRouter:    "openai/text-embedding-3-small"
//	OpenAI direct: "text-embedding-3-small"
//	Ollama:        "nomic-embed-text"
type OpenAI struct {
	client              openai.Client
	model               string
	dims                int
	rateLimitCooldown   time.Duration
	paymentCooldown     time.Duration
	serverErrorCooldown time.Duration
	queryInstruction    string
	now                 func() time.Time
	circuitMu           sync.Mutex
	circuitUntil        time.Time
	circuitStatus       int
}

type OpenAIConfig struct {
	MaxRetries          int
	RateLimitCooldown   time.Duration
	PaymentCooldown     time.Duration
	ServerErrorCooldown time.Duration

	// QueryInstruction turns on ASYMMETRIC query encoding. When set, queries
	// are wrapped as "Instruct: {QueryInstruction}\nQuery: {query}" while
	// documents stay raw — the format Qwen3-Embedding expects.
	//
	// Leave EMPTY for symmetric models such as BAAI/bge-m3: wrapping a query
	// for a model that was not trained on the wrapper corrupts the vector.
	QueryInstruction string
}

type UnavailableError struct {
	StatusCode int
	RetryAt    time.Time
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("embedder provider unavailable (status %d) until %s", e.StatusCode, e.RetryAt.UTC().Format(time.RFC3339))
}

func IsUnavailable(err error) bool {
	var unavailable *UnavailableError
	return errors.As(err, &unavailable)
}

// NewOpenAI creates an OpenAI embedder.
// baseURL: the API endpoint (e.g. "https://openrouter.ai/api/v1")
// apiKey:  the API key for the endpoint
// model:   required — the model string for this endpoint (no default)
// dims:    required — the vector dimension for this model (no default)
// Returns error if model or apiKey is empty, or dims <= 0.
func NewOpenAI(baseURL, apiKey, model string, dims int) (*OpenAI, error) {
	return NewOpenAIWithConfig(baseURL, apiKey, model, dims, OpenAIConfig{})
}

func NewOpenAIWithConfig(baseURL, apiKey, model string, dims int, cfg OpenAIConfig) (*OpenAI, error) {
	if apiKey == "" {
		return nil, errors.New("embedder: apiKey is required")
	}
	if model == "" {
		return nil, errors.New("embedder: model is required")
	}
	if dims <= 0 {
		return nil, errors.New("embedder: dims must be greater than zero")
	}
	if cfg.MaxRetries < 0 {
		return nil, errors.New("embedder: max retries cannot be negative")
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
		dims:                dims,
		rateLimitCooldown:   cfg.RateLimitCooldown,
		paymentCooldown:     cfg.PaymentCooldown,
		serverErrorCooldown: cfg.ServerErrorCooldown,
		queryInstruction:    cfg.QueryInstruction,
		now:                 time.Now,
	}, nil
}

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

// Model returns the model string as passed at construction.
func (o *OpenAI) Model() string {
	return o.model
}

// QueryText applies the asymmetric query wrapper when one is configured.
// With no instruction it returns the query unchanged, making this embedder
// symmetric and byte-identical to its previous behaviour.
func (o *OpenAI) QueryText(query string) string {
	return FormatInstruct(o.queryInstruction, query)
}

// Dims returns the vector dimensions as passed at construction.
func (o *OpenAI) Dims() int {
	return o.dims
}

// Embed generates a vector embedding for the given text using the OpenAI API.
func (o *OpenAI) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := o.Availability(); err != nil {
		return nil, err
	}
	resp, err := o.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: []string{text},
		},
		Model:      o.model,
		Dimensions: openai.Int(int64(o.dims)),
	})
	if err != nil {
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
		return nil, fmt.Errorf("embeddings call failed: %w", err)
	}
	if len(resp.Data) != 1 {
		return nil, fmt.Errorf("embedder: provider returned %d embeddings, want 1", len(resp.Data))
	}

	embedding := resp.Data[0].Embedding
	if len(embedding) != o.dims {
		return nil, fmt.Errorf("embedder: provider returned %d dimensions, want %d", len(embedding), o.dims)
	}
	vec := make([]float32, len(embedding))
	for i := range embedding {
		vec[i] = float32(embedding[i])
	}
	return vec, nil
}
