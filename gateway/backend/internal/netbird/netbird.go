// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package netbird is a small, greenfield NetBird admin-API client used by the
// gateway's optional NetBird integration (setup keys, groups, peers). It imports
// only the standard library and nothing from the gateway.
//
// The NetBird admin API authenticates with an "Authorization: Token <token>"
// header (NOT Bearer). The token is a secret: it is NEVER logged and never placed
// in an error message.
package netbird

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// NetBird admin API path prefixes shared by more than one request builder below.
const (
	groupsPath = "/api/groups"
	groupPath  = "/api/groups/"
	peerPath   = "/api/peers/"
	policyPath = "/api/policies/"
	usersPath  = "/api/users/"
)

// Config is the NetBird admin API base URL + secret token.
type Config struct {
	URL   string
	Token string
}

// Group is a NetBird group. Peers is populated by GetGroup (the list endpoint may
// also include it).
type Group struct {
	ID    string      `json:"id"`
	Name  string      `json:"name"`
	Peers []GroupPeer `json:"peers"`
}

// GroupPeer is the minimal peer reference embedded in a group.
type GroupPeer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GroupRef is the minimal group reference embedded in a peer's group list.
type GroupRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CanonicalGroupsJSON marshals a list of policy-group refs into the canonical
// JSON form stored in ai_servers.netbird_group_ids and compared by the sync
// mirror + editor push, so both writers agree byte-for-byte (no oscillation).
// It sorts a COPY by ID (then Name) for stability and returns "" for an
// empty/nil slice (matching the column's ” default so a no-group peer never
// churns the mirror). The caller is responsible for filtering out the tracking
// group before calling.
func CanonicalGroupsJSON(groups []GroupRef) (string, error) {
	if len(groups) == 0 {
		return "", nil
	}
	sorted := make([]GroupRef, len(groups))
	copy(sorted, groups)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ID != sorted[j].ID {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].Name < sorted[j].Name
	})
	b, err := json.Marshal(sorted)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Peer is a NetBird peer. DNSLabel is the peer's full DNS name (FQDN). Groups is
// the peer's actual group membership (populated by GetPeer). LastSeen is the raw
// ISO-8601/RFC3339 timestamp NetBird reports for the peer's last contact (parsed
// tolerantly later in the sync).
type Peer struct {
	ID                          string     `json:"id"`
	Name                        string     `json:"name"`
	DNSLabel                    string     `json:"dns_label"`
	IP                          string     `json:"ip"`
	Connected                   bool       `json:"connected"`
	SSHEnabled                  bool       `json:"ssh_enabled"`
	LoginExpirationEnabled      bool       `json:"login_expiration_enabled"`
	InactivityExpirationEnabled bool       `json:"inactivity_expiration_enabled"`
	Groups                      []GroupRef `json:"groups"`
	LastSeen                    string     `json:"last_seen"`
}

