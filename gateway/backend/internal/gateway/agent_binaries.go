// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errAgentBinariesUnavailable = errors.New("agent binaries unavailable")

const (
	codeAgentBinaryNotFound      = "agent.binary_not_found"
	codeAgentBinariesUnavailable = "agent.binaries_unavailable"
	msgNoAgentBinariesAvailable  = "no agent binaries available"
)

type agentBinary struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

type agentManifest struct {
	Schema       int           `json:"schema"`
	AgentVersion string        `json:"agent_version"`
	GoVersion    string        `json:"go_version"`
	BuiltAt      string        `json:"built_at"`
	Binaries     []agentBinary `json:"binaries"`
}

// agentConfigMaterial is the single public/runtime-derived input shared by the
// direct config download and both portal agent-token responses. It deliberately
// cannot carry the gateway leaf's private key.
type agentConfigMaterial struct {
	GatewayURL  string `json:"gateway_url"`
	CAFile      string `json:"ca_file"`
	CACacheFile string `json:"ca_cache_file"`
	CAPEM       string `json:"ca_pem"`
}

// loadAgentManifest reads <dir>/manifest.json. An empty dir (feature off), a
// missing/empty file, or a manifest with no binaries yields errAgentBinariesUnavailable.
func loadAgentManifest(dir string) (agentManifest, error) {
	if dir == "" {
		return agentManifest{}, errAgentBinariesUnavailable
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil || len(raw) == 0 {
		return agentManifest{}, errAgentBinariesUnavailable
	}
	var m agentManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return agentManifest{}, err
	}
	if len(m.Binaries) == 0 {
		return agentManifest{}, errAgentBinariesUnavailable
	}
	return m, nil
}

// allowedAgentTargets is the fixed whitelist; user input is only ever matched
// against this set, never joined into a filesystem path.
var allowedAgentTargets = map[string]bool{
	"linux-amd64": true, "linux-arm64": true,
	"darwin-amd64": true, "darwin-arm64": true,
	"windows-amd64": true, "windows-arm64": true,
}

func agentTargetAllowed(target string) bool { return allowedAgentTargets[target] }

func (m agentManifest) find(target string) (agentBinary, bool) {
	for _, b := range m.Binaries {
		if b.OS+"-"+b.Arch == target {
			return b, true
		}
	}
	return agentBinary{}, false
}

// agentBinariesResponse is the portal list payload: the manifest + the mesh-restriction
// state so the frontend can pick the curl base URL.
type agentBinariesResponse struct {
	AgentVersion             string        `json:"agent_version"`
	GoVersion                string        `json:"go_version"`
	BuiltAt                  string        `json:"built_at"`
	Binaries                 []agentBinary `json:"binaries"`
	NetbirdAgentDownloadOnly bool          `json:"netbird_agent_download_only"`
	AgentDownloadBase        string        `json:"agent_download_base"`
}

// buildMeshDownloadBase returns the mesh base URL (http://<dns-or-ip>:<port>) the
// agent-token download must use, or "" when the restriction is not in force. Pure +
// unit-tested; the caller supplies the resolved DNS label (may be "").
func buildMeshDownloadBase(downloadOnly, listenerActive bool, listenerAddr, dnsLabel string) string {
	if !downloadOnly || !listenerActive {
		return ""
	}
	host, port, err := net.SplitHostPort(listenerAddr)
	if err != nil || port == "" {
		return ""
	}
	if dnsLabel != "" {
		host = dnsLabel
	}
	if host == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(host, port)
}

// agentDownloadBase computes the mesh base for the current request context. downloadOnly
// is the caller's already-read NetbirdAgentDownloadOnly value (read once per request, not
// re-read here). A coherently active mesh-TLS leaf wins; before TLS is really active the
// existing HTTP mesh fallback remains available. Gateway peer DNS is TTL-cached, so a
// portal list GET never triggers a live NetBird call every time.
func (s *Server) agentDownloadBase(ctx context.Context, downloadOnly bool) string {
	if s.Portal == nil || !downloadOnly || !s.AgentListenerActive() {
		return ""
	}
	material, meshTLS := s.resolveAgentConfigMaterial(ctx, "")
	return s.agentDownloadBaseFromMaterial(ctx, downloadOnly, material, meshTLS)
}

