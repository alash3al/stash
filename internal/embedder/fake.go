package embedder

import (
	"context"
	"hash/fnv"
)

// Fake returns deterministic unit vectors for tests (one dimension lit per distinct text).
type Fake struct {
	Dimensions int
	ModelName  string
}

func (f *Fake) Embed(_ context.Context, text string) ([]float32, error) {
	dims := f.Dimensions
	if dims <= 0 {
		dims = 8
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	idx := int(h.Sum32() % uint32(dims))
	vec := make([]float32, dims)
	vec[idx] = 1
	return vec, nil
}

func (f *Fake) Model() string {
	if f.ModelName != "" {
		return f.ModelName
	}
	return "fake/test"
}

func (f *Fake) Dims() int {
	if f.Dimensions > 0 {
		return f.Dimensions
	}
	return 8
}
