// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"strings"
)

// GPU vendors on the portal's wire (#434, mixed NVIDIA + AMD hosts).
//
// Every GPU row the portal stores -- a runtime spec's, a GPU budget, an RPC
// member spec's and a cluster member's -- carries a vendor next to its index,
// because one index exists once per vendor on a mixed host. The vendor a
// save stores is normalizeGPUVendor's; the vendor the runtime document
// carries is pushedGPUVendor's: an agent that declares
// AgentFeatureGPUVendorIdentity gets it, an older agent gets the document it
// got before vendors existed, with only the rows it can read.

// agentHasGPUVendors reports whether serverID's agent declares
// AgentFeatureGPUVendorIdentity, and whether this gateway knows its
// declaration (agentRunsWSL's reading). A nil reader knows every agent, as
// one without the feature.
func (s *Service) agentHasGPUVendors(serverID string) (has, known bool) {
	if s.agentFeatures == nil {
		return false, true
	}
	if !s.agentFeatures.Known(serverID) {
		return false, false
	}
	return s.agentFeatures.Has(serverID, AgentFeatureGPUVendorIdentity), true
}

// serverLegacyGPUVendor is the vendor the empty vendor stands for on
// serverID: the persisted AIServer.GPULegacyVendor the backfill wrote, ""
// when none was ever stored. It is deliberately not derived from the latest
// hardware report: an older agent's vendor-less report replaces the stored
// report but must not change which rows that agent is sent.
func (s *Service) serverLegacyGPUVendor(ctx context.Context, serverID string) (string, error) {
	server, err := s.routes.AIServerByID(ctx, serverID)
	if err != nil {
		return "", err
	}
	return server.GPULegacyVendor, nil
}

// normalizeGPUVendor is the vendor a save stores for one GPU row: vendor
// lower-cased; not a valid vendor is ErrRuntimeSpecGPUVendorInvalid; for an
// agent known to lack gpu_vendor_identity, a non-empty vendor other than the
// server's stored legacy vendor (any non-empty vendor when none is stored) is
// ErrRuntimeSpecGPUVendorUnsupported -- the agent never receives the row with
// its vendor, so it would read the bare index as the legacy vendor's card.
// The legacy vendor itself is accepted and stored as sent: it names exactly
// that card, the unflagged document drops it to "" (pushedGPUVendor), and the
// portal echoes it back on every re-save and override of rows a backfill
// rewrote. An empty vendor for a flagged agent is the server's stored legacy
// vendor, so a row saved without a vendor names the same card as one the
// backfill rewrote. An agent the gateway has not heard from yet is not
// refused: the document is held for it until its declaration is known
// (AgentRuntimeConfig).
func normalizeGPUVendor(vendor string, flagged, known bool, legacy string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(vendor))
	if !routing.ValidGPUVendor(v) {
		return "", ErrRuntimeSpecGPUVendorInvalid
	}
	if v != "" && v != legacy && known && !flagged {
		return "", ErrRuntimeSpecGPUVendorUnsupported
	}
	if v == "" && flagged && legacy != "" {
		return legacy, nil
	}
	return v, nil
}

// pushedGPUVendor is how one stored GPU row goes into the runtime document of
// an agent: a flagged agent gets the row with its vendor; an unflagged one
// gets it without, and only when the row is one it reads correctly -- an
// empty vendor, or the server's stored legacy vendor (what the empty vendor
// stands for, so the agent's bare index names that card). With no legacy
// vendor stored, only empty-vendor rows are kept.
func pushedGPUVendor(vendor string, flagged bool, legacy string) (out string, keep bool) {
	if flagged {
		return vendor, true
	}
	return "", vendor == "" || (legacy != "" && vendor == legacy)
}

// gpuVendorRules is what a save or a document needs to know about one
// server's GPU vendors: whether its agent declares gpu_vendor_identity,
// whether that declaration is known, and the stored legacy vendor. stored,
// for a save, holds the GPU keys the saved object (a spec, the budget list,
// a member spec, a cluster member) already has stored (normalizeAt).
type gpuVendorRules struct {
	flagged bool
	known   bool
	legacy  string
	stored  map[routing.GPUKey]struct{}
}

// gpuVendorRulesFor reads serverID's gpuVendorRules.
func (s *Service) gpuVendorRulesFor(ctx context.Context, serverID string) (gpuVendorRules, error) {
	legacy, err := s.serverLegacyGPUVendor(ctx, serverID)
	if err != nil {
		return gpuVendorRules{}, err
	}
	return s.gpuVendorRulesWith(serverID, legacy), nil
}

// gpuVendorRulesWith is gpuVendorRulesFor for a caller that already holds
// the server row.
func (s *Service) gpuVendorRulesWith(serverID, legacy string) gpuVendorRules {
	flagged, known := s.agentHasGPUVendors(serverID)
	return gpuVendorRules{flagged: flagged, known: known, legacy: legacy}
}