// Policy is a NetBird access-control policy: a named, enabled/disabled set of rules.
type Policy struct {
	ID          string       `json:"id,omitempty"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Enabled     bool         `json:"enabled"`
	Rules       []PolicyRule `json:"rules"`
}

// PolicyRule is one rule of a Policy as returned by the NetBird admin API. Sources
// and Destinations are the resolved group objects (read shape); on write they are
// sent as group-ID strings (see PolicyRuleRequest).
type PolicyRule struct {
	ID            string      `json:"id,omitempty"`
	Name          string      `json:"name"`
	Description   string      `json:"description,omitempty"`
	Enabled       bool        `json:"enabled"`
	Action        string      `json:"action"`
	Bidirectional bool        `json:"bidirectional"`
	Protocol      string      `json:"protocol"`
	Ports         []string    `json:"ports,omitempty"`
	PortRanges    []PortRange `json:"port_ranges,omitempty"`
	Sources       []GroupRef  `json:"sources"`
	Destinations  []GroupRef  `json:"destinations"`
}

// PortRange is an inclusive port range on a PolicyRule.
type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// PolicyRequest is the caller-supplied input to CreatePolicy/UpdatePolicy.
type PolicyRequest struct {
	Name        string
	Description string
	Enabled     bool
	Rules       []PolicyRuleRequest
}

// PolicyRuleRequest is one rule of a PolicyRequest. Sources and Destinations are
// group-ID strings (write shape) — the asymmetric counterpart of PolicyRule's
// []GroupRef read shape (mirrors UpdateGroupPeers sending "peers" as []string
// while Group.Peers decodes as []GroupPeer).
type PolicyRuleRequest struct {
	Name          string
	Description   string
	Enabled       bool
	Action        string
	Bidirectional bool
	Protocol      string
	Ports         []string
	Sources       []string
	Destinations  []string
}

// SetupKey is a created setup key. Key is the full plaintext, returned only on
// create.
type SetupKey struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// SetupKeyParams are the caller-supplied inputs to CreateSetupKey. AutoGroups are
// group IDs the enrolling peer is added to.
type SetupKeyParams struct {
	Name       string
	AutoGroups []string
}

const (
	maxBody        = 1 << 20 // 1 MiB response cap
	defaultTimeout = 15 * time.Second

	setupKeyType       = "one-off"
	setupKeyExpiresIn  = 2592000 // 30 days, in seconds
	setupKeyUsageLimit = 1
)

// ErrAuth is returned by any call when NetBird answers 401 or 403. It is
// errors.Is-checkable by callers (e.g. to map to an HTTP status).
var ErrAuth = errors.New("netbird: authentication failed")

// newClient builds a per-call HTTP client with an explicit timeout. It never uses
// http.DefaultClient (which has no timeout).
func newClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &http.Client{Timeout: timeout}
}

// apiURL joins the configured base URL with an API path (which starts with "/").
func apiURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

// nonNil maps a nil slice to an empty (non-nil) slice so it serializes as [] and
// never null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// do performs a JSON request against the NetBird admin API with the
// "Authorization: Token" header, reads a bounded response, and either decodes a 2xx
// body into out (when out != nil) or maps a non-2xx status to a typed error:
// 401/403 -> ErrAuth; any other non-2xx -> an error carrying the status and a
// tolerant message extracted from the body. The token is never included in any
// error.
func do(ctx context.Context, cfg Config, timeout time.Duration, method, path string, body, out any) error {
	status, data, err := sendRequest(ctx, cfg, timeout, method, path, body)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrAuth
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("netbird: %s %s: status %d: %s", method, path, status, tolerantMessage(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("netbird: %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// sendRequest performs the JSON request against the NetBird admin API with the
// "Authorization: Token" header and a bounded read, returning the raw status code
// and (clipped) body without mapping the status to an error — so callers that need
// status-specific handling (e.g. DeleteGroup's idempotent 404) can decide. The
// token is never included in any error.
func sendRequest(ctx context.Context, cfg Config, timeout time.Duration, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("netbird: marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL(cfg.URL, path), reader)
	if err != nil {
		return 0, nil, fmt.Errorf("netbird: create request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+cfg.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := newClient(timeout).Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("netbird: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("netbird: %s %s: read response: %w", method, path, err)
	}
	return resp.StatusCode, data, nil
}

// tolerantMessage extracts a human-readable message from a NetBird error body
// ({"message":...}/{"detail":...}/{"error":...}), falling back to a clipped raw
// body. It never returns the request token (which is not in the body).
func tolerantMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "(empty body)"
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		for _, key := range []string{"message", "detail", "error"} {
			if s, ok := obj[key].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	if len(trimmed) > 512 {
		trimmed = trimmed[:512]
	}
	return trimmed
}

// CreateSetupKey creates a one-off, single-use setup key (30-day expiry) whose
// enrolling peer joins AutoGroups. The returned SetupKey.Key is the full plaintext.
func CreateSetupKey(ctx context.Context, cfg Config, timeout time.Duration, p SetupKeyParams) (SetupKey, error) {
	reqBody := map[string]any{
		"name":        p.Name,
		"type":        setupKeyType,
		"expires_in":  setupKeyExpiresIn,
		"usage_limit": setupKeyUsageLimit,
		"ephemeral":   false,
		"auto_groups": nonNil(p.AutoGroups),
	}
	var out SetupKey
	if err := do(ctx, cfg, timeout, http.MethodPost, "/api/setup-keys", reqBody, &out); err != nil {
		return SetupKey{}, err
	}
	return out, nil
}

// ListGroups returns all NetBird groups.
func ListGroups(ctx context.Context, cfg Config, timeout time.Duration) ([]Group, error) {
	var out []Group
	if err := do(ctx, cfg, timeout, http.MethodGet, groupsPath, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListPeers lists all peers in the NetBird account (for the linkage-editor peer picker).
func ListPeers(ctx context.Context, cfg Config, timeout time.Duration) ([]Peer, error) {
	var out []Peer
	if err := do(ctx, cfg, timeout, http.MethodGet, "/api/peers", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateGroup creates a group with the given name and returns it.
func CreateGroup(ctx context.Context, cfg Config, timeout time.Duration, name string) (Group, error) {
	var out Group
	if err := do(ctx, cfg, timeout, http.MethodPost, groupsPath, map[string]any{"name": name}, &out); err != nil {
		return Group{}, err
	}
	return out, nil
}

// ResolveGroupID returns the ID of the group named name: an empty name yields ""
// with no API call; an existing group's ID wins; otherwise the group is created and
// its new ID returned.
func ResolveGroupID(ctx context.Context, cfg Config, timeout time.Duration, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	groups, err := ListGroups(ctx, cfg, timeout)
	if err != nil {
		return "", err
	}
	for _, g := range groups {
		if g.Name == name {
			return g.ID, nil
		}
	}
	created, err := CreateGroup(ctx, cfg, timeout, name)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

// GetGroup fetches a single group by ID (including its peers).
func GetGroup(ctx context.Context, cfg Config, timeout time.Duration, id string) (Group, error) {
	var out Group
	if err := do(ctx, cfg, timeout, http.MethodGet, groupPath+url.PathEscape(id), nil, &out); err != nil {
		return Group{}, err
	}
	return out, nil
}

// UpdateGroupPeers sets the group's full peer-id membership list via
// PUT /api/groups/{id} with body {"name": g.Name, "peers": peerIDs}. The NetBird
// API requires the group name and replaces the peers with the supplied full list.
// Returns the updated group.
func UpdateGroupPeers(ctx context.Context, cfg Config, timeout time.Duration, g Group, peerIDs []string) (Group, error) {
	body := map[string]any{
		"name":  g.Name,
		"peers": nonNil(peerIDs),
	}
	var out Group
	if err := do(ctx, cfg, timeout, http.MethodPut, groupPath+url.PathEscape(g.ID), body, &out); err != nil {
		return Group{}, err
	}
	return out, nil
}

// SetPeerGroups adjusts a peer's group membership by a delta: for each add id it
// fetches the group and, if the peer is not already a member, PUTs the group with
// the peer appended; for each remove id it fetches the group and, if the peer is a
// member, PUTs the group with the peer dropped. Membership can only be changed via
// the Groups API (read-modify-write per group). Each group is best-effort — a
// single group's failure does not abort the rest; all errors are joined and
// returned. Already-correct groups are a no-op (no PUT).
func SetPeerGroups(ctx context.Context, cfg Config, timeout time.Duration, peerID string, addGroupIDs, removeGroupIDs []string) error {
	var errs []error
	for _, gid := range addGroupIDs {
		if gid == "" {
			continue
		}
		g, err := GetGroup(ctx, cfg, timeout, gid)
		if err != nil {
			errs = append(errs, fmt.Errorf("netbird: add peer to group %s: %w", gid, err))
			continue
		}
		ids := groupPeerIDs(g)
		if containsID(ids, peerID) {
			continue // already a member — no-op
		}
		if _, err := UpdateGroupPeers(ctx, cfg, timeout, g, append(ids, peerID)); err != nil {
			errs = append(errs, fmt.Errorf("netbird: add peer to group %s: %w", gid, err))
		}
	}
	for _, gid := range removeGroupIDs {
		if gid == "" {
			continue
		}
		g, err := GetGroup(ctx, cfg, timeout, gid)
		if err != nil {
			errs = append(errs, fmt.Errorf("netbird: remove peer from group %s: %w", gid, err))
			continue
		}
		ids := groupPeerIDs(g)
		if !containsID(ids, peerID) {
			continue // not a member — no-op
		}
		if _, err := UpdateGroupPeers(ctx, cfg, timeout, g, withoutID(ids, peerID)); err != nil {
			errs = append(errs, fmt.Errorf("netbird: remove peer from group %s: %w", gid, err))
		}
	}
	return errors.Join(errs...)
}

// DeleteGroup deletes a group by ID via DELETE /api/groups/{id}. It is idempotent:
// a 2xx OR a 404 (already gone) returns nil; any other non-2xx returns an error
// (401/403 → ErrAuth). The token is never included in any error.
func DeleteGroup(ctx context.Context, cfg Config, timeout time.Duration, id string) error {
	path := groupPath + url.PathEscape(id)
	status, data, err := sendRequest(ctx, cfg, timeout, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrAuth
	}
	if status == http.StatusNotFound {
		return nil // idempotent: already gone
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("netbird: DELETE %s: status %d: %s", path, status, tolerantMessage(data))
	}
	return nil
}

// groupPeerIDs projects a group's embedded peers to their ids.
func groupPeerIDs(g Group) []string {
	ids := make([]string, 0, len(g.Peers))
	for _, p := range g.Peers {
		ids = append(ids, p.ID)
	}
	return ids
}

// containsID reports whether id is in ids.
func containsID(ids []string, id string) bool {
	for _, existing := range ids {
		if existing == id {
			return true
		}
	}
	return false
}

// withoutID returns a copy of ids with every occurrence of id removed.
func withoutID(ids []string, id string) []string {
	out := make([]string, 0, len(ids))
	for _, existing := range ids {
		if existing != id {
			out = append(out, existing)
		}
	}
	return out
}

// GetPeer fetches a single peer by ID.
func GetPeer(ctx context.Context, cfg Config, timeout time.Duration, id string) (Peer, error) {
	var out Peer
	if err := do(ctx, cfg, timeout, http.MethodGet, peerPath+url.PathEscape(id), nil, &out); err != nil {
		return Peer{}, err
	}
	return out, nil
}

// UpdatePeerName renames a peer. The NetBird PUT requires the three expiration/ssh
// flags; they are PRESERVED from p so only the name changes. Returns the updated
// peer.
func UpdatePeerName(ctx context.Context, cfg Config, timeout time.Duration, p Peer, name string) (Peer, error) {
	body := map[string]any{
		"name":                          name,
		"ssh_enabled":                   p.SSHEnabled,
		"login_expiration_enabled":      p.LoginExpirationEnabled,
		"inactivity_expiration_enabled": p.InactivityExpirationEnabled,
	}
	var out Peer
	if err := do(ctx, cfg, timeout, http.MethodPut, peerPath+url.PathEscape(p.ID), body, &out); err != nil {
		return Peer{}, err
	}
	return out, nil
}

// DeletePeer deletes a peer by ID via DELETE /api/peers/{id}. It is idempotent:
// a 2xx OR a 404 (already gone) returns nil; any other non-2xx returns an error
// (401/403 → ErrAuth). The token is never included in any error.
func DeletePeer(ctx context.Context, cfg Config, timeout time.Duration, id string) error {
	path := peerPath + url.PathEscape(id)
	status, data, err := sendRequest(ctx, cfg, timeout, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrAuth
	}
	if status == http.StatusNotFound {
		return nil // idempotent: already gone
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("netbird: DELETE %s: status %d: %s", path, status, tolerantMessage(data))
	}
	return nil
}

// DeleteSetupKey deletes a setup key by ID via DELETE /api/setup-keys/{id}. It is
// idempotent: a 2xx OR a 404 (already gone) returns nil; any other non-2xx returns
// an error (401/403 → ErrAuth). The token is never included in any error.
func DeleteSetupKey(ctx context.Context, cfg Config, timeout time.Duration, id string) error {
	path := "/api/setup-keys/" + url.PathEscape(id)
	status, data, err := sendRequest(ctx, cfg, timeout, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrAuth
	}
	if status == http.StatusNotFound {
		return nil // idempotent: already gone
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("netbird: DELETE %s: status %d: %s", path, status, tolerantMessage(data))
	}
	return nil
}

// Ping verifies the configured URL + token by listing groups: nil on success,
// ErrAuth on 401/403, or another error on failure.
func Ping(ctx context.Context, cfg Config, timeout time.Duration) error {
	return do(ctx, cfg, timeout, http.MethodGet, groupsPath, nil, nil)
}

// Account is a NetBird account. Settings is kept as a raw map so a
// read-modify-write (GetAccount → patch a few keys → UpdateAccountSettings)
// preserves every setting key we do not manage.
type Account struct {
	ID       string         `json:"id"`
	Settings map[string]any `json:"settings"`
}

// ErrNoAccount is returned when GET /api/accounts yields no account.
var ErrNoAccount = errors.New("netbird: no account returned")

// GetAccount fetches the caller's NetBird account. NetBird "always returns a
// list of one account"; the first element is returned.
func GetAccount(ctx context.Context, cfg Config, timeout time.Duration) (Account, error) {
	var list []Account
	if err := do(ctx, cfg, timeout, http.MethodGet, "/api/accounts", nil, &list); err != nil {
		return Account{}, err
	}
	if len(list) == 0 {
		return Account{}, ErrNoAccount
	}
	return list[0], nil
}

// UpdateAccountSettings writes the account's settings via PUT
// /api/accounts/{id} with body {"settings": settings}. The caller passes the
// COMPLETE merged settings map (existing settings plus the patched keys) so a
// non-partial PUT cannot reset an unmanaged setting.
func UpdateAccountSettings(ctx context.Context, cfg Config, timeout time.Duration, accountID string, settings map[string]any) error {
	body := map[string]any{"settings": settings}
	return do(ctx, cfg, timeout, http.MethodPut, "/api/accounts/"+url.PathEscape(accountID), body, nil)
}

// ListPolicies returns all NetBird access-control policies.
func ListPolicies(ctx context.Context, cfg Config, timeout time.Duration) ([]Policy, error) {
	var out []Policy
	if err := do(ctx, cfg, timeout, http.MethodGet, "/api/policies", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetPolicy fetches a single policy by ID.
func GetPolicy(ctx context.Context, cfg Config, timeout time.Duration, id string) (Policy, error) {
	var out Policy
	if err := do(ctx, cfg, timeout, http.MethodGet, policyPath+url.PathEscape(id), nil, &out); err != nil {
		return Policy{}, err
	}
	return out, nil
}

// CreatePolicy creates a policy and returns it.
func CreatePolicy(ctx context.Context, cfg Config, timeout time.Duration, p PolicyRequest) (Policy, error) {
	var out Policy
	if err := do(ctx, cfg, timeout, http.MethodPost, "/api/policies", policyBody(p), &out); err != nil {
		return Policy{}, err
	}
	return out, nil
}

// UpdatePolicy replaces a policy's fields via PUT /api/policies/{id} and returns the
// updated policy.
func UpdatePolicy(ctx context.Context, cfg Config, timeout time.Duration, id string, p PolicyRequest) (Policy, error) {
	var out Policy
	if err := do(ctx, cfg, timeout, http.MethodPut, policyPath+url.PathEscape(id), policyBody(p), &out); err != nil {
		return Policy{}, err
	}
	return out, nil
}

// DeletePolicy deletes a policy by ID via DELETE /api/policies/{id}. It is
// idempotent: a 2xx OR a 404 (already gone) returns nil; any other non-2xx returns
// an error (401/403 → ErrAuth). The token is never included in any error.
func DeletePolicy(ctx context.Context, cfg Config, timeout time.Duration, id string) error {
	path := policyPath + url.PathEscape(id)
	status, data, err := sendRequest(ctx, cfg, timeout, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrAuth
	}
	if status == http.StatusNotFound {
		return nil // idempotent: already gone
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("netbird: DELETE %s: status %d: %s", path, status, tolerantMessage(data))
	}
	return nil
}

// policyBody builds the write-shape request body for CreatePolicy/UpdatePolicy:
// each rule's sources/destinations are sent as group-ID string slices (nonNil so
// they serialize as [] not null), the read-shape []GroupRef asymmetry noted on
// PolicyRuleRequest.
func policyBody(p PolicyRequest) map[string]any {
	rules := make([]map[string]any, 0, len(p.Rules))
	for _, r := range p.Rules {
		rules = append(rules, map[string]any{
			"name":          r.Name,
			"description":   r.Description,
			"enabled":       r.Enabled,
			"action":        r.Action,
			"bidirectional": r.Bidirectional,
			"protocol":      r.Protocol,
			"ports":         nonNil(r.Ports),
			"sources":       nonNil(r.Sources),
			"destinations":  nonNil(r.Destinations),
		})
	}
	return map[string]any{
		"name":        p.Name,
		"description": p.Description,
		"enabled":     p.Enabled,
		"rules":       rules,
	}
}

// User is a NetBird account user (or service user).
type User struct {
	ID            string `json:"id"`
	IsCurrent     bool   `json:"is_current"`
	IsServiceUser bool   `json:"is_service_user"`
}

// Token is a NetBird personal access token's metadata (never the secret value).
type Token struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ExpirationDate string `json:"expiration_date"`
	LastUsed       string `json:"last_used"`
}

// ErrCurrentUserUnknown is returned when no user reports is_current for the token.
var ErrCurrentUserUnknown = errors.New("netbird: current user not identifiable")

// tokenCreateResponse is the POST .../tokens body: the one-time plaintext plus metadata.
type tokenCreateResponse struct {
	PlainToken string `json:"plain_token"`
	Token      Token  `json:"personal_access_token"`
}

// ListUsers returns account users. serviceUser, when non-nil, sets ?service_user=.
func ListUsers(ctx context.Context, cfg Config, timeout time.Duration, serviceUser *bool) ([]User, error) {
	path := "/api/users"
	if serviceUser != nil {
		path += "?service_user=" + strconv.FormatBool(*serviceUser)
	}
	var out []User
	if err := do(ctx, cfg, timeout, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveCurrentUserID finds the id of the user the configured token belongs to
// via is_current, retrying with the service-user filter (a service-user token is
// the common gateway case, and the default listing may omit service users).
func ResolveCurrentUserID(ctx context.Context, cfg Config, timeout time.Duration) (string, error) {
	if id, err := currentFrom(ctx, cfg, timeout, nil); err != nil {
		return "", err
	} else if id != "" {
		return id, nil
	}
	svc := true
	if id, err := currentFrom(ctx, cfg, timeout, &svc); err != nil {
		return "", err
	} else if id != "" {
		return id, nil
	}
	return "", ErrCurrentUserUnknown
}

func currentFrom(ctx context.Context, cfg Config, timeout time.Duration, serviceUser *bool) (string, error) {
	users, err := ListUsers(ctx, cfg, timeout, serviceUser)
	if err != nil {
		return "", err
	}
	for _, u := range users {
		if u.IsCurrent {
			return u.ID, nil
		}
	}
	return "", nil
}

// ListTokens returns a user's PAT metadata.
func ListTokens(ctx context.Context, cfg Config, timeout time.Duration, userID string) ([]Token, error) {
	var out []Token
	if err := do(ctx, cfg, timeout, http.MethodGet, usersPath+url.PathEscape(userID)+"/tokens", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetToken returns one PAT's metadata.
func GetToken(ctx context.Context, cfg Config, timeout time.Duration, userID, tokenID string) (Token, error) {
	var out Token
	p := usersPath + url.PathEscape(userID) + "/tokens/" + url.PathEscape(tokenID)
	if err := do(ctx, cfg, timeout, http.MethodGet, p, nil, &out); err != nil {
		return Token{}, err
	}
	return out, nil
}

// CreateToken creates a PAT valid expiresInDays days (NetBird: 1-365). The
// returned plain token is the full secret, available only here; it is never logged.
func CreateToken(ctx context.Context, cfg Config, timeout time.Duration, userID, name string, expiresInDays int) (string, Token, error) {
	body := map[string]any{"name": name, "expires_in": expiresInDays}
	var out tokenCreateResponse
	if err := do(ctx, cfg, timeout, http.MethodPost, usersPath+url.PathEscape(userID)+"/tokens", body, &out); err != nil {
		return "", Token{}, err
	}
	return out.PlainToken, out.Token, nil
}

// DeleteToken deletes a PAT. Idempotent: a 404 is treated as success (like DeleteSetupKey).
func DeleteToken(ctx context.Context, cfg Config, timeout time.Duration, userID, tokenID string) error {
	p := usersPath + url.PathEscape(userID) + "/tokens/" + url.PathEscape(tokenID)
	status, data, err := sendRequest(ctx, cfg, timeout, http.MethodDelete, p, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrAuth
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("netbird: DELETE %s: status %d: %s", p, status, tolerantMessage(data))
	}
	return nil
}
