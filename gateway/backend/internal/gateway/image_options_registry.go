// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/provider"
	"slices"
	"sync"
)

// AppImageOptionsRegistry is a thread-safe, in-memory map of the image-generation
// options each EXTERNAL stable-diffusion.cpp application's capability document
// states (samplers, schedulers, output formats, size/batch limits, the server's
// own defaults), keyed by application id. The app-health loop is its only
// writer (cmd/gateway probeSdcppCapabilities); the portal reads it to tell the
// chat which knobs a model offers.
//
// It is held on the Server like AppHealthRegistry: purely volatile (never
// persisted), repopulated by the next probe after a restart, and every method
// is nil-safe so a bare *Server built in a test keeps working. An application
// the loop has not (yet) read a document from is simply absent: callers treat
// absence as "no options known", never as an error.
//
// The stored options hold slices, so Set, Get and Snapshot each copy them:
// nothing a caller holds aliases the registry's storage.
type AppImageOptionsRegistry struct {
	mu    sync.RWMutex
	items map[string]provider.SdcppImageOptions
}

func NewAppImageOptionsRegistry() *AppImageOptionsRegistry {
	return &AppImageOptionsRegistry{items: make(map[string]provider.SdcppImageOptions)}
}

// Set records the latest options read for an application, replacing any earlier
// observation wholesale.
func (r *AppImageOptionsRegistry) Set(appID string, opts provider.SdcppImageOptions) {
	if r == nil {
		return
	}
	opts = cloneSdcppImageOptions(opts)
	r.mu.Lock()
	r.items[appID] = opts
	r.mu.Unlock()
}

// Get returns the options last recorded for an application and whether any were
// recorded at all. A nil registry or an unknown id returns (zero, false).
func (r *AppImageOptionsRegistry) Get(appID string) (provider.SdcppImageOptions, bool) {
	if r == nil {
		return provider.SdcppImageOptions{}, false
	}
	r.mu.RLock()
	opts, ok := r.items[appID]
	r.mu.RUnlock()
	if !ok {
		return provider.SdcppImageOptions{}, false
	}
	return cloneSdcppImageOptions(opts), true
}

// Snapshot returns a copy of every recorded application's options. A nil
// registry returns nil. The returned map (and the slices in it) are safe for
// the caller to mutate.
func (r *AppImageOptionsRegistry) Snapshot() map[string]provider.SdcppImageOptions {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]provider.SdcppImageOptions, len(r.items))
	for id, opts := range r.items {
		out[id] = cloneSdcppImageOptions(opts)
	}
	return out
}

// Retain drops the entries of applications not in live, so options for deleted
// applications do not accumulate for the life of the process. Called each
// app-health cycle with the currently-existing application ids. A nil live set
// evicts everything; a nil registry is a no-op.
func (r *AppImageOptionsRegistry) Retain(live map[string]struct{}) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.items {
		if _, ok := live[id]; !ok {
			delete(r.items, id)
		}
	}
}

// cloneSdcppImageOptions copies the three string lists; everything else in
// provider.SdcppImageOptions is a value type. slices.Clone keeps a nil list nil,
// so "the server named none" survives the round trip with one spelling.
func cloneSdcppImageOptions(o provider.SdcppImageOptions) provider.SdcppImageOptions {
	o.Samplers = slices.Clone(o.Samplers)
	o.Schedulers = slices.Clone(o.Schedulers)
	o.OutputFormats = slices.Clone(o.OutputFormats)
	return o
}
