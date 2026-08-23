// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package trust

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"op-ai-server-agent/internal/gwapi"
	"strings"
	"sync"
)

const maxRefreshBundleBytes = gwapi.MaxResponseBytes

var (
	ErrRefreshUnauthorized = errors.New("gateway CA refresh unauthorized")
	ErrRefreshNotFound     = errors.New("gateway CA bundle unavailable")
)

type Refresher struct {
	base   string
	token  string
	client *http.Client
	store  *Store

	refreshGate chan struct{}
	mu          sync.Mutex
	etag        string
}

func NewRefresher(base, token string, client *http.Client, store *Store) *Refresher {
	if client == nil {
		client = &http.Client{}
	}
	return &Refresher{base: gwapi.TrimBase(base), token: token, client: client, store: store, refreshGate: make(chan struct{}, 1)}
}

func (r *Refresher) Refresh(ctx context.Context) error {
	if r == nil || r.store == nil {
		return nil
	}
	// Keep fetch -> install -> ETag one transaction even for direct callers.
	// Agent normally serializes wakes too, but Refresher owns its own invariant.
	select {
	case r.refreshGate <- struct{}{}:
		defer func() { <-r.refreshGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	u, err := url.Parse(r.base)
	if err != nil {
		return fmt.Errorf("build gateway CA URL: %w", err)
	}
	// The base+path join here goes through net/url (not gwapi.Endpoint's plain
	// concatenation) so it can also strip any query/fragment on the configured
	// base; the base PATH itself is still preserved, via TrimRight, exactly like
	// gwapi.Endpoint does.
	u.Path = strings.TrimRight(u.Path, "/") + "/api/agent/v1/ca"
	u.RawQuery = ""
	u.Fragment = ""

	r.mu.Lock()
	etag := r.etag
	r.mu.Unlock()

	resp, err := gwapi.ConditionalGet(ctx, r.client, u.String(), r.token, etag)
	if err != nil {
		return fmt.Errorf("fetch gateway CA bundle: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w (status %d)", ErrRefreshUnauthorized, resp.StatusCode)
	case http.StatusNotFound:
		return ErrRefreshNotFound
	case http.StatusOK:
		// continue below
	default:
		return fmt.Errorf("gateway CA refresh returned status %d", resp.StatusCode)
	}
	bundle, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBundleBytes+1))
	if err != nil {
		return fmt.Errorf("read gateway CA bundle: %w", err)
	}
	if len(bundle) > maxRefreshBundleBytes {
		return fmt.Errorf("gateway CA bundle exceeds %d bytes", maxRefreshBundleBytes)
	}
	if err := r.store.InstallManagedBundle(bundle); err != nil {
		return fmt.Errorf("install gateway CA bundle: %w", err)
	}
	r.mu.Lock()
	r.etag = gwapi.ResponseETag(resp, "")
	r.mu.Unlock()
	return nil
}

func (r *Refresher) DurableFingerprints() []string {
	if r == nil || r.store == nil {
		return nil
	}
	return r.store.DurableFingerprints()
}
