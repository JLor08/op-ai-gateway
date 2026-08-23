// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/routing"
	"sort"
	"strings"
	"time"
)

// acmeHTTPClientTimeout bounds a SINGLE round trip to the ACME directory.
// certissue.ACMEClient falls back to http.DefaultClient -- which has NO
// timeout -- whenever HTTPClient is left nil, so one wedged request (a
// stalled connection, a server that accepts the TCP handshake and never
// answers) could otherwise stall past even a generous reconcile-pass
// deadline (see cmd/gateway/cert_reconcile.go's certPassTimeout, the PRIMARY
// bound on a whole pass) waiting on a read that never completes. This is
// defense in depth, not a substitute for the pass-level bound: the pass
// deadline also has to cover an authorization that answers promptly but
// never leaves "pending", which no per-request timeout can catch on its own.
const acmeHTTPClientTimeout = 30 * time.Second

// acmeHTTPClient builds the *http.Client every certissue.ACMEClient
// construction site in this file must use as its HTTPClient. It exists so
// that guarantee has exactly ONE place to hold -- a future edit to either
// construction site that drops the field entirely (silently falling back to
// certissue.ACMEClient's own zero-value behavior, which is
// http.DefaultClient and therefore NO timeout at all -- see
// acmeHTTPClientTimeout's doc comment above) is caught by
// TestAcmeHTTPClientHasABoundedTimeout below the moment it's introduced,
// rather than only showing up as a real deployment hanging on a wedged TCP
// connection.
func acmeHTTPClient() *http.Client {
	return &http.Client{Timeout: acmeHTTPClientTimeout}
}

// certOrdersPerPass caps how many NEW orders one reconcile pass may place. A code
// constant (not a setting): it exists only so a misconfiguration cannot burn the
// weekly Let's Encrypt rate limit in one tick.
const certOrdersPerPass = 5

// selfSignedCAValidity is how long a freshly generated internal CA root is valid.
// Ten years is long enough that rotation is rare, and cert_ca_renew_before_days
// (default 365) starts the rotation a full year early so the new root has time to
// propagate to every agent before any leaf is signed by it.
const selfSignedCAValidity = 10 * 365 * 24 * time.Hour

// caWarnRemaining is when a CA becomes an operational problem rather than a
// scheduled task: below this the rotation is overdue and propagation gets tight.
const caWarnRemaining = 30 * 24 * time.Hour

// certUrgentRemaining is the "about to expire" threshold: below it the backoff is
// capped at certUrgentBackoff so a stubborn error cannot sit in a 24h backoff
// while the certificate runs out.
const (
	certUrgentRemaining = 7 * 24 * time.Hour
	certUrgentBackoff   = 15 * time.Minute
)

// certNoBaseDomainMessage/certSealKeyMessage are the operator-actionable
// cert_last_error notes for the module-wide conditions that otherwise abort (or
// silently defeat) a reconcile pass -- ReconcileCertificates' two abort gates
// plus the missing certificate encryption key (review findings F1.1 + the
// follow-up that made the key failure legible in the ACME mode too, see
// noteCertKeyRequired). Each names the cause and the remedy in one sentence and
// NEVER contains key/PEM material -- the seal failure in particular is a
// deliberately wrapped sentinel (ErrCertKeyRequired), never the raw
// err.Error(), so a future caller cannot accidentally leak secret material
// through this field just by changing what is passed in.
const (
	certNoBaseDomainMessage = "certificate reconcile skipped: no base domain is configured " +
		"(cert_base_domain is empty) and the NetBird account's dns_domain could not be used as a " +
		"fallback -- set cert_base_domain, or connect the NetBird module so its DNS domain is available"
	// certSealKeyMessage covers EVERY certificate private key, not just the
	// internal CA's: leaf keys (issueAndStore), the ACME account key
	// (accountFor) and the internal CA key (newCA) are all sealed with the same
	// certificate cipher, so the same missing variable blocks all of them, in
	// BOTH issuer modes.
	certSealKeyMessage = "a certificate private key cannot be sealed for storage, so nothing can be " +
		"issued: a disk-backed store requires OP_AI_GATEWAY_CERT_ENCRYPTION_KEY to be set, because " +
		"every certificate private key -- leaf keys, the ACME account key and the internal CA key -- " +
		"is sealed with it before being written -- set that variable (a 64-character hex AES-256 key) " +
		"and restart the gateway"
	certCAUnavailableMessage = "the internal CA could not be created or loaded this pass -- check the " +
		"gateway logs for the underlying error, then retry"
	// certNoUsableIssuerMessage covers ReconcileCertificates' FIRST gate: the
	// module is on, but neither the internal names nor the edge row have a fully
	// configured issuer mode, so the pass returns before it can issue anything.
	// The only way to reach it with the module on is the acme mode without an
	// account email (CertSettings' ok rule) -- for the internal names, and, when
	// the edge row is wanted at all, for the edge mode too.
	certNoUsableIssuerMessage = "certificate reconcile skipped: no issuer mode is fully configured -- the " +
		"internal issuer mode is \"acme\" but acme_email is empty (the CA has no other way to reach the " +
		"operator), and the edge certificate cannot carry the pass on its own (it is off, has no name, or " +
		"uses \"acme\" with the same missing email) -- set acme_email, or switch an issuer mode to " +
		"\"self_signed\""
)

// certReconcileAbortMessage renders an ensureCA failure into the operator-
// actionable cert_last_error note. ErrCertKeyRequired is the ONE cause this
// layer can name precisely (the certificate seal sentinel, never itself
// carrying key material); anything else falls back to a generic, still-
// actionable note rather than surfacing the raw error text.
func certReconcileAbortMessage(err error) string {
	if errors.Is(err, ErrCertKeyRequired) {
		return certSealKeyMessage
	}
	return certCAUnavailableMessage
}

// ErrCertificateNotFound is returned when a manual renew targets an unknown
// domain, or when the CA bundle is requested before any internal CA exists.
var ErrCertificateNotFound = errors.New("portal: certificate not found")

// ErrPublicCertificateNotManaged is returned by the public-domain export when
// cert_manage_public_domain is off or the requested domain is not in the
// configured cert_public_domains list -- i.e. the operator never asked the gateway
// to manage that name, so it will not hand its material out.
var ErrPublicCertificateNotManaged = errors.New("portal: public domain not managed")

// ErrCertReconcileInProgress is returned by RotateCertificateCA when certMu is
// already held by a running reconcile pass (review finding F1.2). A reconcile
// pass can hold that lock for up to its own deadline (up to several minutes,
// see cmd/gateway/cert_reconcile.go's certPassTimeout) -- and sync.Mutex.Lock
// cannot be interrupted by the HTTP request's context, so a blocking
// acquisition here would leave the CA-rotate request simply hanging for that
// whole window. The gateway maps this to HTTP 409 with a "retry shortly"
// message; the reconcile pass itself is completely unaffected -- it keeps
// running to completion under the exact same mutex as before.
var ErrCertReconcileInProgress = errors.New("portal: a certificate reconcile pass is in progress")

// desiredCert is one wanted certificate: its FQDN plus provenance.
type desiredCert struct {
	Domain   string
	Kind     string // gateway | server | public | edge
	ServerID string
	// Names is the full SAN list. Server/public kinds use exactly []string{Domain};
	// the self-signed gateway may add its explicit bind or NetBird peer IP, while
	// the edge row carries several names (and possibly IPs) because one connection
	// by bare IP sends no SNI and nginx can then only present one certificate.
	Names []string
	// NamesAuthoritative is meaningful for kind=gateway: false means the IP
	// resolver failed this pass, so a DNS-only observation must not remove a
	// previously issued IP SAN. It is deliberately separate from the function's
	// gatewayUnresolved return, which protects pruning when the DNS name itself
	// could not be resolved.
	NamesAuthoritative bool
}

// CertificateDTO is the portal view of a certificate. It carries NO key material
// and NO PEM -- only identity, status and timing.
type CertificateDTO struct {
	Domain        string     `json:"domain"`
	Kind          string     `json:"kind"`
	ServerID      string     `json:"server_id,omitempty"`
	ServerName    string     `json:"server_name,omitempty"`
	Status        string     `json:"status"`
	Fingerprint   string     `json:"fingerprint,omitempty"`
	NotBefore     *time.Time `json:"not_before,omitempty"`
	NotAfter      *time.Time `json:"not_after,omitempty"`
	IssuedAt      *time.Time `json:"issued_at,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	AttemptCount  int        `json:"attempt_count"`
	LastError     string     `json:"last_error,omitempty"`
	// Installed/InstalledFingerprint/InstalledAt/InstalledMode report what the
	// server's ServerAgent last said it has ACTUALLY installed (Phase 2
	// distribution). Only ever populated for a kind=server row that has a report:
	// Installed is true exactly when the reported leaf fingerprint EQUALS the
	// issued one, so a stale install reads as "reported, but different" rather
	// than as "installed". No report at all leaves all four at their zero values,
	// which the portal renders as "never reported" -- deliberately NOT as "not
	// installed": the registry is in-memory, so a gateway restart legitimately
	// erases every report without changing anything on any server.
	Installed            bool       `json:"installed"`
	InstalledFingerprint string     `json:"installed_fingerprint,omitempty"`
	InstalledAt          *time.Time `json:"installed_at,omitempty"`
	InstalledMode        string     `json:"installed_mode,omitempty"`
	// Transport/TransportAt report the last mesh-listener hop the gateway
	// observed for this server's ServerAgent. Only populated for a kind=server
	// row that has an observation: Transport is "tls" iff the newest observation
	// arrived over HTTPS/WSS, "plain" iff it arrived over HTTP/WS. No observation
	// leaves both fields at their zero values -- the portal renders that as "—"
	// rather than "plain", because the registry is in-memory (a gateway restart
	// legitimately erases every observation) and the mesh gate's arming
	// precondition also refuses to arm before a fresh TLS hop is seen.
	Transport   string     `json:"transport,omitempty"`
	TransportAt *time.Time `json:"transport_at,omitempty"`
}

// CertificateServerRefDTO is the deliberately narrow, public identity of a
// server that still has to persist the current gateway trust root. It carries
// no token, report payload, certificate PEM, or private material.
type CertificateServerRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CertificateCADTO is the portal view of the internal CA (self_signed mode).
// It NEVER carries the CA private key -- not even a "set" flag, because the key
// is an implementation detail of the issuer, not something an operator acts on.
type CertificateCADTO struct {
	Present     bool       `json:"present"`
	Subject     string     `json:"subject,omitempty"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	NotBefore   *time.Time `json:"not_before,omitempty"`
	NotAfter    *time.Time `json:"not_after,omitempty"`
	// PreviousFingerprint/PreviousNotAfter describe the rotated-out root that is
	// still published in the trust bundle (the rotation overlap). Empty when no
	// rotation has happened yet.
	PreviousFingerprint string     `json:"previous_fingerprint,omitempty"`
	PreviousNotAfter    *time.Time `json:"previous_not_after,omitempty"`
	// LastError is the reconcile's most recent abort note (cert_last_error, see
	// CertLastError) -- set when a pass gave up before it could place or renew
	// any order (no base domain resolvable, or the internal CA's key cannot be
	// sealed), cleared once a later pass gets past both gates. Review finding
	// F1.1: without this, that state was indistinguishable from "a fresh
	// install that has not reconciled yet". Never contains key/PEM material.
	LastError string `json:"last_error,omitempty"`
}

