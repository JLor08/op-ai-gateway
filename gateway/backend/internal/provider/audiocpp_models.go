// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"op-ai-gateway/internal/routing"
	"strings"
)

// audiocppModelsPath is audio.cpp's OpenAI-shaped model listing. It is fixed by
// the server, not configured per application, and it is the ONLY place audio.cpp
// states what a model can do: there is no separate capability document like
// sd-server's /sdcpp/v1/capabilities, so the capability signal is the task and
// mode fields of each listed model.
//
// The include_params=true query asks the server to add each entry's params array
// (the request params the model declares: objects of name, type, default and
// required), which is the only place audio.cpp states whether a model takes a
// language and what it defaults to (AudioCppVerdicts.AcceptsLanguage). The query
// adds a field to each entry and changes none of the others, so every other read
// of the listing is unaffected by it. endpointURL concatenates the path verbatim,
// and http.NewRequestWithContext splits the query off it, so it reaches the wire
// intact.
const audiocppModelsPath = "/v1/models?include_params=true"

// audio.cpp task values, spelled as the server emits them in /v1/models. The
// listing's task names what the listed model DOES: "tts" synthesises speech from
// text and "clon" (audio.cpp's spelling of voice cloning -- NOT "clone"; the
// server's load API rejects "clone" and the /v1/models listing reports "clon",
// verified against v0.9.1) synthesises it in a reference voice, so both are a
// speech "yes"; "asr" transcribes speech to text, so it is a transcription "yes".
// Every other task (the music tasks, ...) is a model that does neither. One task
// answers BOTH capabilities at once: a model is never both a synthesiser and a
// transcriber. (The transcription_streaming and speech_streaming verdicts are not
// task answers: they are read from the mode field below. The speech_clone verdict
// is a task answer, and the only one that tells "tts" from "clon": see
// AudioCppVerdicts.SpeechClone.)
const (
	audioCppTaskTTS   = "tts"
	audioCppTaskClone = "clon"
	audioCppTaskASR   = "asr"
)

// audioCppParamLanguage is the name of the request param audio.cpp's params array
// uses for a model's language, spelled as the server emits it in
// /v1/models?include_params=true (verified: qwen3_tts defaults it to "auto",
// confucius4_tts to "zh").
const audioCppParamLanguage = "language"

// audio.cpp mode values. The listing's mode names how the model is LOADED:
// "streaming" is a model that serves a streamed answer (stream=true), and
// "offline" one that answers only a one-shot request and refuses stream=true.
// Any other value is no statement this code can act on. The mode is read for an
// ASR model (it says how a TRANSCRIBER streams) and for a speech model, tts or
// clon (it says how a SYNTHESISER streams): parseAudioCppModels.
const (
	audioCppModeStreaming = "streaming"
	audioCppModeOffline   = "offline"
)

