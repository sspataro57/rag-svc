// Package answer glues retrieval and LLM streaming for the web chat UI
// (and, later, MCP's `ask` tool). The Answerer holds a long-lived HTTP
// client and references to retrieval + persistence — one instance per
// serve process is enough.
package answer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/treetop/rag-svc/internal/retrieve"
	"github.com/treetop/rag-svc/internal/store"
)

// Answerer issues RAG answers. Call Stream to run one end-to-end
// request/response cycle; callers forward the emitted events to the
// SSE writer.
type Answerer struct {
	Retrieval  retrieve.Deps
	BaseURL    string // e.g. https://api.openai.com/v1
	APIKey     string
	Model      string
	HTTPClient *http.Client

	// TopK caps the number of retrieved hits included in the prompt.
	TopK int
	// MaxContextChars caps the total character count of context chunks —
	// prevents a single huge chunk from blowing the model's context limit.
	MaxContextChars int
}

// Event is one increment of the streamed answer. Exactly one of Token,
// Hits, Done, Err is set.
type Event struct {
	Kind  string         // "retrieve" | "token" | "done" | "error"
	Hits  []retrieve.Hit // Kind == "retrieve"
	Token string         // Kind == "token"
	Err   error          // Kind == "error"
}

// Request describes what the caller wants answered.
type Request struct {
	Query   string
	Filters retrieve.Filters
}

// Stream runs retrieval → LLM streaming. Events flow out of emit in
// order; emit must not block for long, or streaming stalls.
//
// The function assembles the prompt once, streams tokens from the LLM,
// and returns the accumulated assistant text + the retrieved hits at
// the end so the caller can persist a message row.
func (a *Answerer) Stream(ctx context.Context, req Request, emit func(Event)) (string, []retrieve.Hit, error) {
	topK := a.TopK
	if topK <= 0 {
		topK = 10
	}
	hits, err := retrieve.SearchHits(ctx, a.Retrieval, retrieve.Query{
		Text:    req.Query,
		Filters: req.Filters,
		Limit:   topK,
	})
	if err != nil {
		emit(Event{Kind: "error", Err: err})
		return "", nil, err
	}
	emit(Event{Kind: "retrieve", Hits: hits})

	prompt := BuildPrompt(req.Query, hits, a.MaxContextChars)
	final, err := a.streamLLM(ctx, prompt, emit)
	if err != nil {
		emit(Event{Kind: "error", Err: err})
		return final, hits, err
	}
	emit(Event{Kind: "done"})
	return final, hits, nil
}

// HitsToCitations turns retrieve.Hit into the store-persistable form so
// the web handler can save them alongside the assistant message.
func HitsToCitations(hits []retrieve.Hit) []store.Citation {
	out := make([]store.Citation, 0, len(hits))
	for _, h := range hits {
		out = append(out, store.Citation{
			ID:             h.ID,
			Source:         h.Source,
			Title:          h.Title,
			URL:            h.URL,
			ProjectOrSpace: h.ProjectOrSpace,
			Snippet:        h.Snippet,
			Score:          h.Score,
		})
	}
	return out
}

// ---- prompt assembly ----

const systemPrompt = `You are Treetop's internal knowledge assistant.
Answer the user's question using ONLY the context below, which is drawn
from Treetop's Jira, Confluence, and uploaded documents.

Rules:
1. If the answer isn't in the context, say you don't know — don't guess.
2. Cite sources inline as [^N] where N is the number shown before each
   context item. Use multiple citations where appropriate.
3. Keep answers focused. Don't repeat the question.
4. Markdown formatting is fine for structure (lists, short code blocks).`

// BuildPrompt assembles the messages we send to the LLM. Exposed for
// tests so we can assert shape without running a real completion.
func BuildPrompt(query string, hits []retrieve.Hit, maxChars int) []chatMessage {
	if maxChars <= 0 {
		maxChars = 12000
	}
	var ctxB strings.Builder
	used := 0
	for i, h := range hits {
		entry := fmt.Sprintf("[%d] %s — %s\n%s\n\n", i+1, sanitizeTitle(h.Title), h.URL, stripMarks(h.Snippet))
		if used+len(entry) > maxChars {
			break
		}
		ctxB.WriteString(entry)
		used += len(entry)
	}
	user := fmt.Sprintf("Context:\n%s\nQuestion: %s", ctxB.String(), query)
	return []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: user},
	}
}

func sanitizeTitle(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return "(untitled)"
	}
	return t
}

// stripMarks drops the <mark> tags ts_headline injects; the LLM doesn't
// need them and their presence biases the model.
func stripMarks(s string) string {
	s = strings.ReplaceAll(s, "<mark>", "")
	s = strings.ReplaceAll(s, "</mark>", "")
	return s
}

// ---- OpenAI-compatible streaming ----

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// SSE payload shape from OpenAI's /chat/completions with stream=true.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (a *Answerer) streamLLM(ctx context.Context, msgs []chatMessage, emit func(Event)) (string, error) {
	if a.APIKey == "" {
		return "", errors.New("answer: ANSWER_API_KEY not configured")
	}
	hc := a.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	body, err := json.Marshal(chatRequest{Model: a.Model, Messages: msgs, Stream: true})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(a.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+a.APIKey)

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("answer: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", fmt.Errorf("answer: http %d: %s", resp.StatusCode, string(b))
	}

	var acc strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return acc.String(), fmt.Errorf("answer: api error: %s", chunk.Error.Message)
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				acc.WriteString(c.Delta.Content)
				emit(Event{Kind: "token", Token: c.Delta.Content})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return acc.String(), fmt.Errorf("answer: stream read: %w", err)
	}
	return acc.String(), nil
}