// CertificatesView lists every managed certificate; a problem shows up as
// status/last_error rather than as a missing row. Server names are resolved for
// display (best-effort -- a vanished server just leaves the name empty).
func (s *Service) CertificatesView(ctx context.Context) ([]CertificateDTO, error) {
	certs, err := s.routes.Certificates(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	if servers, err := s.routes.AIServers(ctx); err == nil {
		for _, srv := range servers {
			names[srv.ID] = srv.Name
		}
	}
	out := make([]CertificateDTO, 0, len(certs))
	for _, c := range certs {
		dto := CertificateDTO{
			Domain:        c.Domain,
			Kind:          c.Kind,
			ServerID:      c.ServerID,
			ServerName:    names[c.ServerID],
			Status:        c.Status,
			Fingerprint:   c.Fingerprint,
			NotBefore:     timePtr(c.NotBefore),
			NotAfter:      timePtr(c.NotAfter),
			IssuedAt:      timePtr(c.IssuedAt),
			NextAttemptAt: timePtr(c.NextAttemptAt),
			AttemptCount:  c.AttemptCount,
			LastError:     c.LastError,
		}
		// Only a kind=server row has an agent that could install anything; every
		// other kind stays at the zero values, which the portal renders as "—".
		if s.agentCertReports != nil && c.Kind == "server" && c.ServerID != "" {
			if fp, _, mode, _, reportedAt, ok := s.agentCertReports.CertReport(c.ServerID); ok {
				dto.InstalledFingerprint = fp
				dto.InstalledMode = mode
				dto.InstalledAt = timePtr(reportedAt)
				// Equality against the ISSUED fingerprint, not mere presence: an agent
				// still holding the previous leaf must read as stale, not installed.
				dto.Installed = fp != "" && fp == c.Fingerprint
			}
		}
		// Transport observation lives on the SAME kind=server row as Installed.
		// A gateway/public/edge row has no ServerAgent hop to describe.
		if s.agentTransport != nil && c.Kind == "server" && c.ServerID != "" {
			if transport, at, ok := s.agentTransport.LatestTransport(c.ServerID); ok {
				dto.Transport = transport
				dto.TransportAt = timePtr(at)
			}
		}
		out = append(out, dto)
	}
	return out, nil
}

// timePtr maps the store's zero time to a nil JSON field, so "never happened"
// reads as null rather than as year 1.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

// netbirdDNSDomain is the best-effort fallback for cert_base_domain: the NetBird
// account's own dns_domain, which is exactly the suffix every mesh server name
// already lives under. Any failure (module off, unreachable, no permission)
// yields "" -- the caller then skips the pass rather than guessing a suffix.
func (s *Service) netbirdDNSDomain(ctx context.Context) string {
	net, err := s.NetbirdNetwork(ctx)
	if err != nil {
		slog.Debug("certificate base domain: netbird dns_domain unavailable", "err", err)
		return ""
	}
	return strings.ToLower(strings.TrimSpace(net.DNSDomain))
}

// desiredCertificates builds the wanted name set: the gateway's mesh FQDN, every
// effectively-managed NetBird server, the public domains when that switch is on,
// and the gateway's own edge (nginx) row. Duplicates are collapsed (first
// provenance wins) so one pass can never place two orders for the same name.
//
// internalServable is false when the INTERNAL issuer mode's mandatory field is
// missing (see CertSettings' ok and edgeModeUsable): the internal names then
// cannot be ordered at all, so they are left out entirely rather than turned into
// failed orders -- only the edge row and (review round-1 addition) any public
// domains whose OWN mode is independently usable (publicModeUsable) are wanted;
// both have their own independently-configured issuer mode/ACME account, so
// neither depends on whatever made the internal mode unusable. The caller must
// NOT prune internal rows on such a pass: their absence from the desired set is
// a consequence of the missing setting, not proof that they became unwanted.
//
// The second return value reports that the GATEWAY name could not be resolved this
// pass. With cert_gateway_domain unset (the documented default) that name comes from
// a LIVE NetBird admin-API call, which returns an error on a timeout / 401 / a
// vanished peer. The desired set is then missing a name that is in fact still wanted,
// so the caller MUST NOT prune gateway rows on such a pass — deleting the row would
// destroy the sealed private key and force a fresh order (Let's Encrypt allows only
// 5 duplicate certificates per week), leaving the gateway with no certificate at all
// in the meantime. A resolution that legitimately yields NO name (no gateway peer
// selected) returns ("", nil) and is NOT reported as unresolved: that is a deliberate
// configuration, and pruning a stale gateway row is then correct.
//
// The THIRD return value is the set of kind=server names that were deliberately
// LEFT OUT because the server has no agent token: with no agent there is no
// distribution path, so ordering a certificate for it would only burn an order
// (and, in the ACME mode, one of the five weekly duplicates) for material nobody
// can fetch. They are reported separately rather than silently dropped, so the
// caller can keep a still-valid row alive WITH the reason on it instead of pruning
// it -- see ReconcileCertificates' prune loop.
func (s *Service) desiredCertificates(ctx context.Context, set CertSettings, internalServable bool) ([]desiredCert, map[string]desiredCert, bool, error) {
	out := make([]desiredCert, 0, 8)
	agentless := map[string]desiredCert{}
	seen := map[string]bool{}
	add := func(d desiredCert) {
		if d.Domain == "" || seen[d.Domain] {
			return
		}
		if len(d.Names) == 0 {
			d.Names = []string{d.Domain}
		}
		seen[d.Domain] = true
		out = append(out, d)
	}
	gatewayUnresolved := false
	if !internalServable {
		// Only rows with their OWN independently-configured, usable issuer mode
		// are orderable when the INTERNAL mode itself is not. Deliberately BEFORE
		// the gateway resolution so no live NetBird call is made for a name that
		// cannot be ordered anyway.
		//
		// Public domains are added BEFORE the edge row (review round-1 fix),
		// mirroring the internalServable=true branch below exactly: a name that
		// collides between a public domain and the edge row keeps the more
		// specific public provenance rather than silently becoming an edge one.
		// Gated on publicModeUsable, unlike the unconditional add below -- a
		// public domain whose OWN mode is not usable (e.g. acme with neither a
		// shared nor an own email configured) must not be turned into a failed
		// order on a pass where the internal mode already isn't usable either;
		// it is left out here exactly like the internal rows are, for the same
		// reason.
		if set.ManagePublicDomain && publicModeUsable(set) {
			for _, d := range set.PublicDomains {
				add(desiredCert{Domain: d, Kind: "public"})
			}
		}
		if edge, ok := edgeDesired(set); ok {
			add(edge)
		}
		return out, agentless, gatewayUnresolved, nil
	}
	gw := set.GatewayDomain
	if gw == "" {
		resolved, err := s.ResolveGatewayPeerDNS(ctx)
		if err != nil {
			gatewayUnresolved = true
			slog.Debug("certificate reconcile: gateway name unresolvable this pass", "err", err)
		} else {
			gw = strings.ToLower(strings.TrimSpace(resolved))
		}
	}
	gatewayNames := []string{gw}
	gatewayNamesAuthoritative := true
	if set.IssuerMode == IssuerModeSelfSigned {
		ip := net.ParseIP(strings.TrimSpace(s.agentBindHost))
		if ip == nil {
			resolved, err := s.ResolveGatewayPeerIP(ctx)
			if err != nil {
				gatewayNamesAuthoritative = false
			} else if trimmed := strings.TrimSpace(resolved); trimmed != "" {
				ip = net.ParseIP(trimmed)
				gatewayNamesAuthoritative = ip != nil
			} else {
				// An empty but error-free resolve is a transient blip (the gateway peer
				// was momentarily deselected, or a reconnecting peer reported no IP yet).
				// Treat it as NON-authoritative so a renewal in this pass preserves the
				// stored IP-SAN (via gatewayNamesWithStoredIPs) instead of dropping it.
				gatewayNamesAuthoritative = false
			}
		}
		if ip != nil {
			gatewayNames = append(gatewayNames, ip.String())
		}
	}
	add(desiredCert{
		Domain: gw, Names: gatewayNames, Kind: "gateway",
		NamesAuthoritative: gatewayNamesAuthoritative,
	})
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return nil, nil, gatewayUnresolved, err
	}
	for _, srv := range servers {
		domain := strings.ToLower(strings.TrimSpace(srv.Domain))
		if !srv.NetbirdEnabled || domain == "" {
			continue
		}
		if !certManaged(set.ServerScope, srv.CertificateOverride) {
			continue
		}
		want := desiredCert{Domain: domain, Kind: "server", ServerID: srv.ID}
		if !s.serverHasAgentToken(ctx, srv.ID) {
			// No agent token -> no ServerAgent -> nothing can ever fetch this
			// certificate (GET /api/agent/v1/certificate is authenticated by exactly
			// that token). Recorded, not silently dropped: agentless[] carries it to
			// the caller, which keeps a still-valid row alive with the reason on it.
			if !seen[domain] {
				agentless[domain] = want
			}
			continue
		}
		add(want)
	}
	if set.ManagePublicDomain {
		for _, d := range set.PublicDomains {
			add(desiredCert{Domain: d, Kind: "public"})
		}
	}
	// LAST, so an edge name that collides with an already-wanted internal name keeps
	// the internal provenance (add's first-wins rule) rather than silently turning a
	// mesh certificate into the edge one.
	if edge, ok := edgeDesired(set); ok {
		add(edge)
	}
	// A name that is wanted for ANOTHER reason too (an edge/public/gateway name that
	// happens to equal a server's domain) is genuinely wanted -- the missing agent
	// token then says nothing about it, so it must not be reported as agentless.
	for domain := range agentless {
		if seen[domain] {
			delete(agentless, domain)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, agentless, gatewayUnresolved, nil
}

// serverHasAgentToken reports whether serverID has a reporting-agent token, i.e.
// whether a ServerAgent could authenticate against the Phase 2 distribution
// endpoint at all.
//
// A STORE ERROR is treated as "has one" (keep managing the certificate): a
// transient read failure must never silently stop certificate management for a
// server that is in fact fine -- the same fail-direction every other reconcile
// step in this file takes.
func (s *Service) serverHasAgentToken(ctx context.Context, serverID string) bool {
	_, ok, err := s.routes.AgentTokenByServer(ctx, serverID)
	if err != nil {
		slog.Debug("certificate reconcile: agent-token lookup failed; keeping the server managed",
			"server_id", serverID, "err", err)
		return true
	}
	return ok
}

// gatewayNamesWithStoredIPs preserves the last known-good IP SANs while the
// gateway peer resolver is unavailable. Only IPs from a parseable stored leaf
// are carried forward; configured DNS remains the canonical name set, and an
// authoritative later resolver result still replaces stale IPs normally.
func gatewayNamesWithStoredIPs(names []string, cert routing.Certificate) []string {
	leaf := parseLeaf(cert.FullchainPEM)
	if leaf == nil {
		return names
	}
	result := append([]string(nil), names...)
	seen := make(map[string]bool, len(result)+len(leaf.IPAddresses))
	for _, name := range result {
		name = strings.TrimSpace(name)
		if ip := net.ParseIP(name); ip != nil {
			name = ip.String()
		} else {
			name = strings.ToLower(name)
		}
		if name != "" {
			seen[name] = true
		}
	}
	for _, raw := range leaf.IPAddresses {
		ip := net.ParseIP(raw.String())
		if ip == nil || seen[ip.String()] {
			continue
		}
		result = append(result, ip.String())
		seen[ip.String()] = true
	}
	return result
}

// certManaged applies the scope x override matrix: scope=all manages everything
// except an explicit exclude; scope=selected manages only an explicit include.
func certManaged(scope, override string) bool {
	if scope == "all" {
		return override != "exclude"
	}
	return override == "include"
}

// httpsSwitchInScope reports whether server is in scope for the P4
// https-auto-switch reconcile, given the resolved global cert_https_switch_mode
// (see CertHTTPSSwitchMode). Mirrors certManaged's scope x override matrix,
// applied to server.HTTPSSwitchOverride instead of a (scope, override) pair:
//   - "manual": never in scope (the gateway changes no app scheme).
//   - "auto": every server is in scope EXCEPT one overridden "exclude" (opt-out).
//   - "selected": only a server overridden "include" is in scope (opt-in).
//
// The one 3-state HTTPSSwitchOverride column (never two booleans) is what
// makes this safe against a mode flip: there is no stale opposite flag that
// could resurrect under the new mode -- see the routing.AIServer field doc.
func httpsSwitchInScope(server routing.AIServer, mode string) bool {
	switch mode {
	case "auto":
		return server.HTTPSSwitchOverride != "exclude"
	case "selected":
		return server.HTTPSSwitchOverride == "include"
	default: // "manual" and anything unknown
		return false
	}
}

// renewDue reports whether cert must be (re)issued now. The threshold is
// min(renewBeforeDays, lifetime/3) PLUS a deterministic per-domain jitter of up
// to a third of that window -- the jitter ADDS (renews earlier), so it never
// shortens the safety margin, and it de-synchronizes a cohort of certificates
// issued on the same day.
// caFingerprint is "" in the acme mode; in the self_signed mode it is the CURRENT
// root's fingerprint.
//
// The issuer-mismatch rule below fires ONLY for a leaf that was itself signed by an
// internal CA (IssuerFingerprint != ""), i.e. exactly the CA-ROTATION case. It must
// NOT fire for an ACME leaf (empty fingerprint) after a switch to the self_signed
// mode: forcing a re-issue there would put an internal-CA leaf in front of clients
// that do not have the root yet -- the opposite of continuity. An ACME leaf simply
// lives out its term and its normal renewal comes from the new issuer. The mirror
// case (self_signed -> acme) is covered by caFingerprint being "" in the acme mode,
// so a still-valid internal leaf is left alone there too.
//
// It is the composition of its two independent reasons (renewDueByTime ||
// issuerMismatch), kept as ONE predicate so every caller that only asks "is this
// row due?" is unaffected by the decomposition. The reconcile pass consumes the
// two halves SEPARATELY, because the CA-rotation propagation brake may hold back
// the issuer-mismatch reason and must NEVER hold back the time reason -- see
// caRotationPropagated.
func renewDue(cert routing.Certificate, renewBeforeDays int, now time.Time, domain, caFingerprint string) bool {
	return renewDueByTime(cert, renewBeforeDays, now, domain) || issuerMismatch(cert, caFingerprint)
}

// renewDueByTime is renewDue's status/time half: a row that is not "active" (or
// carries no expiry at all) is due immediately, otherwise the jittered
// min(renewBeforeDays, lifetime/3) window decides. This half is the GENUINE
// expiry signal and is never gated on anything.
func renewDueByTime(cert routing.Certificate, renewBeforeDays int, now time.Time, domain string) bool {
	if cert.Status != "active" || cert.NotAfter.IsZero() {
		return true
	}
	window := time.Duration(renewBeforeDays) * 24 * time.Hour
	if lifetime := cert.NotAfter.Sub(cert.NotBefore); lifetime > 0 {
		if third := lifetime / 3; third < window {
			window = third
		}
	}
	window += jitter(domain, window)
	return now.Add(window).After(cert.NotAfter)
}

// issuerMismatch is renewDue's CA-ROTATION half: the row was signed by an
// internal CA whose root is no longer the current one. It stays false for an
// ACME leaf (no issuer fingerprint) and in the acme mode (empty caFingerprint),
// for the continuity reasons spelled out in renewDue's doc comment above.
func issuerMismatch(cert routing.Certificate, caFingerprint string) bool {
	return caFingerprint != "" && cert.IssuerFingerprint != "" && cert.IssuerFingerprint != caFingerprint
}

// certCAPropagationTimeout bounds how long the propagation brake may hold back a
// leaf whose only reason to be re-issued is the CA rotation. After it, the
// re-issue happens regardless of what the agents report: a leaf must never be
// stuck on a root that is on its way out just because one agent went quiet.
const certCAPropagationTimeout = 24 * time.Hour

// caRotation is the once-per-pass snapshot the propagation brake needs. It is
// read ONCE per reconcile pass (never per row) and only when a brake could
// possibly engage, so a deployment with no report registry does not read
// anything extra at all.
type caRotation struct {
	// rotatedAt is cert_ca_rotated_at; zero when no rotation was ever recorded.
	rotatedAt time.Time
	// prevFingerprint is the fingerprint of cert_ca_prev_cert_pem, "" when there
	// is no previous root or it cannot be parsed.
	prevFingerprint string
	// prevValid reports that this previous root is still un-expired, i.e. still
	// published in the trust bundle (CertificateCABundlePEM's own rule).
	prevValid bool
}

// rotationAppliesToPreviousLeaf is the bounded overlap predicate shared by the
// gateway leaf decision and the fleet status. A leaf can only be held while it
// still chains to the valid previous root and the 24-hour propagation window is
// open. Any other state releases immediately.
func rotationAppliesToPreviousLeaf(cert routing.Certificate, rot caRotation, now time.Time) bool {
	if rot.rotatedAt.IsZero() || now.Sub(rot.rotatedAt) > certCAPropagationTimeout {
		return false
	}
	if rot.prevFingerprint == "" || !rot.prevValid {
		return false
	}
	return cert.IssuerFingerprint == rot.prevFingerprint
}

// gatewayTrustPropagation evaluates the COMPLETE ServerAgent fleet that relies
// on the gateway mesh endpoint: every non-disabled AI server with an agent token,
// independent of whether it has a kind=server certificate or which cert_mode it
// uses. A server is ready only when its report contains the current root in the
// durable fingerprint set. Store errors fail safe for this bounded gateway-leaf
// brake; known server identities remain visible in pending.
func (s *Service) gatewayTrustPropagation(ctx context.Context, currentRoot string, rotation caRotation) (ready bool, pending []CertificateServerRefDTO) {
	now := s.clock().UTC()
	if currentRoot == "" || rotation.rotatedAt.IsZero() || now.Sub(rotation.rotatedAt) > certCAPropagationTimeout ||
		rotation.prevFingerprint == "" || !rotation.prevValid {
		return true, []CertificateServerRefDTO{}
	}
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		slog.Warn("gateway CA propagation fleet unavailable; holding the previous gateway leaf", "err", err)
		return false, []CertificateServerRefDTO{}
	}

	// A real store cannot return duplicate ids, but collapse them here so a
	// malformed/adapted source can never duplicate names in the operator alert.
	refsByID := make(map[string]CertificateServerRefDTO, len(servers))
	for _, server := range servers {
		if server.Status == routing.ServerStatusDisabled {
			continue
		}
		ref := CertificateServerRefDTO{ID: server.ID, Name: server.Name}
		if old, exists := refsByID[server.ID]; !exists || ref.Name < old.Name {
			refsByID[server.ID] = ref
		}
	}
	refs := make([]CertificateServerRefDTO, 0, len(refsByID))
	for _, ref := range refsByID {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Name == refs[j].Name {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Name < refs[j].Name
	})

	pending = make([]CertificateServerRefDTO, 0, len(refs))
	for _, ref := range refs {
		_, hasToken, tokenErr := s.routes.AgentTokenByServer(ctx, ref.ID)
		if tokenErr != nil {
			slog.Warn("gateway CA propagation agent-token lookup failed; holding the previous gateway leaf",
				"server_id", ref.ID, "err", tokenErr)
			pending = append(pending, ref)
			continue
		}
		if !hasToken {
			continue
		}
		propagated := false
		if s.agentCertReports != nil {
			_, caFingerprints, _, _, _, ok := s.agentCertReports.CertReport(ref.ID)
			if ok {
				for _, fingerprint := range caFingerprints {
					if fingerprint == currentRoot {
						propagated = true
						break
					}
				}
			}
		}
		if !propagated {
			slog.Debug("holding the previous gateway leaf until the agent durably trusts the current root",
				"server_id", ref.ID, "rotated_at", rotation.rotatedAt)
			pending = append(pending, ref)
		}
	}
	return len(pending) == 0, pending
}

// GatewayCARotationPendingServers exposes only the names needed by the
// certificate status response. It intentionally reuses gatewayTrustPropagation
// so the portal and reconcile cannot disagree about which fleet members matter.
func (s *Service) GatewayCARotationPendingServers(ctx context.Context) []CertificateServerRefDTO {
	ca, err := s.CertificateCAView(ctx)
	if err != nil || !ca.Present || ca.Fingerprint == "" {
		if err != nil {
			slog.Warn("gateway CA propagation status unavailable", "err", err)
		}
		return []CertificateServerRefDTO{}
	}
	rotation := s.readCARotation(ctx, s.clock().UTC())
	_, pending := s.gatewayTrustPropagation(ctx, ca.Fingerprint, rotation)
	return pending
}

// MeshTLSPendingServers lists every non-disabled AI server that has a
// reporting-agent token but whose LATEST observed mesh transport is not TLS
// (either plaintext, or never observed). It is the "who would this lock out"
// list the arming confirm dialog shows before enabling cert_mesh_require_tls.
// Derived from the fleet (AIServers + AgentTokenByServer) rather than from the
// transport registry alone, so a server that has never connected at all still
// appears. A server without an agent token cannot reach the mesh listener, so it
// is never pending. Never returns nil.
func (s *Service) MeshTLSPendingServers(ctx context.Context) []CertificateServerRefDTO {
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		slog.Warn("mesh TLS pending-fleet unavailable", "err", err)
		return []CertificateServerRefDTO{}
	}
	pending := make([]CertificateServerRefDTO, 0, len(servers))
	for _, server := range servers {
		if server.Status == routing.ServerStatusDisabled {
			continue
		}
		if _, hasToken, tokenErr := s.routes.AgentTokenByServer(ctx, server.ID); tokenErr != nil || !hasToken {
			// A lookup error must not hide a server from the "who gets locked out"
			// list, but without a token it cannot reach the mesh listener at all.
			if tokenErr != nil {
				slog.Warn("mesh TLS pending-fleet agent-token lookup failed", "server_id", server.ID, "err", tokenErr)
			}
			continue
		}
		transport := ""
		if s.agentTransport != nil {
			if t, _, ok := s.agentTransport.LatestTransport(server.ID); ok {
				transport = t
			}
		}
		if transport != "tls" {
			pending = append(pending, CertificateServerRefDTO{ID: server.ID, Name: server.Name})
		}
	}
	return normalizeCertificateServerRefs(pending)
}