// AudioCppVerdicts is what one audio.cpp /v1/models listing says about ONE
// model, one verdict per capability the listing can answer.
//
// Speech and Transcription are each routing.CapabilityYes,
// routing.CapabilityNo, or "" for "no answer". Both are BIDIRECTIONAL: the
// listing states each listed model's task, so a model it names with a task that
// does not serve the capability is a real "no", like sdcpp's exhaustive
// supported_modes and unlike Ollama's capability array. The two verdicts are
// decided together from the one task, so they are either both set or both "".
// "" is reserved for everything the listing does not say: the model is not in
// it (the listing was first taken to name LOADED models only, but audio.cpp
// v0.9.1 also names an unloaded one with loaded:false, so a missing entry says
// only that the server does not name the model), the listing is empty, it
// cannot be read, or the named entry carries no task. See
// routing.CapabilitySourceAudioCppModels.
//
// TranscriptionStreaming is the third axis, a refinement of Transcription: it says
// whether a TRANSCRIBER can answer a stream=true transcription, which is what the
// gateway's transcription handler decides stream-versus-buffered on. It is stated
// for an asr model ONLY, from its mode: routing.CapabilityYes for mode "streaming",
// routing.CapabilityNo for mode "offline", and "" for everything else -- no mode, an
// empty or unknown one, one of the wrong type, no entry for the model at all, and
// every model that is not an asr one (tts, clon, music, an entry with no task),
// whatever its mode. A model that does not transcribe has no answer on a
// transcription capability, not even a "no". The mode never sets or clears the
// task's verdicts (Speech, Transcription): an odd mode costs only this axis.
//
// SpeechClone is the fourth axis, a refinement of Speech: it says whether a speech
// model synthesises only IN A REFERENCE VOICE, which a text-only caller (the
// portal chat's speech picker) cannot supply. It is stated for the two speech
// tasks ONLY, from the task alone: routing.CapabilityNo for task "tts" and
// routing.CapabilityYes for task "clon". It is "" for everything else -- an asr
// model, any other task (music, an unknown one), an entry with no task, no entry
// for the model at all, an empty listing, an unreadable body -- because a model
// that does not synthesise speech has no answer on a speech refinement, not even
// a "no". It is set exactly when Speech is "yes", and the mode never moves it: it
// refines Speech, it never replaces it (a clone model is still a Speech "yes", so
// it stays a /v1/audio/speech candidate), and it gates no route.
//
// SpeechStreaming is the fifth axis, the speech twin of TranscriptionStreaming and
// a refinement of Speech: it says whether a SYNTHESISER can answer a stream=true
// speech request, which is what the gateway's speech handler decides
// stream-versus-buffered on. It is stated for a speech model ONLY -- the two
// speech tasks, tts and clon -- from the same mode field: routing.CapabilityYes for
// mode "streaming", routing.CapabilityNo for mode "offline", and "" for everything
// else -- no mode, an empty or unknown one, one of the wrong type, no entry for the
// model at all, and every model that is not a speech one (asr, music, an entry
// with no task), whatever its mode. A model that does not synthesise speech has no
// answer on a speech capability, not even a "no", and an asr model's mode is its
// TranscriptionStreaming, never this. Like TranscriptionStreaming it gates no
// route, and the mode never sets or clears the task's verdicts: an odd mode costs
// only this axis.
//
// The five verdicts are the capability axis and are decided from the task
// (Speech, Transcription, SpeechClone) and, for an asr model or a speech model,
// the mode (TranscriptionStreaming, SpeechStreaming) alone. Whether the model is
// loaded RIGHT NOW is the health axis: an asr model the server lists as unloaded
// still carries task "asr", and stays a transcription "yes".
//
// Loaded is that health axis, carried beside the verdicts because the one
// listing states both: true only when the listing's entry for the model says
// loaded:true (a boolean). Everything the listing does not say -- no entry, no
// field, a null or a non-boolean, an unreadable body -- is false, because the
// caller acts on it by asking the server for the model's voices, and the server
// answers for the model it has loaded, so a model not observed loaded must never
// be asked about. It decides no verdict and no verdict decides it: it is set by
// the probe (fetchAudioCppModels), not by parseAudioCppModels, whose table is the
// capability rule shared verbatim with the agent's copy.
//
// Path is the identity axis, set the same way as Loaded and for the same reason (the
// one listing states it, and it is no part of the shared capability rule): the model
// file the listing states for the model's id, verbatim, or "" for everything it does not
// say -- no entry, no field, a null, a blank or non-string value, an unreadable body. A
// rate measured on a model is a property of THAT model (the streamed speech billing
// reads the output byte rate cached from a benchmark), and the model behind one id can
// change under it (the server restarted with another file, the id re-pointed), which no
// rename of the mapping signals; the health loop compares this string tick to tick to
// notice. It decides no verdict and no verdict decides it, and an entry the server
// lists as unloaded states its path too, so it is read whether or not the model is
// loaded.
//
// AcceptsLanguage and LanguageDefault are the language axis, set the same way as Loaded
// and Path and for the same reason (the one listing states it when asked for its params,
// and it is no part of the shared capability rule): AcceptsLanguage is true only when the
// listing's entry for the model declares a request param named "language", and
// LanguageDefault is that param's default when it is a string ("auto", "zh"), or "" for a
// param that states none or a non-string one. Both are false/"" for everything the listing
// does not say -- no entry, no params field, a params field of the wrong shape, no
// language param among them, an unreadable body -- because a model not observed to take a
// language must never be sent one. It decides no verdict and no verdict decides it; an
// odd params shape costs only this axis.
type AudioCppVerdicts struct {
	Speech                 string
	Transcription          string
	TranscriptionStreaming string
	SpeechClone            string
	SpeechStreaming        string
	Loaded                 bool
	Path                   string
	AcceptsLanguage        bool
	LanguageDefault        string
}

