package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/treetop/rag-svc/internal/auth"
	"github.com/treetop/rag-svc/internal/blob"
	"github.com/treetop/rag-svc/internal/chunk"
	"github.com/treetop/rag-svc/internal/sources/document"
	"github.com/treetop/rag-svc/internal/store"
)

const (
	maxUploadBytes = 50 << 20 // 50 MB per spec
)

// uploadResponse is the success shape for POST /upload.
type uploadResponse struct {
	SourceID       int64  `json:"source_id"`
	SHA256         string `json:"sha256"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	Chunks         int    `json:"chunks"`
	Extraction     string `json:"extraction"`
	Deduplicated   bool   `json:"deduplicated"`
	SizeBytes      int    `json:"size_bytes"`
	PagesExtracted int    `json:"pages_extracted,omitempty"`
}

// handleUpload receives a single multipart file, extracts it, chunks it,
// embeds it, and writes sources + chunks. Response shape is stable so
// the web UI (step 8) and CLI clients can rely on it.
//
// Preconditions: the caller has been authenticated by auth.Middleware and
// a User lives in ctx.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.retrieval == nil || s.blob == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "upload not configured"})
		return
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		// Shouldn't happen — auth.Middleware guards this handler.
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthenticated"})
		return
	}

	// Cap the total request body; the parser enforces this on read.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		// Distinguish "too big" from other parse failures.
		if errors.Is(err, http.ErrNotMultipart) || strings.Contains(err.Error(), "no multipart") {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "multipart/form-data body required"})
			return
		}
		if strings.Contains(err.Error(), "http: request body too large") {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "file exceeds 50MB"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "malformed upload: " + err.Error()})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing file field"})
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "read file: " + err.Error()})
		return
	}

	contentType := header.Header.Get("Content-Type")
	kind, err := document.DetectKind(contentType, header.Filename)
	if err != nil {
		writeJSON(w, http.StatusUnsupportedMediaType, errorResponse{
			Error: fmt.Sprintf("unsupported file type: %s (%s)", contentType, filepath.Ext(header.Filename)),
		})
		return
	}

	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])

	// Dedup: if this SHA is already stored as a document, short-circuit.
	if existing, found, err := s.findDocumentBySHA(r.Context(), sha); err != nil {
		s.logger.Error("upload: dedup lookup failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		return
	} else if found {
		s.logger.Info("upload: dedup hit", "user", user.Email, "sha", sha, "source_id", existing.SourceID)
		writeJSON(w, http.StatusOK, uploadResponse{
			SourceID:     existing.SourceID,
			SHA256:       sha,
			Title:        existing.Title,
			URL:          existing.URL,
			Chunks:       existing.ChunkCount,
			Extraction:   existing.Extraction,
			Deduplicated: true,
			SizeBytes:    len(raw),
		})
		return
	}

	// Extract.
	norm, err := document.Extract(raw, kind, header.Filename)
	if err != nil {
		if errors.Is(err, document.ErrScannedPDF) {
			writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "scanned_pdf: OCR not supported in v1"})
			return
		}
		s.logger.Error("upload: extract failed", "err", err, "kind", kind, "filename", header.Filename)
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "extraction failed: " + err.Error()})
		return
	}

	// Store blob in S3/MinIO. Key includes the extension so downloads can
	// round-trip the original filename semantics.
	ext := strings.ToLower(filepath.Ext(header.Filename))
	blobKey := "documents/" + sha + ext
	storeContentType := contentType
	if storeContentType == "" {
		storeContentType = "application/octet-stream"
	}
	if err := s.blob.Put(r.Context(), blobKey, raw, storeContentType); err != nil {
		s.logger.Error("upload: blob put failed", "err", err, "key", blobKey)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "blob storage failed"})
		return
	}

	// Chunk + embed.
	chunks, err := chunk.Document(norm, chunk.DocumentOptions{})
	if err != nil {
		s.logger.Error("upload: chunk failed", "err", err, "sha", sha)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "chunking failed"})
		return
	}

	rows := make([]store.ChunkRow, 0, len(chunks))
	if len(chunks) > 0 {
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Content
		}
		vectors, err := embedInBatches(r.Context(), s.retrieval.Embedder, texts, s.cfg.LLM.EmbedBatchSize)
		if err != nil {
			s.logger.Error("upload: embed failed", "err", err, "sha", sha)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "embed failed"})
			return
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

	url := "/documents/" + sha
	extra := map[string]any{
		"filename":          header.Filename,
		"content_type":      contentType,
		"size_bytes":        len(raw),
		"uploader":          user.Email,
		"extraction_method": string(kind),
		"blob_key":          blobKey,
	}
	if norm.Pages > 0 {
		extra["pages_extracted"] = norm.Pages
	}

	sourceID, err := s.store.UpsertSource(r.Context(), store.SourceRow{
		SourceType:   "document",
		SourceKey:    sha,
		Title:        norm.Title,
		URL:          url,
		BodyMarkdown: norm.Body,
		Extra:        extra,
		UpdatedAt:    norm.UpdatedAt,
	})
	if err != nil {
		s.logger.Error("upload: upsert source failed", "err", err, "sha", sha)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "store failed"})
		return
	}
	if err := s.store.ReplaceChunks(r.Context(), sourceID, rows); err != nil {
		s.logger.Error("upload: replace chunks failed", "err", err, "source_id", sourceID)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "store failed"})
		return
	}

	s.logger.Info("upload",
		"user", user.Email,
		"filename", header.Filename,
		"sha", sha,
		"kind", kind,
		"chunks", len(rows),
		"size_bytes", len(raw),
	)
	writeJSON(w, http.StatusCreated, uploadResponse{
		SourceID:       sourceID,
		SHA256:         sha,
		Title:          norm.Title,
		URL:            url,
		Chunks:         len(rows),
		Extraction:     string(kind),
		Deduplicated:   false,
		SizeBytes:      len(raw),
		PagesExtracted: norm.Pages,
	})
}

// ---- dedup lookup ----

type existingDoc struct {
	SourceID   int64
	Title      string
	URL        string
	Extraction string
	ChunkCount int
}

func (s *Server) findDocumentBySHA(ctx context.Context, sha string) (*existingDoc, bool, error) {
	const q = `
SELECT s.id, s.title, s.url,
       COALESCE((s.extra->>'extraction_method'), '') AS extraction,
       (SELECT COUNT(*) FROM chunks WHERE source_id = s.id) AS chunk_count
FROM sources s
WHERE s.source_type = 'document' AND s.source_key = $1`
	var e existingDoc
	err := s.store.Pool().QueryRow(ctx, q, sha).Scan(&e.SourceID, &e.Title, &e.URL, &e.Extraction, &e.ChunkCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &e, true, nil
}

// embedInBatches is duplicated from internal/ingest to avoid a package
// import cycle (ingest imports http isn't actually wrong, but keeping
// http self-contained simplifies the dependency graph for tests).
func embedInBatches(ctx context.Context, e interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}, texts []string, batchSize int) ([][]float32, error) {
	if batchSize <= 0 {
		batchSize = 96
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := e.Embed(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// keep blob package import alive even when unused by static analysis
// after future refactors.
var _ = blob.SanitizeKey
