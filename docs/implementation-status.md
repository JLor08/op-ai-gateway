# Implementation status — #251 (voice picker + per-mapping cache)

Branch-local working file (removed before the PR; never on `main`).

## Task 1 — audio.cpp voices contract (live-verified v0.9.1, source of truth)

Verified against a live audio.cpp **v0.9.1** server (operator LAN host; hostname
deliberately omitted per the no-local-hostnames rule) with the `audio8-tts`
model (`family: audio8_tts`, `task: tts`, `mode: offline`) loaded.

- **`GET /v1/audio/voices`** returns `{"voices": ["demo_1_man","demo_2_man","demo_3_woman","demo_4_woman"]}`
  — a **flat JSON array of voice-name strings**, no per-voice metadata (no id/language/gender objects).
- **Per-loaded-model, param ignored.** The response is the voices of whatever model is
  currently **loaded** on that server; the **`?model=` query param is IGNORED** by the raw
  audio.cpp server (same result for no param, a valid id, and a bogus id). ⇒ The gateway
  fetches voices when it observes a mapping's model **loaded** and attributes the result to
  **that** mapping; the gateway's own `/v1/audio/voices?model=` endpoint uses the param to
  read the cache, not to drive the upstream.
- **`/v1/models`** carries **`loaded: true|false`** per model (and `id`, `task`, `mode`,
  `family`, `path`). Task 5 decodes `loaded` from here.
- **VOICE_FIELD = `voice`** (the EXISTING speech-request field — no new `ChatAudioSettings`
  field needed; Task 9's "iff VOICE_FIELD != voice" branch is NOT taken).
  - `POST /v1/audio/speech {voice:"demo_1_man"}` → **200 `audio/wav`**.
  - `{voice:"zzz_bogus_voice"}` → **500** `{"error":{"message":"missing Audio8 TTS cached
    voice reference directory: ...\\references\\zzz_bogus_voice","type":"server_error"}}`.
    So `voice` is honored **and validated** against the cached voice references.
  - `{voice_id:"..."}` is **ignored** (valid and bogus both → 200 default audio) → not our field.
  - No voice → 200 (a default voice).
- **Matching rule:** a listed name maps to a cached voice-reference directory; treat matching
  as **exact** (it is a directory name). An unknown free-text voice returns **500** (not 400)
  — the accepted free-text risk; it flows through `normalizeSpeechUpstreamError` like other
  audio.cpp 500s.

Consequences for the plan:
- Parser (Task 3): decode `{"voices": [string...]}` → `[]provider.Voice{Name: s}` (Name only).
- Endpoints (Task 7/8): the cache row stores the name list; the gateway endpoint's `?model=`
  reads the cache (the upstream's param is moot).
- Picker (Task 9): combobox over the names; key on `voice`; no ChatAudioSettings change.
- Filter/sniff (Task 10): sniff and filter on `voice`.