// AudioCppModelsProber is an optional provider capability: it GETs
// target.Endpoint + /v1/models?include_params=true and derives the speech,
// transcription, transcription streaming, speech clone and speech streaming
// verdicts the listing states for modelID, whether it states the model as loaded
// right now, and whether it declares a language request param and that param's
// default. A transport
// failure or a non-2xx status is an error and carries no verdict, so the caller
// writes nothing. A 2xx body that does not name the model, or cannot be read, is
// NOT an error, only empty verdicts and Loaded false -- the caller writes
// nothing for an empty verdict either.
//
// It is a client method, like ProbeSdcppCapabilities, and not a free function,
// because the request must ride the client's own *http.Client: cmd/gateway
// builds that one on the outbound app transport, which carries the mesh routing
// and TLS settings a separate client would bypass.
type AudioCppModelsProber interface {
	ProbeAudioCppModels(ctx context.Context, target routing.Target, modelID string) (AudioCppVerdicts, error)
}

var _ AudioCppModelsProber = (*OpenAICompatibleClient)(nil)

// ProbeAudioCppModels reads audio.cpp's model listing through this client's
// transport. See AudioCppModelsProber.
func (c *OpenAICompatibleClient) ProbeAudioCppModels(ctx context.Context, target routing.Target, modelID string) (AudioCppVerdicts, error) {
	return fetchAudioCppModels(ctx, c.http, target, modelID)
}

