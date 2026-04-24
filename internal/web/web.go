// Package web owns the HTMX chat UI: templates, handlers, SSE streaming,
// plus the document download proxy and OIDC-backed login flow. It leans
// on internal/answer for the RAG pipeline and internal/store for
// conversation persistence.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/treetop/rag-svc/internal/answer"
	"github.com/treetop/rag-svc/internal/auth"
	"github.com/treetop/rag-svc/internal/blob"
	"github.com/treetop/rag-svc/internal/retrieve"
	"github.com/treetop/rag-svc/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Deps bundles everything the web handlers need. Keep it small — if a
// handler wants something more, add it here rather than reaching into a
// global.
type Deps struct {
	Answerer *answer.Answerer
	Store    *store.Store
	Blob     blob.Client
	OIDC     *auth.OIDC // nil in stub mode
	Logger   *slog.Logger
	// StubMode mirrors config: true when OIDC_ISSUER is empty and we
	// rely on /dev/login + stub cookies.
	StubMode bool
}

// Handler is the top-level web package handler — the caller composes
// this with the existing HTTP server's middleware stack.
type Handler struct {
	deps   Deps
	tpls   *template.Template
	static http.Handler
}

// NewHandler parses templates once at startup. A template parse error
// surfaces here so the caller can refuse to serve rather than fail on
// the first page view.
func NewHandler(deps Deps) (*Handler, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	tpls, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("web: static fs: %w", err)
	}
	return &Handler{
		deps:   deps,
		tpls:   tpls,
		static: http.FileServer(http.FS(staticSub)),
	}, nil
}

// Mount installs the routes onto mux. authMW is the existing auth
// middleware the caller built; the web handler needs to mix authenticated
// pages (/chat, /documents, /logout, /chat/messages, /documents/{sha})
// with unauthenticated ones (/login, /auth/callback, /static, /).
func (h *Handler) Mount(mux *http.ServeMux, authMW func(http.Handler) http.Handler) {
	mux.Handle("GET /static/", http.StripPrefix("/static/", h.static))

	// Use "{$}" so the root pattern matches ONLY "/" — otherwise Go 1.22's
	// mux treats "GET /" as a prefix that conflicts with every other
	// registered path.
	mux.HandleFunc("GET /{$}", h.handleRoot)
	mux.HandleFunc("GET /login", h.handleLogin)
	mux.HandleFunc("GET /auth/callback", h.handleCallback)
	mux.HandleFunc("POST /logout", h.handleLogout)

	authed := http.NewServeMux()
	authed.HandleFunc("GET /chat", h.handleChatPage)
	authed.HandleFunc("POST /chat/messages", h.handleChatMessage)
	authed.HandleFunc("GET /chat/stream", h.handleChatStream)
	authed.HandleFunc("GET /documents", h.handleDocumentsPage)
	authed.HandleFunc("GET /documents/{sha}", h.handleDocumentDownload)

	mux.Handle("/chat", authMW(authed))
	mux.Handle("/chat/messages", authMW(authed))
	mux.Handle("/chat/stream", authMW(authed))
	mux.Handle("/documents", authMW(authed))
	mux.Handle("/documents/", authMW(authed))
}

// ---- root ----

