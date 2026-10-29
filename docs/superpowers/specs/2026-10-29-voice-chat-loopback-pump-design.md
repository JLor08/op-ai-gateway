# Voice Chat ASR loopback — concurrent PCM feed (design)

**Issue:** #331
**Status:** approved design (brainstormed 2026-10-29; Option B chosen)

## Intent & why

A Voice Chat turn stalls mid-session — even while the user speaks — with the
upstream error `live request body stalled: no data within the idle timeout`
(audio.cpp aborts because its live-transcription request body received no PCM
within its idle timeout). The same ASR model works STANDALONE.

Root cause (verified by runtime measurement + code trace, see #331): the Voice
Chat opens the per-turn ASR loopback from the single state-machine run loop. On
the first PCM frame, `onPCM` → `openTurnLoopback` → `openLiveLoopback`'s
`http.DefaultClient.Do` (Expect: 100-continue) BLOCKS that one goroutine for the
whole two-hop dial, and the first PCM frame is written only after `Do` returns.
So no PCM flows into the loopback request body during the dial, while /v1 has
already opened the audio.cpp request and started its idle timer. On a cold ASR
model the dial exceeds that idle timeout → the stall. The standalone path avoids
it by pumping PCM into the same pipe from a dedicated goroutine (`liveReadLoop`)
concurrently with the dial.

## Chosen design (Option B — dedicated pump goroutine, mirroring the standalone path)

Feed PCM into the loopback request body concurrently with the dial, and keep the
state-machine run loop responsive (it also drives the silence/finalize timers,
transcript handling and teardown, which must not stall behind pipe backpressure).

- The run loop keeps the half-duplex gate (`micOpen`) authoritative. On the first
  gated PCM frame of a turn (no loopback open or opening), it sets up the per-turn
  loopback **asynchronously**: create the pipe and a per-turn buffered PCM channel,
  start a dedicated **pump goroutine** that drains the channel into the pipe
  writer, and start the **dial** (`openLiveLoopback` + status handling) in its own
  goroutine. The run loop then forwards every gated frame — including the first —
  to the buffered channel and returns immediately; it never calls `Do` and never
  blocks on `pw.Write`.
- The dial's outcome is delivered back to the run loop as an **event** on a new
  select case (not a synchronous return, and never a cross-goroutine write of
  machine state): on success the run loop records the loopback (enables the ASR
  relay / `asrCh`); on a non-2xx refusal or a dial error it surfaces the gateway
  code + message as the error frame and tears the turn down — exactly as the
  current synchronous failure paths do.
- When the per-turn channel is full (the pump is briefly behind, e.g. during the
  dial), the run loop **drops the oldest buffered frame** to make room rather than
  block — a bounded, mic-stays-live policy; a dropped frame of audio is
  acceptable and never blocks the state machine.
- The per-chunk loopback lifecycle is unchanged (open on the first gated PCM of a
  chunk, close on `finalizeChunk`); with the concurrent feed, each dial is fed
  from its first frame, so re-dials no longer stall.

## Invariants & constraints

- **Single-owner preserved.** Only the run loop reads/writes machine fields. The
  pump and dial goroutines use locals and channels; the run loop sets
  `asrCh`/`respBody`/loopback fields only in response to the dial-outcome event.
  The change must be clean under `go test -race`.
- **Behavior-preserving** for: a successful turn, a non-2xx loopback refusal
  (forward its code+message), a dial error, cancel/resume, finalize, and full
  teardown (no leaked goroutine; the pipe and the dial are cancelled; the pump and
  relay are joined), including teardown that races a still-in-flight dial.
- **Backend only.** `gateway/backend/internal/gateway`. No wire-protocol change,
  no `ServerAgent` version bump, no schema/migration. The standalone live
  transcription path is untouched.

## Non-goals

- Not changing the per-chunk re-dial into a session-long loopback (out of scope;
  the concurrent feed removes the stall it caused).
- Not touching the standalone `liveReadLoop` path, `openLiveLoopback`,
  `handleBearerLiveTranscription`, or `proxyLiveDuplex`.
- Not changing audio.cpp's idle timeout (not in this repo) or the client.

## Left to the builder

- The exact names/shape of the new fields (the per-turn PCM channel, the
  dial-outcome result type and its channel), the buffer capacity, and whether the
  pump and dial are one goroutine or two — as long as the run loop never blocks on
  the dial or on `pw.Write`, the single-owner invariant holds, and teardown is
  leak-free.
- The harness extension needed to test it (a way to delay the fake ASR child's
  acceptance / body read and to record the PCM it receives with ordering).
