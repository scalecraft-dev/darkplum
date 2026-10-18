package handler

// Tool items: an MCP server the chat may call. This file holds what is
// specific to them -- the configuration stored in the item body, the
// administrator's host allowlist, credential storage in the cluster Secret,
// and the verify route that connects to the server and caches its tools.
//
// Residency: a tool call sends conversation content to the server named in
// the item. Nothing is reachable until an administrator lists the host
// (Admin > Library > Tool servers), the same posture as the external-vendor
// model tier: off by default, opened deliberately, recorded on every call.
//
// Credentials: the bearer token a server may need is written to the cluster
// Secret darkplum-tool-credentials (data key = item id) through the api's
// patch-only Role and read back from the Secret's file mount at call time,
// exactly as vendor billing keys are handled (billing.go). Postgres keeps
// only the last four characters so the UI can say it is set.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
	"github.com/scalecraft/darkplum/api/internal/kube"
	"github.com/scalecraft/darkplum/api/internal/mcp"
)

const (
	// settingToolHosts is the org_settings key holding the JSON array of
	// allowed MCP server host patterns.
	settingToolHosts = "tool_allowed_hosts"
	toolHostsMax     = 100
	// toolVerifyTimeout bounds initialize + tools/list on verify.
	toolVerifyTimeout = 20 * time.Second
	toolCredentialMax = 4096
	toolCatalogMax    = 128
)

var toolHostPatternRE = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// toolConfig is the JSON a tool item's body holds.
type toolConfig struct {
	ServerURL string `json:"server_url"`
	// AllowedTools narrows which of the server's tools the chat may call;
	// empty means all of them.
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

// toolCatalog is what verify cached in library_items.tool_catalog.
type toolCatalog struct {
	ServerName string     `json:"server_name,omitempty"`
	Tools      []mcp.Tool `json:"tools"`
}

func parseToolConfig(body string) (toolConfig, error) {
	var cfg toolConfig
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, errors.New("must be a JSON object {server_url, allowed_tools?}")
	}
	cfg.ServerURL = strings.TrimSpace(cfg.ServerURL)
	if cfg.ServerURL == "" || len(cfg.ServerURL) > 2048 {
		return cfg, errors.New("server_url is required (at most 2048 characters)")
	}
	u, err := url.Parse(cfg.ServerURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return cfg, errors.New("server_url must be an http(s) URL")
	}
	if u.User != nil {
		return cfg, errors.New("server_url must not carry credentials; store the token separately")
	}
	if len(cfg.AllowedTools) > toolCatalogMax {
		return cfg, fmt.Errorf("allowed_tools: at most %d entries", toolCatalogMax)
	}
	for i, t := range cfg.AllowedTools {
		t = strings.TrimSpace(t)
		if t == "" || len(t) > 128 {
			return cfg, fmt.Errorf("allowed_tools[%d]: must be 1-128 characters", i)
		}
		cfg.AllowedTools[i] = t
	}
	return cfg, nil
}

// toolCredentialStore writes tool credentials to the cluster Secret.
// Satisfied by *kube.Writer (kube.BillingSecrets is the same three methods).
type toolCredentialStore = kube.BillingSecrets

// toolCredentialReader reads a tool's credential from the Secret's file
// mount. Dir "" means no mount (local dev without a cluster).
type toolCredentialReader struct{ Dir string }