// fetchAudioCppModels is the GET behind ProbeAudioCppModels. Modelled on
// fetchSdcppDoc: it honours target.Timeout, attaches the per-application
// upstream credential, and bounds the body at 1 MiB. A transport failure or a
// non-2xx status is an ErrUnavailable; decoding is the tolerant
// parseAudioCppModels.
func fetchAudioCppModels(ctx context.Context, httpClient *http.Client, target routing.Target, modelID string) (AudioCppVerdicts, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, audiocppModelsPath), nil)
	if err != nil {
		return AudioCppVerdicts{}, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	applyUpstreamAuth(ctx, req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return AudioCppVerdicts{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AudioCppVerdicts{}, unavailableStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return AudioCppVerdicts{}, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	// The verdicts, the loaded flag, the path and the language param are four reads
	// of the one body: the verdicts are the capability rule, pinned by the table the
	// agent's copy shares, and loaded, the path and the language param are the
	// health, identity and request-param axes beside them (see AudioCppVerdicts).
	// The listing is one small document per probe, so decoding it four times costs
	// nothing worth coupling them.
	v := parseAudioCppModels(body, modelID)
	v.Loaded = parseAudioCppLoaded(body, modelID)
	v.Path = parseAudioCppPath(body, modelID)
	v.AcceptsLanguage, v.LanguageDefault = parseAudioCppLanguageParam(body, modelID)
	return v, nil
}

// audioCppModelsDoc is the part of the listing this code reads: each entry's id,
// task, mode, loaded, path and params. The rest of an entry (object, owned_by,
// family) answers no question this code asks and is left undecoded, so an unknown
// or added field never breaks the read.
//
// Mode, Loaded, Path and Params are held raw and decoded on their own
// (audioCppStreamingVerdict, audioCppLoadedFlag, audioCppPathString,
// audioCppLanguageParam): a mode, a loaded, a path or a params of a type this code
// does not expect must cost only the streaming verdicts, only the loaded flag,
// only the path or only the language axis, never the speech and transcription
// verdicts the task decides, and a typed field would fail the whole document's
// decode on one odd value.
type audioCppModelsDoc struct {
	Data []struct {
		ID     string          `json:"id"`
		Task   string          `json:"task"`
		Mode   json.RawMessage `json:"mode"`
		Loaded json.RawMessage `json:"loaded"`
		Path   json.RawMessage `json:"path"`
		Params json.RawMessage `json:"params"`
	} `json:"data"`
}

// parseAudioCppModels derives the speech, transcription,
// transcription_streaming, speech_clone and speech_streaming verdicts for modelID
// from a /v1/models body. It is
// TOLERANT, unlike the strict sdcpp document parser: a body that is not the
// listing, or an entry of the wrong shape, yields the empty verdict rather than
// an error, so a probe never breaks over an unexpected shape.
//
// The rule, with the loaded entry's task authoritative:
//   - the entry whose id is modelID, with task "tts" or "clon" ->
//     speech "yes", transcription "no"
//   - the entry whose id is modelID, with task "asr" ->
//     speech "no", transcription "yes"
//   - the entry whose id is modelID, with any other non-empty task ->
//     both "no"
//   - the entry whose id is modelID, with no task -> both "" (an absent field
//     is not a statement, and a "no" can switch the model's route off)
//   - no entry for modelID, an empty data list, or an unreadable body -> both ""
//
// The streaming verdict is decided beside those, for an ASR model only, from the
// entry's mode:
//   - the entry whose id is modelID, with task "asr" and mode "streaming" ->
//     streaming "yes"
//   - the entry whose id is modelID, with task "asr" and mode "offline" ->
//     streaming "no"
//   - the entry whose id is modelID, with task "asr" and no mode, an empty, blank
//     or unknown one, or one that is not a string -> streaming ""
//   - the entry whose id is modelID, with any other task (tts, clon, music, an
//     unknown one) or none -> streaming "", WHATEVER its mode: "transcription
//     streaming" is the transcriber's capability, so a model that does not
//     transcribe is no "no" on it either
//   - no entry for modelID, an empty data list, or an unreadable body -> streaming ""
//
// A mode of an unexpected type leaves the task's verdicts alone: it costs only the
// streaming verdict.
//
// The speech_streaming verdict is decided beside those, for a SPEECH model only
// (task "tts" or "clon"), from the SAME mode field and by the same mapping:
//   - the entry whose id is modelID, with task "tts" or "clon" and mode "streaming"
//     -> speech_streaming "yes"
//   - the entry whose id is modelID, with task "tts" or "clon" and mode "offline"
//     -> speech_streaming "no"
//   - the entry whose id is modelID, with task "tts" or "clon" and no mode, an
//     empty, blank or unknown one, or one that is not a string -> speech_streaming ""
//   - the entry whose id is modelID, with any other task (asr, music, an unknown
//     one) or none -> speech_streaming "", WHATEVER its mode: "speech streaming"
//     is the synthesiser's capability, so a model that does not synthesise speech
//     is no "no" on it either (an asr model's mode is its transcription_streaming
//     verdict, never this one)
//   - no entry for modelID, an empty data list, or an unreadable body ->
//     speech_streaming ""
//
// The speech_clone verdict is decided beside those, for the two speech tasks only,
// from the entry's task alone:
//   - the entry whose id is modelID, with task "tts" -> speech_clone "no"
//   - the entry whose id is modelID, with task "clon" -> speech_clone "yes"
//   - the entry whose id is modelID, with any other task (asr, music, an unknown
//     one) or none -> speech_clone "", WHATEVER its mode: "speech clone" refines
//     speech, so a model that does not synthesise speech is no "no" on it either
//   - no entry for modelID, an empty data list, or an unreadable body ->
//     speech_clone ""
//
// Only id, task and mode decide a verdict: loaded (and family, path) never
// does. Whether a model is loaded is the health axis, not the capability axis, so
// the server listing an asr model with loaded:false does not turn its
// transcription "yes" into anything else. The loaded flag is read separately, by
// parseAudioCppLoaded, and is not part of this rule.
//
// The id match is exact. The task and mode matches ignore case and surrounding
// whitespace: a "TTS" or "ASR" the server might one day emit must not fall to
// the "no" default, which is a bidirectional verdict that switches a route off.
// The first entry that carries the id answers.
//
// This is a DUPLICATE, on purpose, of parseAudioCppModels in
// server-agent/internal/collector/audiocpp.go, which reads the same listing
// from an audio.cpp child the agent launched. The two are separate Go modules
// and cannot share code, the same precedent as parseSdcppCapabilities. The
// audioCppModelCases table, duplicated verbatim in both modules' tests, pins
// both copies, and TestAudioCppModelsCasesMatchTheAgentsTable holds the two
// tables identical. Whoever changes this rule must change that copy, and both
// tables, identically.
func parseAudioCppModels(body []byte, modelID string) AudioCppVerdicts {
	if modelID == "" {
		return AudioCppVerdicts{}
	}
	var doc audioCppModelsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return AudioCppVerdicts{}
	}
	for _, entry := range doc.Data {
		if entry.ID != modelID {
			continue
		}
		var v AudioCppVerdicts
		switch strings.ToLower(strings.TrimSpace(entry.Task)) {
		case "":
		case audioCppTaskTTS:
			v.Speech, v.Transcription = routing.CapabilityYes, routing.CapabilityNo
			v.SpeechClone = routing.CapabilityNo
			v.SpeechStreaming = audioCppStreamingVerdict(entry.Mode)
		case audioCppTaskClone:
			v.Speech, v.Transcription = routing.CapabilityYes, routing.CapabilityNo
			v.SpeechClone = routing.CapabilityYes
			v.SpeechStreaming = audioCppStreamingVerdict(entry.Mode)
		case audioCppTaskASR:
			v.Speech, v.Transcription = routing.CapabilityNo, routing.CapabilityYes
			v.TranscriptionStreaming = audioCppStreamingVerdict(entry.Mode)
		default:
			v.Speech, v.Transcription = routing.CapabilityNo, routing.CapabilityNo
		}
		return v
	}
	return AudioCppVerdicts{}
}

// parseAudioCppLoaded reports whether a /v1/models body states modelID as
// currently loaded: the first entry whose id is modelID, with a loaded field that
// is the boolean true. Like parseAudioCppModels it is tolerant -- it never
// fails -- and every statement it cannot read is false: no entry for modelID, an
// empty modelID, an entry without a loaded field or with a null, a string or a
// number there, an empty data list, a body that is not the listing. The id match
// is exact, as in parseAudioCppModels, so the two always speak of the same entry.
//
// Loaded is the health axis (see AudioCppVerdicts): it decides no capability
// verdict, and no verdict decides it, so an asr model the server lists as
// unloaded keeps its transcription "yes" while this says false.
func parseAudioCppLoaded(body []byte, modelID string) bool {
	if modelID == "" {
		return false
	}
	var doc audioCppModelsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return false
	}
	for _, entry := range doc.Data {
		if entry.ID != modelID {
			continue
		}
		return audioCppLoadedFlag(entry.Loaded)
	}
	return false
}

