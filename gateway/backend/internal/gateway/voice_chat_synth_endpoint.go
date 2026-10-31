// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"strings"
)

// voiceChatSynthMaxBytes caps the body of a synth request. It carries one spoken section of
// an answer -- a few paragraphs -- so 256 KiB is far above any real one and keeps a
// session cookie from pushing an unbounded text at a speech model through this route.
const voiceChatSynthMaxBytes int64 = 256 << 10

// voiceChatSynthRequest is the body of POST /api/portal/voice-chat-synth: the text of one
// spoken section, the gateway speech model it was spoken with and, optionally, the voice
// and language to speak it in (blank leaves the model's own default). There is deliberately
// no chat id: the request has no thread to name, which is what keeps it from ever
// committing anything to one.
type voiceChatSynthRequest struct {
	Text     string `json:"text"`
	TTSModel string `json:"tts_model"`
	Voice    string `json:"voice,omitempty"`
	Language string `json:"language,omitempty"`
}

// handlePortalVoiceChatSynth serves POST /api/portal/voice-chat-synth: it speaks ONE
// section's text again, on demand, and answers the audio. It is the replay's way back to a
// section whose stored audio is gone (the turn kept its text, its audio was pruned): the
// portal asks for that one section again instead of replaying a blob.
//
// It is NON-COMMITTING by construction, not by care. The request names no chat, so the
// handler has no thread to read, extend or save; it starts no run, writes no turn and no
// chat_blobs row, and the audio goes to the caller and nowhere else. A thread whose
// audio was pruned stays exactly as it was; the re-spoken section is the browser's to play
// and to drop.
//
// Auth is requireWebScope(gateway:use), as the voice-chat ticket's: a session cookie on
// this POST also needs the X-OP-CSRF header (403 auth.csrf_required), a bearer token does
// not, and a service token is refused 401 whatever it carries. The method is POST for the
// reason the header is required: the call spends the user's speech budget.
//
// In order:
//
//  1. The body is read under voiceChatSynthMaxBytes (413 past it) and must be one JSON
//     object of this shape (400 request.invalid_json). Model, voice and language are
//     trimmed; a voice or language that is blank is not sent at all.
//  2. A blank model is 400 speech.model_required, and blank text 400 speech.input_required
//     -- the speech core's own rules (validateSpeechRequest) applied to the body this
//     handler builds, so they cannot drift (a blank model is reported first). Both precede
//     the gate, so the precise code wins over the gate's generic refusal.
//  3. The model goes through the gate the live Voice Chat applies to its speech leg
//     (resolveVoiceTTS: speech-capable, a streaming model, an offerable one, reachable by
//     the caller): a model a session could not have spoken with is no model the replay can
//     re-speak with. A refusal is 400 portal.voice_chat_tts_unavailable -- the ticket's
//     code for the same leg -- and never carries the cause, which is for the log.
//  4. The synthesis is the speech core's, unchanged: the request is built as the
//     /v1/audio/speech body {model, input, voice?, language?}, admitted by the one
//     preflight (inferencePreflight) and relayed by relaySpeech, which answers the
//     upstream's audio with the upstream's Content-Type -- or its error, at its status --
//     and records the usage row. Billing is therefore inherited whole: one audio_second row
//     for the user, and no second billing path here. It is a one-shot request (no stream
//     and no response format), so the answer is the upstream's WAV.
//
// The server's 30 s read and write deadlines are lifted (liftInferenceDeadlines) before the
// body is read, as for the /v1 speech endpoint this is a portal door onto: a synthesis can
// outlive them, and left armed they would cut the audio off on its way to the browser.
func (s *Server) handlePortalVoiceChatSynth(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	req, raw, ok := readVoiceChatSynthRequest(w, r)
	if !ok {
		return
	}
	if _, err := s.resolveVoiceTTS(r, &token, req.TTSModel); err != nil {
		slog.Debug("voice chat synth refused: the speech model cannot be used", "model", req.TTSModel, "err", err)
		writeVoiceChatRefusal(w, voiceChatTTSUnavailable)
		return
	}
	pf, handled := s.inferencePreflight(w, r, token, raw, speechInferenceShape(req.TTSModel, req.Voice))
	if handled {
		return
	}
	s.relaySpeech(w, r, token, pf.Req, raw)
}

// readVoiceChatSynthRequest reads and validates the body of a synth request, and returns it
// trimmed together with the /v1/audio/speech body the synthesis is relayed as. On any
// refusal it writes the response and returns ok == false: a body past the cap is 413, one
// that is not a JSON object of this shape 400 request.invalid_json, a blank model 400
// speech.model_required and blank text 400 speech.input_required (the last two are the
// speech core's own validation of the body built here).
func readVoiceChatSynthRequest(w http.ResponseWriter, r *http.Request) (voiceChatSynthRequest, []byte, bool) {
	body, ok := readRawJSONLimit(w, r, voiceChatSynthMaxBytes)
	if !ok {
		return voiceChatSynthRequest{}, nil, false
	}
	var req voiceChatSynthRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return voiceChatSynthRequest{}, nil, false
	}
	req.TTSModel = strings.TrimSpace(req.TTSModel)
	req.Voice, req.Language = strings.TrimSpace(req.Voice), strings.TrimSpace(req.Language)
	// A map of strings always encodes.
	raw, _ := json.Marshal(speechBodyForSynth(req))
	if verr := validateSpeechRequest(raw, req.TTSModel); verr != nil {
		writeRequestError(w, verr)
		return voiceChatSynthRequest{}, nil, false
	}
	return req, raw, true
}

// speechBodyForSynth is the /v1/audio/speech body of a synth request: the model and the
// text always, the voice and the language only when the request names one (the discipline
// speechBodyFields keeps for a run's settings), so the upstream stays on its own default
// rather than being handed an inert blank.
func speechBodyForSynth(req voiceChatSynthRequest) map[string]string {
	body := map[string]string{"model": req.TTSModel, "input": req.Text}
	if req.Voice != "" {
		body["voice"] = req.Voice
	}
	if req.Language != "" {
		body["language"] = req.Language
	}
	return body
}
