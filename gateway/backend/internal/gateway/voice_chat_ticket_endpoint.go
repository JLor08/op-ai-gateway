// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// DefaultVoiceCoLoadTimeout is the default deadline of the Voice Chat co-load gate when
// ServerDeps.VoiceCoLoadTimeout is unset (<= 0): long enough to cold-start the three
// models one after the other, short enough that a model that never comes up refuses the
// mode instead of holding the browser's request open without end. It is the literal of the
// OP_AI_GATEWAY_VOICE_COLOAD_TIMEOUT default (config cannot import this package).
const DefaultVoiceCoLoadTimeout = 120 * time.Second

// The PCM shape the browser streams up on the voice-chat WebSocket, the same 16 kHz mono
// signed 16-bit shape the portal's live transcription sends (the frontend's
// LIVE_TRANSCRIPTION_SAMPLE_RATE). The ticket seals it, so the WebSocket never takes a rate
// from the client, and it is inside liveByteRate's ranges.
const (
	voiceSampleRate   = 16000
	voiceChannels     = 1
	voiceSampleFormat = "s16le"
)

// voiceTicketAudioMaxBytes caps the JSON of the chat's speech settings that rides a ticket
// (voice, speed, language, the advanced passthrough: all of it user-typed free text). The
// ticket is presented on a Sec-WebSocket-Protocol header, one header line, and a proxy
// refuses a line past its limit (8 KB at nginx's default), so everything the ticket carries
// must fit well under it. The ticket's fixed part -- three resolved routing targets, each
// with its sealed upstream credential -- is a few KB once sealed and encoded, and a
// credential is the part that varies most between deployments (a long token seals to
// 200+ characters, three of them ride the ticket). The user's part is therefore kept small
// on purpose: 1 KB, which with credentials of that size on all three legs still leaves the
// whole token under voiceTicketMaxBytes. A speech knob is a name or a number and a
// passthrough a handful of keys, so 1 KB is generous for any real settings; the mint fails
// only after the gate's cold start, so a ticket that cannot fit is better prevented here
// than discovered there.
const voiceTicketAudioMaxBytes = 1024

// voiceTicketMaxBytes is the most an encoded ticket may be handed out at. The token shares
// its header line with the header name and the fixed subprotocol, so a 7 KB ceiling keeps
// the line under an 8 KB limit with a margin. It guards the part no settings cap reaches (a
// pathologically long model name or upstream credential in a routing target): such a mint
// fails closed, logged, instead of producing a ticket the WebSocket upgrade can never
// present.
const voiceTicketMaxBytes = 7 * 1024

// voiceChatTicketRequest is the body of POST /api/portal/voice-chat-ticket: the chat the
// turns belong to, the three gateway models of the loop -- the live-transcription model,
// the chat model and the streaming speech model -- and, optionally, the voice the speech
// model answers in (the portal's voice dropdown; blank leaves the chat's stored voice, or
// the model's default).
type voiceChatTicketRequest struct {
	ChatID   string `json:"chat_id"`
	ASRModel string `json:"asr_model"`
	LLMModel string `json:"llm_model"`
	TTSModel string `json:"tts_model"`
	Voice    string `json:"voice,omitempty"`
}

// voiceChatTicketResponse is the 200 body: the opaque ticket and nothing of the decision it
// carries.
type voiceChatTicketResponse struct {
	Ticket string `json:"ticket"`
}