// publicCertRow resolves the stored certificate for a public-domain export. It
// enforces BOTH gates before touching material: (1) cert_manage_public_domain is
// on AND the domain is in the configured cert_public_domains list
// (ErrPublicCertificateNotManaged otherwise), and (2) a complete, valid, NOT
// expired row with kind=="public" exists for that domain (ErrCertificateNotFound
// otherwise). The kind check is what prevents a name collision from exporting a
// mesh/edge key: certificates.domain is a primary key, so if cert_gateway_domain
// (or the edge primary) equals a public name, the single stored row is
// kind=gateway/edge and this returns 404 -- never that foreign key (spec §7).
func (s *Service) publicCertRow(ctx context.Context, domain string) (routing.Certificate, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return routing.Certificate{}, ErrPublicCertificateNotManaged
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return routing.Certificate{}, err
	}
	if !CertManagePublicDomain(values) {
		return routing.Certificate{}, ErrPublicCertificateNotManaged
	}
	managed := false
	for _, d := range CertPublicDomains(values) {
		if d == domain {
			managed = true
			break
		}
	}
	if !managed {
		return routing.Certificate{}, ErrPublicCertificateNotManaged
	}
	cert, err := s.routes.CertificateByDomain(ctx, domain)
	if err != nil {
		return routing.Certificate{}, ErrCertificateNotFound
	}
	if cert.Kind != "public" || !certValid(cert, s.clock().UTC()) {
		return routing.Certificate{}, ErrCertificateNotFound
	}
	return cert, nil
}

