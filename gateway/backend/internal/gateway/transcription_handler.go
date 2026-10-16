// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

// defaultMaxAudioTranscriptionBodyBytes is the default bound on the multipart read
// of POST /v1/audio/transcriptions, an operator setting
// (ServerDeps.MaxAudioTranscriptionBodyBytes, config
// OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES; the Server's value is
// transcriptionBodyLimit). Like the image edit, the upload is held in memory --
// the handler peeks the form before routing and may rebuild it afterwards (a
// model-name rewrite) -- so an unbounded read is a memory exposure the endpoint has
// to answer for. It is a setting of its own, not the image-edit one, so an operator
// can size audio independently. The default is the agent runtime router's own
// request cap (agentRouterMaxBodyBytes): a body bound for an agent-launched model
// must also fit what the agent accepts after the gateway has prepared it.
const defaultMaxAudioTranscriptionBodyBytes = 32 << 20

// maxTranscriptionFieldBytes bounds ONE text field the handler reads out of the
// form (model, response_format, stream, ...). The audio file part is never read
// into a field: it is counted and relayed.
const maxTranscriptionFieldBytes = 1 << 20

// transcriptionBodyLimit is the multipart read cap in force: the configured one, or
// the default for a Server that was never given one (a bare &Server{}).
func (s *Server) transcriptionBodyLimit() int64 {
	if s.maxAudioTranscriptionBodyBytes > 0 {
		return s.maxAudioTranscriptionBodyBytes
	}
	return defaultMaxAudioTranscriptionBodyBytes
}
