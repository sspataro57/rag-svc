// Package embed wraps an OpenAI-compatible embeddings endpoint. The Embedder
// interface lets ingestion swap the real client for a deterministic fake in
// tests or when no API key is configured.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Dim is the embedding dimension for text-embedding-3-small, matching the
// vector(1536) column in the chunks table.
const Dim = 1536

// Embedder computes vector embeddings for a batch of input strings. Caller is
// responsible for keeping the batch within EMBED_BATCH_SIZE.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	Model() string
	Dim() int
}

type Client struct {
	baseURL    string
	apiKey     string
	model      string
	dim        int
	batchSize  int
	httpClient *http.Client
	maxRetries int
}

type Options struct {
	BaseURL    string
	APIKey     string
	Model      string
	Dim        int
	BatchSize  int
	HTTPClient *http.Client
	MaxRetries int
}

func New(opts Options) *Client {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = 5
	}
	if opts.Dim == 0 {
		opts.Dim = Dim
	}
	return &Client{
		baseURL:    opts.BaseURL,
		apiKey:     opts.APIKey,
		model:      opts.Model,
		dim:        opts.Dim,
		batchSize:  opts.BatchSize,
		httpClient: opts.HTTPClient,
		maxRetries: opts.MaxRetries,
	}
}

func (c *Client) Model() string { return c.model }
func (c *Client) Dim() int      { return c.dim }

type embedRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// Embed sends inputs to the embeddings endpoint in a single request. For
// batching across EMBED_BATCH_SIZE, the caller should split the slice upstream;
// this lets the orchestrator control parallelism and retry scope.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{
		Input:          inputs,
		Model:          c.model,
		EncodingFormat: "float",
	})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		vectors, retryable, err := c.doOnce(ctx, body, len(inputs))
		if err == nil {
			return vectors, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("embed: exhausted retries: %w", lastErr)
}

func (c *Client) doOnce(ctx context.Context, body []byte, n int) ([][]float32, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network / timeout errors are retryable.
		return nil, true, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		// Honor Retry-After if present; otherwise let backoff() decide.
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				select {
				case <-ctx.Done():
					return nil, false, ctx.Err()
				case <-time.After(time.Duration(secs) * time.Second):
				}
			}
		}
		return nil, true, fmt.Errorf("embed: http %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, false, fmt.Errorf("embed: http %d: %s", resp.StatusCode, string(data))
	}

	var parsed embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, false, fmt.Errorf("embed: decode: %w", err)
	}
	if parsed.Error != nil {
		return nil, false, fmt.Errorf("embed: api error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != n {
		return nil, false, fmt.Errorf("embed: expected %d vectors, got %d", n, len(parsed.Data))
	}
	// Responses aren't guaranteed to arrive in input order — use the index
	// field to place each vector.
	out := make([][]float32, n)
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= n {
			return nil, false, fmt.Errorf("embed: vector index %d out of range", item.Index)
		}
		if len(item.Embedding) != c.dim {
			return nil, false, fmt.Errorf("embed: expected dim %d, got %d", c.dim, len(item.Embedding))
		}
		out[item.Index] = item.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, false, fmt.Errorf("embed: missing vector at index %d", i)
		}
	}
	return out, false, nil
}

// backoff returns an exponential delay with jitter. attempt is 1-indexed.
func backoff(attempt int) time.Duration {
	base := time.Second
	d := base << (attempt - 1)
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	jitter := time.Duration(rand.Float64() * float64(d) * 0.3)
	return d + jitter
}

var _ Embedder = (*Client)(nil)

// ErrEmptyInput is returned when Embed is given nothing to embed.
var ErrEmptyInput = errors.New("embed: empty input")
