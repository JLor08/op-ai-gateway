// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/netbird"
	"time"
)

// RotateNetbirdTokenResult reports the outcome of a rotation to the caller/UI.
type RotateNetbirdTokenResult struct {
	ExpirationDate string `json:"expiration_date"`
	DaysRemaining  int    `json:"days_remaining"`
	OldDeleted     bool   `json:"old_deleted"`
	OldUnknown     bool   `json:"old_unknown"`
}

// NetbirdTokenStatusDTO is the read-only token validity for display. Known is
// false when the module is off/unconfigured or the token id can't be resolved.
type NetbirdTokenStatusDTO struct {
	Known          bool   `json:"known"`
	Name           string `json:"name"`
	ExpirationDate string `json:"expiration_date"`
	DaysRemaining  int    `json:"days_remaining"`
	LastUsed       string `json:"last_used"`
}

const (
	netbirdManagedTokenName    = "op-gateway"
	netbirdTokenValidityDays   = 365
	netbirdTokenRotateCooldown = time.Hour
)

// daysRemaining returns ceil((exp - now)/24h), floored at 0. Empty/unparseable exp -> 0.
func daysRemaining(expiration string, now time.Time) int {
	exp, err := time.Parse(time.RFC3339, expiration)
	if err != nil {
		return 0
	}
	d := exp.Sub(now)
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Hours() / 24))
}

// RotateNetbirdToken creates a fresh PAT, verifies it, switches the stored token
// to it, and best-effort deletes the old one. Any failure before persistence
// leaves the stored token intact (rollback) and deletes the orphan new token.
// Serialized by netbirdTokenMu (manual button + auto-loop share it).
//
// The route is system-scoped (requireWebScope("system") at the handler); as of
// PT-2 Part 2b this also checks isSystem(principal) itself (ErrPrincipalForbidden
// otherwise) as defense-in-depth against a future internal caller that bypasses
// the HTTP gate. The auto-loop's own MaybeRotateNetbirdToken calls
// rotateNetbirdTokenLocked directly and is unaffected.
func (s *Service) RotateNetbirdToken(ctx context.Context, principal auth.Token) (RotateNetbirdTokenResult, error) {
	if !isSystem(principal) {
		return RotateNetbirdTokenResult{}, ErrPrincipalForbidden
	}
	s.netbird.tokenMu.Lock()
	defer s.netbird.tokenMu.Unlock()
	return s.rotateNetbirdTokenLocked(ctx)
}