// voiceChatRefusal words the response for one of the codes the endpoint refuses with, and
// gives the status its kind maps to. The wording is the endpoint's own, from the code alone:
// the cause the gate carries (an agent's reply, an internal model or server name) is for the
// log and never the client's. A code nobody mapped is a 500 ticket failure, never echoed.
func voiceChatRefusal(code string) (int, apierror.Body) {
	switch code {
	case voiceChatASRUnavailable:
		return http.StatusBadRequest, apierror.Response(code, "the chosen transcription model cannot be used for a voice chat", "")
	case voiceChatLLMUnavailable:
		return http.StatusBadRequest, apierror.Response(code, "the chosen chat model cannot be used for a voice chat", "")
	case voiceChatTTSUnavailable:
		return http.StatusBadRequest, apierror.Response(code, "the chosen speech model cannot be used for a voice chat", "")
	case voiceChatAudioSettingsInvalid:
		return http.StatusBadRequest, apierror.Response(code, "the chat's speech settings cannot be used for a voice chat: they are unreadable or too large", "")
	case voiceChatCoLoadFailed:
		return http.StatusConflict, apierror.Response(code, "the chosen models could not be loaded together", "")
	default:
		return http.StatusInternalServerError, apierror.Response(voiceChatTicketFailed, "voice chat ticket could not be issued", "")
	}
}

func writeVoiceChatRefusal(w http.ResponseWriter, code string) {
	status, body := voiceChatRefusal(code)
	writeJSON(w, status, body)
}

// voiceCoLoadBound is the deadline the gate runs under: the configured one, or the default
// for a Server that was not built through New.
func (s *Server) voiceCoLoadBound() time.Duration {
	if s.voiceCoLoadTimeout > 0 {
		return s.voiceCoLoadTimeout
	}
	return DefaultVoiceCoLoadTimeout
}