// PublicCertificateBundlePEM returns the PUBLIC chain of a managed public-domain
// certificate (never a key), so an upstream reverse proxy can serve it and retire
// its own certbot. Gated by publicCertRow.
func (s *Service) PublicCertificateBundlePEM(ctx context.Context, domain string) (string, error) {
	cert, err := s.publicCertRow(ctx, domain)
	if err != nil {
		return "", err
	}
	return cert.FullchainPEM, nil
}

// PublicCertificateKeyPEM returns the PRIVATE KEY of a managed public-domain
// certificate. The kind=="public" gate in publicCertRow guarantees this can never
// be a mesh (kind=gateway) or edge key. A sealed key that cannot be opened
// surfaces as ErrCertKeyRequired, mapped to an actionable 400 by the handler.
func (s *Service) PublicCertificateKeyPEM(ctx context.Context, domain string) (string, error) {
	cert, err := s.publicCertRow(ctx, domain)
	if err != nil {
		return "", err
	}
	key, err := s.openCertSecret(cert.KeySealed)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", ErrCertificateNotFound
	}
	return key, nil
}

// normalizeCertificateServerRefs dedups by id (keeping the lexically smaller
// name) and sorts by name then id, so the operator alert is stable and never
// lists a server twice. Kept in the portal package so both the portal method and
// the gateway handler share one ordering.
func normalizeCertificateServerRefs(refs []CertificateServerRefDTO) []CertificateServerRefDTO {
	byID := make(map[string]CertificateServerRefDTO, len(refs))
	for _, ref := range refs {
		if old, ok := byID[ref.ID]; !ok || ref.Name < old.Name {
			byID[ref.ID] = ref
		}
	}
	out := make([]CertificateServerRefDTO, 0, len(byID))
	for _, ref := range byID {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// caRotationPropagated reports whether a kind=server row may already be re-signed
// under the CURRENT internal root. It gates ONLY the issuer-mismatch reason: a leaf
// that is genuinely expiring is renewed no matter what an agent reported.
// Fails OPEN (true) when: no report registry is wired; the row is not kind=server;
// there is no cert_ca_rotated_at timestamp; the wait exceeded
// certCAPropagationTimeout (24h); NO report exists for this server at all; the last
// report says cert_mode=="off" or carries no fingerprint (the agent installs
// nothing, so there is nothing to propagate); or the row's issuer is NOT the
// still-valid previous root.
//
// That fail-open list is the whole design, not a set of edge cases. In the DEFAULT
// configuration an agent runs with cert_mode=off and therefore reports nothing at
// all -- a brake that held back on "no report" would block EVERY rotation for a
// full 24h on every deployment that has not opted into distribution. And the last
// clause is what keeps the brake from pinning a leaf to a root that has already
// dropped out of the published bundle: only a leaf still chaining to the
// STILL-VALID previous root has anything to lose by being re-signed early.
//
// It holds back exactly one case: the agent is installing files, it has told us
// which roots it holds, and the current root is not among them -- re-signing now
// would hand that server a leaf its own clients cannot verify.
func (s *Service) caRotationPropagated(want desiredCert, cert routing.Certificate, currentCAFingerprint string, rot caRotation, now time.Time) bool {
	if s.agentCertReports == nil {
		return true
	}
	if want.Kind != "server" || want.ServerID == "" {
		return true
	}
	if rot.rotatedAt.IsZero() || now.Sub(rot.rotatedAt) > certCAPropagationTimeout {
		return true
	}
	if rot.prevFingerprint == "" || !rot.prevValid {
		return true
	}
	if cert.IssuerFingerprint != rot.prevFingerprint {
		return true
	}
	fingerprint, caFingerprints, mode, _, _, ok := s.agentCertReports.CertReport(want.ServerID)
	if !ok || mode == "off" || fingerprint == "" {
		return true
	}
	for _, fp := range caFingerprints {
		if fp == currentCAFingerprint {
			return true
		}
	}
	slog.Debug("holding back a CA-rotation re-issue: the new root has not reached this server's agent yet",
		"domain", cert.Domain, "server_id", want.ServerID, "rotated_at", rot.rotatedAt)
	return false
}

// certDue is renewDue plus the CA-rotation propagation brake, i.e. exactly the
// decision the reconcile pass makes per row. The two halves are deliberately kept
// apart here: the TIME half is honored unconditionally (an expiring certificate is
// always renewed), and only the ISSUER-MISMATCH half can be held back while the
// rotated-in root has demonstrably not reached this server's agent yet.
func (s *Service) certDue(want desiredCert, cert routing.Certificate, set CertSettings, now time.Time, rowCA string, rot caRotation, gatewayReady bool) bool {
	if renewDueByTime(cert, set.RenewBeforeDays, now, want.Domain) {
		return true
	}
	if !issuerMismatch(cert, rowCA) {
		return false
	}
	switch want.Kind {
	case "gateway":
		return gatewayReady || !rotationAppliesToPreviousLeaf(cert, rot, now)
	case "server":
		return s.caRotationPropagated(want, cert, rowCA, rot, now)
	default:
		return true
	}
}

// certNoAgentTokenReason is the last_error note on a still-valid certificate whose
// server has no reporting-agent token: nothing can fetch it, so it is not renewed,
// but it is also not destroyed -- it simply runs out.
const certNoAgentTokenReason = "no agent token: no distribution path"

// readCARotation loads the propagation brake's per-pass snapshot. A settings read
// failure yields the zero value, which makes the brake fail open (rotatedAt is
// then zero) -- a store blip must never be able to STOP a rotation from
// propagating.
func (s *Service) readCARotation(ctx context.Context, now time.Time) caRotation {
	out := caRotation{}
	if s.settings == nil {
		return out
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		slog.Debug("certificate reconcile: CA rotation state unavailable", "err", err)
		return out
	}
	if raw := strings.TrimSpace(values[certCARotatedAtKey]); raw != "" {
		if ts, perr := time.Parse(time.RFC3339, raw); perr == nil {
			out.rotatedAt = ts.UTC()
		}
	}
	prev := values[certCAPrevCertKey]
	if prev == "" {
		return out
	}
	out.prevFingerprint = certissue.FingerprintPEM(prev)
	if block, _ := pem.Decode([]byte(prev)); block != nil {
		if pc, perr := x509.ParseCertificate(block.Bytes); perr == nil && pc.NotAfter.After(now) {
			out.prevValid = true
		}
	}
	return out
}

// jitter is a stable per-domain offset in [0, window/3), in whole days when the
// window is at least three days (otherwise zero -- a short-lived certificate has
// no room to spread and renews often anyway). The modulo runs on the unsigned
// hash: converting to a signed int first could yield a NEGATIVE offset on a
// 32-bit platform, which would SHORTEN the safety window instead of widening it.
func jitter(domain string, window time.Duration) time.Duration {
	days := window / (24 * time.Hour) / 3
	if days <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(domain))
	return time.Duration(h.Sum32()%uint32(days)) * 24 * time.Hour
}

// backoffFor maps the consecutive-failure count to the next retry delay
// (5m, 1h, 6h, then 24h). remaining is the certificate's remaining validity: when
// it is under certUrgentRemaining the delay is capped at certUrgentBackoff.
func backoffFor(attempts int, remaining time.Duration) time.Duration {
	steps := []time.Duration{5 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}
	idx := attempts - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(steps) {
		idx = len(steps) - 1
	}
	d := steps[idx]
	if remaining > 0 && remaining < certUrgentRemaining && d > certUrgentBackoff {
		d = certUrgentBackoff
	}
	return d
}

// setCertLastError best-effort records msg as cert_last_error (review finding
// F1.1). A store failure here is only Debug-logged and otherwise swallowed --
// the reconcile pass already gave up for its OWN reason; a failure to record
// WHY must never additionally crash or retry the pass. Nil-safe (s.settings
// may be nil in a minimal test Service).
func (s *Service) setCertLastError(ctx context.Context, msg string) {
	if s.settings == nil {
		return
	}
	if err := s.settings.SetSystemSetting(ctx, certLastErrorKey, msg, s.clock().UTC()); err != nil {
		slog.Debug("certificate reconcile: could not record the last-error note", "err", err)
	}
}

// clearCertLastError best-effort clears a stale abort note once a pass
// advances past both of ReconcileCertificates' abort gates (a resolvable base
// domain, and -- in the self_signed mode -- a usable internal CA). Called
// UNCONDITIONALLY at that point (even when there was nothing to clear): the
// note describes the OUTCOME of the two gates specifically, not of the pass as
// a whole, so as soon as both gates are satisfied it is no longer true,
// regardless of what happens later in the same pass (a separate failure
// further down, e.g. the desired set or the certificate list being
// unavailable, is its own already-logged condition and is not this note's
// concern -- scope intentionally limited to the two silent dead-ends F1.1
// names).
//
// The ONE exception is a blocked seal path: while the certificate encryption
// key is missing on a disk-backed store, NOTHING can be issued in either mode
// (see certSealBlocked), so that note stays -- and is (re)asserted here rather
// than erased. Without this the note would FLAP in the ACME mode: it is written
// per attempt by issueAndStore, but a domain sitting in its failure backoff
// produces no attempt, so the passes in between would clear a condition that is
// still completely true. Asserting it (instead of merely skipping the clear)
// also means a stale note from gate 1 cannot outlive its own cause here.
func (s *Service) clearCertLastError(ctx context.Context) {
	if s.certSealBlocked() {
		s.setCertLastError(ctx, certSealKeyMessage)
		return
	}
	s.setCertLastError(ctx, "")
}

// certSealBlocked reports whether sealing a certificate private key would be
// refused right now with ErrCertKeyRequired. It mirrors sealCertSecret's
// refusal condition exactly (no certificate cipher AND a non-volatile settings
// store) and reads no secret material.
func (s *Service) certSealBlocked() bool {
	return s.cert.cipher == nil && !s.settingsVolatile
}

// noteCertKeyRequired surfaces a missing OP_AI_GATEWAY_CERT_ENCRYPTION_KEY at
// the MODULE level (cert_last_error) and at Warn in the log. It is called from
// the per-domain refusals inside issueAndStore AND from the self_signed ensureCA
// gate, so BOTH issuer modes emit a log line that names the variable rather than
// just the bare sentinel. domain may be "" (the gate fails before any single
// domain is in play), in which case the attribute is omitted.
//
// Without it the ACME mode reported this cause almost invisibly: the abort gates
// only cover the base domain and -- self_signed ONLY -- the internal CA, and
// clearCertLastError then runs, so an ACME pass produced no module-level note at
// all; just a per-domain row carrying the bare sentinel string and a Debug line
// naming no variable. Yet the cause is a deployment-wide misconfiguration (no
// certificate can ever be issued, in either mode), so it belongs in the same
// operator-facing field as the two gates.
//
// Any OTHER cause is deliberately left alone: those are per-domain conditions
// already recorded on the row by recordCertFailure, and promoting them to the
// module note would make one bad domain look like a module-wide outage.
func (s *Service) noteCertKeyRequired(ctx context.Context, domain string, cause error) {
	if !errors.Is(cause, ErrCertKeyRequired) {
		return
	}
	// Warn, not Debug: this is invisible at the default log level otherwise, and
	// it names the variable so the message is actionable on its own. The sentinel
	// carries no key material.
	attrs := []any{"variable", "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", "err", cause}
	if domain != "" {
		attrs = append([]any{"domain", domain}, attrs...)
	}
	slog.Warn("certificate issuance blocked: no certificate encryption key is configured", attrs...)
	s.setCertLastError(ctx, certSealKeyMessage)
}

// ReconcileCertificates runs ONE pass: build the desired set, prune what is no
// longer wanted, then issue/renew what is due (most urgent first, capped). It is
// best-effort throughout -- every failure is recorded on the row and logged; a
// still-valid certificate is NEVER replaced or deleted by a failed attempt. With
// the module off (or with NEITHER the internal nor the edge issuer mode usable) it
// returns immediately without touching the store, the network, or the internal CA.
func (s *Service) ReconcileCertificates(ctx context.Context) {
	set, internalServable, err := s.CertSettings(ctx)
	if err != nil {
		return
	}
	// CertSettings reports ok=false as soon as the INTERNAL mode's mandatory field
	// is missing -- and the internal mode DEFAULTS to acme. An operator who wants
	// only the edge certificate (module on, internal mode left at its default, no
	// acme_email, edge mode self_signed) would otherwise get nothing issued,
	// silently. So ok=false means "the internal names are not servable", not "do
	// nothing": the pass continues whenever the edge row is usable on its own, and
	// desiredCertificates then yields ONLY that row. With the module off,
	// CertSettings returns a zero struct, so EdgeEnabled is false and this is
	// byte-identical to the previous early return.
	//
	// The public-domain row gets the SAME exemption (review round-1 fix): it has
	// its own independently-configured issuer mode (PublicIssuerMode, following
	// the global one only until set) and, since U-T3, its own independently-
	// configurable ACME account. Without publicWanted/publicModeUsable here, a
	// deployment that left the internal/global ACME unconfigured (the default)
	// but fully configured a public domain -- self_signed, or acme with its own
	// non-shared email+directory -- got "no usable issuer" and that fully
	// configured public domain was never even attempted.
	_, edgeWanted := edgeDesired(set)
	publicWanted := set.ManagePublicDomain && len(set.PublicDomains) > 0
	if !internalServable && !(edgeWanted && edgeModeUsable(set)) && !(publicWanted && publicModeUsable(set)) {
		// Silent dead end otherwise: nothing is issued and nothing says why. Gated on
		// the module actually being ON -- CertSettings returns the ZERO struct when it
		// is off (IssuerMode is never empty otherwise, CertIssuerMode always falls back
		// to a known mode), and the off state must stay a strict no-op: no store write,
		// no note.
		if set.IssuerMode != "" {
			s.setCertLastError(ctx, certNoUsableIssuerMessage)
		}
		return
	}
	s.cert.mu.Lock()
	defer s.cert.mu.Unlock()
	base := set.BaseDomain
	if base == "" {
		// Best-effort: fall back to the NetBird account's dns_domain. Empty means we
		// cannot tell which names are reachable -> skip the pass rather than guess.
		base = s.netbirdDNSDomain(ctx)
	}
	if base == "" && !edgeWanted && !publicWanted {
		// Only INTERNAL names need the base domain (for the ACME under-base rule and
		// the CA subject suffix). The edge row is exempt (an edge-only pass has
		// nothing to derive it for) and so is a public-only pass (review round-2
		// fix): public domains are standalone FQDNs the operator already typed in
		// full, not names derived from a base domain, and the under-base ACME rule
		// itself already exempts want.Kind == "public" (~line 1244) -- so neither
		// exemption here is new, this just stops the pass from returning before it
		// ever reaches that rule.
		slog.Debug("certificate reconcile skipped: no base domain configured or derivable")
		s.setCertLastError(ctx, certNoBaseDomainMessage)
		return
	}
	// self_signed mode: make sure a usable CA exists and rotate it in time. A
	// failure here leaves the existing CA in force and skips the pass -- it must
	// never take down working leaves. A CA is needed when ANY of the three modes
	// is self_signed: the edge and public rows' modes are each independent of the
	// internal one. The public clause is a review round-1 addition: issueCertificate's
	// self_signed branch only ever LOADS the stored CA (loadCA), it never creates
	// one -- ensureCA is the ONLY path that mints the first CA. Without this clause,
	// a public-only self_signed deployment (internal mode acme, unconfigured) would
	// never get a CA created at all, and every self-signed public issuance would
	// fail forever with "no internal CA stored", even after the servability-gate
	// fix above lets the pass proceed.
	caFingerprint := ""
	if set.IssuerMode == IssuerModeSelfSigned ||
		(edgeWanted && set.EdgeIssuerMode == IssuerModeSelfSigned) ||
		(publicWanted && set.modeFor(certPublicKind) == IssuerModeSelfSigned) {
		ca, err := s.ensureCA(ctx, set, base)
		if err != nil {
			slog.Warn("internal CA unavailable; skipping certificate pass", "err", err)
			s.setCertLastError(ctx, certReconcileAbortMessage(err))
			// The line above carries only err.Error(), which for a missing certificate
			// key is the bare "system.cert_key_required" sentinel -- an operator
			// reading the LOGS would not learn which variable to set (the portal note
			// does name it). noteCertKeyRequired adds exactly that Warn; its note write
			// is the same string certReconcileAbortMessage just wrote, so it is
			// idempotent. Non-key causes -- a store error, an unparseable stored cert --
			// are left with the generic message above and get no extra line.
			s.noteCertKeyRequired(ctx, "", err)
			return
		}
		caFingerprint = ca.Fingerprint()
	}
	// Past both abort gates: whatever note was recorded there no longer
	// describes this pass. See clearCertLastError's doc comment for exactly
	// what this clears (and does not clear).
	s.clearCertLastError(ctx)
	now := s.clock().UTC()
	// The CA-rotation propagation brake's per-pass snapshot. Read ONCE (never per
	// row), and only when the brake could engage at all: without a report registry
	// or without a current internal root, caRotationPropagated fails open anyway,
	// so a deployment that does not use Phase 2 distribution issues NO extra
	// settings read under certMu. cert_ca_rotated_at and cert_ca_prev_cert_pem
	// cannot be derived from anything the pass already holds -- caFingerprint is the
	// CURRENT root, and the brake has to compare against the PREVIOUS one.
	rotation := caRotation{}
	gatewayReady := true
	if caFingerprint != "" {
		rotation = s.readCARotation(ctx, now)
		gatewayReady, _ = s.gatewayTrustPropagation(ctx, caFingerprint, rotation)
	}
	desired, agentless, gatewayUnresolved, err := s.desiredCertificates(ctx, set, internalServable)
	if err != nil {
		slog.Debug("certificate reconcile: desired set unavailable", "err", err)
		return
	}
	if set.EdgeEnabled && !edgeWanted {
		// Deliberately NOT an abort: the internal certificates must keep working. The
		// operator turned the feature on and has not finished configuring it, which is
		// exactly what cert_last_error is for. It has to run AFTER clearCertLastError
		// above, which would otherwise erase the note it just recorded.
		s.setCertLastError(ctx, certEdgeNamesMissingMessage)
	}
	wanted := map[string]desiredCert{}
	for _, d := range desired {
		wanted[d.Domain] = d
	}
	existing, err := s.routes.Certificates(ctx)
	if err != nil {
		slog.Debug("certificate reconcile: list failed", "err", err)
		return
	}
	byDomain := map[string]routing.Certificate{}
	for _, c := range existing {
		byDomain[c.Domain] = c
		if _, keep := wanted[c.Domain]; keep {
			continue
		}
		// A pass that could not resolve the gateway name is missing a name that is
		// still wanted, so its absence from the desired set proves nothing. Keep the
		// row (and its sealed key) and retry next pass; only a pass that actually
		// knows the gateway name may prune one.
		if gatewayUnresolved && c.Kind == "gateway" {
			slog.Warn("keeping gateway certificate: its name could not be resolved this pass",
				"domain", c.Domain)
			continue
		}
		// A server that lost (or never had) its agent token is not "unwanted": the
		// operator still has it opted in, there is just nothing to deliver to. A row
		// that still carries VALID material keeps it and runs out normally, with the
		// reason recorded so the omission is visible in the portal rather than silent.
		// A row WITHOUT material has nothing to preserve, so it is pruned like any
		// other unwanted name. The kind check keeps this exact: agentless[] is about
		// SERVER rows, and rewriting some other kind's row with a server provenance
		// would be worse than the prune it is trying to prevent.
		if want, ok := agentless[c.Domain]; ok && c.Kind == "server" {
			if certValid(c, now) {
				// Record ONLY the reason: keep the row's own status and attempt
				// count. Flipping a still-valid row to "skipped" would make
				// renewDueByTime's first clause (status != "active") permanently
				// true, so the moment an agent token appears the row is re-issued
				// on the spot -- burning an order for a certificate that may still
				// have most of its life left, and resetting the backoff ladder that
				// a preceding failure had built up. The omission stays visible: the
				// reason rides last_error either way.
				s.recordCertStatus(ctx, want, c, c.Status, certNoAgentTokenReason, now, c.AttemptCount)
				continue
			}
		}
		// A dormant edge row is STALE, not unwanted: the switch being off (or its name
		// list momentarily empty/mistyped) is not a delete order. Deleting it would
		// destroy the sealed private key and force a fresh order. The row is removed in
		// exactly ONE situation: the first name changed and the certificate for the new
		// first name was issued and stored successfully (handled in issueAndStore).
		if c.Kind == certEdgeKind {
			continue
		}
		// A pass on which the internal names were not enumerated at all (their issuer
		// mode is missing its mandatory field) knows nothing about whether they are
		// still wanted -- so it may not prune them either.
		if !internalServable {
			slog.Warn("keeping certificate: its issuer mode is not fully configured this pass",
				"domain", c.Domain, "kind", c.Kind)
			continue
		}
		if err := s.routes.DeleteCertificate(ctx, c.Domain); err != nil {
			slog.Debug("certificate prune failed", "domain", c.Domain, "err", err)
		}
	}
	type candidate struct {
		want      desiredCert
		remaining time.Duration
		// caFingerprint is the PER-ROW issuer fingerprint: the internal root's when
		// this row's own mode is self_signed, "" when it is acme. Carried alongside
		// the row so the issuance below stamps the right provenance even when the
		// two modes differ within one pass.
		caFingerprint string
	}
	pending := make([]candidate, 0, len(desired))
	for _, want := range desired {
		cert := byDomain[want.Domain]
		if want.Kind == "gateway" && !want.NamesAuthoritative {
			want.Names = gatewayNamesWithStoredIPs(want.Names, cert)
		}
		// ACME ONLY: a name outside the base domain can never satisfy HTTP-01 (the
		// wildcard A record does not cover it), so record the reason instead of
		// burning failed orders. The internal CA has no such constraint: it can sign
		// any name. A name that ALREADY has valid material (e.g. issued while the
		// self_signed mode was active) keeps it and stays "active" -- a mode switch
		// must never invalidate a working certificate; only the reason is recorded.
		//
		// The mode is read PER ROW (set.modeFor): the edge row's mode is switchable
		// independently, so reading the global one here would silently record
		// "skipped" for a self_signed edge row under a global acme mode -- which is
		// the feature's primary entry case. The edge kind is exempt outright, like
		// "public": the base-domain rule exists for the NetBird wildcard A record,
		// which has nothing to do with the gateway's own edge name.
		if set.modeFor(want.Kind) == IssuerModeACME && want.Kind != "public" &&
			want.Kind != certEdgeKind && !underDomain(want.Domain, base) {
			reason := fmt.Sprintf("not under base domain %s", base)
			if certValid(cert, now) {
				s.recordCertStatus(ctx, want, cert, "active", reason, now, cert.AttemptCount)
			} else {
				s.recordCertStatus(ctx, want, cert, "skipped", reason, now, 0)
			}
			continue
		}
		if !cert.NextAttemptAt.IsZero() && cert.NextAttemptAt.After(now) {
			continue // backing off
		}
		// renewDue's issuer-mismatch clause compares the row's issuer against the
		// CURRENT internal root -- so it must only ever see a fingerprint for a row
		// whose OWN mode is self_signed. Handing an acme row the internal
		// fingerprint would re-issue it on every pass; handing a self_signed row ""
		// would never notice a CA rotation.
		rowCA := ""
		if set.modeFor(want.Kind) == IssuerModeSelfSigned {
			rowCA = caFingerprint
		}
		// SAN-drift re-issuance is EDGE-ONLY (design decision, review-confirmed): the
		// gateway leaf is deliberately NOT self-healed on a mesh-IP change. The
		// canonical client name is the FQDN and a raw-IP gateway_url is unsupported,
		// so a changed/added IP-SAN is picked up only at the next time-based renewal --
		// never on a drift check. This avoids re-issue flapping on transient NetBird
		// resolver blips (an empty/error resolve is treated as non-authoritative above,
		// which preserves the stored IP-SAN via gatewayNamesWithStoredIPs). See
		// spec §3.5 / §13.
		edgeSANDrift := want.Kind == certEdgeKind && sanDrift(cert, want.Names)
		if edgeSANDrift {
			// Fall through to issuance: the configured name set changed, so the stored
			// leaf no longer covers what nginx has to present. Age alone would not
			// notice this.
			slog.Info("edge certificate name set changed; re-issuing",
				"domain", want.Domain, "names", strings.Join(want.Names, ","))
		} else if !s.certDue(want, cert, set, now, rowCA, rotation, gatewayReady) {
			continue
		}
		remaining := time.Duration(0)
		if !cert.NotAfter.IsZero() {
			remaining = cert.NotAfter.Sub(now)
		}
		pending = append(pending, candidate{want: want, remaining: remaining, caFingerprint: rowCA})
	}
	// Most urgent first (a missing certificate has remaining 0 => front).
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].remaining < pending[j].remaining })
	if len(pending) > certOrdersPerPass {
		slog.Info("certificate reconcile capped", "pending", len(pending), "per_pass", certOrdersPerPass)
		pending = pending[:certOrdersPerPass]
	}
	for _, c := range pending {
		s.issueAndStore(ctx, set, c.want, byDomain[c.want.Domain], now, c.caFingerprint, wanted)
	}
	// A restart with an empty (fresh) volume must re-materialize the files even
	// when nothing was issued THIS pass (e.g. the stored certificate is not yet
	// due for renewal) -- the issueAndStore call above only covers the row that
	// was actually (re-)issued. Gated on edgeWanted, so an edge-off pass never
	// touches the output directory (the no-op invariance) and never even reads
	// the certificate list a second time when there is nothing to deliver.
	if edgeWanted {
		if err := s.DeliverEdgeCertificate(ctx); err != nil {
			slog.Debug("edge delivery at end of pass failed", "err", err)
		}
	}
}