func (r toolCredentialReader) Read(itemID string) (string, bool) {
	if r.Dir == "" || !isValidUUID(itemID) {
		return "", false
	}
	for attempt := 0; attempt < 3; attempt++ {
		b, err := os.ReadFile(filepath.Join(r.Dir, itemID))
		if err == nil {
			k := strings.TrimSpace(string(b))
			return k, k != ""
		}
		// A missing file is the common case (most servers need no token)
		// and is answered at once; only a transient read error during the
		// kubelet's symlink swap is retried.
		if os.IsNotExist(err) {
			return "", false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", false
}

// toolService is the MCP side shared by the library handler (verify,
// credentials, admin host list) and the chat proxy (execution).
type toolService struct {
	db       *pgxpool.Pool
	activity *activity.Recorder
	secrets  toolCredentialStore
	creds    toolCredentialReader
	// justSet remembers a credential written in this process for a few
	// minutes, bridging the kubelet's Secret-mount sync delay so a tool
	// verified right after its token was saved uses that token.
	mu      sync.Mutex
	justSet map[string]justSetCred
	// policyOverride replaces the database-backed allowlist (tests).
	policyOverride *mcp.HostPolicy
}

// NewToolService wires the MCP side: credentials go to the cluster Secret
// through secrets (nil = no cluster, credentials cannot be stored) and are
// read back from credentialsDir ("" = none).
func NewToolService(db *pgxpool.Pool, rec *activity.Recorder, secrets kube.BillingSecrets, credentialsDir string) *toolService {
	return newToolService(db, rec, secrets, toolCredentialReader{Dir: credentialsDir})
}

type justSetCred struct {
	token string
	until time.Time
}

func newToolService(db *pgxpool.Pool, rec *activity.Recorder, secrets toolCredentialStore, creds toolCredentialReader) *toolService {
	return &toolService{db: db, activity: rec, secrets: secrets, creds: creds, justSet: map[string]justSetCred{}}
}

// credential returns the token for an item, preferring one set in this
// process within the mount sync window.
func (s *toolService) credential(itemID string) (string, bool) {
	s.mu.Lock()
	if c, ok := s.justSet[itemID]; ok {
		if time.Now().Before(c.until) {
			s.mu.Unlock()
			return c.token, c.token != ""
		}
		delete(s.justSet, itemID)
	}
	s.mu.Unlock()
	return s.creds.Read(itemID)
}

func (s *toolService) rememberCredential(itemID, token string) {
	s.mu.Lock()
	s.justSet[itemID] = justSetCred{token: token, until: time.Now().Add(5 * time.Minute)}
	s.mu.Unlock()
}

// hostPolicy reads the administrator's allowlist. A read failure yields an
// empty policy: nothing is reachable rather than everything.
func (s *toolService) hostPolicy(ctx context.Context) *mcp.HostPolicy {
	if s.policyOverride != nil {
		return s.policyOverride
	}
	hosts, _, err := readToolHosts(ctx, s.db)
	if err != nil {
		slog.WarnContext(ctx, "tool hosts setting read failed; refusing all tool servers", "err", err)
		return &mcp.HostPolicy{}
	}
	return &mcp.HostPolicy{Allowed: hosts}
}

func readToolHosts(ctx context.Context, db *pgxpool.Pool) (hosts []string, updatedAt *time.Time, err error) {
	value, at, ok, err := readSetting(ctx, db, settingToolHosts)
	if err != nil || !ok {
		return []string{}, nil, err
	}
	if err := json.Unmarshal([]byte(value), &hosts); err != nil {
		return []string{}, nil, err
	}
	if hosts == nil {
		hosts = []string{}
	}
	return hosts, &at, nil
}

// client builds an MCP client for an item, with its credential if any.
func (s *toolService) client(ctx context.Context, itemID string, cfg toolConfig) *mcp.Client {
	c := &mcp.Client{URL: cfg.ServerURL, Policy: s.hostPolicy(ctx)}
	if tok, ok := s.credential(itemID); ok {
		c.Headers = map[string]string{"Authorization": "Bearer " + tok}
	}
	return c
}

// RemoveCredentialsQuietly is removeCredentialQuietly for several items,
// for the user-delete path (handler/users.go) once its transaction commits.
func (s *toolService) RemoveCredentialsQuietly(ctx context.Context, itemIDs []string) {
	for _, id := range itemIDs {
		s.removeCredentialQuietly(ctx, id)
	}
}

// removeCredentialQuietly deletes the Secret key for a deleted item; a
// failure is logged, never surfaced (the item is already gone).
func (s *toolService) removeCredentialQuietly(ctx context.Context, itemID string) {
	s.mu.Lock()
	delete(s.justSet, itemID)
	s.mu.Unlock()
	if s.secrets == nil || !s.secrets.Available() {
		return
	}
	if err := s.secrets.RemoveNamedSecretKey(ctx, kube.ToolCredentialsSecret, itemID); err != nil {
		slog.WarnContext(ctx, "tool credential cleanup failed", "item", itemID, "err", err)
	}
}

// toolErrorJSON maps an MCP failure to a closed set of codes with
// admin-readable text; the server's own words are never relayed to the
// browser (they could carry internal hostnames).
func toolErrorJSON(c *gin.Context, err error, host string) {
	var rpc *mcp.Error
	switch {
	case errors.Is(err, mcp.ErrNotAllowed):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{
			"code":    "tool_host_not_allowed",
			"message": fmt.Sprintf("%s is not on the list of allowed tool servers. An administrator can add it under Admin > Library.", host),
			"host":    host,
		}})
	case errors.As(err, &rpc):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{
			"code":    "tool_server_error",
			"message": fmt.Sprintf("The server answered with an error (code %d).", rpc.Code),
		}})
	case errors.Is(err, context.DeadlineExceeded):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{
			"code":    "tool_server_unreachable",
			"message": "The server did not answer in time.",
		}})
	default:
		msg := "The server could not be reached."
		if strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403") {
			msg = "The server refused the credential."
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{"code": "tool_auth_failed", "message": msg}})
			return
		}
		if strings.Contains(err.Error(), "HTTP ") {
			msg = "The server answered with an unexpected HTTP status."
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{"code": "tool_server_unreachable", "message": msg}})
	}
}

