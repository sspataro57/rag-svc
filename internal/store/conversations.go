package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Conversation is a single user chat thread.
type Conversation struct {
	ID        uuid.UUID
	UserEmail string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Message is a single turn in a Conversation. Role is "user" or "assistant".
type Message struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	Role           string
	Content        string
	Citations      []Citation
	CreatedAt      time.Time
}

// Citation is the per-hit metadata persisted alongside an assistant
// message so page reloads can re-render the sources panel without
// re-running retrieval.
type Citation struct {
	ID             string  `json:"id"`
	Source         string  `json:"source"`
	Title          string  `json:"title"`
	URL            string  `json:"url"`
	ProjectOrSpace string  `json:"project_or_space,omitempty"`
	Snippet        string  `json:"snippet,omitempty"`
	Score          float64 `json:"score"`
}

// ErrConversationNotFound is returned when a conversation doesn't exist or
// the user isn't its owner. We use one error so the web handler can't
// leak existence of other users' conversations via distinct error codes.
var ErrConversationNotFound = errors.New("store: conversation not found")

// ListConversations returns the caller's conversations sorted newest-first.
func (s *Store) ListConversations(ctx context.Context, userEmail string, limit int) ([]Conversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, user_email, title, created_at, updated_at
FROM conversations
WHERE user_email = $1
ORDER BY updated_at DESC
LIMIT $2`, userEmail, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list conversations: %w", err)
	}
	defer rows.Close()
	out := make([]Conversation, 0, limit)
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.UserEmail, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateConversation starts a new empty thread owned by userEmail. Title
// can be empty — the web handler typically fills it in after the first
// user message lands.
func (s *Store) CreateConversation(ctx context.Context, userEmail, title string) (Conversation, error) {
	var c Conversation
	err := s.pool.QueryRow(ctx, `
INSERT INTO conversations (user_email, title)
VALUES ($1, $2)
RETURNING id, user_email, title, created_at, updated_at`,
		userEmail, title,
	).Scan(&c.ID, &c.UserEmail, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return Conversation{}, fmt.Errorf("store: create conversation: %w", err)
	}
	return c, nil
}

// GetConversation returns the conversation owned by userEmail, else
// ErrConversationNotFound. Access control lives here — never leak across
// users.
func (s *Store) GetConversation(ctx context.Context, id uuid.UUID, userEmail string) (Conversation, error) {
	var c Conversation
	err := s.pool.QueryRow(ctx, `
SELECT id, user_email, title, created_at, updated_at
FROM conversations
WHERE id = $1 AND user_email = $2`, id, userEmail,
	).Scan(&c.ID, &c.UserEmail, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, ErrConversationNotFound
	}
	if err != nil {
		return Conversation{}, err
	}
	return c, nil
}

// RenameConversation replaces the conversation title. Used after the first
// user message to derive a human-readable title from the question.
func (s *Store) RenameConversation(ctx context.Context, id uuid.UUID, userEmail, title string) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE conversations SET title = $3, updated_at = now()
WHERE id = $1 AND user_email = $2`, id, userEmail, title)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// GetMessages returns every message in a conversation, ordered by time.
// The user_email check prevents cross-user leaks.
func (s *Store) GetMessages(ctx context.Context, convID uuid.UUID, userEmail string) ([]Message, error) {
	if _, err := s.GetConversation(ctx, convID, userEmail); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, conversation_id, role, content, citations, created_at
FROM messages
WHERE conversation_id = $1
ORDER BY created_at ASC`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var citations []byte
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &citations, &m.CreatedAt); err != nil {
			return nil, err
		}
		if len(citations) > 0 {
			if err := json.Unmarshal(citations, &m.Citations); err != nil {
				// Corrupt citations shouldn't take down the page; log
				// silently and keep the message body.
				m.Citations = nil
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AppendMessage writes a message and bumps the conversation's updated_at.
// citations may be nil for user messages.
func (s *Store) AppendMessage(ctx context.Context, convID uuid.UUID, role, content string, citations []Citation) (Message, error) {
	var citationsJSON []byte
	if len(citations) > 0 {
		b, err := json.Marshal(citations)
		if err != nil {
			return Message{}, fmt.Errorf("store: marshal citations: %w", err)
		}
		citationsJSON = b
	} else {
		citationsJSON = []byte("[]")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var m Message
	err = tx.QueryRow(ctx, `
INSERT INTO messages (conversation_id, role, content, citations)
VALUES ($1, $2, $3, $4)
RETURNING id, conversation_id, role, content, citations, created_at`,
		convID, role, content, citationsJSON,
	).Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &citationsJSON, &m.CreatedAt)
	if err != nil {
		return Message{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE conversations SET updated_at = now() WHERE id = $1`, convID); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, err
	}
	m.Citations = citations
	return m, nil
}
