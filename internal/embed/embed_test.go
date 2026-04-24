package embed

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestFake_DeterministicAndUnitNormal(t *testing.T) {
	f := NewFake(1536)
	v1, err := f.Embed(context.Background(), []string{"hello world"})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := f.Embed(context.Background(), []string{"hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if len(v1[0]) != 1536 {
		t.Fatalf("dim: got %d want 1536", len(v1[0]))
	}
	for i := range v1[0] {
		if v1[0][i] != v2[0][i] {
			t.Fatalf("not deterministic at index %d: %v vs %v", i, v1[0][i], v2[0][i])
		}
	}
	// Unit-norm check.
	var sum float64
	for _, x := range v1[0] {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if math.Abs(norm-1.0) > 1e-5 {
		t.Errorf("expected unit-norm vector, got norm=%f", norm)
	}
}

func TestFake_DifferentInputsDifferentVectors(t *testing.T) {
	f := NewFake(64)
	out, err := f.Embed(context.Background(), []string{"foo", "bar"})
	if err != nil {
		t.Fatal(err)
	}
	same := true
	for i := range out[0] {
		if out[0][i] != out[1][i] {
			same = false
			break
		}
	}
	if same {
		t.Errorf("expected distinct vectors for distinct inputs, got identical vectors")
	}
}

func TestClient_EmbedSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("auth header: got %q want Bearer test-key", auth)
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		resp := embedResponse{Model: req.Model}
		for i := range req.Input {
			vec := make([]float32, 4)
			for j := range vec {
				vec[j] = float32(i + j)
			}
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: vec})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", Dim: 4, BatchSize: 96})
	out, err := c.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 vectors, got %d", len(out))
	}
	for i, v := range out {
		if v[0] != float32(i) {
			t.Errorf("vec %d: got %v", i, v)
		}
	}
}

func TestClient_EmbedRetriesOn5xx(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := embedResponse{Model: req.Model}
		resp.Data = append(resp.Data, struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}{Index: 0, Embedding: []float32{1, 2}})
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m", Dim: 2, MaxRetries: 5})
	// Shrink backoff by driving attempts fast: the first two calls will sleep
	// 1s + 2s which is fine for a unit test runtime.
	out, err := c.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts: got %d want 3", got)
	}
	if out[0][0] != 1 {
		t.Errorf("unexpected vector: %v", out)
	}
}

func TestClient_EmbedDoesNotRetry4xx(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m", Dim: 2, MaxRetries: 3})
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts: got %d want 1 (4xx should not retry)", got)
	}
}