// underDomain reports whether name is base itself or a subdomain of it.
func underDomain(name, base string) bool {
	return name == base || strings.HasSuffix(name, "."+base)
}

// certValid reports whether cert carries usable, unexpired material.
func certValid(cert routing.Certificate, now time.Time) bool {
	return cert.FullchainPEM != "" && cert.KeySealed != "" && !cert.NotAfter.IsZero() && cert.NotAfter.After(now)
}

// issueAndStore places one order and persists the outcome. On ANY failure -- the
// order itself or the sealing of the returned key -- the previous certificate
// material is preserved verbatim and only the failure bookkeeping changes.
//
// wanted is the pass's full desired set, used ONLY by the edge row's
// superseded-row prune below; pass nil from a caller that has no desired set (the
// prune is then skipped rather than guessing -- see pruneSupersededEdgeRows).
func (s *Service) issueAndStore(ctx context.Context, set CertSettings, want desiredCert, prev routing.Certificate, now time.Time, caFingerprint string, wanted map[string]desiredCert) {
	res, err := s.cert.issuer(ctx, set, want)
	if err != nil {
		// In the ACME mode the missing certificate key surfaces HERE (the account
		// key is sealed inside the issuer), not at an abort gate -- so report it
		// module-wide, exactly as the self_signed gate does.
		s.noteCertKeyRequired(ctx, want.Domain, err)
		s.recordCertFailure(ctx, want, prev, err, now)
		return
	}
	// An issuer that reports success but hands back an incomplete Result would
	// otherwise nil-panic on res.Leaf (killing the whole reconcile loop) or store an
	// empty "active" certificate that then gets distributed as garbage. Treat it as
	// a normal failure instead: the previous material stays, the row records why.
	if res.Leaf == nil || res.FullchainPEM == "" || res.KeyPEM == "" {
		s.recordCertFailure(ctx, want, prev, errors.New("issuer returned an incomplete certificate"), now)
		return
	}
	sealed, err := s.sealCertSecret(res.KeyPEM)
	if err != nil {
		// No key available -> do NOT store a certificate without its key. The order
		// already went upstream, so this failure gets the same backoff as a failed
		// order: without it a missing encryption key would re-order every pass.
		slog.Warn("certificate obtained but the key cannot be sealed", "domain", want.Domain, "err", err)
		s.noteCertKeyRequired(ctx, want.Domain, err)
		s.recordCertFailure(ctx, want, prev, err, now)
		return
	}
	cert := routing.Certificate{
		Domain:            want.Domain,
		Kind:              want.Kind,
		ServerID:          want.ServerID,
		FullchainPEM:      res.FullchainPEM,
		KeySealed:         sealed,
		Fingerprint:       res.Fingerprint,
		IssuerFingerprint: caFingerprint, // "" in the acme mode
		NotBefore:         res.Leaf.NotBefore.UTC(),
		NotAfter:          res.Leaf.NotAfter.UTC(),
		IssuedAt:          now,
		Status:            "active",
		CreatedAt:         prev.CreatedAt,
		UpdatedAt:         now,
	}
	if cert.CreatedAt.IsZero() {
		cert.CreatedAt = now
	}
	if err := s.routes.UpsertCertificate(ctx, cert); err != nil {
		slog.Warn("certificate store failed", "domain", want.Domain, "err", err)
		return
	}
	slog.Info("certificate issued", "domain", want.Domain, "kind", want.Kind, "not_after", cert.NotAfter)
	// Phase 2 distribution doorbell: push a cert_update frame to any of this
	// server's currently-open agent WebSocket connections so a waiting
	// ServerAgent does not have to wait for its next poll/reconnect. The
	// Kind=="server" clause is defensive belt-and-braces, not the effective
	// guard: only a "server" row ever carries a non-empty ServerID, so
	// ServerID != "" alone already selects exactly these rows. Synchronous but
	// guaranteed fast -- see ServiceDeps.OnCertificateIssued's contract and
	// AgentStreamRegistry.NotifyCertUpdate, which only marshals and does a
	// non-blocking enqueue, never a network write, so this call can never stall
	// the reconcile pass whose certMu is held for its whole duration.
	if want.Kind == "server" && want.ServerID != "" && s.cert.onIssued != nil {
		s.cert.onIssued(want.ServerID, cert.Fingerprint)
	}
	if want.Kind == certEdgeKind {
		s.pruneSupersededEdgeRows(ctx, want.Domain, wanted)
		// Best-effort: materialize the freshly-issued edge certificate for nginx
		// immediately, rather than waiting for the end-of-pass call below (or, in
		// the worst case, the NEXT reconcile pass) to notice it. A failure here is
		// Debug-only -- DeliverEdgeCertificate already recorded it via
		// noteEdgeWriteError (Warn + the in-memory edge-delivery status), and a
		// delivery problem must never abort or roll back an otherwise-successful
		// issuance.
		if err := s.DeliverEdgeCertificate(ctx); err != nil {
			slog.Debug("edge delivery after issuance failed", "err", err)
		}
	}
}

