// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/store"
	"time"
)

// ErrCaptureCipherMissing is returned by CaptureDetail when a stored capture
// was sealed (KeyVersion > 0) but no cipher is configured to open it. This is
// a misconfiguration that cannot normally occur (persistCapture only ever
// writes KeyVersion > 0 when it had a live cipher at write time) and must be
// mapped to a 500 by the handler, never to store.ErrNotFound (it must not
// look like "capture missing").
var ErrCaptureCipherMissing = errors.New("portal.capture_cipher_missing")

// CaptureDetail is the decrypted, access-checked capture returned to the portal.
// req/resp bodies are raw as sent; the frontend derives Pretty/Chat views.
// APIFlavor/HTTPStatus/CreatedAt are sourced from the store.CaptureRow (the
// usage_events JOIN), NOT from the encrypted envelope.
type CaptureDetail struct {
	ID          string              `json:"id"`
	APIFlavor   string              `json:"api_flavor"`
	HTTPStatus  int                 `json:"http_status"`
	CreatedAt   string              `json:"created_at"`
	ReqHeaders  map[string][]string `json:"req_headers"`
	ReqBody     string              `json:"req_body"`
	RespHeaders map[string][]string `json:"resp_headers"`
	RespBody    string              `json:"resp_body"`
	Truncated   bool                `json:"truncated"`
	// Translated* carry the TRANSLATED upstream exchange (the request the gateway
	// sent to the Chat-Completions upstream + the raw upstream response), present
	// only when the built-in translation ran; omitted otherwise so the frontend
	// shows the extra sections only when there is something to show.
	TranslatedReqHeaders  map[string][]string `json:"translated_req_headers,omitempty"`
	TranslatedReqBody     string              `json:"translated_req_body,omitempty"`
	TranslatedRespHeaders map[string][]string `json:"translated_resp_headers,omitempty"`
	TranslatedRespBody    string              `json:"translated_resp_body,omitempty"`
	// Secret is the capture's secret flag; CanToggleSecret is true only for the
	// owner (admins never receive a secret capture, and cannot toggle a
	// non-secret one they merely view). SP-2c/2d.
	Secret          bool `json:"secret"`
	CanToggleSecret bool `json:"can_toggle_secret"`
}

// captureEnvelope is the gzipped-then-encrypted plaintext written by the capture
// pipeline (P4). It is exactly five fields; http_status/api_flavor are NOT in the
// envelope — the detail sources them from the joined usage_events row.
type captureEnvelope struct {
	ReqHeaders            map[string][]string `json:"req_headers"`
	ReqBody               string              `json:"req_body"`
	RespHeaders           map[string][]string `json:"resp_headers"`
	RespBody              string              `json:"resp_body"`
	Truncated             bool                `json:"truncated"`
	TranslatedReqHeaders  map[string][]string `json:"translated_req_headers,omitempty"`
	TranslatedReqBody     string              `json:"translated_req_body,omitempty"`
	TranslatedRespHeaders map[string][]string `json:"translated_resp_headers,omitempty"`
	TranslatedRespBody    string              `json:"translated_resp_body,omitempty"`
}

// CaptureDetail loads, access-checks, and decrypts a capture. The per-row gate is
// owner-or-admin. A disabled feature (nil deps), a missing capture, or an
// unauthorized principal all return store.ErrNotFound (no existence leak). A
// decryption/parse failure returns the underlying error (mapped to 500 by the
// handler) so no ciphertext or partial plaintext leaks.
func (s *Service) CaptureDetail(principal auth.Token, usageEventID string) (CaptureDetail, error) {
	if s.captures == nil {
		return CaptureDetail{}, store.ErrNotFound
	}
	row, err := s.captures.Capture(context.Background(), usageEventID)
	if err != nil {
		return CaptureDetail{}, err
	}
	isOwner := principal.UserID == row.OwnerUserID
	// Non-owner access: allowed only for a non-secret row AND an admin. A secret
	// row is strictly owner-only (SP-2c) — even an admin gets 404, no leak.
	if !isOwner && (row.Secret || !isAdmin(principal)) {
		return CaptureDetail{}, store.ErrNotFound
	}
	// KeyVersion 0 is the RAM-fallback path (SP-C+ P4): plain gzip, never
	// sealed. KeyVersion > 0 was sealed by a live cipher at write time, so an
	// absent cipher here is a misconfiguration, not "no capture".
	compressed := row.Blob
	if row.KeyVersion > 0 {
		if s.cipher == nil {
			return CaptureDetail{}, ErrCaptureCipherMissing
		}
		compressed, err = s.cipher.Open(row.Blob)
		if err != nil {
			return CaptureDetail{}, err
		}
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return CaptureDetail{}, err
	}
	defer gz.Close()
	plain, err := io.ReadAll(gz)
	if err != nil {
		return CaptureDetail{}, err
	}
	var env captureEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		return CaptureDetail{}, err
	}
	return CaptureDetail{
		ID:                    usageEventID,
		APIFlavor:             row.APIFlavor,
		HTTPStatus:            row.HTTPStatus,
		CreatedAt:             row.CreatedAt.Format(time.RFC3339),
		ReqHeaders:            env.ReqHeaders,
		ReqBody:               env.ReqBody,
		RespHeaders:           env.RespHeaders,
		RespBody:              env.RespBody,
		Truncated:             env.Truncated,
		TranslatedReqHeaders:  env.TranslatedReqHeaders,
		TranslatedReqBody:     env.TranslatedReqBody,
		TranslatedRespHeaders: env.TranslatedRespHeaders,
		TranslatedRespBody:    env.TranslatedRespBody,
		Secret:                row.Secret,
		CanToggleSecret:       isOwner,
	}, nil
}

// DeleteCapture removes the persisted capture blob for usageEventID; the
// owning usage_events row is untouched (separate tables, linked only by FK).
// The owner|admin gate mirrors CaptureDetail exactly, including the
// no-existence-leak 404 for a non-owner/non-admin principal. Unlike
// CaptureDetail there is no cipher guard: a RAM-mode capture (nil cipher) must
// still be deletable, so the only fail-closed switch here is s.captures == nil.
func (s *Service) DeleteCapture(principal auth.Token, usageEventID string) error {
	if s.captures == nil {
		return store.ErrNotFound
	}
	row, err := s.captures.Capture(context.Background(), usageEventID)
	if err != nil {
		return err
	}
	isOwner := principal.UserID == row.OwnerUserID
	// Same gate as CaptureDetail: a secret row is owner-only, so an admin can
	// neither read nor delete a secret capture they do not own (SP-2c).
	if !isOwner && (row.Secret || !isAdmin(principal)) {
		return store.ErrNotFound
	}
	return s.captures.DeleteCapture(context.Background(), usageEventID)
}

// SetCaptureSecret flips the secret flag on a capture. Unlike CaptureDetail /
// DeleteCapture (owner-or-admin for non-secret rows), toggling is strictly
// owner-only (SP-2d): a non-owner — even an admin — gets ErrNotFound. A
// disabled feature (nil deps) or a missing capture also return ErrNotFound.
func (s *Service) SetCaptureSecret(principal auth.Token, usageEventID string, secret bool) error {
	if s.captures == nil {
		return store.ErrNotFound
	}
	row, err := s.captures.Capture(context.Background(), usageEventID)
	if err != nil {
		return err
	}
	if principal.UserID != row.OwnerUserID {
		return store.ErrNotFound
	}
	return s.captures.SetCaptureSecret(context.Background(), usageEventID, secret)
}