// normalizeAt is normalizeGPUVendor under r for the GPU at index, except
// that a row whose (vendor, index) the saved object already has stored is
// not refused as unsupported: an agent downgraded after a flagged save gets
// such a row's spec re-sent whole by every override and re-save. A new
// foreign-vendor row is still refused.
func (r gpuVendorRules) normalizeAt(vendor string, index int) (string, error) {
	v, err := normalizeGPUVendor(vendor, r.flagged, r.known, r.legacy)
	if errors.Is(err, ErrRuntimeSpecGPUVendorUnsupported) {
		lowered := strings.ToLower(strings.TrimSpace(vendor))
		if _, ok := r.stored[routing.GPUKey{Vendor: lowered, Index: index}]; ok {
			return lowered, nil
		}
	}
	return v, err
}

// withStored is r for a save of an object whose stored GPU keys are keys.
func (r gpuVendorRules) withStored(keys []routing.GPUKey) gpuVendorRules {
	r.stored = make(map[routing.GPUKey]struct{}, len(keys))
	for _, k := range keys {
		r.stored[k] = struct{}{}
	}
	return r
}

// resolve is the card key names on this server: an empty vendor is the
// legacy vendor's. Two keys that resolve alike name one card.
func (r gpuVendorRules) resolve(key routing.GPUKey) routing.GPUKey {
	return resolveLegacyGPUKey(key, r.legacy)
}

// resolveLegacyGPUKey is key with an empty vendor read as legacy.
func resolveLegacyGPUKey(key routing.GPUKey, legacy string) routing.GPUKey {
	if key.Vendor == "" {
		key.Vendor = legacy
	}
	return key
}

// normalizeSpecGPUVendors returns gpus with every vendor normalized under r
// (a copy; gpus is not changed), and then refuses one card named twice
// (validateRuntimeSpecGPUs, by resolved key, so an empty vendor and the
// legacy vendor at one index collide).
func (r gpuVendorRules) normalizeSpecGPUVendors(gpus []RuntimeSpecGPUDTO) ([]RuntimeSpecGPUDTO, error) {
	if gpus == nil {
		return nil, nil
	}
	out := make([]RuntimeSpecGPUDTO, len(gpus))
	seen := make(map[routing.GPUKey]struct{}, len(gpus))
	for i, g := range gpus {
		vendor, err := r.normalizeAt(g.Vendor, g.Index)
		if err != nil {
			return nil, err
		}
		g.Vendor = vendor
		key := r.resolve(routing.GPUKey{Vendor: g.Vendor, Index: g.Index})
		if _, dup := seen[key]; dup {
			return nil, ErrRuntimeSpecGPUInvalid
		}
		seen[key] = struct{}{}
		out[i] = g
	}
	return out, nil
}

// pushedSpecGPUs is gpus as an agent under r receives them
// (pushedGPUVendor): an unflagged agent gets each index at most once, the
// first kept row of it. dropped reports a row the agent does not get.
func (r gpuVendorRules) pushedSpecGPUs(gpus []AgentRuntimeSpecGPUDTO) (out []AgentRuntimeSpecGPUDTO, dropped bool) {
	out = make([]AgentRuntimeSpecGPUDTO, 0, len(gpus))
	seen := make(map[routing.GPUKey]struct{}, len(gpus))
	for _, g := range gpus {
		vendor, keep := pushedGPUVendor(g.Vendor, r.flagged, r.legacy)
		key := routing.GPUKey{Vendor: vendor, Index: g.Index}
		if _, dup := seen[key]; !keep || dup {
			dropped = true
			continue
		}
		seen[key] = struct{}{}
		g.Vendor = vendor
		out = append(out, g)
	}
	return out, dropped
}

// pushedBudgets is budgets as an agent under r receives them, by
// pushedSpecGPUs' rule.
func (r gpuVendorRules) pushedBudgets(budgets []routing.ServerGPUBudget) (out []routing.ServerGPUBudget, dropped bool) {
	out = make([]routing.ServerGPUBudget, 0, len(budgets))
	seen := make(map[routing.GPUKey]struct{}, len(budgets))
	for _, b := range budgets {
		vendor, keep := pushedGPUVendor(b.GPUVendor, r.flagged, r.legacy)
		key := routing.GPUKey{Vendor: vendor, Index: b.GPUIndex}
		if _, dup := seen[key]; !keep || dup {
			dropped = true
			continue
		}
		seen[key] = struct{}{}
		b.GPUVendor = vendor
		out = append(out, b)
	}
	return out, dropped
}

// lookupGPUKey finds key in m, or the row that names the same card on a
// server whose legacy vendor is legacy: an empty vendor and the legacy
// vendor at one index are one card.
func lookupGPUKey[T any](m map[routing.GPUKey]T, key routing.GPUKey, legacy string) (T, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	var zero T
	if legacy == "" {
		return zero, false
	}
	switch key.Vendor {
	case "":
		key.Vendor = legacy
	case legacy:
		key.Vendor = ""
	default:
		return zero, false
	}
	v, ok := m[key]
	return v, ok
}