// pruneSupersededEdgeRows deletes edge rows whose domain is no longer the primary
// name. Called ONLY after a successful upsert of the new primary, so the key of a
// working certificate is never destroyed on a failed or partial issuance.
//
// wanted is the pass's full desired set and is consulted as a HARD guard: a stored
// row can carry kind='edge' while its domain is simultaneously a wanted INTERNAL
// name. edgeDesired is added LAST, so on such a collision add's first-wins rule
// drops the edge entry and the row keeps kind='edge' until its next actual re-issue
// -- for up to a full certificate lifetime. Matching on kind+domain alone would
// then delete a still-wanted certificate together with its unrecoverable sealed key
// (and, in the ACME mode, spend one of the five weekly duplicates re-ordering it).
// A nil map means the caller does not know the desired set, in which case NOTHING is
// pruned: not knowing is never a reason to delete a private key.
func (s *Service) pruneSupersededEdgeRows(ctx context.Context, keep string, wanted map[string]desiredCert) {
	if wanted == nil {
		return
	}
	existing, err := s.routes.Certificates(ctx)
	if err != nil {
		return
	}
	for _, c := range existing {
		if c.Kind != certEdgeKind || c.Domain == keep {
			continue
		}
		if _, stillWanted := wanted[c.Domain]; stillWanted {
			continue
		}
		if err := s.routes.DeleteCertificate(ctx, c.Domain); err != nil {
			slog.Debug("superseded edge certificate prune failed", "domain", c.Domain, "err", err)
		}
	}
}

// recordCertFailure records a failed attempt: status=error, the reason, an
// incremented attempt counter and a backoff target. The stored material and every
// stored time (not_before/not_after/issued_at) are carried over UNCHANGED, so a
// still-valid certificate keeps serving and keeps being distributable.
func (s *Service) recordCertFailure(ctx context.Context, want desiredCert, prev routing.Certificate, cause error, now time.Time) {
	attempts := prev.AttemptCount + 1
	remaining := time.Duration(0)
	if !prev.NotAfter.IsZero() {
		remaining = prev.NotAfter.Sub(now)
	}
	delay := backoffFor(attempts, remaining)
	if ra, ok := certissue.RetryAfter(cause); ok && ra > delay {
		delay = ra
	}
	next := prev
	next.Domain, next.Kind, next.ServerID = want.Domain, want.Kind, want.ServerID
	next.Status = "error"
	next.LastError = cause.Error()
	next.AttemptCount = attempts
	next.NextAttemptAt = now.Add(delay)
	next.UpdatedAt = now
	if next.CreatedAt.IsZero() {
		next.CreatedAt = now
	}
	if remaining > 0 && remaining < certUrgentRemaining {
		slog.Warn("certificate renewal failing while the certificate is about to expire",
			"domain", want.Domain, "remaining", remaining.String(), "err", cause)
	} else {
		slog.Debug("certificate order failed", "domain", want.Domain, "attempt", attempts, "err", cause)
	}
	if upErr := s.routes.UpsertCertificate(ctx, next); upErr != nil {
		slog.Debug("certificate error record failed", "domain", want.Domain, "err", upErr)
	}
}

// recordCertStatus writes a non-issuing status (skipped, or an "active but the
// current issuer cannot serve this name" note) without touching the stored
// certificate material or its times.
func (s *Service) recordCertStatus(ctx context.Context, want desiredCert, prev routing.Certificate, status, reason string, now time.Time, attempts int) {
	if prev.Status == status && prev.LastError == reason && prev.AttemptCount == attempts {
		return // no churn
	}
	next := prev
	next.Domain, next.Kind, next.ServerID = want.Domain, want.Kind, want.ServerID
	next.Status = status
	next.LastError = reason
	next.AttemptCount = attempts
	next.UpdatedAt = now
	if next.CreatedAt.IsZero() {
		next.CreatedAt = now
	}
	if err := s.routes.UpsertCertificate(ctx, next); err != nil {
		slog.Debug("certificate status record failed", "domain", want.Domain, "err", err)
	}
}