// agentDownloadBaseFromMaterial applies the restriction/listener decision to an
// already resolved config snapshot. Token responses use this form so their config
// and download base cannot come from two different certificate-store reads.
func (s *Server) agentDownloadBaseFromMaterial(ctx context.Context, downloadOnly bool, material agentConfigMaterial, meshTLS bool) string {
	if s.Portal == nil || !downloadOnly || !s.AgentListenerActive() {
		return ""
	}
	if meshTLS {
		return material.GatewayURL
	}
	dns := s.cachedGatewayPeerDNS(ctx) // best-effort; "" → IP:port fallback
	return buildMeshDownloadBase(true, true, s.AgentListenerAddr(), dns)
}

// agentDNSCacheTTL bounds how often agentDownloadBase re-resolves the gateway peer's
// NetBird DNS name via a live NetBird admin-API call (up to ~15s).
const agentDNSCacheTTL = 60 * time.Second

// cachedGatewayPeerDNS returns the gateway peer's NetBird DNS name, refreshing at most
// once per agentDNSCacheTTL. Best-effort: on a resolve error it caches "" for the TTL
// (fail-open to the IP:port fallback). The lock is held across the (timeout-bounded)
// resolve so concurrent callers single-flight one NetBird call.
func (s *Server) cachedGatewayPeerDNS(ctx context.Context) string {
	s.agentDNSMu.Lock()
	defer s.agentDNSMu.Unlock()
	if time.Now().Before(s.agentDNSExp) {
		return s.agentDNSVal
	}
	dns, _ := s.Portal.ResolveGatewayPeerDNS(ctx)
	s.agentDNSVal = dns
	s.agentDNSExp = time.Now().Add(agentDNSCacheTTL)
	return dns
}

func agentBinariesPayload(m agentManifest, downloadOnly bool, base string) agentBinariesResponse {
	return agentBinariesResponse{
		AgentVersion:             m.AgentVersion,
		GoVersion:                m.GoVersion,
		BuiltAt:                  m.BuiltAt,
		Binaries:                 m.Binaries,
		NetbirdAgentDownloadOnly: downloadOnly,
		AgentDownloadBase:        base,
	}
}

// serveAgentBinary streams a whitelisted target from the manifest dir. Target is only
// ever matched against the whitelist + the manifest; the filename comes from the
// manifest (builder-written), never from user input → no path traversal.
func (s *Server) serveAgentBinary(w http.ResponseWriter, r *http.Request, m agentManifest, target string) {
	if !agentTargetAllowed(target) {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeAgentBinaryNotFound, "unknown target", ""))
		return
	}
	b, ok := m.find(target)
	if !ok {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeAgentBinaryNotFound, "target not built", ""))
		return
	}
	f, err := os.Open(filepath.Join(s.agentBinaryDir, b.Filename))
	if err != nil {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeAgentBinaryNotFound, "binary missing", ""))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("agent.binary_read_failed", "stat failed", ""))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+b.Filename+"\"")
	if b.SHA256 != "" {
		w.Header().Set("X-Checksum-SHA256", b.SHA256)
	}
	http.ServeContent(w, r, b.Filename, info.ModTime(), f)
}

// --- Portal (session) handlers ---

func (s *Server) handlePortalAgentBinaries(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := s.requireWebScope(w, r, scopeGatewayUse); !ok {
		return
	}
	m, err := loadAgentManifest(s.agentBinaryDir)
	if err != nil {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeAgentBinariesUnavailable, msgNoAgentBinariesAvailable, ""))
		return
	}
	downloadOnly := s.Portal != nil && s.Portal.NetbirdAgentDownloadOnly(r.Context())
	writeJSON(w, http.StatusOK, agentBinariesPayload(m, downloadOnly, s.agentDownloadBase(r.Context(), downloadOnly)))
}

func (s *Server) handlePortalAgentBinaryDownload(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := s.requireWebScope(w, r, scopeGatewayUse); !ok {
		return
	}
	m, err := loadAgentManifest(s.agentBinaryDir)
	if err != nil {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeAgentBinariesUnavailable, msgNoAgentBinariesAvailable, ""))
		return
	}
	target := strings.TrimPrefix(r.URL.Path, "/api/portal/agent-binaries/")
	s.serveAgentBinary(w, r, m, target)
}