func (s *Service) rotateNetbirdTokenLocked(ctx context.Context) (RotateNetbirdTokenResult, error) {
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil {
		return RotateNetbirdTokenResult{}, err
	}
	if !ok {
		return RotateNetbirdTokenResult{}, ErrNetbirdModuleDisabled
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	userID, err := netbird.ResolveCurrentUserID(ctx, ncfg, netbirdCallTimeout)
	if err != nil {
		return RotateNetbirdTokenResult{}, err
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return RotateNetbirdTokenResult{}, err
	}
	oldID, oldUnknown := s.resolveOldTokenID(ctx, ncfg, userID, values[netbirdTokenIDKey])

	plain, meta, err := netbird.CreateToken(ctx, ncfg, netbirdCallTimeout, userID, netbirdManagedTokenName, netbirdTokenValidityDays)
	if err != nil {
		return RotateNetbirdTokenResult{}, err
	}
	// Verify with the NEW token before switching. Any failure -> delete the orphan, keep the old.
	verifyCfg := netbird.Config{URL: cfg.URL, Token: plain}
	if verr := netbird.Ping(ctx, verifyCfg, netbirdCallTimeout); verr != nil {
		_ = netbird.DeleteToken(ctx, ncfg, netbirdCallTimeout, userID, meta.ID)
		return RotateNetbirdTokenResult{}, verr
	}
	// Persist ONLY after a successful verify. Seal first so a disk-without-key
	// rejection surfaces before any write; on any persist error, delete the orphan.
	sealed, err := s.sealSecret(plain)
	if err != nil {
		_ = netbird.DeleteToken(ctx, ncfg, netbirdCallTimeout, userID, meta.ID)
		if errors.Is(err, ErrSMTPKeyRequired) {
			return RotateNetbirdTokenResult{}, ErrNetbirdKeyRequired
		}
		return RotateNetbirdTokenResult{}, err
	}
	// Write the metadata (id/expires_at) BEFORE the credential so `netbird_token`
	// is switched by the LAST write. This keeps the rollback safe across the
	// non-atomic multi-write: if ANY write fails, `netbird_token` still points at
	// the OLD (working) token, so the DeleteToken(meta.ID) below removes only the
	// new orphan and never bricks the module. The only residual on a failure after
	// id/expires but before the token write is stale metadata over a still-valid old
	// credential, which self-heals on the next rotation/resolve.
	for _, w := range []settingWrite{
		{netbirdTokenIDKey, meta.ID},
		{netbirdTokenExpiresAtKey, meta.ExpirationDate},
		{netbirdTokenKey, sealed},
	} {
		if err := s.settings.SetSystemSetting(ctx, w.key, w.value, s.clock()); err != nil {
			_ = netbird.DeleteToken(ctx, ncfg, netbirdCallTimeout, userID, meta.ID)
			return RotateNetbirdTokenResult{}, err
		}
	}
	// Best-effort delete of the old token (never fatal — it expires anyway).
	deleted := false
	if oldID != "" {
		if derr := netbird.DeleteToken(ctx, ncfg, netbirdCallTimeout, userID, oldID); derr != nil {
			slog.Debug("netbird: delete old token failed", "error", derr)
		} else {
			deleted = true
		}
	}
	return RotateNetbirdTokenResult{
		ExpirationDate: meta.ExpirationDate,
		DaysRemaining:  daysRemaining(meta.ExpirationDate, s.clock()),
		OldDeleted:     deleted,
		OldUnknown:     oldUnknown,
	}, nil
}

// resolveOldTokenID picks the id of the currently-configured token to delete:
// the stored id if present, else the sole token when the user has exactly one
// (unambiguous), else unknown. Never guesses among multiple tokens.
func (s *Service) resolveOldTokenID(ctx context.Context, ncfg netbird.Config, userID, storedID string) (string, bool) {
	if storedID != "" {
		return storedID, false
	}
	toks, err := netbird.ListTokens(ctx, ncfg, netbirdCallTimeout, userID)
	if err == nil && len(toks) == 1 {
		return toks[0].ID, false
	}
	return "", true
}

// NetbirdTokenStatus returns the current token's live validity for display. It is
// a pure read (persists nothing) and never returns a token value.
func (s *Service) NetbirdTokenStatus(ctx context.Context) (NetbirdTokenStatusDTO, error) {
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil || !ok {
		return NetbirdTokenStatusDTO{Known: false}, nil // module off/unconfigured: no NetBird call, no error
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	userID, err := netbird.ResolveCurrentUserID(ctx, ncfg, netbirdCallTimeout)
	if err != nil {
		return NetbirdTokenStatusDTO{Known: false}, nil
	}
	values, _ := s.settings.SystemSettings(ctx)
	tokenID := values[netbirdTokenIDKey]
	if tokenID == "" {
		if toks, lerr := netbird.ListTokens(ctx, ncfg, netbirdCallTimeout, userID); lerr == nil && len(toks) == 1 {
			tokenID = toks[0].ID
		}
	}
	if tokenID == "" {
		return NetbirdTokenStatusDTO{Known: false}, nil
	}
	tok, err := netbird.GetToken(ctx, ncfg, netbirdCallTimeout, userID, tokenID)
	if err != nil {
		return NetbirdTokenStatusDTO{Known: false}, nil
	}
	return NetbirdTokenStatusDTO{
		Known:          true,
		Name:           tok.Name,
		ExpirationDate: tok.ExpirationDate,
		DaysRemaining:  daysRemaining(tok.ExpirationDate, s.clock()),
		LastUsed:       tok.LastUsed,
	}, nil
}

// MaybeRotateNetbirdToken is the auto-rotation tick called by the reconcile loop.
// It rotates only a gateway-managed token (known id + expiry) within the operator
// threshold, cooldown-guarded so a persistently failing rotation can't hammer
// NetBird. Best-effort: all errors are logged, never surfaced.
func (s *Service) MaybeRotateNetbirdToken(ctx context.Context) {
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return
	}
	threshold := s.NetbirdTokenRotateBeforeDays(values)
	if threshold <= 0 { // auto-rotation disabled
		return
	}
	id := values[netbirdTokenIDKey]
	expRaw := values[netbirdTokenExpiresAtKey]
	if id == "" || expRaw == "" {
		return // not gateway-managed yet -> manual only
	}
	if daysRemaining(expRaw, s.clock()) > threshold {
		return // not near expiry
	}
	s.netbird.tokenMu.Lock()
	defer s.netbird.tokenMu.Unlock()
	now := s.clock()
	if now.Before(s.netbird.nextTokenRotateAttempt) {
		return // cooldown
	}
	s.netbird.nextTokenRotateAttempt = now.Add(netbirdTokenRotateCooldown)
	if _, err := s.rotateNetbirdTokenLocked(ctx); err != nil {
		slog.Warn("netbird: auto token rotation failed (old token still active)", "error", err)
	}
}

// resolveStoredTokenMeta best-effort fills netbird_token_id/_expires_at from the
// current single token, or clears them when ambiguous. Errors are ignored.
func (s *Service) resolveStoredTokenMeta(ctx context.Context) {
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil || !ok {
		return
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	userID, err := netbird.ResolveCurrentUserID(ctx, ncfg, netbirdCallTimeout)
	if err != nil {
		return
	}
	toks, err := netbird.ListTokens(ctx, ncfg, netbirdCallTimeout, userID)
	if err != nil {
		return
	}
	id, exp := "", ""
	if len(toks) == 1 {
		id, exp = toks[0].ID, toks[0].ExpirationDate
	}
	_ = s.settings.SetSystemSetting(ctx, netbirdTokenIDKey, id, s.clock())
	_ = s.settings.SetSystemSetting(ctx, netbirdTokenExpiresAtKey, exp, s.clock())
}