// RenewCertificateNow marks domain due for the next reconcile pass: the backoff
// target is cleared and the status set to "pending" (renewDue treats any
// non-active status as due). The stored certificate MATERIAL and its times are
// left untouched, so the certificate stays valid and distributable until a new one
// actually arrives. The attempt counter is deliberately KEPT: a manual click must
// not let a caller hammer past the rate limit.
//
// The route is system-scoped (requireWebScope("system") at the handler); as of
// PT-2 Part 2b this also checks isSystem(principal) itself (ErrPrincipalForbidden
// otherwise) as defense-in-depth against a future internal caller that bypasses
// the HTTP gate.
func (s *Service) RenewCertificateNow(ctx context.Context, principal auth.Token, domain string) error {
	if !isSystem(principal) {
		return ErrPrincipalForbidden
	}
	return s.markCertificateDue(ctx, domain)
}

// markCertificateDue is the shared "make this row due, keep its material" write
// behind RenewCertificateNow and ReissueAllCertificates. It re-reads the row
// immediately before writing so a concurrent reconcile that just issued fresh
// material is not rolled back to an older snapshot.
func (s *Service) markCertificateDue(ctx context.Context, domain string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	cert, err := s.routes.CertificateByDomain(ctx, domain)
	if err != nil {
		return ErrCertificateNotFound
	}
	cert.NextAttemptAt = time.Time{}
	cert.Status = "pending"
	cert.UpdatedAt = s.clock().UTC()
	return s.routes.UpsertCertificate(ctx, cert)
}

// issueCertificate is the production certIssuer. It forks on the row's OWN issuer
// mode (set.modeFor(want.Kind), NOT the global one -- the edge row's mode is
// switchable independently): the internal CA signs locally, ACME places an HTTP-01
// order. Everything downstream (storage, renewal, distribution) is identical for
// both. A row with no explicit SAN list falls back to its single domain. Server
// and public rows keep that one-name behavior; gateway and edge rows may carry
// multiple SANs.
func (s *Service) issueCertificate(ctx context.Context, set CertSettings, want desiredCert) (certissue.Result, error) {
	names := want.Names
	if len(names) == 0 {
		names = []string{want.Domain}
	}
	if set.modeFor(want.Kind) == IssuerModeSelfSigned {
		values, err := s.settings.SystemSettings(ctx)
		if err != nil {
			return certissue.Result{}, err
		}
		ca, err := s.loadCA(values)
		if err != nil {
			return certissue.Result{}, err
		}
		validity := time.Duration(set.SelfSignedValidityDays) * 24 * time.Hour
		return ca.IssueFor(names, validity, s.clock().UTC())
	}
	if s.cert.acmeChallenges == nil {
		return certissue.Result{}, errors.New("portal: no acme challenge store wired")
	}
	// Resolve the ACME account PER CONTEXT: the edge and public kinds may each
	// use their own directory/email (certAcmeConfigFor returns the global trio
	// for every other kind, and for edge/public whenever that context's
	// *ACMEShared is true -- the default, which is what keeps an unconfigured
	// deployment byte-neutral with the single-account behavior that preceded
	// this task).
	directory, email, _ := set.certAcmeConfigFor(want.Kind)
	key, accountURI, err := s.accountFor(ctx, directory, email)
	if err != nil {
		return certissue.Result{}, err
	}
	client := &certissue.ACMEClient{
		DirectoryURL: directory,
		Email:        email,
		AccountKey:   key,
		AccountURI:   accountURI,
		Challenges:   s.cert.acmeChallenges,
		HTTPClient:   acmeHTTPClient(),
	}
	// Obtain hard-rejects any IP in its name list -- HTTP-01 has no way to validate
	// a bare address. The settings layer already refuses that combination on write
	// (an acme edge mode with an IP name), so this stays a defense in depth: the
	// error is recorded on the row and the previous material is kept.
	return client.Obtain(ctx, names...)
}

// accountFor loads (or, on first use / directory change, registers) the ACME
// account for `directory`, keyed by directory so identical directories across
// contexts (internal, edge, public) share one account. email is the contact used
// only when registering a new account. The GLOBAL directory (acme_directory_url)
// resolves to the pre-unification unprefixed KV slot (see acmeAccountKeysFor),
// so a gateway upgrading from the single-slot layout adopts its already
// -registered account verbatim -- no re-registration on upgrade.
//
// An UNOPENABLE stored account key is treated as ABSENT: it is logged and the
// function falls through to mint + register a fresh account. Returning the error
// instead would wedge the ACME mode PERMANENTLY -- every later pass would fail
// identically on the same stored bytes, and recovery would need manual DB
// surgery -- which directly contradicts the promise that the certificate key can
// be rotated independently (a rotated key cannot open the old account key). The
// fall-through is safe: an account key is regenerable, and when the key is
// genuinely MISSING (rather than changed) sealCertSecret below refuses before
// any network call, so no plaintext can be written and no registration is
// attempted. These failures are deterministic, not transient, so retrying with
// the same bytes could never succeed anyway.
func (s *Service) accountFor(ctx context.Context, directory, email string) (*ecdsa.PrivateKey, string, error) {
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return nil, "", err
	}
	globalDir := strings.TrimSpace(ACMEDirectoryURL(values))
	keyK, uriK, dirK := acmeAccountKeysFor(directory, globalDir)
	storedDir := strings.TrimSpace(values[dirK])
	sealedKey := values[keyK]
	if sealedKey != "" && storedDir == strings.TrimSpace(directory) {
		if pemStr, openErr := s.openCertSecret(sealedKey); openErr != nil {
			// No key material in the error (openCertSecret wraps a MAC failure).
			slog.Warn("stored ACME account key could not be opened; registering a fresh account instead",
				"variable", "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", "err", openErr)
		} else if key, parseErr := certissue.ParseECKeyPEM(pemStr); parseErr != nil {
			slog.Warn("stored ACME account key could not be parsed; registering a fresh account instead", "err", parseErr)
		} else {
			return key, strings.TrimSpace(values[uriK]), nil
		}
	}
	key, err := certissue.GenerateAccountKey()
	if err != nil {
		return nil, "", err
	}
	pemStr, err := certissue.MarshalECKeyPEM(key)
	if err != nil {
		return nil, "", err
	}
	sealed, err := s.sealCertSecret(pemStr)
	if err != nil {
		return nil, "", err
	}
	client := &certissue.ACMEClient{DirectoryURL: directory, Email: email, AccountKey: key, Challenges: s.cert.acmeChallenges, HTTPClient: acmeHTTPClient()}
	uri, err := client.Register(ctx)
	if err != nil {
		return nil, "", err
	}
	// SystemSettingsStore writes ONE key at a time (SetSystemSetting), so persist
	// the three account fields individually. The key goes LAST: a crash between the
	// writes then leaves no key referencing a half-recorded account.
	now := s.clock().UTC()
	for _, kv := range [][2]string{{uriK, uri}, {dirK, directory}, {keyK, sealed}} {
		if err := s.settings.SetSystemSetting(ctx, kv[0], kv[1], now); err != nil {
			return nil, "", err
		}
	}
	return key, uri, nil
}

// errCAKeyMismatch is loadCA's pairing-check failure: the stored cert AND key
// each parse fine on their own, but do not belong together. This is the
// SPECIFIC signature of a crash between newCA's two writes of a rotation or a
// first creation -- the cert column was already overwritten with the new
// root, the key column was not yet caught up (see newCA's doc comment) -- as
// opposed to "no CA stored yet" or "the key cannot be opened" (e.g. a missing
// encryption key), which are different failures with a different correct
// recovery. ensureCA/RotateCertificateCA check for this SPECIFIC sentinel via
// resolvePreviousCertPEM (review finding F1.3): only in this one case is the
// cert column's value an orphan that must NOT be forwarded as the next
// "previous" root.
var errCAKeyMismatch = errors.New("portal: stored internal CA key does not match its certificate")

// loadCA reconstructs the stored internal CA (public cert + sealed key) and
// verifies that the two actually belong together. The pair check matters because
// the root and its key are two separate settings writes: a crash between them
// would otherwise leave a CA that signs leaves nobody can verify. A mismatch is
// reported as errCAKeyMismatch so ensureCA regenerates a consistent root (keeping
// the old public one in the bundle).
func (s *Service) loadCA(values map[string]string) (*certissue.SelfSignedCA, error) {
	certPEM := values[certCACertKey]
	sealed := values[certCAKeySealedKey]
	if certPEM == "" || sealed == "" {
		return nil, errors.New("portal: no internal CA stored")
	}
	keyPEM, err := s.openCertSecret(sealed)
	if err != nil {
		return nil, err
	}
	ca, err := certissue.LoadCA(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	pub, ok := ca.Cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&ca.Key.PublicKey) {
		return nil, errCAKeyMismatch
	}
	return ca, nil
}

// resolvePreviousCertPEM chooses what to record as the OUTGOING ("previous")
// CA root right before generating a new one (review finding F1.3). Normally
// that is simply the current cert column (values[certCACertKey]) -- the cert
// that WAS actively signing leaves right up until now, e.g. because its key
// became inaccessible (a missing encryption key) or it is a normal scheduled
// rotation. The ONE exception is errCAKeyMismatch: the specific signature of a
// crash between newCA's two writes (see errCAKeyMismatch's doc comment). In
// that case the cert column holds an orphan that never had a matching key and
// so never signed anything, while cert_ca_prev_cert_pem ALREADY correctly
// holds the genuinely-previous, still-leaf-signing root (newCA writes that
// column FIRST, before the new cert becomes referenceable) -- forwarding the
// orphan there instead would silently discard the one root every currently-
// deployed leaf actually chains to. If the cert column cannot even be parsed,
// loadCA never reaches the pairing check (it fails earlier, with a different
// error), so this function is not reached with that error and the caller
// keeps forwarding the cert column verbatim, exactly as before this fix.
// newCA's own defensive fingerprint check drops the returned value if it
// happens to be empty or unparseable, so this never needs to re-validate it.
func resolvePreviousCertPEM(values map[string]string, loadErr error) string {
	if errors.Is(loadErr, errCAKeyMismatch) {
		return values[certCAPrevCertKey]
	}
	return values[certCACertKey]
}

// ensureCA returns the usable internal CA, creating one on first use and ROTATING
// it once it enters its renewal window. Rotation order matters: the new root is
// stored and the OLD PUBLIC root is kept in cert_ca_prev_cert_pem, so the trust
// bundle carries both. Only after that do leaves get re-signed (the renewDue
// issuer-fingerprint mismatch drives that on this and the following passes) --
// otherwise a client holding only the old root would break the instant a server
// presented a leaf of the new one.
func (s *Service) ensureCA(ctx context.Context, set CertSettings, base string) (*certissue.SelfSignedCA, error) {
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return nil, err
	}
	now := s.clock().UTC()
	ca, err := s.loadCA(values)
	if err != nil {
		// No (or unreadable) CA: create the first one. resolvePreviousCertPEM
		// (review finding F1.3) preserves the genuine previous root instead of
		// discarding it when this is specifically the cert-without-its-
		// matching-key crash window -- see its doc comment.
		//
		// A CA that EXISTS but cannot be opened (e.g. the certificate encryption
		// key was rotated) recovers gracefully here -- but it must not do so
		// silently: minting a new root means every client has to trust the new
		// bundle, so say it out loud. "no internal CA stored" is the ordinary
		// first-run path and stays quiet.
		//
		// This states the INTENT only: newCA can still refuse (a missing
		// certificate encryption key makes sealCertSecret fail BEFORE any
		// SetSystemSetting, leaving the stored root byte-identical), so claiming
		// a new root as fact here would tell an operator mid-incident to
		// re-trust a bundle that never changed. The OUTCOME is reported by
		// newCA's own "internal CA created" line on success, and by the caller's
		// abort Warn plus noteCertKeyRequired on failure.
		if values[certCACertKey] != "" {
			slog.Warn("stored internal CA could not be opened; attempting to mint a NEW root (if that succeeds, clients must trust the new bundle)",
				"variable", "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", "err", err)
		}
		return s.newCA(ctx, base, now, resolvePreviousCertPEM(values, err))
	}
	remaining := ca.Cert.NotAfter.Sub(now)
	window := time.Duration(set.CARenewBeforeDays) * 24 * time.Hour
	if remaining > window {
		return ca, nil
	}
	if remaining < caWarnRemaining {
		slog.Warn("internal CA is about to expire; rotating now",
			"not_after", ca.Cert.NotAfter, "remaining", remaining.String())
	} else {
		slog.Info("internal CA entered its renewal window; rotating", "not_after", ca.Cert.NotAfter)
	}
	rotated, err := s.newCA(ctx, base, now, values[certCACertKey])
	if err != nil {
		// Keep serving with the existing CA rather than losing issuance entirely.
		slog.Warn("internal CA rotation failed; keeping the current root", "err", err)
		return ca, nil
	}
	return rotated, nil
}