// audioCppLoadedFlag maps one entry's raw loaded field onto a bool: true only for
// the JSON boolean true. An absent field, a null, and a value that is not a
// boolean at all (the string "true", a number) are false -- a loaded model is
// never guessed.
func audioCppLoadedFlag(raw json.RawMessage) bool {
	var loaded bool
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return false
	}
	return loaded
}

// parseAudioCppPath returns the model file a /v1/models body states for modelID: the
// first entry whose id is modelID, with a path that is a non-blank string, kept
// verbatim. Like parseAudioCppLoaded it is tolerant -- it never fails -- and every
// statement it cannot read is "": no entry for modelID, an empty modelID, an entry
// without a path or with a null, a number or an object there, a blank string, an empty
// data list, a body that is not the listing. The id match is exact, as in
// parseAudioCppModels, so the two always speak of the same entry.
//
// The path is the identity axis (see AudioCppVerdicts.Path): it decides no capability
// verdict, and an entry the server lists as unloaded states it too.
func parseAudioCppPath(body []byte, modelID string) string {
	if modelID == "" {
		return ""
	}
	var doc audioCppModelsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	for _, entry := range doc.Data {
		if entry.ID != modelID {
			continue
		}
		return audioCppPathString(entry.Path)
	}
	return ""
}

// audioCppPathString maps one entry's raw path onto its string: the JSON string
// itself, verbatim, unless it is blank. An absent field, a null and a value that is
// not a string at all are "" -- an identity is never guessed.
func audioCppPathString(raw json.RawMessage) string {
	var path string
	if err := json.Unmarshal(raw, &path); err != nil || strings.TrimSpace(path) == "" {
		return ""
	}
	return path
}