func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	// Root bounces to /chat if the cookie's present, else to /login.
	if _, err := r.Cookie("rag_svc_session"); err == nil {
		http.Redirect(w, r, "/chat", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---- login / callback / logout ----

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	// In real OIDC mode, `start=1` triggers the redirect to the IdP. The
	// bare /login renders the landing page so unauth'd users don't bounce
	// straight to the provider and lose the "what is this" moment.
	if h.deps.OIDC != nil && r.URL.Query().Get("start") == "1" {
		h.deps.OIDC.HandleLogin(w, r)
		return
	}
	h.render(w, "login", map[string]any{
		"Title": "Sign in",
		"Stub":  h.deps.StubMode,
	})
}

func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	if h.deps.OIDC == nil {
		http.Error(w, "OIDC not configured", http.StatusNotFound)
		return
	}
	h.deps.OIDC.HandleCallback(w, r)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: "rag_svc_session", Value: "", Path: "/", MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---- chat page ----

type convView struct {
	store.Conversation
	Relative string
}

type messageView struct {
	store.Message
	HTML     template.HTML
	Relative string
}

type chatPageData struct {
	Title         string
	UserEmail     string
	Conversations []convView
	Messages      []messageView
	ActiveConvID  string
}

func (h *Handler) handleChatPage(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())

	convs, err := h.deps.Store.ListConversations(r.Context(), user.Email, 50)
	if err != nil {
		h.deps.Logger.Error("chat: list conversations", "err", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	data := chatPageData{
		Title:     "Chat",
		UserEmail: user.Email,
	}
	for _, c := range convs {
		data.Conversations = append(data.Conversations, convView{
			Conversation: c,
			Relative:     relative(c.UpdatedAt),
		})
	}

	// /chat?new=1 forces a fresh empty view. /chat?c=<uuid> loads a
	// specific thread. Unadorned /chat picks the most recent thread if
	// any exist, else a blank starting state.
	newOnly := r.URL.Query().Get("new") == "1"
	if !newOnly {
		activeID := r.URL.Query().Get("c")
		if activeID == "" && len(convs) > 0 {
			activeID = convs[0].ID.String()
		}
		if activeID != "" {
			convUUID, err := uuid.Parse(activeID)
			if err == nil {
				msgs, err := h.deps.Store.GetMessages(r.Context(), convUUID, user.Email)
				if err != nil && !errors.Is(err, store.ErrConversationNotFound) {
					h.deps.Logger.Error("chat: get messages", "err", err)
				}
				for _, m := range msgs {
					data.Messages = append(data.Messages, messageView{
						Message:  m,
						HTML:     renderMessageHTML(m),
						Relative: relative(m.CreatedAt),
					})
				}
				data.ActiveConvID = activeID
			}
		}
	}
	h.render(w, "chat", data)
}

// ---- chat streaming ----

// handleChatMessage persists the user's message and returns an HTML
// fragment: the user bubble plus an assistant placeholder that uses
// htmx-ext-sse to subscribe to /chat/stream. The actual LLM streaming
// happens in handleChatStream — EventSource is GET-only, so the POST→GET
// split is forced on us by the browser API.
func (h *Handler) handleChatMessage(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	query := strings.TrimSpace(r.FormValue("query"))
	if query == "" {
		http.Error(w, "query required", http.StatusBadRequest)
		return
	}

	// Resolve or create the conversation.
	var conv store.Conversation
	if cid := r.FormValue("conversation_id"); cid != "" {
		id, err := uuid.Parse(cid)
		if err == nil {
			c, err := h.deps.Store.GetConversation(r.Context(), id, user.Email)
			if err == nil {
				conv = c
			} else if !errors.Is(err, store.ErrConversationNotFound) {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}
		}
	}
	if conv.ID == uuid.Nil {
		c, err := h.deps.Store.CreateConversation(r.Context(), user.Email, truncate(query, 80))
		if err != nil {
			http.Error(w, "create conversation: "+err.Error(), http.StatusInternalServerError)
			return
		}
		conv = c
	}

	userMsg, err := h.deps.Store.AppendMessage(r.Context(), conv.ID, "user", query, nil)
	if err != nil {
		http.Error(w, "append user message: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// HX-Push-Url lets the URL reflect the active conversation without a
	// page reload so refresh keeps the user on the same thread.
	w.Header().Set("HX-Push-Url", "/chat?c="+conv.ID.String())

	// User bubble.
	fmt.Fprint(w, renderPartial(h.tpls, "message", messageView{
		Message:  userMsg,
		HTML:     renderMessageHTML(userMsg),
		Relative: relative(userMsg.CreatedAt),
	}))
	// Assistant placeholder — htmx-ext-sse opens an EventSource to
	// /chat/stream and swaps events into the placeholder.
	streamURL := fmt.Sprintf("/chat/stream?c=%s&m=%s", conv.ID, userMsg.ID)
	fmt.Fprintf(w, `
<article class="msg msg-assistant"
         hx-ext="sse"
         sse-connect="%s"
         sse-swap="done"
         hx-swap="outerHTML"
         hx-target="this">
  <header class="msg-head">
    <span class="role">Assistant</span>
    <span class="time">now</span>
  </header>
  <div class="msg-body" sse-swap="token" hx-swap="beforeend"><em class="hint">thinking…</em></div>
</article>
`, streamURL)

	// Update the hidden conversation_id field in the composer so the
	// NEXT message stays in the same thread. HTMX hx-swap-oob picks
	// this up by id.
	fmt.Fprintf(w, `<input type="hidden" name="conversation_id" value="%s" form="chat-form" hx-swap-oob="true" id="conversation-id-field" />`, conv.ID.String())
}

// handleChatStream does the RAG + LLM streaming part. Protected by auth
// middleware; verifies the user owns the conversation before streaming
// anything.
func (h *Handler) handleChatStream(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	convIDStr := r.URL.Query().Get("c")
	msgIDStr := r.URL.Query().Get("m")
	convID, err := uuid.Parse(convIDStr)
	if err != nil {
		http.Error(w, "bad c", http.StatusBadRequest)
		return
	}
	msgID, err := uuid.Parse(msgIDStr)
	if err != nil {
		http.Error(w, "bad m", http.StatusBadRequest)
		return
	}
	// Ownership check via GetMessages (verifies conversation belongs to
	// user). Finding the user message content also gives us the query.
	msgs, err := h.deps.Store.GetMessages(r.Context(), convID, user.Email)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var userMsg *store.Message
	for i := range msgs {
		if msgs[i].ID == msgID && msgs[i].Role == "user" {
			userMsg = &msgs[i]
			break
		}
	}
	if userMsg == nil {
		http.Error(w, "user message not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	var hits []retrieve.Hit
	finalText, hits, streamErr := h.deps.Answerer.Stream(r.Context(), answer.Request{
		Query: userMsg.Content,
	}, func(e answer.Event) {
		switch e.Kind {
		case "retrieve":
			hits = e.Hits
		case "token":
			// HTMX sse-swap replaces the placeholder's .msg-body on first
			// token. We want append, so use hx-swap=beforeend on the
			// target. The "token" event data is the raw delta — when it
			// replaces/appends, the "thinking…" hint gets wiped after
			// the first token because sse-swap fires its swap rule on
			// each event and <em class="hint"> is still in the DOM
			// until then.
			writeSSEEvent(w, "token", htmlEscape(e.Token))
			flusher.Flush()
		case "error":
			writeSSEEvent(w, "token", "\n\n_error: "+e.Err.Error()+"_")
			flusher.Flush()
		}
	})
	if streamErr != nil {
		h.deps.Logger.Error("chat: stream failed", "err", streamErr, "conv", convID)
		if finalText == "" {
			finalText = "I ran into an error: " + streamErr.Error()
		}
	}

	assistant, err := h.deps.Store.AppendMessage(r.Context(), convID, "assistant", finalText, answer.HitsToCitations(hits))
	if err != nil {
		h.deps.Logger.Error("chat: persist assistant", "err", err)
		writeSSEEvent(w, "done", `<article class="msg msg-assistant"><div class="msg-body"><em>error persisting response</em></div></article>`)
		flusher.Flush()
		return
	}
	// `done` event: full rendered assistant bubble replaces the placeholder
	// (sse-swap="done" on the outer article with hx-swap="outerHTML").
	final := renderPartial(h.tpls, "message", messageView{
		Message:  assistant,
		HTML:     renderMessageHTML(assistant),
		Relative: relative(assistant.CreatedAt),
	})
	writeSSEEvent(w, "done", final)
	flusher.Flush()
}

// ---- documents ----

type docView struct {
	Title      string
	URL        string
	Filename   string
	Extraction string
	SizeHuman  string
	Relative   string
}

func (h *Handler) handleDocumentsPage(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	rows, err := h.deps.Store.Pool().Query(r.Context(), `
SELECT title, url, extra, indexed_at
FROM sources WHERE source_type = 'document' ORDER BY indexed_at DESC LIMIT 200`)
	if err != nil {
		http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var docs []docView
	for rows.Next() {
		var title, url string
		var extraRaw []byte
		var indexedAt time.Time
		if err := rows.Scan(&title, &url, &extraRaw, &indexedAt); err != nil {
			continue
		}
		var extra map[string]any
		_ = json.Unmarshal(extraRaw, &extra)
		docs = append(docs, docView{
			Title:      title,
			URL:        url,
			Filename:   stringOf(extra, "filename"),
			Extraction: stringOf(extra, "extraction_method"),
			SizeHuman:  humanBytes(intOf(extra, "size_bytes")),
			Relative:   relative(indexedAt),
		})
	}
	h.render(w, "documents", map[string]any{
		"Title":     "Documents",
		"UserEmail": user.Email,
		"Documents": docs,
	})
}

func (h *Handler) handleDocumentDownload(w http.ResponseWriter, r *http.Request) {
	if h.deps.Blob == nil {
		http.Error(w, "blob storage unavailable", http.StatusServiceUnavailable)
		return
	}
	sha := r.PathValue("sha")
	if sha == "" {
		http.NotFound(w, r)
		return
	}
	// Look up the source row to find the blob_key (which includes the
	// extension) and the filename for Content-Disposition.
	var blobKey, filename, contentType string
	err := h.deps.Store.Pool().QueryRow(r.Context(), `
SELECT extra->>'blob_key', extra->>'filename', extra->>'content_type'
FROM sources WHERE source_type = 'document' AND source_key = $1`, sha,
	).Scan(&blobKey, &filename, &contentType)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if blobKey == "" {
		http.NotFound(w, r)
		return
	}
	data, storedType, err := h.deps.Blob.Get(r.Context(), blobKey)
	if err != nil {
		h.deps.Logger.Error("documents: blob get", "err", err, "key", blobKey)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if storedType == "" {
		storedType = contentType
	}
	if storedType == "" {
		storedType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", storedType)
	// Inline by default; browser will download on explicit ?download=1.
	disp := "inline"
	if r.URL.Query().Get("download") == "1" {
		disp = "attachment"
	}
	if filename != "" {
		w.Header().Set("Content-Disposition", disp+"; filename=\""+sanitizeFilename(filename)+"\"")
	}
	_, _ = w.Write(data)
}

// ---- template helpers ----

func (h *Handler) render(w http.ResponseWriter, contentTemplate string, data any) {
	// ParseFS always includes every template in the glob, so to pick a
	// specific content block we clone and redefine "content" as the
	// requested block.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tpl := h.tpls.Lookup(contentTemplate)
	if tpl == nil {
		http.Error(w, "missing template: "+contentTemplate, http.StatusInternalServerError)
		return
	}
	// Build a tiny in-memory shell template that wires the requested
	// content block into the base layout.
	root, err := template.New("root").Parse(`{{template "layout" .}}`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Copy layout + the chosen content block into root's template set.
	if _, err := root.AddParseTree("layout", h.tpls.Lookup("layout").Tree); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := root.AddParseTree("content", tpl.Tree); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Also register the message partial so the chat content block can
	// call `{{template "message" .}}`.
	if m := h.tpls.Lookup("message"); m != nil {
		_, _ = root.AddParseTree("message", m.Tree)
	}
	if err := root.Execute(w, data); err != nil {
		h.deps.Logger.Error("web: render", "template", contentTemplate, "err", err)
	}
}

// renderPartial renders a single named template to a string for embedding
// in SSE payloads.
func renderPartial(tpls *template.Template, name string, data any) string {
	var sb strings.Builder
	_ = tpls.ExecuteTemplate(&sb, name, data)
	return sb.String()
}

func renderMessageHTML(m store.Message) template.HTML {
	// Very small markdown → HTML pass: paragraphs, line breaks, footnote
	// references, inline code, bold/italic. Running a full markdown
	// parser is overkill for step 8; we can swap in blackfriday/goldmark
	// later if the LLM's output starts needing richer formatting.
	return template.HTML(lightMarkdown(m.Content, len(m.Citations)))
}

// ---- utility ----

func writeSSEEvent(w http.ResponseWriter, event, data string) {
	// SSE spec: each line of data is prefixed with "data: ". We send a
	// single frame with potentially multi-line HTML; replace newlines
	// accordingly.
	fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}

func relative(t time.Time) string {
	diff := time.Since(t)
	if diff < time.Minute {
		return "just now"
	}
	if diff < time.Hour {
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	}
	if diff < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	}
	if diff < 30*24*time.Hour {
		return fmt.Sprintf("%dd ago", int(diff.Hours()/24))
	}
	return t.Format("Jan 2, 2006")
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func humanBytes(n int) string {
	if n == 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := int64(n) / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func stringOf(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func intOf(m map[string]any, k string) int {
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	return 0
}

func sanitizeFilename(s string) string {
	s = strings.ReplaceAll(s, "\"", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// htmlEscape minimally escapes tokens before sending them over SSE. The
// browser receives them as literal text in the message body; without
// escaping, an LLM that emits `<script>` would execute it in the page.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}