// newCA generates and persists a fresh root. prevCertPEM (the outgoing PUBLIC
// root, may be empty) is stored as the previous root so the bundle keeps it until
// it expires. The key is written LAST so a crash mid-rotation leaves the previous
// root recorded before the new one becomes referenceable; loadCA's pair check
// catches the remaining cert-without-its-key window and forces a regeneration.
func (s *Service) newCA(ctx context.Context, base string, now time.Time, prevCertPEM string) (*certissue.SelfSignedCA, error) {
	subject := "OP AI Gateway Internal CA"
	if base != "" {
		subject += " (" + base + ")"
	}
	ca, certPEM, keyPEM, err := certissue.NewCA(subject, selfSignedCAValidity)
	if err != nil {
		return nil, err
	}
	sealed, err := s.sealCertSecret(keyPEM)
	if err != nil {
		return nil, err
	}
	// Drop an unparseable outgoing root -- it would only bloat the bundle. An
	// EXPIRED one is filtered at read time by CertificateCABundlePEM, which is the
	// authority on what is still worth publishing.
	if prevCertPEM != "" && certissue.FingerprintPEM(prevCertPEM) == "" {
		prevCertPEM = ""
	}
	// cert_ca_rotated_at goes FIRST -- before the previous root, before the new one,
	// and (as before) with the sealed key last. The asymmetry is deliberate: a
	// timestamp WITHOUT a completed rotation is inert (the stored root is unchanged,
	// so every agent already reports it and caRotationPropagated holds nothing back),
	// whereas a completed rotation WITHOUT a timestamp would silently disable the
	// propagation brake for exactly the rotation it was built for.
	for _, kv := range [][2]string{
		{certCARotatedAtKey, now.UTC().Format(time.RFC3339)},
		{certCAPrevCertKey, prevCertPEM},
		{certCACertKey, certPEM},
		{certCAKeySealedKey, sealed},
	} {
		if err := s.settings.SetSystemSetting(ctx, kv[0], kv[1], now); err != nil {
			return nil, err
		}
	}
	if s.cert.onCABundleChanged != nil {
		s.cert.onCABundleChanged(ca.Fingerprint())
	}
	slog.Info("internal CA created", "subject", subject, "not_after", ca.Cert.NotAfter,
		"fingerprint", ca.Fingerprint(), "rotated", prevCertPEM != "")
	return ca, nil
}

// CertificateCAView reports the internal CA for the portal: subject, validity and
// the previous root's tail. It NEVER carries the CA key.
func (s *Service) CertificateCAView(ctx context.Context) (CertificateCADTO, error) {
	out := CertificateCADTO{}
	if s.settings == nil {
		return out, nil
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return out, err
	}
	// Surfaced regardless of whether a CA is present yet -- a reconcile pass
	// that could not even resolve a base domain never gets far enough to
	// create one, and that state must not read identically to "not started".
	out.LastError = CertLastError(values)
	block, _ := pem.Decode([]byte(values[certCACertKey]))
	if block == nil {
		return out, nil // not present yet -> Present stays false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return out, nil
	}
	out.Present = true
	out.Subject = cert.Subject.CommonName
	out.Fingerprint = certissue.FingerprintPEM(values[certCACertKey])
	out.NotBefore = timePtr(cert.NotBefore.UTC())
	out.NotAfter = timePtr(cert.NotAfter.UTC())
	if prev := values[certCAPrevCertKey]; prev != "" {
		if pb, _ := pem.Decode([]byte(prev)); pb != nil {
			if pc, perr := x509.ParseCertificate(pb.Bytes); perr == nil {
				out.PreviousFingerprint = certissue.FingerprintPEM(prev)
				out.PreviousNotAfter = timePtr(pc.NotAfter.UTC())
			}
		}
	}
	return out, nil
}

// CertificateCABundlePEM is the trust anchor handed to clients: the current root
// plus the previous root while that one is still valid (the rotation overlap). It
// contains public certificates only.
func (s *Service) CertificateCABundlePEM(ctx context.Context) (string, error) {
	if s.settings == nil {
		return "", ErrCertificateNotFound
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return "", err
	}
	return certificateCABundlePEM(values, s.clock().UTC())
}

// certificateCABundlePEM constructs the public trust bundle from one coherent
// settings snapshot. The current root must parse; the optional previous root is
// appended only while it remains parseable and unexpired.
func certificateCABundlePEM(values map[string]string, now time.Time) (string, error) {
	current := values[certCACertKey]
	block, _ := pem.Decode([]byte(current))
	if block == nil {
		return "", ErrCertificateNotFound
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return "", ErrCertificateNotFound
	}
	bundle := current
	if !strings.HasSuffix(bundle, "\n") {
		bundle += "\n"
	}
	if prev := values[certCAPrevCertKey]; prev != "" {
		if pb, _ := pem.Decode([]byte(prev)); pb != nil {
			if pc, perr := x509.ParseCertificate(pb.Bytes); perr == nil && pc.NotAfter.After(now) {
				bundle += prev
			}
		}
	}
	return bundle, nil
}

// RotateCertificateCA forces a rotation now (the portal button). The leaves are
// re-issued by the following reconcile passes via the issuer-fingerprint mismatch,
// so the new root is distributed BEFORE anything is signed with it.
//
// It acquires certMu via TryLock, not Lock (review finding F1.2): a running
// reconcile pass can hold certMu for up to its own deadline, and
// sync.Mutex.Lock cannot be interrupted by ctx -- a blocking acquisition would
// leave this HTTP request hanging for that whole window instead of failing
// fast. Lock unavailable -> ErrCertReconcileInProgress (mapped to HTTP 409 by
// the caller); this changes ONLY that one failure mode -- the successful path
// (lock immediately available) is unchanged.
//
// The route is system-scoped (requireWebScope("system") at the handler); as of
// PT-2 Part 2b this also checks isSystem(principal) itself (ErrPrincipalForbidden
// otherwise) as defense-in-depth against a future internal caller that bypasses
// the HTTP gate.
func (s *Service) RotateCertificateCA(ctx context.Context, principal auth.Token) error {
	if !isSystem(principal) {
		return ErrPrincipalForbidden
	}
	set, ok, err := s.CertSettings(ctx)
	if err != nil {
		return err
	}
	if !ok || set.IssuerMode != IssuerModeSelfSigned {
		return ErrCertInvalid
	}
	if !s.cert.mu.TryLock() {
		return ErrCertReconcileInProgress
	}
	defer s.cert.mu.Unlock()
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return err
	}
	base := set.BaseDomain
	if base == "" {
		base = s.netbirdDNSDomain(ctx)
	}
	// resolvePreviousCertPEM (review finding F1.3): if the currently-stored
	// CA cert/key pair specifically fails loadCA's pairing check, the cert
	// column is an orphan of an aborted rotation and must not become the new
	// "previous" root -- see resolvePreviousCertPEM's doc comment.
	_, loadErr := s.loadCA(values)
	_, err = s.newCA(ctx, base, s.clock().UTC(), resolvePreviousCertPEM(values, loadErr))
	return err
}

// ReissueAllCertificates marks EVERY managed certificate due, so the next reconcile
// passes re-issue them with the currently configured issuer. This is the deliberate
// "switch the issuer NOW" action -- the reconcile itself never forces a re-issue on
// a mode change, because a client that lacks the new trust anchor would break. The
// caller (portal button) is confirm-gated and warns about exactly that.
// Material, times and the attempt counter are left untouched: each row keeps
// serving until its replacement actually arrives, and a repeated click cannot
// hammer past a rate limit.
//
// The route is system-scoped (requireWebScope("system") at the handler); as of
// PT-2 Part 2b this also checks isSystem(principal) itself (ErrPrincipalForbidden
// otherwise) as defense-in-depth against a future internal caller that bypasses
// the HTTP gate.
func (s *Service) ReissueAllCertificates(ctx context.Context, principal auth.Token) error {
	if !isSystem(principal) {
		return ErrPrincipalForbidden
	}
	certs, err := s.routes.Certificates(ctx)
	if err != nil {
		return err
	}
	marked := 0
	for _, cert := range certs {
		if err := s.markCertificateDue(ctx, cert.Domain); err != nil {
			if errors.Is(err, ErrCertificateNotFound) {
				continue // pruned between the list and the write -- nothing to mark
			}
			return err
		}
		marked++
	}
	slog.Info("all certificates marked for re-issue", "count", marked)
	return nil
}

// SetServerCertificateOverride sets a server's certificate opt-in/opt-out. Gated by
// the shared server-manage chokepoint (authorizeServer), so an owner may opt their
// own server in or out; an unknown/unauthorized id is a 404-no-leak
// ErrServerNotFound. The value is validated BEFORE the authorization lookup only in
// the sense of shape -- nothing is persisted unless both checks pass.
func (s *Service) SetServerCertificateOverride(ctx context.Context, token auth.Token, serverID, override string) (ServerDTO, error) {
	normalized := strings.ToLower(strings.TrimSpace(override))
	switch normalized {
	case "", "include", "exclude":
	default:
		return ServerDTO{}, ErrCertInvalid
	}
	server, err := s.authorizeServer(ctx, token, serverID)
	if err != nil {
		return ServerDTO{}, err
	}
	if err := s.routes.UpdateServerCertificateOverride(ctx, server.ID, normalized); err != nil {
		return ServerDTO{}, err
	}
	server.CertificateOverride = normalized
	return s.serverDTO(ctx, server)
}

// SetServerHTTPSSwitchOverride sets a server's https-auto-switch opt-in/opt-out
// (P4). Mirrors SetServerCertificateOverride exactly: gated by the shared
// server-manage chokepoint (authorizeServer), so an owner may opt their own
// server in or out; an unknown/unauthorized id is a 404-no-leak
// ErrServerNotFound. The value is a single 3-state column ("" | "include" |
// "exclude") -- setting "include" clears a stored "exclude" and vice versa,
// since there is only ever one stored value; this is what makes a later flip
// of the global cert_https_switch_mode unable to resurrect a stale opposite
// flag (see httpsSwitchInScope, the mode-dependent resolver).
func (s *Service) SetServerHTTPSSwitchOverride(ctx context.Context, token auth.Token, serverID, override string) (ServerDTO, error) {
	normalized := strings.ToLower(strings.TrimSpace(override))
	switch normalized {
	case "", "include", "exclude":
	default:
		return ServerDTO{}, ErrCertInvalid
	}
	server, err := s.authorizeServer(ctx, token, serverID)
	if err != nil {
		return ServerDTO{}, err
	}
	if err := s.routes.UpdateServerHTTPSSwitchOverride(ctx, server.ID, normalized); err != nil {
		return ServerDTO{}, err
	}
	server.HTTPSSwitchOverride = normalized
	return s.serverDTO(ctx, server)
}
