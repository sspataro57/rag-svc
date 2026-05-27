package cache

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// EmbedCache caches query embeddings so the hot retrieval path doesn't pay
// for a round-trip to the embeddings API on repeated queries.
//
// Cache keys include the model name so a model change invalidates old
// entries automatically (a 1536-dim vector from text-embedding-3-small and a
// 3072-dim vector from text-embedding-3-large must never alias). CLAUDE.md
// specifies md5(text); we extend to md5(model||text) for this reason.
type EmbedCache struct {
	client redis.UniversalClient
	prefix string
	ttl    time.Duration
	dim    int
}

// NewEmbedCache builds the cache wrapper. ttl of 0 disables expiration.
// dim is checked on every Get to guard against cross-model corruption in the
// event a cached vector slips through. globalPrefix is prepended to the
// cache's own "emb:v1:" prefix so multiple rag-svc instances can share a
// Valkey cluster without colliding.
func NewEmbedCache(client redis.UniversalClient, ttl time.Duration, dim int, globalPrefix string) *EmbedCache {
	return &EmbedCache{
		client: client,
		prefix: globalPrefix + "emb:v1:",
		ttl:    ttl,
		dim:    dim,
	}
}

// Key derives the cache key for (model, text). Exposed so callers can pre-hash
// when they already have the key in hand.
func (c *EmbedCache) Key(model, text string) string {
	sum := md5.Sum([]byte(model + "|" + text))
	return c.prefix + hex.EncodeToString(sum[:])
}

// Get returns the cached vector for (model, text). The second return value is
// false for a cache miss (no error returned in that case).
func (c *EmbedCache) Get(ctx context.Context, model, text string) ([]float32, bool, error) {
	data, err := c.client.Get(ctx, c.Key(model, text)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("embed cache: get: %w", err)
	}
	v, err := decodeVector(data)
	if err != nil {
		return nil, false, err
	}
	if c.dim > 0 && len(v) != c.dim {
		// Stale key from a different model — treat as miss and delete so
		// the next call repopulates with the correct dimension.
		_ = c.client.Del(ctx, c.Key(model, text)).Err()
		return nil, false, nil
	}
	return v, true, nil
}

// Set stores v for (model, text) with the configured TTL.
func (c *EmbedCache) Set(ctx context.Context, model, text string, v []float32) error {
	data, err := encodeVector(v)
	if err != nil {
		return err
	}
	if err := c.client.Set(ctx, c.Key(model, text), data, c.ttl).Err(); err != nil {
		return fmt.Errorf("embed cache: set: %w", err)
	}
	return nil
}

// encodeVector packs []float32 as little-endian bytes: 4 bytes per float,
// no header. We treat the wire format as internal; the cache lives in our
// own Redis and no other process consumes it.
func encodeVector(v []float32) ([]byte, error) {
	if len(v) == 0 {
		return nil, errors.New("embed cache: empty vector")
	}
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf, nil
}

func decodeVector(data []byte) ([]float32, error) {
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("embed cache: corrupt vector (len=%d not divisible by 4)", len(data))
	}
	out := make([]float32, len(data)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return out, nil
}