// handlePortalVoiceChatTicket serves POST /api/portal/voice-chat-ticket: the one
// authenticated, CSRF-protected step before a Voice Chat session, the twin of
// handlePortalLiveTranscriptionTicket. A browser WebSocket cannot send X-OP-CSRF, so the
// upgrade cannot be protected the way every other portal mutation is; this POST is, and
// what it hands back -- a short-lived, single-use, encrypted ticket (voiceTicketStore) -- is
// what the upgrade presents. It carries the server's own decision (the three resolved
// targets, the chat's speech settings, the PCM shape), so the WebSocket never takes a model,
// a route or a rate from the client.
//
// Auth is requireWebScope(gateway:use): a session cookie on this POST also needs the
// X-OP-CSRF header (403 auth.csrf_required), a bearer token does not, and a service token is
// refused 401 whatever it carries. The method is POST for the reason the header is required.
//
// The body is {chat_id, asr_model, llm_model, tts_model, voice?}. In order:
//
//  1. A server with no ticket store or no portal service fails closed, 500
//     portal.voice_chat_ticket_failed, before anything is spent on the request.
//  2. The request is validated: a blank model is 400 model.name_required (the family's own
//     code, naming the field) and a body that is not one JSON object of this shape is 400
//     request.invalid_json. Models are trimmed.
//  3. The chat is read, owner-scoped, as every portal chat read is: a blank id, an unknown
//     chat and another user's chat are all 404 portal.chat_not_found. This precedes the
//     gate on purpose -- a request that cannot succeed must not have cold-started three
//     models first.
//  4. The chat's stored speech settings (settings.audio of the chat document, the same
//     ChatAudioSettings a speech run persists; a chat with none gets the zero settings, the
//     model's default voice) are snapshotted into the ticket. They must decode, and their
//     JSON must be at most voiceTicketAudioMaxBytes, else 400
//     portal.voice_chat_audio_settings_invalid: the ticket rides a header line, and a
//     proxy drops one past its limit. The request's voice, when it names one, is the
//     ticket's Audio.Voice (over a stored one -- a text thread has none, a speech
//     thread's is its own), and counts against that cap like the rest.
//  5. The models go through the co-load gate (voiceCoLoadGate) under a deadline of
//     s.voiceCoLoadBound(), because the gate's ensure can block for a whole cold start --
//     which is why the server's 30 s read and write deadlines are lifted first
//     (liftInferenceDeadlines), as for every other request that outlives them. A
//     refusal is the gate's code at the status its kind maps to (voiceChatRefusal): an
//     unavailable leg 400, models that cannot be held together 409. A gate that outlives
//     its deadline refuses as portal.voice_chat_coload_failed whichever step the time ran
//     out in -- the models could not be made resident in time -- unless the browser itself
//     went away.
//  6. The ticket is minted with the three resolved targets, the chat, the principal's
//     attribution, the settings and the PCM shape, and answered 200 {"ticket": "..."}.
//     A preset-clone speech leg with no voice named by now (the request's, or the chat's
//     stored one) speaks its first preset, never a blank voice (withCloneDefaultVoice). A
//     mint that fails, or a token longer than voiceTicketMaxBytes, is 500
//     portal.voice_chat_ticket_failed.
//  7. Once the ticket is issued, and only then, the speech leg's voice is primed in the
//     background (startVoicePrewarm): one short synthesis that makes the session's first
//     spoken reply warm. It is gated by OP_AI_GATEWAY_VOICE_PREWARM and by the ticket
//     carrying a voice, runs on its own context (never the request's), is never awaited, and
//     a failure of it is logged and nothing else: the answer above is the same with or
//     without it.
//
// Like the live ticket it is a gate, not a relay: no usage row is recorded, no capture is
// written, and no admission is spent (each /v1 core spends its own per turn). The priming
// is no exception: it goes straight to the upstream, so it is neither billed nor stored.
func (s *Server) handlePortalVoiceChatTicket(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if s.voiceTickets == nil || s.Portal == nil {
		slog.Error("voice chat ticket not minted: the server has no ticket store or no portal service")
		writeVoiceChatRefusal(w, voiceChatTicketFailed)
		return
	}
	req, ok := readVoiceChatTicketRequest(w, r)
	if !ok {
		return
	}
	chatID := req.ChatID
	audio, ok := s.voiceChatAudioSettings(w, r, token, chatID, req.Voice)
	if !ok {
		return
	}

	bound := s.voiceCoLoadBound()
	// The gate can hold this request for the whole of bound, far longer than the 30 s the
	// server's control-plane read and write deadlines allow (newHTTPServer arms both when the
	// headers are read). Left armed, the read deadline cancels the request's context and the
	// write deadline drops the connection, so a cold start past 30 s would reach the browser
	// as a dead connection instead of the ticket or the refusal. Everything that reads the
	// request (the body, the chat) is done by now; the deadline of this request is bound.
	liftInferenceDeadlines(w)
	gateCtx, cancel := context.WithTimeout(r.Context(), bound)
	defer cancel()
	asr, llm, tts, code, err := s.voiceCoLoadGate(gateCtx, &token, req.ASRModel, req.LLMModel, req.TTSModel)
	if code != "" {
		s.writeVoiceCoLoadRefusal(gateCtx, w, r, chatID, code, err)
		return
	}

	audio = s.withCloneDefaultVoice(r.Context(), tts, audio)

	ticket, err := s.voiceTickets.Mint(voiceTicketClaims{
		ASR:    asr,
		LLM:    llm,
		TTS:    tts,
		ChatID: chatID,
		// The WebSocket has no principal, so the attribution of the turns' usage rows rides
		// in the ticket (as the live ticket's does).
		UserID:       token.UserID,
		TokenID:      token.ID,
		ServiceID:    token.ServiceID,
		ProjectID:    token.ProjectID,
		Audio:        audio,
		SampleRate:   voiceSampleRate,
		Channels:     voiceChannels,
		SampleFormat: voiceSampleFormat,
	})
	if err != nil {
		slog.Error("voice chat ticket not minted", "chat", chatID, "err", err)
		writeVoiceChatRefusal(w, voiceChatTicketFailed)
		return
	}
	if len(ticket) > voiceTicketMaxBytes {
		slog.Error("voice chat ticket not issued: it would not fit a proxy's header line", "chat", chatID, "bytes", len(ticket), "max", voiceTicketMaxBytes)
		writeVoiceChatRefusal(w, voiceChatTicketFailed)
		return
	}
	// The ticket is issued, so a session will open on this speech target and voice: warm them in
	// the background. It returns at once and the ticket never waits for it or depends on it.
	s.startVoicePrewarm(tts, audio.Voice)
	writeJSON(w, http.StatusOK, voiceChatTicketResponse{Ticket: ticket})
}