// loadOwnedTool fetches a tool item the user owns, or (nil, nil).
func (h *LibraryHandler) loadOwnedTool(ctx context.Context, me, id string) (*libraryRow, error) {
	r, err := scanLibraryRow(h.db.QueryRow(ctx, `SELECT `+librarySelectCols+libraryFrom+` WHERE li.id = $2 AND li.owner_user_id = $1 AND li.kind = 'tool'`, me, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// VerifyTool handles POST /api/v1/library/:id/tools/verify (owner):
// connects to the server, lists its tools, caches them on the item.
func (h *LibraryHandler) VerifyTool(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	r, err := h.loadOwnedTool(ctx, me, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "verify failed"})
		return
	}
	if r == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	cfg, err := parseToolConfig(r.body)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{"code": "tool_config_invalid", "message": err.Error()}})
		return
	}
	host := ""
	if u, err := url.Parse(cfg.ServerURL); err == nil {
		host = u.Hostname()
	}
	vctx, cancel := context.WithTimeout(ctx, toolVerifyTimeout)
	defer cancel()
	client := h.tools.client(vctx, id, cfg)
	if err := client.Initialize(vctx); err != nil {
		toolErrorJSON(c, err, host)
		return
	}
	tools, err := client.ListTools(vctx)
	if err != nil {
		toolErrorJSON(c, err, host)
		return
	}
	if len(tools) > toolCatalogMax {
		tools = tools[:toolCatalogMax]
	}
	cat := toolCatalog{ServerName: client.ServerName(), Tools: tools}
	if cat.Tools == nil {
		cat.Tools = []mcp.Tool{}
	}
	b, _ := json.Marshal(cat)
	if _, err := h.db.Exec(ctx, `UPDATE library_items SET tool_catalog = $2, tool_catalog_at = now() WHERE id = $1 AND owner_user_id = $3`, id, b, me); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "verify failed"})
		return
	}
	evt := activity.FromContext(c, activity.LibraryToolVerified)
	evt.TargetType = activity.StrPtr("library_item")
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{"host": host, "tool_count": len(tools)}
	h.activity.Record(ctx, evt)
	r, err = h.loadVisible(ctx, me, id)
	if err != nil || r == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "verify failed"})
		return
	}
	c.JSON(http.StatusOK, r.toWire(me))
}

type toolCredentialRequest struct {
	Token string `json:"token"`
}

// PutToolCredential handles PUT /api/v1/library/:id/tools/credential
// (owner): stores the bearer token in the cluster Secret. Never stored in
// Postgres, never logged, never returned.
func (h *LibraryHandler) PutToolCredential(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	var req toolCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": `body must be {"token": string}`})
		return
	}
	tok := strings.TrimSpace(req.Token)
	if tok == "" || len(tok) > toolCredentialMax || strings.ContainsAny(tok, " \t\r\n") || !utf8.ValidString(tok) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("token must be 1-%d characters with no whitespace", toolCredentialMax)})
		return
	}
	r, err := h.loadOwnedTool(ctx, me, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save credential failed"})
		return
	}
	if r == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	if h.tools.secrets == nil || !h.tools.secrets.Available() {
		kubeErrorJSON(c, kube.ErrNotInCluster)
		return
	}
	if err := h.tools.secrets.SetNamedSecretKey(ctx, kube.ToolCredentialsSecret, id, []byte(tok)); err != nil {
		kubeErrorJSON(c, err)
		return
	}
	h.tools.rememberCredential(id, tok)
	last4 := tok
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	if _, err := h.db.Exec(ctx, `UPDATE library_items SET tool_credential_last4 = $2, updated_at = now() WHERE id = $1 AND owner_user_id = $3`, id, last4, me); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save credential failed"})
		return
	}
	evt := activity.FromContext(c, activity.LibraryToolCredentialSet)
	evt.TargetType = activity.StrPtr("library_item")
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{"last4": last4}
	h.activity.Record(ctx, evt)
	r, err = h.loadVisible(ctx, me, id)
	if err != nil || r == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save credential failed"})
		return
	}
	c.JSON(http.StatusOK, r.toWire(me))
}

