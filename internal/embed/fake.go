package embed

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
)

// Fake is a deterministic stand-in used by tests and by the ingest command
// when no EMBED_API_KEY is configured. It produces stable unit-length vectors
// from an input's SHA-256 digest so the chunk-store path can be exercised end
// to end without paying for a real embeddings call.
type Fake struct {
	ModelName string
	Dimension int
}

func NewFake(dim int) *Fake {
	if dim == 0 {
		dim = Dim
	}
	return &Fake{ModelName: "fake", Dimension: dim}
}

func (f *Fake) Model() string { return f.ModelName }
func (f *Fake) Dim() int      { return f.Dimension }

func (f *Fake) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, s := range inputs {
		out[i] = vectorFromString(s, f.Dimension)
	}
	return out, nil
}

func vectorFromString(s string, dim int) []float32 {
	v := make([]float32, dim)
	seed := sha256.Sum256([]byte(s))
	// Each 4 bytes of the digest deterministically seeds one slot; we cycle
	// the digest to fill the whole vector. Values are mapped into [-1, 1].
	for i := 0; i < dim; i++ {
		b := seed[(i*4)%len(seed):]
		if len(b) < 4 {
			// Shouldn't happen with a 32-byte digest and Dim=1536,
			// but guard anyway.
			seed = sha256.Sum256(seed[:])
			b = seed[:4]
		}
		u := binary.BigEndian.Uint32(b[:4])
		// Scale u to roughly [-1, 1], spreading distinct inputs apart.
		v[i] = float32(float64(u)/float64(math.MaxUint32)*2 - 1)
		// Rotate by reseeding periodically so later slots aren't pure
		// repeats of the first 8.
		if i%8 == 7 {
			seed = sha256.Sum256(append(seed[:], byte(i)))
		}
	}
	// Unit-normalize so cosine similarity behaves like the real thing.
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := float32(math.Sqrt(sum))
	if norm == 0 {
		return v
	}
	for i := range v {
		v[i] /= norm
	}
	return v
}

var _ Embedder = (*Fake)(nil)