// readVoiceChatTicketRequest reads and validates the body of the ticket request, and
// returns it with the chat id, the three models and the voice trimmed. On any refusal it writes the
// response and returns ok == false: a body that is not one JSON object of this shape is
// 400 request.invalid_json, and a blank model is 400 model.name_required naming the field
// (asr_model, then llm_model, then tts_model).
func readVoiceChatTicketRequest(w http.ResponseWriter, r *http.Request) (voiceChatTicketRequest, bool) {
	raw, ok := readRawJSON(w, r)
	if !ok {
		return voiceChatTicketRequest{}, false
	}
	var req voiceChatTicketRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return voiceChatTicketRequest{}, false
	}
	req.ChatID = strings.TrimSpace(req.ChatID)
	req.ASRModel, req.LLMModel, req.TTSModel = strings.TrimSpace(req.ASRModel), strings.TrimSpace(req.LLMModel), strings.TrimSpace(req.TTSModel)
	req.Voice = strings.TrimSpace(req.Voice)
	for _, leg := range []struct{ field, model string }{{"asr_model", req.ASRModel}, {"llm_model", req.LLMModel}, {"tts_model", req.TTSModel}} {
		if leg.model == "" {
			writeJSON(w, http.StatusBadRequest, apierror.Response(audioVoicesModelRequired, leg.field+" is required", ""))
			return voiceChatTicketRequest{}, false
		}
	}
	return req, true
}

// writeVoiceCoLoadRefusal answers a refused co-load gate with code and logs why.
func (s *Server) writeVoiceCoLoadRefusal(gateCtx context.Context, w http.ResponseWriter, r *http.Request, chatID, code string, err error) {
	// The deadline is the endpoint's own, so it owns the answer for it: a deadline that
	// hit while a leg was still being resolved reads there as an unavailable leg. A
	// browser that went away is not a deadline, and nobody reads that response anyway.
	if r.Context().Err() == nil && errors.Is(gateCtx.Err(), context.DeadlineExceeded) {
		slog.Warn("voice chat ticket refused: the models were not resident together within the deadline", "deadline", s.voiceCoLoadBound(), "chat", chatID, "gate_code", code, "err", err)
		code = voiceChatCoLoadFailed
	} else {
		slog.Debug("voice chat ticket refused", "code", code, "chat", chatID, "err", err)
	}
	writeVoiceChatRefusal(w, code)
}

// voiceChatAudioSettings reads the speech settings of the caller's chat for the ticket, with
// the voice the request names (blank: none) laid over the stored one. On any refusal it
// writes the response and returns ok == false: a chat that is not the caller's or does not
// exist is the portal's own 404 (the owner-scoped GetChat, no leak of which ids exist), and
// settings that cannot ride a ticket -- the requested voice included, which is measured with
// the rest -- are voiceChatAudioSettingsInvalid.
func (s *Server) voiceChatAudioSettings(w http.ResponseWriter, r *http.Request, token auth.Token, chatID, voice string) (portal.ChatAudioSettings, bool) {
	if chatID == "" {
		writePortalChatError(w, portal.ErrChatNotFound)
		return portal.ChatAudioSettings{}, false
	}
	chat, err := s.Portal.GetChat(r.Context(), token, chatID)
	if err != nil {
		writePortalChatError(w, err)
		return portal.ChatAudioSettings{}, false
	}
	audio, err := chatDocAudioSettings(chat.Content)
	if err != nil {
		slog.Debug("voice chat ticket refused: the chat's speech settings do not decode", "chat", chatID, "err", err)
		writeVoiceChatRefusal(w, voiceChatAudioSettingsInvalid)
		return portal.ChatAudioSettings{}, false
	}
	if voice != "" {
		audio.Voice = voice
	}
	// The ticket carries what the settings marshal to, so that is what the cap measures.
	encoded, err := json.Marshal(audio)
	if err != nil || len(encoded) > voiceTicketAudioMaxBytes {
		slog.Debug("voice chat ticket refused: the chat's speech settings are over the cap", "chat", chatID, "bytes", len(encoded), "max", voiceTicketAudioMaxBytes, "err", err)
		writeVoiceChatRefusal(w, voiceChatAudioSettingsInvalid)
		return portal.ChatAudioSettings{}, false
	}
	return audio, true
}