// DeleteToolCredential handles DELETE /api/v1/library/:id/tools/credential.
func (h *LibraryHandler) DeleteToolCredential(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	r, err := h.loadOwnedTool(ctx, me, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "remove credential failed"})
		return
	}
	if r == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	if r.credLast4 != nil {
		if h.tools.secrets == nil || !h.tools.secrets.Available() {
			kubeErrorJSON(c, kube.ErrNotInCluster)
			return
		}
		if err := h.tools.secrets.RemoveNamedSecretKey(ctx, kube.ToolCredentialsSecret, id); err != nil {
			kubeErrorJSON(c, err)
			return
		}
	}
	h.tools.mu.Lock()
	delete(h.tools.justSet, id)
	h.tools.mu.Unlock()
	if _, err := h.db.Exec(ctx, `UPDATE library_items SET tool_credential_last4 = NULL, updated_at = now() WHERE id = $1 AND owner_user_id = $2`, id, me); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "remove credential failed"})
		return
	}
	evt := activity.FromContext(c, activity.LibraryToolCredentialRemoved)
	evt.TargetType = activity.StrPtr("library_item")
	evt.TargetID = activity.StrPtr(id)
	h.activity.Record(ctx, evt)
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Admin: allowed tool servers
// ---------------------------------------------------------------------------

type toolHostsResponse struct {
	Hosts     []string   `json:"hosts"`
	UpdatedAt *time.Time `json:"updated_at"`
}

// GetToolHosts handles GET /api/v1/settings/tool-hosts (admin).
func (h *LibraryHandler) GetToolHosts(c *gin.Context) {
	hosts, at, err := readToolHosts(c.Request.Context(), h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read setting failed"})
		return
	}
	c.JSON(http.StatusOK, toolHostsResponse{Hosts: hosts, UpdatedAt: at})
}

type putToolHostsRequest struct {
	Hosts []string `json:"hosts"`
}

// PutToolHosts handles PUT /api/v1/settings/tool-hosts (admin): replaces
// the allowlist. Each entry is a hostname or a "*.suffix" pattern.
func (h *LibraryHandler) PutToolHosts(c *gin.Context) {
	ctx := c.Request.Context()
	var req putToolHostsRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Hosts == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": `body must be {"hosts": [string]}`})
		return
	}
	if len(req.Hosts) > toolHostsMax {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("at most %d hosts", toolHostsMax)})
		return
	}
	clean := make([]string, 0, len(req.Hosts))
	seen := map[string]bool{}
	for _, hst := range req.Hosts {
		hst = strings.ToLower(strings.TrimSpace(hst))
		if hst == "" || seen[hst] {
			continue
		}
		if len(hst) > 253 || !toolHostPatternRE.MatchString(hst) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%q is not a hostname or *.suffix pattern", hst)})
			return
		}
		if msg := toolHostTooBroad(hst); msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		seen[hst] = true
		clean = append(clean, hst)
	}
	before, _, _ := readToolHosts(ctx, h.db)
	b, _ := json.Marshal(clean)
	var actor *string
	if s := c.GetString("user_id"); s != "" {
		actor = &s
	}
	if _, err := h.db.Exec(ctx, `
		INSERT INTO org_settings (key, value, updated_at, updated_by) VALUES ($1, $2, now(), $3)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by
	`, settingToolHosts, string(b), actor); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save setting failed"})
		return
	}
	evt := activity.FromContext(c, activity.SettingsToolHostsChanged)
	evt.TargetType = activity.StrPtr("org_setting")
	evt.Detail = map[string]any{"setting": settingToolHosts, "before": before, "after": clean}
	h.activity.Record(ctx, evt)
	h.GetToolHosts(c)
}

// toolHostTooBroad refuses allowlist entries that would admit far more than
// one organisation's servers: a wildcard over a whole top-level domain
// ("*.com"), and address literals in the ranges the dialer exists to keep
// out (loopback, link-local including the cloud metadata service,
// unspecified, multicast). A specific private address is allowed: in-cluster
// servers are the common case.
func toolHostTooBroad(host string) string {
	if strings.HasPrefix(host, "*.") {
		if strings.Count(strings.TrimPrefix(host, "*."), ".") < 1 {
			return fmt.Sprintf("%q is too broad: a wildcard needs at least two labels after it (*.corp.example)", host)
		}
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Sprintf("%q is a reserved address and cannot be allowed", host)
		}
	}
	return ""
}
