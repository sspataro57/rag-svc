package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/treetop/rag-svc/internal/chunk"
	"github.com/treetop/rag-svc/internal/sources/document"
	"github.com/treetop/rag-svc/internal/store"
)

// Mirrors maxIngestBatchSize for /ingest/jira.
const maxIngestDocBatchSize = 100

// Mirrors maxUploadBytes (50MiB) but applied to the *whole* JSON request,
// since one batch may carry several markdown bodies.
const maxIngestDocBytes = 50 << 20

// ingestDocumentRequest is the wire shape for POST /ingest/document.
// Backend feeders (e.g. a script that walks ~/portfolio-summaries/*.md)
// produce these directly — no multipart, no blob storage, no SHA round
// trip needed. The caller picks the source_key, which lets a later
// re-ingest of the same project (under the same key) replace its rows
// idempotently.
type ingestDocumentRequest struct {
	Documents []ingestDocument `json:"documents"`
}

type ingestDocument struct {
	Key       string         `json:"key"`     // required → sources.source_key
	Title     string         `json:"title"`   // required
	Content   string         `json:"content"` // required, markdown
	URL       string         `json:"url,omitempty"`
	UpdatedAt time.Time      `json:"updated_at"` // required
	Extra     map[string]any `json:"extra,omitempty"`
}

type ingestDocumentResponse struct {
	Upserted int                   `json:"upserted"`
	Chunks   int                   `json:"chunks"`
	Failed   int                   `json:"failed"`
	Errors   []ingestDocumentError `json:"errors,omitempty"`
}

type ingestDocumentError struct {
	Key   string `json:"key"`
	Error string `json:"error"`
}

// handleIngestDocument is the document analog of handleIngestJira.
// Chunks → embeds → upserts each markdown body. Bearer-authed.
//
// Failure model identical to /ingest/jira: per-document errors are
// collected; status 200 if any document succeeded, 502 only when every
// document failed (caller can treat as hard error).
func (s *Server) handleIngestDocument(w http.ResponseWriter, r *http.Request) {
	if s.retrieval == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "ingest not configured"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxIngestDocBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "read body: " + err.Error()})
		return
	}
	var req ingestDocumentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	if len(req.Documents) == 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "documents is empty"})
		return
	}
	if len(req.Documents) > maxIngestDocBatchSize {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("batch too large: %d > %d", len(req.Documents), maxIngestDocBatchSize)})
		return
	}

	resp := ingestDocumentResponse{}
	for _, in := range req.Documents {
		stats, err := s.ingestOneDocument(r.Context(), in)
		if err != nil {
			resp.Failed++
			resp.Errors = append(resp.Errors, ingestDocumentError{Key: in.Key, Error: err.Error()})
			continue
		}
		resp.Upserted++
		resp.Chunks += stats
	}

	status := http.StatusOK
	if resp.Upserted == 0 {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, resp)
}

func (s *Server) ingestOneDocument(ctx context.Context, in ingestDocument) (int, error) {
	key := strings.TrimSpace(in.Key)
	if key == "" {
		return 0, errors.New("key is required")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return 0, errors.New("title is required")
	}
	content := in.Content
	if strings.TrimSpace(content) == "" {
		return 0, errors.New("content is required")
	}
	if in.UpdatedAt.IsZero() {
		return 0, errors.New("updated_at is required")
	}

	norm := &document.NormalizedDocument{
		Title:      title,
		Body:       content,
		Extraction: document.KindMarkdown,
		UpdatedAt:  in.UpdatedAt,
	}

	chunks, err := chunk.Document(norm, chunk.DocumentOptions{})
	if err != nil {
		return 0, fmt.Errorf("chunk: %w", err)
	}

	rows := make([]store.ChunkRow, 0, len(chunks))
	if len(chunks) > 0 {
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Content
		}
		vectors, err := embedInBatches(ctx, s.retrieval.Embedder, texts, s.cfg.LLM.EmbedBatchSize)
		if err != nil {
			return 0, fmt.Errorf("embed: %w", err)
		}
		for i, c := range chunks {
			rows = append(rows, store.ChunkRow{
				ChunkIndex: c.Index,
				Content:    c.Content,
				TokenCount: c.TokenCount,
				Kind:       string(c.Kind),
				Embedding:  vectors[i],
			})
		}
	}

	extra := in.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	// Audit fields layered on top of caller-provided extra. Mirrors the
	// /upload handler's shape so /search rendering and downstream tools
	// can rely on these keys regardless of which path produced the row.
	if _, ok := extra["extraction_method"]; !ok {
		extra["extraction_method"] = string(document.KindMarkdown)
	}
	if _, ok := extra["source"]; !ok {
		extra["source"] = "ingest_api"
	}
	if _, ok := extra["content_sha256"]; !ok {
		sum := sha256.Sum256([]byte(content))
		extra["content_sha256"] = hex.EncodeToString(sum[:])
	}

	sourceID, err := s.store.UpsertSource(ctx, store.SourceRow{
		SourceType:   "document",
		SourceKey:    key,
		Title:        title,
		URL:          in.URL,
		BodyMarkdown: content,
		Extra:        extra,
		UpdatedAt:    in.UpdatedAt,
	})
	if err != nil {
		return 0, fmt.Errorf("upsert: %w", err)
	}
	if err := s.store.ReplaceChunks(ctx, sourceID, rows); err != nil {
		return 0, fmt.Errorf("chunks: %w", err)
	}
	return len(rows), nil
}