// --- Agent-token handler (both muxes; the public-mux route is gated in routes()) ---

func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	principal, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	target := strings.TrimPrefix(r.URL.Path, "/api/agent/v1/download/")
	// The config download is independent of the built binaries (it is generated,
	// not served from the manifest), so handle it BEFORE loading the manifest — a
	// missing/unbuilt manifest must not 404 the config. The token is filled from the
	// caller's own bearer, so the downloaded server-agent.json is ready to use.
	if target == "config" {
		s.serveAgentConfig(w, r, principal.Secret)
		return
	}
	m, err := loadAgentManifest(s.agentBinaryDir)
	if err != nil {
		writeJSON(w, http.StatusNotFound, apierror.Response(codeAgentBinariesUnavailable, msgNoAgentBinariesAvailable, ""))
		return
	}
	if target == "manifest" {
		writeJSON(w, http.StatusOK, agentBinariesPayload(m, false, ""))
		return
	}
	s.serveAgentBinary(w, r, m, target)
}

// serveAgentConfig returns a ready-to-use, annotated server-agent.json. The
// request origin is the fallback until mesh TLS is really active; once it is,
// the listener's runtime address and stored public certificate material take
// precedence. The token is the caller's own bearer, so the file needs no manual edit.
func (s *Server) serveAgentConfig(w http.ResponseWriter, r *http.Request, token string) {
	body := buildAgentConfigJSON(s.agentConfigMaterial(r.Context(), requestOrigin(r)), token)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="server-agent.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// agentConfigMaterial derives the generated agent's transport and trust from
// what the mesh listener can really serve now. A stored certificate alone is
// insufficient: until the runtime status is active, requestOrigin remains the
// safe and backward-compatible source.
func (s *Server) agentConfigMaterial(ctx context.Context, fallbackOrigin string) agentConfigMaterial {
	result, _ := s.resolveAgentConfigMaterial(ctx, fallbackOrigin)
	return result
}

// resolveAgentConfigMaterial additionally reports whether the returned material
// represents the coherently active mesh-TLS leaf rather than the request fallback.
// Keeping that bit outside the JSON DTO lets download routing reuse this exact
// snapshot without adding transport bookkeeping to the public config object.
func (s *Server) resolveAgentConfigMaterial(ctx context.Context, fallbackOrigin string) (agentConfigMaterial, bool) {
	result := agentConfigMaterial{GatewayURL: fallbackOrigin}
	state := s.AgentListenerTLSState()
	if !state.Active || s.Portal == nil {
		return result, false
	}
	material, err := s.Portal.GatewayMeshCertificate(ctx)
	if err != nil {
		return result, false
	}
	domain := strings.TrimSpace(material.Domain)
	_, port, err := net.SplitHostPort(strings.TrimSpace(state.Address))
	if domain == "" || port == "" || err != nil {
		return result, false
	}
	if state.Fingerprint != "" && material.Fingerprint != "" && state.Fingerprint != material.Fingerprint {
		return result, false
	}
	result.GatewayURL = "https://" + net.JoinHostPort(domain, port)
	if strings.TrimSpace(material.IssuerFingerprint) != "" && strings.TrimSpace(material.CABundlePEM) != "" {
		result.CACacheFile = "server-agent-ca.pem"
		result.CAPEM = material.CABundlePEM
	}
	return result, true
}

// requestOrigin reconstructs the scheme://host the client used. The bundled nginx sets
// `Host $host` (the real public host) + `X-Forwarded-Proto $scheme`; the mesh listener
// is reached directly (plain http). Falls back to r.TLS when no forwarded proto.
func requestOrigin(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host
}

// buildAgentConfigJSON produces the annotated (JSONC) server-agent.json. The agent's
// config loader strips whole-line // comments before parsing, so the comments are
// the operator's primary documentation for this file -- an operator who downloads
// it reads nothing else before starting the agent.
//
// Every key the agent can read belongs here, at its own default, with a comment
// saying what the value does AND what its empty value means -- the two are not the
// same thing per key, which is the whole reason the comments exist:
// runtime_allowed_binaries empty means NOTHING may start, while
// runtime_allowed_dirs empty means ANY work_dir is accepted.
//
// This text is duplicated, unavoidably (two languages, two Go modules), in the
// frontend's buildServerAgentConfig (AgentTokenSection.tsx) behind the portal's
// download button. Neither copy can import the other, so both are held to one
// generated golden -- server-agent/testdata/server-agent.config.jsonc -- byte for
// byte, and that golden's key set is checked against every json tag on the agent's
// own fileConfig by reflection. See the golden's doc in agent_binaries_test.go for
// the full chain and how to regenerate it.
func buildAgentConfigJSON(material agentConfigMaterial, token string) string {
	gw, _ := json.Marshal(material.GatewayURL)
	caFile, _ := json.Marshal(material.CAFile)
	caCacheFile, _ := json.Marshal(material.CACacheFile)
	caPEM, _ := json.Marshal(material.CAPEM)
	tok, _ := json.Marshal(token)
	return `{
  // The gateway base URL the agent sends telemetry to (origin only, no path).
  // Required. Example: https://gateway.example.com. Under a NetBird-only
  // restriction, use the gateway's mesh URL here instead.
  "gateway_url": ` + string(gw) + `,

  // The per-server agent bearer token, shown once when generated in the portal.
  // It identifies this server to the gateway. Required. Keep this file private
  // (e.g. chmod 600) because it holds the token.
  "token": ` + string(tok) + `,

  // Telemetry transport: "websocket" (default; one persistent connection, cheap
  // for high-frequency sending) or "post" (one HTTP POST per sample).
  "transport": "websocket",

  // Collection cadence as a Go duration, e.g. "500ms", "1s", "5s". Clamped up to
  // a 250ms floor. Default "1s".
  "interval": "1s",

  // POST-mode re-send cadence for the static hardware inventory (self-heals a
  // gateway restart). Floored at "1m"; ignored under the websocket transport.
  // Default "30m".
  "system_report_interval": "30m",

  // Optional inference /metrics (Prometheus) URL to scrape for active/queued
  // request counts. Empty disables it.
  "metrics_url": "",

  // Optional endpoint polled each cycle for currently-loaded models, e.g.
  // "/running" for llama-swap, "/props" for llama.cpp, "/v1/models" for vLLM.
  // Empty disables it.
  "model_status_url": "",

  // How to parse model_status_url: "openai" | "llama_swap" | "llama_cpp" |
  // "litellm" | "" or "auto" (tolerant union of all shapes). Empty = auto.
  "model_status_format": "",

  // Optional LibreHardwareMonitor Remote Web Server /data.json URL for CPU (and
  // best-effort system) power watts. Empty disables it.
  "lhm_url": "",

  // Certificate installation mode: "off" (default, never fetch a certificate),
  // "files" (write fullchain/cert/chain/ca/privkey into cert_dir and run
  // cert_reload_command on change), or "proxy" (everything "files" does, and
  // additionally runs the agent-side TLS-terminating reverse proxy: it serves
  // the routes the gateway publishes -- merged with cert_proxy_routes below --
  // with the installed leaf). cert_dir is required when this is not "off".
  "cert_mode": "off",

  // Directory certificate files are written to. Required when cert_mode is not
  // "off".
  "cert_dir": "",

  // Local command run after a changed certificate is fully installed on disk.
  // This value comes ONLY from this local file -- the gateway can never deliver
  // a command to run. On Windows, keep the value quote-free (no embedded quotes).
  "cert_reload_command": "",

  // Certificate poll cadence as a Go duration, e.g. "15m". Empty or "0"/"0s" means
  // automatic (websocket transport polls every 6h, post every 15m). A configured
  // positive value below "1m" is clamped up to "1m".
  "cert_poll_interval": "",

  // Local routes for the cert_mode "proxy" listener, each an object
  // {"listen": <port>, "upstream": "http://host:port"}. Config-file only: no
  // env var, no flag. Empty (the default) means the agent serves only the
  // routes the gateway publishes. Ignored unless cert_mode is "proxy".
  "cert_proxy_routes": [],

  // How a cert_proxy_routes entry is merged with a gateway-published route on
  // the SAME listen port: "fallback" (the default; the local route fills only
  // a port the gateway did not publish) or "override" (the local route wins).
  // Empty resolves to "fallback".
  "cert_proxy_routes_mode": "fallback",

  // Optional operator-managed CA bundle. Generated configs leave this empty;
  // the agent never writes this file.
  "ca_file": ` + string(caFile) + `,

  // Optional agent-managed CA cache, relative to this config file when not
  // absolute. Self-signed gateway configs use "server-agent-ca.pem".
  "ca_cache_file": ` + string(caCacheFile) + `,

  // Optional inline CA bootstrap bundle. Present only when the gateway's
  // currently served leaf is signed by the internal CA.
  "ca_pem": ` + string(caPEM) + `,

  // Where the agent-managed model runtime's launch specs come from: "gateway"
  // (the default -- fetched from the portal-maintained runtime-config
  // endpoint) or "file" (read from runtime_config below and reported upward
  // read-only, so the portal shows them but cannot change them).
  "runtime_source": "gateway",

  // Path to the local runtime-config JSON file. REQUIRED when runtime_source
  // is "file"; never read otherwise. Relative to this config file when not
  // absolute.
  "runtime_config": "",

  // Absolute paths a launch spec's "binary" must match EXACTLY -- compared
  // byte for byte, so spell each one as the spec does:
  // "/opt/llama/llama-server" on POSIX, "C:/llama/llama-server.exe" on
  // Windows (forward slashes; a backslash would need JSON escaping).
  // EMPTY (the default) MEANS NOTHING MAY START -- a deliberate hard refusal,
  // not a permissive default: every spec reports not_permitted until its
  // binary is listed here. Comes ONLY from this local file, and it is the
  // operator's half of the contract: the gateway decides when and how a model
  // process runs, this list decides whether it may run at all.
  "runtime_allowed_binaries": [],

  // Permitted "work_dir" prefixes for launch specs. Empty (the default) means
  // ANY work_dir is accepted -- deliberately the inverse of the list above,
  // so an operator who does not care need not enumerate the filesystem. A
  // spec that sets no work_dir runs in the directory its binary lives in
  // (which is on the allowlist above, so it is trusted), whether or not this
  // list is set. A bare entry, or a trailing "/*" ("/srv/models" and
  // "/srv/models/*" are the same), permits that directory and its whole
  // subtree; it is not a glob. Containment is a lexical path-prefix check that
  // does not resolve symlinks: defense in depth, not the boundary.
  "runtime_allowed_dirs": [],

  // Where the last known-good runtime-config document from the gateway is
  // cached, so the agent can start (and keep) model processes before its first
  // successful gateway contact. Empty (the default) means
  // "server-agent-runtime.cache.json" next to the agent binary; a relative
  // value is resolved beside this config file.
  "runtime_cache": "",

  // Bind host for the agent-managed model runtime's router port -- the port the
  // gateway sends inference requests to for a server_agent application. This
  // value comes ONLY from this local file: the gateway supplies the router's
  // PORT, never its bind host. Empty (the default) means derive one from the
  // mesh leaf installed in cert_dir, and ALL INTERFACES -- with a warning in
  // the agent log -- when there is none, which is what the empty cert_dir this
  // file ships with gives you. Set it explicitly (the mesh IP, or
  // "127.0.0.1") on any host that is not mesh-only.
  "runtime_router_bind": "",

  // How much of each managed model process's stdout+stderr this agent keeps
  // in memory, in BYTES, so an operator can read it AFTER the fact -- the
  // buffer belongs to the SPEC and outlives the process, so a crashed model's
  // output is still there when its log view is opened, and a restart appends
  // with a visible boundary marker instead of wiping the history. 0 (the
  // default) means 1048576 (1 MiB), enough for a full model startup plus its
  // context; anything below 65536 is raised to it. Kept in RAM only and NEVER
  // written to disk -- this content can include prompt text.
  "runtime_log_buffer_bytes": 0,

  // Ceiling on the SUM of those buffers across every spec, in BYTES, so a
  // server with twenty specs is not twenty times the number above. 0 (the
  // default) means 16777216 (16 MiB). The agent keeps at most
  // total/per-spec buffers, evicting the least-recently-written one nobody is
  // watching to stay inside the ceiling.
  "runtime_log_buffer_total_bytes": 0,

  // Skip TLS certificate verification. Self-signed dev gateways only. Default false.
  "tls_insecure": false,

  // Verbose mode: emit detailed debug logs to stderr. Default false.
  "verbose": false
}
`
}
