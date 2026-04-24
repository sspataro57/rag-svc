package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestClient(t *testing.T) (redis.UniversalClient, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client, mr
}

func TestEmbedCache_SetGetRoundTrip(t *testing.T) {
	client, _ := newTestClient(t)
	c := NewEmbedCache(client, time.Minute, 4)
	v := []float32{0.1, -0.25, 1.5, -0.75}

	if err := c.Set(context.Background(), "m1", "hello", v); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get(context.Background(), "m1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got) != len(v) {
		t.Fatalf("len: got %d want %d", len(got), len(v))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Errorf("mismatch at %d: got %v want %v", i, got[i], v[i])
		}
	}
}

func TestEmbedCache_Miss(t *testing.T) {
	client, _ := newTestClient(t)
	c := NewEmbedCache(client, time.Minute, 4)
	_, ok, err := c.Get(context.Background(), "m", "never-set")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected miss")
	}
}

func TestEmbedCache_DifferentModelsDifferentKeys(t *testing.T) {
	client, _ := newTestClient(t)
	c := NewEmbedCache(client, time.Minute, 4)

	if err := c.Set(context.Background(), "small", "query", []float32{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	// Different model — should miss.
	_, ok, err := c.Get(context.Background(), "large", "query")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected miss when model differs")
	}
}

func TestEmbedCache_WrongDimensionEvicts(t *testing.T) {
	client, mr := newTestClient(t)
	c := NewEmbedCache(client, time.Minute, 4)
	// Seed with a 3-dim vector directly so we can provoke the dim mismatch.
	if err := c.Set(context.Background(), "m", "q", []float32{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	_, ok, err := c.Get(context.Background(), "m", "q")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected miss due to dim mismatch")
	}
	// Key should have been evicted.
	if _, err := mr.Get(c.Key("m", "q")); err == nil {
		t.Errorf("expected key to be deleted, still present")
	}
}
