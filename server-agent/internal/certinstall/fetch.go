// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"op-ai-server-agent/internal/gwapi"
	"time"
)

// certificatePath is the gateway route this installer polls, on both the
// NetBird-bound agent mux and the (netbird_only-gated) public mux.
const certificatePath = "/api/agent/v1/certificate"

// ModeOff is the ONE cert_mode value that disables this installer entirely --
// no HTTP call, no disk read, no disk write. It intentionally duplicates the
// literal string rather than importing the agent's config package (this
// package takes everything as constructor parameters -- see the package
// doc); both ultimately answer to the same server-side sentinel the
// gateway's AgentCertReport.Mode validation accepts ({"off","files","proxy"}).
const ModeOff = "off"

// Installer fetches, atomically installs, and (on real change) reload-hooks
// the TLS certificate of exactly ONE server -- the one identified by its
// bearer token. It imports nothing from cmd/gateway or the gateway module;
// its only outside dependency is the HTTP contract of
// GET /api/agent/v1/certificate.
type Installer struct {
	base  string // gatewayURL, trimmed of any trailing '/'
	token string
	http  *http.Client

	dir           string
	reloadCommand string
	mode          string
}

// New builds an Installer. gatewayURL is joined with certificatePath by
// plain string concatenation -- deliberately NOT url.JoinPath, mirroring the
// house contract in client.go/ws.go: concatenation preserves an existing
// base PATH (a gateway reachable at "https://gw/base" must be polled at
// "https://gw/base/api/agent/v1/certificate"), where a path-joining helper
// would silently normalize that away, or a "//api/..." would only work by
// accident of a ServeMux redirect.
//
// mode is the configured cert_mode. Only ModeOff ("off") disables the
// installer; anything else -- including "proxy" -- installs files exactly like
// "files" does. In "proxy" mode the agent additionally runs a TLS reverse proxy
// (see internal/proxy) that CONSUMES the files this installer writes; that proxy
// lives entirely outside this package, which still only ever installs files.
func New(gatewayURL, token string, httpClient *http.Client, dir, reloadCommand, mode string) *Installer {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Installer{
		base:          gwapi.TrimBase(gatewayURL),
		token:         token,
		http:          httpClient,
		dir:           dir,
		reloadCommand: reloadCommand,
		mode:          mode,
	}
}

// certResponse mirrors handleAgentCertificate's JSON body field-for-field.
type certResponse struct {
	Domain       string     `json:"domain"`
	Fingerprint  string     `json:"fingerprint"`
	FullchainPEM string     `json:"fullchain_pem"`
	KeyPEM       string     `json:"key_pem"`
	CABundlePEM  string     `json:"ca_bundle_pem"`
	ETag         string     `json:"etag"`
	NotBefore    *time.Time `json:"not_before"`
	NotAfter     *time.Time `json:"not_after"`
}

// Sync fetches the certificate, installs it on a real change, runs the
// reload hook, and ALWAYS returns the report as currently derived from disk
// -- on every path: 200-with-change, 200-with-no-actual-change, 304, 404, an
// auth/transport/decode error. The bool reports whether an install (and
// therefore a hook run) actually happened.
//
// With mode == ModeOff this makes no HTTP call and touches no disk at all.
func (in *Installer) Sync(ctx context.Context) (Report, bool, error) {
	if in.mode == ModeOff {
		return Report{Mode: ModeOff}, false, nil
	}

	state := readDiskState(in.dir)
	ifNoneMatch := ""
	if state.memoValid() {
		ifNoneMatch = state.etag
	}

	resp, err := in.fetch(ctx, ifNoneMatch)
	if err != nil {
		slog.Debug("certificate fetch failed", "err", err)
		return in.Report(), false, nil
	}
	defer gwapi.DrainLimited(resp) // allow connection reuse

	switch resp.StatusCode {
	case http.StatusNotModified:
		return in.Report(), false, nil
	case http.StatusNotFound:
		// "Nothing to install for me right now" -- existing files are left
		// alone; a 404 after a transient store error must never look like a
		// revocation.
		return in.Report(), false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		slog.Warn("certificate fetch unauthorized", "status", resp.StatusCode)
		return in.Report(), false, nil
	case http.StatusOK:
		// handled below
	default:
		slog.Debug("certificate fetch returned unexpected status", "status", resp.StatusCode)
		return in.Report(), false, nil
	}

	var body certResponse
	if err := json.NewDecoder(gwapi.LimitReader(resp)).Decode(&body); err != nil {
		slog.Debug("certificate response decode failed", "err", err)
		return in.Report(), false, nil
	}

	// Refuse a response whose OWN chain and key do not pair. Installing it would
	// hand the TLS consumer a certificate it cannot serve -- and, because the
	// unpaired disk state below counts as a change, every later poll would reinstall
	// it and re-run the operator's reload command forever: the exact unbounded loop
	// the empty-bundle clause avoids. A mismatch here is a gateway-side defect, so
	// it is a Warn, and the previously installed files are left alone.
	if !responsePairs(body) {
		slog.Warn("certificate response chain and key do not match; not installing",
			"fingerprint", body.Fingerprint)
		return in.Report(), false, nil
	}

	state = readDiskState(in.dir) // re-read: a concurrent writer could have changed it since the request went out
	// !state.keyPaired FIRST: a half-completed rename phase (the chain from install
	// N+1 next to the key from install N) makes readDiskState report the NEW leaf's
	// fingerprint, so comparing fingerprints alone finds no difference, decides
	// "unchanged", and leaves the mismatched key in place FOREVER -- the very wedge
	// the pairing check exists to break. The unpaired state must therefore count as
	// a change in its own right, not merely suppress the If-None-Match header.
	//
	// The bundle is compared only when the response actually carries one, because
	// install() deliberately does not write (or delete) ca.pem for an empty bundle:
	// comparing unconditionally would report "changed" on every single poll and
	// re-run the operator's reload command forever.
	changed := !state.keyPaired ||
		body.Fingerprint != state.leafFingerprint ||
		(body.CABundlePEM != "" && !bytes.Equal([]byte(body.CABundlePEM), state.caPEM))
	if !changed {
		saveETagSidecar(in.dir, gwapi.ResponseETag(resp, body.ETag))
		return in.Report(), false, nil
	}

	if err := in.install(body); err != nil {
		slog.Debug("certificate install failed", "err", err)
		return in.Report(), false, err
	}
	saveETagSidecar(in.dir, gwapi.ResponseETag(resp, body.ETag))
	runReloadHook(ctx, in.reloadCommand)
	return in.Report(), true, nil
}

// fetch issues the conditional GET. ifNoneMatch == "" omits the header (an
// unconditional fetch). It shares its transport contract (bearer auth,
// plain-concatenation base+path join, If-None-Match) with
// proxy.RoutesClient.Fetch via gwapi.Endpoint.
func (in *Installer) fetch(ctx context.Context, ifNoneMatch string) (*http.Response, error) {
	ep := &gwapi.Endpoint{Base: in.base, Token: in.token, Client: in.http}
	return ep.GetConditional(ctx, certificatePath, ifNoneMatch)
}

// responsePairs reports whether the served leaf and private key belong together.
// It mirrors readDiskState's pairing check, applied to the wire instead of to disk.
func responsePairs(body certResponse) bool {
	leaf := parseLeafCert([]byte(body.FullchainPEM))
	if leaf == nil {
		return false
	}
	signer := parsePrivateKey([]byte(body.KeyPEM))
	if signer == nil {
		return false
	}
	return publicKeysMatch(leaf, signer)
}
