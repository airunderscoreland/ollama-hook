package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// DatabaseHook logs all interactions to PostgreSQL without blocking the request flow.
type DatabaseHook struct {
	db     *sql.DB
	logger *slog.Logger

	// In-flight conversations indexed by reqID (protected by mu).
	mu            sync.Mutex
	conversations map[string]*conversationState

	// Background writer channels.
	convCh   chan ConversationRecord
	msgCh    chan MessageRecord
	updateCh chan ConversationUpdate

	// Graceful shutdown.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// conversationState holds mutable per-request state while the request is in flight.
type conversationState struct {
	id        string
	startTime time.Time
	tokens    []string        // accumulated assistant response chunks
	thinking  []string        // accumulated thinking chunks
	toolCalls json.RawMessage // last tool_calls payload from the model
}

// ConversationRecord is queued for async insert when a request starts.
type ConversationRecord struct {
	ID         string
	Model      string
	Endpoint   string
	StartedAt  time.Time
	UserPrompt string
}

// MessageRecord is queued for async insert when a request completes.
type MessageRecord struct {
	ID             string
	ConversationID string
	Role           string
	Content        string
	Thinking       *string
	ToolCalls      json.RawMessage
	Timestamp      time.Time
	TokenCount     int
	ResponseTimeMs *int
}

// ConversationUpdate is queued for async update when a request completes.
type ConversationUpdate struct {
	ID          string
	EndedAt     time.Time
	TotalTokens int
	DurationMs  int
}

func NewDatabaseHook(dbURL string, logger *slog.Logger) (*DatabaseHook, error) {
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	h := &DatabaseHook{
		db:            db,
		logger:        logger,
		conversations: make(map[string]*conversationState),
		convCh:        make(chan ConversationRecord, 100),
		msgCh:         make(chan MessageRecord, 1000),
		updateCh:      make(chan ConversationUpdate, 100),
		ctx:           ctx,
		cancel:        cancel,
	}

	h.wg.Add(3)
	go h.conversationWriter()
	go h.messageWriter()
	go h.updateWriter()

	logger.Info("database hook initialized")
	return h, nil
}

func (h *DatabaseHook) OnRequestStart(reqID, model, endpoint, userPrompt string) {
	convID := uuid.New().String()

	h.mu.Lock()
	h.conversations[reqID] = &conversationState{
		id:        convID,
		startTime: time.Now(),
	}
	h.mu.Unlock()

	select {
	case h.convCh <- ConversationRecord{
		ID:         convID,
		Model:      model,
		Endpoint:   endpoint,
		StartedAt:  time.Now(),
		UserPrompt: userPrompt,
	}:
	default:
		h.logger.Warn("conversation queue full, dropping record")
	}
}

func (h *DatabaseHook) OnToken(reqID, token string, _ /*tokenCount*/ int, _ /*elapsed*/ time.Duration) {
	h.mu.Lock()
	if conv, ok := h.conversations[reqID]; ok {
		conv.tokens = append(conv.tokens, token)
	}
	h.mu.Unlock()
}

func (h *DatabaseHook) OnThinking(reqID, content string) {
	h.mu.Lock()
	if conv, ok := h.conversations[reqID]; ok {
		conv.thinking = append(conv.thinking, content)
	}
	h.mu.Unlock()
}

func (h *DatabaseHook) OnToolCalls(reqID string, calls json.RawMessage) {
	h.mu.Lock()
	if conv, ok := h.conversations[reqID]; ok {
		conv.toolCalls = calls
	}
	h.mu.Unlock()
}

func (h *DatabaseHook) OnRequestComplete(reqID string, duration time.Duration, totalTokens int) {
	h.mu.Lock()
	conv, ok := h.conversations[reqID]
	if ok {
		delete(h.conversations, reqID)
	}
	h.mu.Unlock()

	if !ok {
		h.logger.Warn("no conversation found for completion", "req_id", reqID)
		return
	}

	var thinkingPtr *string
	if len(conv.thinking) > 0 {
		s := joinStrings(conv.thinking)
		thinkingPtr = &s
	}

	msg := MessageRecord{
		ID:             uuid.New().String(),
		ConversationID: conv.id,
		Role:           "assistant",
		Content:        joinStrings(conv.tokens),
		Thinking:       thinkingPtr,
		ToolCalls:      conv.toolCalls,
		Timestamp:      time.Now(),
		TokenCount:     totalTokens,
		ResponseTimeMs: intPtr(int(duration.Milliseconds())),
	}

	select {
	case h.msgCh <- msg:
	default:
		h.logger.Warn("message queue full, dropping assistant message")
	}

	select {
	case h.updateCh <- ConversationUpdate{
		ID:          conv.id,
		EndedAt:     time.Now(),
		TotalTokens: totalTokens,
		DurationMs:  int(duration.Milliseconds()),
	}:
	default:
		h.logger.Warn("update queue full, dropping conversation update")
	}
}

func (h *DatabaseHook) OnError(reqID string, err error) {
	h.logger.Error("conversation error", "req_id", reqID, "error", err)

	// Clean up in-flight state so we don't leak the map entry.
	h.mu.Lock()
	delete(h.conversations, reqID)
	h.mu.Unlock()
}

// Background workers.

func (h *DatabaseHook) conversationWriter() {
	defer h.wg.Done()
	for {
		select {
		case conv := <-h.convCh:
			h.writeConversation(conv)
		case <-h.ctx.Done():
			// Drain remaining records.
			for {
				select {
				case conv := <-h.convCh:
					h.writeConversation(conv)
				default:
					return
				}
			}
		}
	}
}

func (h *DatabaseHook) messageWriter() {
	defer h.wg.Done()

	batch := make([]MessageRecord, 0, 10)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	flush := func() {
		if len(batch) > 0 {
			h.writeMessages(batch)
			batch = batch[:0]
		}
	}

	for {
		select {
		case msg := <-h.msgCh:
			batch = append(batch, msg)
			if len(batch) >= 10 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-h.ctx.Done():
			// Drain remaining messages.
			for {
				select {
				case msg := <-h.msgCh:
					batch = append(batch, msg)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (h *DatabaseHook) updateWriter() {
	defer h.wg.Done()
	for {
		select {
		case update := <-h.updateCh:
			h.updateConversation(update)
		case <-h.ctx.Done():
			for {
				select {
				case update := <-h.updateCh:
					h.updateConversation(update)
				default:
					return
				}
			}
		}
	}
}

// Database operations.

func (h *DatabaseHook) writeConversation(conv ConversationRecord) {
	_, err := h.db.Exec(`
		INSERT INTO conversations (id, model_name, endpoint, started_at, user_prompt)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING`,
		conv.ID, conv.Model, conv.Endpoint, conv.StartedAt, conv.UserPrompt)
	if err != nil {
		h.logger.Error("failed to write conversation", "error", err)
	}
}

func (h *DatabaseHook) writeMessages(messages []MessageRecord) {
	tx, err := h.db.Begin()
	if err != nil {
		h.logger.Error("failed to start message transaction", "error", err)
		return
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO messages (id, conversation_id, role, content, thinking, tool_calls, timestamp, token_count, response_time_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`)
	if err != nil {
		h.logger.Error("failed to prepare message statement", "error", err)
		return
	}
	defer stmt.Close()

	for _, msg := range messages {
		var toolCallsArg interface{}
		if len(msg.ToolCalls) > 0 {
			toolCallsArg = []byte(msg.ToolCalls)
		}
		if _, err := stmt.Exec(msg.ID, msg.ConversationID, msg.Role, msg.Content,
			msg.Thinking, toolCallsArg, msg.Timestamp, msg.TokenCount, msg.ResponseTimeMs); err != nil {
			h.logger.Error("failed to write message", "error", err)
		}
	}

	if err := tx.Commit(); err != nil {
		h.logger.Error("failed to commit messages", "error", err)
	}
}

func (h *DatabaseHook) updateConversation(update ConversationUpdate) {
	_, err := h.db.Exec(`
		UPDATE conversations
		SET ended_at = $2, total_tokens = $3, duration_ms = $4
		WHERE id = $1`,
		update.ID, update.EndedAt, update.TotalTokens, update.DurationMs)
	if err != nil {
		h.logger.Error("failed to update conversation", "error", err)
	}
}

func (h *DatabaseHook) Close() error {
	h.cancel()
	h.wg.Wait()
	return h.db.Close()
}

// ExternalLogRequest is the payload for POST /_proxy/log.
type ExternalLogRequest struct {
	Model       string          `json:"model"`
	Endpoint    string          `json:"endpoint"`
	SourceType  string          `json:"source_type"`
	Query       string          `json:"query"`
	Response    string          `json:"response"`
	DurationMs  int             `json:"duration_ms"`
	TotalTokens int             `json:"total_tokens"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

// WriteExternalConversation writes a completed conversation from an external
// caller (e.g. a voice assistant automation) synchronously in a single transaction.
func (h *DatabaseHook) WriteExternalConversation(req ExternalLogRequest) (string, error) {
	convID := uuid.New().String()
	now := time.Now()
	startedAt := now.Add(-time.Duration(req.DurationMs) * time.Millisecond)

	var metadataArg interface{}
	if len(req.Metadata) > 0 {
		metadataArg = []byte(req.Metadata)
	}

	tx, err := h.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO conversations (id, model_name, endpoint, source_type, started_at, ended_at, user_prompt, total_tokens, duration_ms, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		convID, req.Model, req.Endpoint, req.SourceType, startedAt, now,
		req.Query, req.TotalTokens, req.DurationMs, metadataArg)
	if err != nil {
		return "", fmt.Errorf("insert conversation: %w", err)
	}

	_, err = tx.Exec(`
		INSERT INTO messages (id, conversation_id, role, content, thinking, tool_calls, timestamp, token_count, response_time_ms)
		VALUES ($1, $2, 'user', $3, NULL, NULL, $4, 0, NULL)`,
		uuid.New().String(), convID, req.Query, startedAt)
	if err != nil {
		return "", fmt.Errorf("insert user message: %w", err)
	}

	_, err = tx.Exec(`
		INSERT INTO messages (id, conversation_id, role, content, thinking, tool_calls, timestamp, token_count, response_time_ms)
		VALUES ($1, $2, 'assistant', $3, NULL, NULL, $4, 0, $5)`,
		uuid.New().String(), convID, req.Response, now, req.DurationMs)
	if err != nil {
		return "", fmt.Errorf("insert assistant message: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	return convID, nil
}

func joinStrings(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p)
	}
	return b.String()
}

func intPtr(i int) *int { return &i }

func init() {
	RegisterPlugin("db", newDatabasePlugin)
}

// newDatabasePlugin builds the "db" plugin from config. Returns (nil, nil)
// if disabled. Also logs the applied migration version as a startup sanity
// check — a mismatch usually means `ollama-hook --migrate` hasn't been run.
func newDatabasePlugin(cfg *Config, logger *slog.Logger) (Hook, error) {
	pc := cfg.Plugins.DB
	if !pc.Enabled {
		return nil, nil
	}

	dsn, err := resolveSecret(pc.URL, pc.URLFile)
	if err != nil {
		return nil, fmt.Errorf("resolving db url: %w", err)
	}
	if dsn == "" {
		return nil, fmt.Errorf("plugins.db.enabled is true but no url or url_file is configured")
	}

	hook, err := NewDatabaseHook(dsn, logger)
	if err != nil {
		return nil, err
	}

	if v, dirty, err := MigrationVersion(dsn); err != nil {
		logger.Warn("could not read migration version (run --migrate if schema is missing)", "error", err)
	} else {
		logger.Info("database schema version", "version", v, "dirty", dirty)
	}

	return hook, nil
}