// parseAudioCppLanguageParam reports whether a /v1/models?include_params=true body
// states that modelID takes a language, and that param's default: the first entry
// whose id is modelID, scanned for its first param named "language". Like
// parseAudioCppPath it is tolerant -- it never fails -- and every statement it
// cannot read is (false, ""): no entry for modelID, an empty modelID, an entry
// without a params field or with a null, a string or an object there, a params
// array with no language param, an empty data list, a body that is not the listing.
// The id match is exact, as in parseAudioCppModels, so the two always speak of the
// same entry.
//
// The language param's presence and its default are two statements: a language param
// whose default is absent, null or not a string is (true, ""), because the model does
// take a language and merely states no default for it.
//
// The language axis (see AudioCppVerdicts.AcceptsLanguage) decides no capability
// verdict, and no verdict decides it. It is a SEPARATE read from parseAudioCppModels,
// whose table is the capability rule shared verbatim with the agent's copy, and an odd
// params shape costs only this axis.
func parseAudioCppLanguageParam(body []byte, modelID string) (accepts bool, def string) {
	if modelID == "" {
		return false, ""
	}
	var doc audioCppModelsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, ""
	}
	for _, entry := range doc.Data {
		if entry.ID != modelID {
			continue
		}
		return audioCppLanguageParam(entry.Params)
	}
	return false, ""
}

// audioCppLanguageParam finds the first param named "language" in one entry's raw
// params array and reads its default. The array is decoded as raw elements and each
// element on its own, so a param of an odd shape (not an object, a name that is not a
// string) is skipped and cannot hide a language param beside it. The name matches
// ignoring case and surrounding whitespace, like the task and mode matches, and exactly
// otherwise: "language_code" is not "language". The default is the JSON string itself,
// verbatim; an absent default, a null and a value that is not a string at all are "" --
// a default is never guessed.
func audioCppLanguageParam(raw json.RawMessage) (accepts bool, def string) {
	var params []json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		return false, ""
	}
	for _, p := range params {
		var param struct {
			Name    string          `json:"name"`
			Default json.RawMessage `json:"default"`
		}
		if err := json.Unmarshal(p, &param); err != nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(param.Name)) != audioCppParamLanguage {
			continue
		}
		var d string
		if err := json.Unmarshal(param.Default, &d); err != nil {
			d = ""
		}
		return true, d
	}
	return false, ""
}

// audioCppStreamingVerdict maps one entry's raw mode onto a streaming verdict:
// the transcription streaming one for an asr entry and the speech streaming one
// for a tts or clon entry. The caller asks it for those entries only, so each
// streaming capability stays its own kind of model's. "streaming" is
// routing.CapabilityYes and "offline" routing.CapabilityNo, matched ignoring case
// and surrounding whitespace, and everything else is "". Everything else covers an absent field, a null, an empty
// or blank string, a mode this code does not know (a "no" is only ever the
// server's own word for a model that cannot stream, never a guess), and a value
// that is not a string at all.
func audioCppStreamingVerdict(raw json.RawMessage) string {
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case audioCppModeStreaming:
		return routing.CapabilityYes
	case audioCppModeOffline:
		return routing.CapabilityNo
	default:
		return ""
	}
}