// chatDocAudioSettings reads settings.audio out of a chat document: the speech settings a
// speech run persisted (PrepareChatRun stores the run's settings in the document, and clears
// audio for a chat that is not a speech thread). The document is schema-free client JSON,
// so like parseChatDoc it is tolerant of the document as a whole -- one without readable
// settings has no speech settings, the zero value -- but not of the audio member itself:
// settings that are there and do not decode are an error, never a silent fall back to the
// default voice the user did not pick.
func chatDocAudioSettings(content json.RawMessage) (portal.ChatAudioSettings, error) {
	var doc struct {
		Settings json.RawMessage `json:"settings"`
	}
	if json.Unmarshal(content, &doc) != nil || len(doc.Settings) == 0 {
		return portal.ChatAudioSettings{}, nil
	}
	var settings struct {
		Audio json.RawMessage `json:"audio"`
	}
	if json.Unmarshal(doc.Settings, &settings) != nil || len(settings.Audio) == 0 {
		return portal.ChatAudioSettings{}, nil
	}
	var audio portal.ChatAudioSettings
	if err := json.Unmarshal(settings.Audio, &audio); err != nil {
		return portal.ChatAudioSettings{}, err
	}
	return audio, nil
}

// withCloneDefaultVoice names a voice for a preset-clone speech leg that has none: a clone model
// has no default voice of its own to fall back on (it synthesises in a reference voice it is
// handed, or a named preset), so a ticket that names none for it speaks its first preset --
// routing.FirstPresetVoice of the mapping's voices cache, the one definition of "the preset a
// caller may ask for". The portal pre-selects that same voice in its dropdown; this is for the
// request that could not (the voices were still loading, an older portal). A voice that is
// already named, and a leg that is not a clone model, are left as they are, and so is a ticket
// whose cache cannot be read: the mint is not worth failing over a default, so it falls back to
// what it did before, the blank voice (logged).
func (s *Server) withCloneDefaultVoice(ctx context.Context, tts routing.Target, audio portal.ChatAudioSettings) portal.ChatAudioSettings {
	if audio.Voice != "" || s.Routes == nil {
		return audio
	}
	voice, err := s.cloneDefaultVoice(ctx, tts.RouteID)
	if err != nil {
		slog.Warn("voice chat ticket: the clone model's first preset could not be read, so it keeps a blank voice", "route", tts.RouteID, "err", err)
		return audio
	}
	audio.Voice = voice
	return audio
}

// cloneDefaultVoice is the first preset voice of the mapping routeID when it is a voice-clone
// model (speech_clone yes), and "" otherwise or when its voices cache lists none.
func (s *Server) cloneDefaultVoice(ctx context.Context, routeID string) (string, error) {
	rows, err := s.Routes.MappingCapabilities(ctx, routeID)
	if err != nil {
		return "", err
	}
	if routing.CapabilityRowsByName(rows)[routing.CapabilitySpeechClone].Verdict != routing.CapabilityYes {
		return "", nil
	}
	cached, err := s.Routes.ModelMappingCacheForMappings(ctx, []string{routeID}, routing.ModelMappingCacheVoices)
	if err != nil {
		return "", err
	}
	return routing.FirstPresetVoice(cached[routeID]), nil
}
