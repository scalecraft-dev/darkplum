package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
)

// Organisation settings: a key/value table (migration 025) an admin edits
// from Admin > Models > Policy or the API. The first setting is the platform
// system prompt; the built-in text (system-prompt.md) is the default when no
// row is set.

const (
	settingSystemPrompt = "system_prompt"
	// systemPromptMaxChars bounds a custom prompt. A page of instructions is
	// a few thousand characters; this leaves room without letting the
	// prompt become the whole context window.
	systemPromptMaxChars = 20000
	// systemPromptCacheTTL bounds how long the chat proxy serves a cached
	// prompt after it changed. A save is reflected on the next turn after
	// at most this long; the admin's own GET always reads the row.
	systemPromptCacheTTL = 5 * time.Second
)

// SettingsHandler serves GET/PUT /api/v1/settings/system-prompt.
type SettingsHandler struct {
	db       *pgxpool.Pool
	activity *activity.Recorder
}

func NewSettingsHandler(db *pgxpool.Pool, rec *activity.Recorder) *SettingsHandler {
	return &SettingsHandler{db: db, activity: rec}
}

// readSetting returns the stored value, or ok=false when the key has no row.
func readSetting(ctx context.Context, db *pgxpool.Pool, key string) (value string, updatedAt time.Time, ok bool, err error) {
	err = db.QueryRow(ctx, `SELECT value, updated_at FROM org_settings WHERE key = $1`, key).Scan(&value, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	return value, updatedAt, true, nil
}

type systemPromptResponse struct {
	// SystemPrompt is the text in force: the custom one, or the built-in
	// default. The {{current_date}} placeholder is left as written.
	SystemPrompt string `json:"system_prompt"`
	// Custom is true when an admin-set prompt is in force.
	Custom bool `json:"custom"`
	// Default is the built-in text, so the editor can offer "reset" and
	// show what it resets to.
	Default   string     `json:"default"`
	UpdatedAt *time.Time `json:"updated_at"`
}

// GetSystemPrompt handles GET /api/v1/settings/system-prompt.
func (h *SettingsHandler) GetSystemPrompt(c *gin.Context) {
	value, updatedAt, ok, err := readSetting(c.Request.Context(), h.db, settingSystemPrompt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read setting failed"})
		return
	}
	resp := systemPromptResponse{SystemPrompt: systemPrompt, Default: systemPrompt}
	if ok {
		resp.SystemPrompt, resp.Custom = value, true
		resp.UpdatedAt = &updatedAt
	}
	c.JSON(http.StatusOK, resp)
}

type putSystemPromptRequest struct {
	// SystemPrompt null (or blank) restores the built-in default.
	SystemPrompt *string `json:"system_prompt"`
}

// PutSystemPrompt handles PUT /api/v1/settings/system-prompt.
func (h *SettingsHandler) PutSystemPrompt(c *gin.Context) {
	var req putSystemPromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be {\"system_prompt\": string | null}"})
		return
	}
	ctx := c.Request.Context()
	var actor *string
	if s := c.GetString("user_id"); s != "" {
		actor = &s
	}

	custom := req.SystemPrompt != nil && strings.TrimSpace(*req.SystemPrompt) != ""
	if custom {
		text := strings.TrimSpace(*req.SystemPrompt)
		if utf8.RuneCountInString(text) > systemPromptMaxChars {
			c.JSON(http.StatusBadRequest, gin.H{"error": "system_prompt must be at most 20000 characters"})
			return
		}
		if _, err := h.db.Exec(ctx, `
			INSERT INTO org_settings (key, value, updated_at, updated_by) VALUES ($1, $2, now(), $3)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by
		`, settingSystemPrompt, text, actor); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "save setting failed"})
			return
		}
	} else if _, err := h.db.Exec(ctx, `DELETE FROM org_settings WHERE key = $1`, settingSystemPrompt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "reset setting failed"})
		return
	}
	systemPromptCache.invalidate()

	evt := activity.FromContext(c, activity.SettingsSystemPromptChanged)
	evt.TargetType = activity.StrPtr("org_setting")
	detail := map[string]any{"setting": settingSystemPrompt, "custom": custom}
	if custom {
		detail["length"] = utf8.RuneCountInString(strings.TrimSpace(*req.SystemPrompt))
	}
	evt.Detail = detail
	h.activity.Record(ctx, evt)

	h.GetSystemPrompt(c)
}

// promptCache is the chat proxy's view of the setting: one row read per
// systemPromptCacheTTL instead of per turn, invalidated on every save made
// through this process.
type promptCache struct {
	mu      sync.Mutex
	value   string
	custom  bool
	fetched time.Time
}

var systemPromptCache promptCache

func (p *promptCache) invalidate() {
	p.mu.Lock()
	p.fetched = time.Time{}
	p.mu.Unlock()
}

// get returns the custom prompt and true, or "" and false when the built-in
// default applies. A read failure keeps whatever was cached (or the default):
// a chat must not fail because a settings read did.
func (p *promptCache) get(ctx context.Context, db *pgxpool.Pool) (string, bool) {
	if db == nil {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.fetched) < systemPromptCacheTTL {
		return p.value, p.custom
	}
	value, _, ok, err := readSetting(ctx, db, settingSystemPrompt)
	if err != nil {
		return p.value, p.custom
	}
	p.value, p.custom, p.fetched = value, ok, time.Now()
	return p.value, p.custom
}
