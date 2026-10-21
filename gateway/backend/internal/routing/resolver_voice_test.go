// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The voice -> server filter (#251): a speech request that names a voice is
// restricted to the candidates whose cached voice list names it, but only when at
// least one candidate does -- otherwise it routes exactly as it does without a voice.
// These tests pin both halves, for a single model and for a group, and the one
// dedicated failure (ErrVoiceUnavailable).

var voiceNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// voiceReq is a speech request naming voice ("" names none). The session id lets a
// group test pre-seed the pin the request would be keyed under.
func voiceReq(model, voice string) inference.Request {
	req := speechReq(model)
	req.RequiredVoice = voice
	req.ClientSessionID = "s1"
	return req
}

var voicePrincipal = auth.Token{ID: "tok_voice", UserID: "usr_voice", Active: true}

// cacheVoices writes a mapping's voices cache the way the health loop does (scope "",
// a JSON array of names). A nil list writes NO row, which is "never cached"; an empty
// non-nil one writes the row "[]", which is "cached, offers nothing".
func cacheVoices(t *testing.T, mem *MemoryStore, mappingID string, voices []string) {
	t.Helper()
	if voices == nil {
		return
	}
	payload, err := json.Marshal(voices)
	if err != nil {
		t.Fatalf("marshal voices: %v", err)
	}
	if err := mem.UpsertModelMappingCache(context.Background(), ModelMappingCacheRow{MappingID: mappingID, CacheType: ModelMappingCacheVoices, Payload: string(payload), UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("UpsertModelMappingCache %s: %v", mappingID, err)
	}
}

// addSpeechMapping adds one server + audio application + (speech, yes) mapping
// serving gatewayName, so several of them are several CANDIDATES of one model.
func addSpeechMapping(t *testing.T, mem *MemoryStore, serverID, appID, mappingID, gatewayName string) {
	t.Helper()
	ctx := context.Background()
	if err := mem.CreateAIServer(ctx, AIServer{ID: serverID, Name: serverID, Domain: serverID + ".test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: voiceNow, UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", serverID, err)
	}
	if err := mem.CreateApplication(ctx, Application{ID: appID, ServerID: serverID, Type: ProviderAudioCpp, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: voiceNow, UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := mem.CreateMapping(ctx, ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gatewayName, AppModelName: gatewayName + "-up", Status: ServerStatusActive, CreatedAt: voiceNow, UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("CreateMapping %s: %v", mappingID, err)
	}
	if err := mem.UpsertTelemetry(ctx, ServerTelemetry{ServerID: serverID, ReportedAt: voiceNow, LatencyMS: 100, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("UpsertTelemetry %s: %v", serverID, err)
	}
	seedSpeechRows(t, mem, mappingID, []CapabilityRow{speechRow(CapabilityYes, voiceNow)})
}

// voiceModelStore is one gateway model, "tts", on two servers: srv_a (map_a) offering
// voicesA and srv_b (map_b) offering voicesB. A nil list is "never cached".
func voiceModelStore(t *testing.T, voicesA, voicesB []string) *MemoryStore {
	t.Helper()
	mem := NewMemoryStore()
	addSpeechMapping(t, mem, "srv_a", "app_a", "map_a", "tts")
	addSpeechMapping(t, mem, "srv_b", "app_b", "map_b", "tts")
	cacheVoices(t, mem, "map_a", voicesA)
	cacheVoices(t, mem, "map_b", voicesB)
	return mem
}

func voiceClock() func() time.Time { return func() time.Time { return voiceNow } }

func resolveVoice(t *testing.T, r *Resolver, req inference.Request) Target {
	t.Helper()
	target, err := r.Resolve(context.Background(), voicePrincipal, req)
	if err != nil {
		t.Fatalf("Resolve(%q, voice %q): %v", req.Model, req.RequiredVoice, err)
	}
	return target
}

// --- standard (single-model) path -------------------------------------------------

// A voice that some candidate lists restricts the model to the candidates that list it.
// The no-voice control serves srv_a, so the srv_b answer is the filter at work and not
// the order the store happens to return.
func TestVoiceStandardRestrictsToTheServersThatHaveIt(t *testing.T) {
	r := NewResolver(voiceModelStore(t, []string{"alba"}, []string{"bella"}), voiceClock(), nil)
	for _, tc := range []struct{ name, voice, wantServer string }{
		{"no voice is today's choice", "", "srv_a"},
		{"a voice only the second server has", "bella", "srv_b"},
		{"a voice only the first server has", "alba", "srv_a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveVoice(t, r, voiceReq("tts", tc.voice)); got.ServerID != tc.wantServer {
				t.Fatalf("server = %q, want %q", got.ServerID, tc.wantServer)
			}
		})
	}
}

// Several candidates that all list the voice stay candidates: the filter narrows to
// "those that have it", not to one.
func TestVoiceStandardKeepsEveryCandidateThatHasTheVoice(t *testing.T) {
	r := NewResolver(voiceModelStore(t, []string{"alba", "bella"}, []string{"bella"}), voiceClock(), nil)
	// srv_a (first in the store's order, equal score) still wins: both are in the pool.
	if got := resolveVoice(t, r, voiceReq("tts", "bella")); got.ServerID != "srv_a" {
		t.Fatalf("server = %q, want srv_a (both servers list bella)", got.ServerID)
	}
}

// THE regression (#251): a healthy sibling of the SAME model whose voices were NEVER
// cached (an older static audio.cpp with no voices endpoint, an agent too old to report
// them, or a model never observed loaded during a health tick) has UNKNOWN voice status,
// not absent. Unknown must stay eligible (fail open), so it remains a failover/load target
// and a voiced request is not 503'd when a healthy server could serve it. srv_a lists
// bella; srv_b is plain tts, healthy, and has no voices row at all.
func TestVoiceStandardUncachedSiblingStaysEligible(t *testing.T) {
	t.Run("the voice's server is down: the uncached sibling serves it", func(t *testing.T) {
		r := NewResolver(voiceModelStore(t, []string{"bella"}, nil), voiceClock(), fakeReachability{unreachable: map[string]bool{"app_a": true}})
		got, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts", "bella"))
		if err != nil {
			t.Fatalf("err = %v, want the uncached sibling to serve bella (unknown != absent)", err)
		}
		if got.ServerID != "srv_b" {
			t.Fatalf("server = %q, want srv_b", got.ServerID)
		}
	})
	t.Run("the voice's server is busy: the uncached sibling serves it", func(t *testing.T) {
		r := NewResolver(voiceModelStore(t, []string{"bella"}, nil), voiceClock(), nil)
		r.SetServerBusyChecker(fakeBusy{busy: map[string]bool{"srv_a": true}})
		if got := resolveVoice(t, r, voiceReq("tts", "bella")); got.ServerID != "srv_b" {
			t.Fatalf("server = %q, want srv_b (srv_a busy; the uncached sibling stays an eligible failover target)", got.ServerID)
		}
	})
	// The contrast that proves the filter still narrows on KNOWN absence: when the sibling
	// has a cached row that lists a DIFFERENT voice it is known-absent, so with the voice's
	// server down the request is voice_unavailable -- the sibling is known not to have it.
	t.Run("a known-absent sibling does not rescue: voice_unavailable", func(t *testing.T) {
		r := NewResolver(voiceModelStore(t, []string{"bella"}, []string{"ciao"}), voiceClock(), fakeReachability{unreachable: map[string]bool{"app_a": true}})
		_, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts", "bella"))
		if !errors.Is(err, ErrVoiceUnavailable) {
			t.Fatalf("err = %v, want ErrVoiceUnavailable (srv_b is known to lack bella)", err)
		}
	})
}

// FAIL-OPEN on UNKNOWN: when NO candidate is known to lack the voice -- nothing cached
// anywhere, or a mix of a voice-listing candidate and an uncached (unknown) sibling -- the
// voice narrows nothing and the request routes exactly as it does without a voice (an
// existing bearer client that sends a free-text voice to a fleet not yet voice-cached
// keeps working; the upstream stays the authority on the name). It is silent at the
// default log level too.
func TestVoiceStandardUnknownVoiceDoesNotFilter(t *testing.T) {
	for _, tc := range []struct {
		name             string
		voicesA, voicesB []string
		ask              string
	}{
		{name: "nothing cached anywhere", ask: "bella"},
		{name: "a voice one lists while the sibling is uncached", voicesA: []string{"bella"}, ask: "bella"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			r := NewResolver(voiceModelStore(t, tc.voicesA, tc.voicesB), voiceClock(), nil)
			want := resolveVoice(t, r, voiceReq("tts", "")) // today's behavior
			got := resolveVoice(t, r, voiceReq("tts", tc.ask))
			if got.ServerID != want.ServerID || got.RouteID != want.RouteID {
				t.Fatalf("target = %+v, want today's %+v", got, want)
			}
			if out := logs.String(); out != "" {
				t.Fatalf("log = %q, want nothing at the default level", out)
			}
		})
	}
}

// KNOWN-ABSENT everywhere is NOT fail-open: when every candidate has a cached voices row
// and none lists the voice (a cached-empty "[]" list included), the gateway KNOWS no
// server offers it, so the pool narrows to empty and the request is voice_unavailable --
// it is not routed to a server already known to reject it. Both servers are healthy here;
// the voice alone empties the pool.
func TestVoiceStandardKnownAbsentEverywhereIsVoiceUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name             string
		voicesA, voicesB []string
		ask              string
	}{
		{name: "both list other voices", voicesA: []string{"alba"}, voicesB: []string{"bella"}, ask: "nobody-has-this"},
		{name: "both cached empty", voicesA: []string{}, voicesB: []string{}, ask: "bella"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver(voiceModelStore(t, tc.voicesA, tc.voicesB), voiceClock(), nil)
			_, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts", tc.ask))
			if !errors.Is(err, ErrVoiceUnavailable) {
				t.Fatalf("err = %v, want ErrVoiceUnavailable (every candidate is known to lack %q)", err, tc.ask)
			}
			if errors.Is(err, ErrNoHealthyHost) || errors.Is(err, ErrNoModelRoute) {
				t.Fatalf("err = %v also matches a generic routing sentinel", err)
			}
		})
	}
}

// An undecodable voices row is treated as UNKNOWN voice status and kept eligible (fail
// open, logged at Warn): its mapping stays a candidate instead of being dropped. map_a is
// known-absent for bella and map_b's row is corrupt, so the corrupt-but-eligible map_b is
// exactly where bella routes -- and the warning names it.
func TestVoiceStandardUndecodableRowStaysEligible(t *testing.T) {
	logs := captureSlog(t)
	mem := voiceModelStore(t, []string{"alba"}, []string{"bella"})
	if err := mem.UpsertModelMappingCache(context.Background(), ModelMappingCacheRow{MappingID: "map_b", CacheType: ModelMappingCacheVoices, Payload: `{not json`, UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}
	r := NewResolver(mem, voiceClock(), nil)
	if got := resolveVoice(t, r, voiceReq("tts", "bella")); got.ServerID != "srv_b" {
		t.Fatalf("server = %q, want srv_b (its row is undecodable, so it is kept eligible)", got.ServerID)
	}
	if out := logs.String(); !strings.Contains(out, "undecodable") || !strings.Contains(out, "mapping_id=map_b") {
		t.Fatalf("log = %q, want a warning naming the undecodable row of map_b", out)
	}
}

// captureSlog swaps the process-global slog default for a buffer at INFO, the level a
// real gateway runs at, so a Warn is observable and a Debug is not.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A voice cached on a mapping whose model is NOT a plain tts one any more (it became a
// clone model after the cache was written) is not a known voice: the same read-side
// verdict gate the voices endpoint applies. bella is cached only on map_b, so once map_b
// is a clone the sole tts candidate (map_a, cached ["alba"]) is KNOWN to lack bella and
// the request is voice_unavailable; were the stale clone row honored, bella would instead
// be forced onto srv_b.
func TestVoiceStandardIgnoresAVoiceCachedOnANonTTSMapping(t *testing.T) {
	mem := voiceModelStore(t, []string{"alba"}, []string{"bella"})
	if err := mem.UpsertMappingCapabilities(context.Background(), "map_b", []CapabilityRow{{Capability: CapabilitySpeechClone, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: voiceNow}}); err != nil {
		t.Fatalf("flip map_b to a clone model: %v", err)
	}
	r := NewResolver(mem, voiceClock(), nil)
	if _, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts", "bella")); !errors.Is(err, ErrVoiceUnavailable) {
		t.Fatalf("err = %v, want ErrVoiceUnavailable: bella is only cached on a clone mapping, so no tts candidate lists it", err)
	}
}

// The voice only exists on a server the principal is not provisioned for: it is not a
// candidate at all, so the filter sees only the reachable pool. srv_a is reachable and
// KNOWN to lack bella, so the request is voice_unavailable (the unprovisioned server's
// bella is never considered). Once that server is provisioned too the pool narrows to it.
func TestVoiceStandardOnlyConsidersWhatTheCallerMayReach(t *testing.T) {
	r := NewResolver(voiceModelStore(t, []string{"alba"}, []string{"bella"}), voiceClock(), nil)
	r.SetProvisioningGate(fakeGate{allow: map[string]bool{"srv_a": true}})
	if _, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts", "bella")); !errors.Is(err, ErrVoiceUnavailable) {
		t.Fatalf("err = %v, want ErrVoiceUnavailable (only srv_a is reachable and it is known to lack bella)", err)
	}
	// And the provisioned pool narrows to the voice's server once it is reachable.
	r.SetProvisioningGate(fakeGate{allow: map[string]bool{"srv_a": true, "srv_b": true}})
	if got := resolveVoice(t, r, voiceReq("tts", "bella")); got.ServerID != "srv_b" {
		t.Fatalf("server = %q, want srv_b once the voice's server is provisioned", got.ServerID)
	}
}

// The voice's only server is down: the voice NARROWED the pool to a non-empty set none
// of which is usable, so the answer is the dedicated sentinel and not ErrNoHealthyHost,
// even though the OTHER server is perfectly healthy (it just lacks the voice). The
// control proves the fixture: the same request without that voice is served.
func TestVoiceStandardVoiceOnADownServerIsVoiceUnavailable(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(t *testing.T, mem *MemoryStore, r *Resolver){
		"unreachable": func(t *testing.T, mem *MemoryStore, r *Resolver) {
			r.checker = fakeReachability{unreachable: map[string]bool{"app_b": true}}
		},
		"busy": func(t *testing.T, mem *MemoryStore, r *Resolver) {
			r.SetServerBusyChecker(fakeBusy{busy: map[string]bool{"srv_b": true}})
		},
		"unhealthy": func(t *testing.T, mem *MemoryStore, r *Resolver) {
			if err := mem.SetServerHealth(ctx, "srv_b", HealthUnhealthy); err != nil {
				t.Fatalf("SetServerHealth: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			mem := voiceModelStore(t, []string{"alba"}, []string{"bella"})
			r := NewResolver(mem, voiceClock(), nil)
			setup(t, mem, r)

			_, err := r.Resolve(ctx, voicePrincipal, voiceReq("tts", "bella"))
			if !errors.Is(err, ErrVoiceUnavailable) {
				t.Fatalf("err = %v, want ErrVoiceUnavailable", err)
			}
			if errors.Is(err, ErrNoHealthyHost) || errors.Is(err, ErrNoModelRoute) {
				t.Fatalf("err = %v also matches a generic routing sentinel", err)
			}
			if got := resolveVoice(t, r, voiceReq("tts", "alba")); got.ServerID != "srv_a" {
				t.Fatalf("control: alba served by %q, want srv_a", got.ServerID)
			}
			if got := resolveVoice(t, r, voiceReq("tts", "")); got.ServerID != "srv_a" {
				t.Fatalf("control: no voice served by %q, want srv_a", got.ServerID)
			}
		})
	}
}

// The sentinel is about the VOICE: when every candidate lists it the voice narrowed
// nothing, and a model that is down is down. So both servers listing the voice and both
// unreachable, or a lone mapping listing it and unreachable, stay ErrNoHealthyHost.
func TestVoiceStandardDownServersThatAllHaveTheVoiceStayNoHealthyHost(t *testing.T) {
	t.Run("two servers, both list it, both down", func(t *testing.T) {
		r := NewResolver(voiceModelStore(t, []string{"bella"}, []string{"bella"}), voiceClock(), fakeReachability{unreachable: map[string]bool{"app_a": true, "app_b": true}})
		_, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts", "bella"))
		if !errors.Is(err, ErrNoHealthyHost) || errors.Is(err, ErrVoiceUnavailable) {
			t.Fatalf("err = %v, want ErrNoHealthyHost and not ErrVoiceUnavailable", err)
		}
	})
	t.Run("a single mapping that lists it, down", func(t *testing.T) {
		mem := NewMemoryStore()
		addSpeechMapping(t, mem, "srv_a", "app_a", "map_a", "tts")
		cacheVoices(t, mem, "map_a", []string{"bella"})
		r := NewResolver(mem, voiceClock(), fakeReachability{unreachable: map[string]bool{"app_a": true}})
		_, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts", "bella"))
		if !errors.Is(err, ErrNoHealthyHost) || errors.Is(err, ErrVoiceUnavailable) {
			t.Fatalf("err = %v, want ErrNoHealthyHost and not ErrVoiceUnavailable", err)
		}
	})
}

// A request that names no voice never reads the cache: the no-op invariant (a resolver
// serving chat, and speech without a voice, costs exactly what it did before the filter).
func TestVoiceNoVoiceNeverReadsTheVoicesCache(t *testing.T) {
	counting := &voiceCacheStore{resolverStore: voiceModelStore(t, []string{"alba"}, []string{"bella"})}
	r := NewResolver(counting, voiceClock(), nil)
	r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))

	resolveVoice(t, r, voiceReq("tts", ""))
	if counting.cacheReads != 0 {
		t.Fatalf("cache reads for a standard request without a voice = %d, want 0", counting.cacheReads)
	}
	resolveVoice(t, r, voiceReq("tts", "bella"))
	if counting.cacheReads != 1 {
		t.Fatalf("cache reads for a standard request with a voice = %d, want exactly 1 (one bulk read)", counting.cacheReads)
	}
}

// voiceCacheStore counts, and can fail, the voices cache read.
type voiceCacheStore struct {
	resolverStore
	cacheReads int
	failCache  error
}

func (s *voiceCacheStore) ModelMappingCacheForMappings(ctx context.Context, ids []string, cacheType string) (map[string][]ModelMappingCacheRow, error) {
	s.cacheReads++
	if s.failCache != nil {
		return nil, s.failCache
	}
	return s.resolverStore.ModelMappingCacheForMappings(ctx, ids, cacheType)
}

// FAIL-OPEN on the voices CACHE read: the cache exists only for this preference, so when
// it cannot be read there is no voice information, which is the "nothing lists the voice"
// case -- the request routes as it would without a voice instead of failing. The read IS
// attempted (cacheReads == 1), so this is the error being absorbed and not the read being
// skipped, and the outage is not silent (a Warn).
func TestVoiceCacheReadFailureFailsOpen(t *testing.T) {
	boom := errors.New("cache store down")
	for _, tc := range []struct {
		name   string
		model  string
		newMem func(t *testing.T) *MemoryStore
		setup  func(r *Resolver)
	}{
		{"standard", "tts", func(t *testing.T) *MemoryStore { return voiceModelStore(t, []string{"alba"}, []string{"bella"}) }, func(*Resolver) {}},
		{"group", "tts-group", func(t *testing.T) *MemoryStore { return voiceGroupStore(t, []string{"alba"}, []string{"bella"}) }, func(r *Resolver) { r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"})) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			// Today's choice, from a healthy store, with no voice at all.
			control := NewResolver(tc.newMem(t), voiceClock(), nil)
			tc.setup(control)
			want := resolveVoice(t, control, voiceReq(tc.model, ""))

			broken := &voiceCacheStore{resolverStore: tc.newMem(t), failCache: boom}
			r := NewResolver(broken, voiceClock(), nil)
			tc.setup(r)
			// bella is only listed by srv_b; were the filter applied the answer would be srv_b.
			got, err := r.Resolve(context.Background(), voicePrincipal, voiceReq(tc.model, "bella"))
			if err != nil {
				t.Fatalf("err = %v, want the request routed as without a voice", err)
			}
			if got.ServerID != want.ServerID || got.RouteID != want.RouteID {
				t.Fatalf("target = %+v, want today's %+v", got, want)
			}
			if broken.cacheReads != 1 {
				t.Fatalf("cache reads = %d, want the one failing read attempted", broken.cacheReads)
			}
			if out := logs.String(); !strings.Contains(out, "voices cache could not be read") || !strings.Contains(out, "cache store down") {
				t.Fatalf("log = %q, want a warning naming the failed cache read", out)
			}
		})
	}

	// Not narrowed means not refused: with srv_b (the only server that would list bella)
	// down, the broken cache must not turn into voice_unavailable either -- it is served by
	// the healthy server, as without a voice.
	t.Run("never voice_unavailable", func(t *testing.T) {
		captureSlog(t)
		store := &voiceCacheStore{resolverStore: voiceModelStore(t, []string{"alba"}, []string{"bella"}), failCache: boom}
		r := NewResolver(store, voiceClock(), fakeReachability{unreachable: map[string]bool{"app_b": true}})
		if got := resolveVoice(t, r, voiceReq("tts", "bella")); got.ServerID != "srv_a" {
			t.Fatalf("server = %q, want srv_a", got.ServerID)
		}
	})
}

// The capability VERDICT read is the other half of the filter and keeps the capability
// gate's posture: it reads the rows filterCapable has just read and refuses on, so its
// failure is returned, not absorbed. voiceVerdictStore lets filterCapable's own reads
// through and fails the filter's.
func TestVoiceVerdictReadFailureIsReturned(t *testing.T) {
	boom := errors.New("verdict store down")
	t.Run("standard", func(t *testing.T) {
		store := &voiceVerdictStore{resolverStore: voiceModelStore(t, []string{"alba"}, []string{"bella"}), okCalls: 1, err: boom} // filterCapable reads once
		r := NewResolver(store, voiceClock(), nil)
		if _, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts", "bella")); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the store's error", err)
		}
		if store.calls != 2 {
			t.Fatalf("verdict reads = %d, want 2 (filterCapable's, then the voice filter's failing one)", store.calls)
		}
	})
	t.Run("group", func(t *testing.T) {
		store := &voiceVerdictStore{resolverStore: voiceGroupStore(t, []string{"alba"}, []string{"bella"}), okCalls: 2, err: boom} // one filterCapable read per member
		r := NewResolver(store, voiceClock(), nil)
		r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
		if _, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts-group", "bella")); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the store's error", err)
		}
		if store.calls != 3 {
			t.Fatalf("verdict reads = %d, want 3 (one per member, then the voice filter's failing one)", store.calls)
		}
	})
	// A request without a voice never makes the filter's read, so the same store is fine.
	t.Run("no voice", func(t *testing.T) {
		store := &voiceVerdictStore{resolverStore: voiceModelStore(t, []string{"alba"}, []string{"bella"}), okCalls: 1, err: boom}
		r := NewResolver(store, voiceClock(), nil)
		resolveVoice(t, r, voiceReq("tts", ""))
		if store.calls != 1 {
			t.Fatalf("verdict reads = %d, want only filterCapable's", store.calls)
		}
	})
}

// voiceVerdictStore passes the first okCalls capability-verdict reads through and fails
// every later one.
type voiceVerdictStore struct {
	resolverStore
	okCalls int
	calls   int
	err     error
}

func (s *voiceVerdictStore) MappingCapabilitiesForMappings(ctx context.Context, ids []string) (map[string][]CapabilityRow, error) {
	s.calls++
	if s.calls > s.okCalls {
		return nil, s.err
	}
	return s.resolverStore.MappingCapabilitiesForMappings(ctx, ids)
}

// Capacity: the admission queue waits on the voice's servers only, because the pool is
// narrowed before the queue's server list is built. Both servers are at their cap; the
// request for bella may only be served by srv_b, so it is srv_b's slot it waits for.
func TestVoiceStandardQueuesOnTheVoicesServersOnly(t *testing.T) {
	ctx := context.Background()
	mem := voiceModelStore(t, []string{"alba"}, []string{"bella"})
	for _, id := range []string{"map_a", "map_b"} {
		m, err := mem.MappingByID(ctx, id)
		if err != nil {
			t.Fatalf("MappingByID %s: %v", id, err)
		}
		m.MaxConcurrency = 1
		if err := mem.UpdateMapping(ctx, m); err != nil {
			t.Fatalf("UpdateMapping %s: %v", id, err)
		}
	}
	r := NewResolver(mem, voiceClock(), nil)
	r.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_a": 1, "srv_b": 1}}, 30*time.Second)
	adm := &fakeAdmission{onWait: func(int) error { return ErrAdmissionQueueTimeout }}
	r.SetAdmissionController(adm)

	_, err := r.Resolve(ctx, voicePrincipal, voiceReq("tts", "bella"))
	if !errors.Is(err, ErrAdmissionQueueTimeout) {
		t.Fatalf("err = %v, want the admission timeout (a queue outcome is not voice_unavailable)", err)
	}
	if want := []string{"srv_b"}; !reflect.DeepEqual(adm.gotServers, want) {
		t.Fatalf("queued on %v, want only the voice's server %v", adm.gotServers, want)
	}
}

// A voice-carrying request does not take a sticky pin: a pin is keyed on the coarse
// flavor and no speech request ever writes one (capability-carrying requests are
// guarded), so none can exist today; if one did it would bypass the filter, which is
// exactly what skipping it prevents. Here a pin to srv_a is planted by hand.
func TestVoiceStandardIgnoresAStickyPin(t *testing.T) {
	ctx := context.Background()
	mem := voiceModelStore(t, []string{"alba"}, []string{"bella"})
	key := AffinityKey{APITokenID: voicePrincipal.ID, Model: "tts", APIFlavor: APIFlavorOpenAIAudio, SessionID: "s1"}
	if err := mem.UpsertAffinity(ctx, RouteAffinity{ID: affinityID(key), APITokenID: voicePrincipal.ID, UserID: voicePrincipal.UserID, Model: "tts", APIFlavor: APIFlavorOpenAIAudio, SessionID: "s1", ApplicationID: "app_a", ServerID: "srv_a", ExpiresAt: voiceNow.Add(time.Hour), LastUsedAt: voiceNow, CreatedAt: voiceNow, UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("plant pin: %v", err)
	}
	r := NewResolver(mem, voiceClock(), nil)
	if got := resolveVoice(t, r, voiceReq("tts", "bella")); got.ServerID != "srv_b" {
		t.Fatalf("server = %q, want srv_b: the pin to srv_a must not outrank a known voice", got.ServerID)
	}
}

// An explicit server override names ONE server and bypasses candidate selection
// entirely: the voice has nothing to choose between and does not veto the override.
func TestVoiceServerOverrideIsNotFilteredByTheVoice(t *testing.T) {
	r := NewResolver(voiceModelStore(t, []string{"alba"}, []string{"bella"}), voiceClock(), nil)
	req := voiceReq("tts", "bella")
	req.ServerOverrideID = "srv_a"
	if got := resolveVoice(t, r, req); got.ServerID != "srv_a" {
		t.Fatalf("server = %q, want the overridden srv_a", got.ServerID)
	}
}

// --- group path --------------------------------------------------------------------

// voiceGroup is "tts-group" over coder-a (priority 0) then coder-b (priority 1).
func voiceGroup(policy GroupPolicy) *fakeGroups {
	return &fakeGroups{groups: map[string]fakeGroup{"tts-group": {policy: policy, members: []GroupMember{
		{ID: "gm_a", GroupID: "grp_voice", MemberGatewayName: "coder-a", Priority: 0},
		{ID: "gm_b", GroupID: "grp_voice", MemberGatewayName: "coder-b", Priority: 1},
	}}}}
}

// voiceGroupStore is the seeded two-member speech store with a (speech, yes) verdict on
// both mappings and the given voice lists (nil = never cached).
func voiceGroupStore(t *testing.T, voicesA, voicesB []string) *MemoryStore {
	t.Helper()
	mem := seededSpeechStore(t, voiceNow)
	for id, voices := range map[string][]string{"map_a": voicesA, "map_b": voicesB} {
		seedSpeechRows(t, mem, id, []CapabilityRow{speechRow(CapabilityYes, voiceNow)})
		cacheVoices(t, mem, id, voices)
	}
	return mem
}

func setGenTPS(t *testing.T, mem *MemoryStore, mappingID string, tps float64) {
	t.Helper()
	m, err := mem.MappingByID(context.Background(), mappingID)
	if err != nil {
		t.Fatalf("MappingByID %s: %v", mappingID, err)
	}
	m.GenTokensPerSecond = tps
	if err := mem.UpdateMapping(context.Background(), m); err != nil {
		t.Fatalf("UpdateMapping %s: %v", mappingID, err)
	}
}

// THE group case: only member B has the voice, A is first in priority order and healthy.
// A per-member filter would leave A fully eligible and the walk would serve it; the
// group-level pre-pass narrows the flattened group first, so B is the only member the
// walk sees. The controls show the order really is A-first without the voice.
func TestVoiceGroupPrePassNarrowsAcrossMembers(t *testing.T) {
	r := NewResolver(voiceGroupStore(t, []string{"alba"}, []string{"bella"}), voiceClock(), nil)
	r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
	for _, tc := range []struct{ name, voice, wantServer string }{
		{"no voice keeps the priority order", "", "srv_a"},
		{"a voice only the second member has", "bella", "srv_b"},
		{"a voice only the first member has", "alba", "srv_a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveVoice(t, r, voiceReq("tts-group", tc.voice)); got.ServerID != tc.wantServer {
				t.Fatalf("server = %q, want %q", got.ServerID, tc.wantServer)
			}
		})
	}
}

// Fail-open for a group: with nothing cached anywhere the group routes as it always did.
func TestVoiceGroupWithNothingCachedRoutesAsToday(t *testing.T) {
	r := NewResolver(voiceGroupStore(t, nil, nil), voiceClock(), nil)
	r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
	if got := resolveVoice(t, r, voiceReq("tts-group", "bella")); got.ServerID != "srv_a" {
		t.Fatalf("server = %q, want srv_a (nothing cached, so no filter)", got.ServerID)
	}
}

// The regression (#251) on the group path: a member of UNKNOWN voice status (never
// cached) stays eligible across the group pre-pass, so it is a live failover target when
// the voice's member is down. coder-a lists bella; coder-b is healthy but never cached.
func TestVoiceGroupUncachedSiblingStaysEligible(t *testing.T) {
	t.Run("the voice's member is down: the uncached member serves it", func(t *testing.T) {
		r := NewResolver(voiceGroupStore(t, []string{"bella"}, nil), voiceClock(), fakeReachability{unreachable: map[string]bool{"app_a": true}})
		r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
		got, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts-group", "bella"))
		if err != nil {
			t.Fatalf("err = %v, want the uncached member to serve bella (unknown != absent)", err)
		}
		if got.ServerID != "srv_b" {
			t.Fatalf("server = %q, want srv_b", got.ServerID)
		}
	})
	t.Run("a known-absent member does not rescue: voice_unavailable", func(t *testing.T) {
		r := NewResolver(voiceGroupStore(t, []string{"bella"}, []string{"ciao"}), voiceClock(), fakeReachability{unreachable: map[string]bool{"app_a": true}})
		r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
		_, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts-group", "bella"))
		if !errors.Is(err, ErrVoiceUnavailable) {
			t.Fatalf("err = %v, want ErrVoiceUnavailable (coder-b is known to lack bella)", err)
		}
	})
}

// The voice only exists behind a server the principal is not provisioned for: not in the
// reachable pool. The one reachable member (coder-a) is known to lack bella, so no
// reachable member offers the voice and the group has no route for it (ErrNoModelRoute --
// the group path reads a member whose every candidate the voice excludes as memberNoMapping,
// NOT as a real-but-gated member, so the standard path's ErrVoiceUnavailable does not apply
// here). Once the voice's member is provisioned the group narrows to it.
func TestVoiceGroupOnlyConsidersWhatTheCallerMayReach(t *testing.T) {
	r := NewResolver(voiceGroupStore(t, []string{"alba"}, []string{"bella"}), voiceClock(), nil)
	r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
	r.SetProvisioningGate(fakeGate{allow: map[string]bool{"srv_a": true}})
	if _, err := r.Resolve(context.Background(), voicePrincipal, voiceReq("tts-group", "bella")); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("err = %v, want ErrNoModelRoute (bella is behind an unprovisioned member)", err)
	}
	r.SetProvisioningGate(fakeGate{allow: map[string]bool{"srv_a": true, "srv_b": true}})
	if got := resolveVoice(t, r, voiceReq("tts-group", "bella")); got.ServerID != "srv_b" {
		t.Fatalf("server = %q, want srv_b once the voice's member is provisioned", got.ServerID)
	}
}

// One member with two candidates, only one of which lists the voice: the restriction is
// per MAPPING over the flattened group, not per member name.
func TestVoiceGroupNarrowsWithinAMemberToTheMappingThatHasIt(t *testing.T) {
	mem := voiceGroupStore(t, []string{"alba"}, []string{"alba"})
	// A third server also serves coder-a, and it alone lists bella.
	addSpeechMapping(t, mem, "srv_c", "app_c", "map_c", "coder-a")
	cacheVoices(t, mem, "map_c", []string{"bella"})
	r := NewResolver(mem, voiceClock(), nil)
	r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
	if got := resolveVoice(t, r, voiceReq("tts-group", "bella")); got.ServerID != "srv_c" {
		t.Fatalf("server = %q, want srv_c: the only mapping in the group that lists bella", got.ServerID)
	}
}

// member_order=speed ranks members on their voice-bearing candidates only: A is faster,
// but B is the only member with the voice.
func TestVoiceGroupSpeedOrderRanksOnlyTheVoiceMembers(t *testing.T) {
	mem := voiceGroupStore(t, []string{"alba"}, []string{"bella"})
	setGenTPS(t, mem, "map_a", 50)
	setGenTPS(t, mem, "map_b", 5)
	r := NewResolver(mem, voiceClock(), nil)
	r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky", MemberOrder: MemberOrderSpeed}))
	if got := resolveVoice(t, r, voiceReq("tts-group", "")); got.ServerID != "srv_a" {
		t.Fatalf("control: no voice served by %q, want the faster srv_a", got.ServerID)
	}
	if got := resolveVoice(t, r, voiceReq("tts-group", "bella")); got.ServerID != "srv_b" {
		t.Fatalf("server = %q, want srv_b (the only member with the voice, however slow)", got.ServerID)
	}
}

// The speed ranking scores a member on the candidates the voice leaves it, not on the
// ones it excluded. coder-a has a fast mapping without bella and a slow one with it;
// coder-b has a medium mapping with it. Ranked on its voice candidate coder-a is the
// SLOWEST member for bella (5 < 20) and coder-b goes first; ranked on its fast
// voice-less mapping it would lead (50) and the walk would serve its slow candidate.
func TestVoiceGroupSpeedOrderScoresMembersOnTheirVoiceCandidates(t *testing.T) {
	mem := voiceGroupStore(t, []string{"alba"}, []string{"bella"})
	addSpeechMapping(t, mem, "srv_c", "app_c", "map_c", "coder-a")
	cacheVoices(t, mem, "map_c", []string{"bella"})
	setGenTPS(t, mem, "map_a", 50)
	setGenTPS(t, mem, "map_c", 5)
	setGenTPS(t, mem, "map_b", 20)
	r := NewResolver(mem, voiceClock(), nil)
	r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky", MemberOrder: MemberOrderSpeed}))

	if got := resolveVoice(t, r, voiceReq("tts-group", "")); got.ServerID != "srv_a" {
		t.Fatalf("control: no voice served by %q, want the fastest candidate srv_a", got.ServerID)
	}
	if got := resolveVoice(t, r, voiceReq("tts-group", "bella")); got.ServerID != "srv_b" {
		t.Fatalf("server = %q, want srv_b: ranked on the voice-bearing candidates coder-b (20) outruns coder-a (5)", got.ServerID)
	}
}

// A sticky pin to a member that lacks the voice is not kept for a request that needs it.
// (No speech request writes a pin today, so it is planted by hand.)
func TestVoiceGroupDoesNotKeepAPinnedMemberWithoutTheVoice(t *testing.T) {
	ctx := context.Background()
	mem := voiceGroupStore(t, []string{"alba"}, []string{"bella"})
	key := AffinityKey{APITokenID: voicePrincipal.ID, Model: "tts-group", APIFlavor: APIFlavorOpenAIAudio, SessionID: "s1"}
	if err := mem.UpsertAffinity(ctx, RouteAffinity{ID: affinityID(key), APITokenID: voicePrincipal.ID, UserID: voicePrincipal.UserID, Model: "tts-group", ResolvedModel: "coder-a", APIFlavor: APIFlavorOpenAIAudio, SessionID: "s1", ApplicationID: "app_a", ServerID: "srv_a", ExpiresAt: voiceNow.Add(time.Hour), LastUsedAt: voiceNow, CreatedAt: voiceNow, UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("plant pin: %v", err)
	}
	r := NewResolver(mem, voiceClock(), nil)
	r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
	if got := resolveVoice(t, r, voiceReq("tts-group", "alba")); got.ServerID != "srv_a" {
		t.Fatalf("control: the pin to srv_a serves a voice srv_a has, got %q", got.ServerID)
	}
	if got := resolveVoice(t, r, voiceReq("tts-group", "bella")); got.ServerID != "srv_b" {
		t.Fatalf("server = %q, want srv_b: the pinned member lacks the voice", got.ServerID)
	}
}

// climb_up never climbs to a better-ranked member that lacks the voice: the climb asks
// "which better member is available", and that is the voice-bearing set.
func TestVoiceGroupClimbUpSkipsAMemberWithoutTheVoice(t *testing.T) {
	ctx := context.Background()
	mem := voiceGroupStore(t, []string{"alba"}, []string{"bella"})
	key := AffinityKey{APITokenID: voicePrincipal.ID, Model: "tts-group", APIFlavor: APIFlavorOpenAIAudio, SessionID: "s1"}
	if err := mem.UpsertAffinity(ctx, RouteAffinity{ID: affinityID(key), APITokenID: voicePrincipal.ID, UserID: voicePrincipal.UserID, Model: "tts-group", ResolvedModel: "coder-b", APIFlavor: APIFlavorOpenAIAudio, SessionID: "s1", ApplicationID: "app_b", ServerID: "srv_b", ExpiresAt: voiceNow.Add(time.Hour), LastUsedAt: voiceNow, CreatedAt: voiceNow, UpdatedAt: voiceNow}); err != nil {
		t.Fatalf("plant pin: %v", err)
	}
	r := NewResolver(mem, voiceClock(), nil)
	r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: modeClimbUp}))
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_a": {"coder-a-up"}}}) // the better member is loaded: a free climb
	warmer := &fakeWarmer{}
	r.SetModelWarmer(warmer)

	if got := resolveVoice(t, r, voiceReq("tts-group", "")); got.ServerID != "srv_a" {
		t.Fatalf("control: with no voice the session climbs to the loaded srv_a, got %q", got.ServerID)
	}
	if got := resolveVoice(t, r, voiceReq("tts-group", "bella")); got.ServerID != "srv_b" {
		t.Fatalf("server = %q, want srv_b: the better member lacks the voice, so there is nothing to climb to", got.ServerID)
	}
	if len(warmer.warmed) != 0 {
		t.Fatalf("warmed = %v: a member the voice excludes must not be warmed either", warmer.warmed)
	}
}

// The relaxation ladder relaxes loaded_only and the speed floor and NEVER the voice.
// A is the loaded member, first in priority and healthy; B has the voice but is cold.
// The loaded-only attempt finds nothing usable, the relaxed attempt serves B -- and at no
// point is A, which lacks the voice, a candidate. Then with B down as well the group is
// refused with ErrVoiceUnavailable instead of falling back to A.
func TestVoiceGroupRestrictionIsNotRelaxedByTheLadder(t *testing.T) {
	ctx := context.Background()
	t.Run("loaded_only is relaxed, the voice is not", func(t *testing.T) {
		mem := voiceGroupStore(t, []string{"alba"}, []string{"bella"})
		r := NewResolver(mem, voiceClock(), nil)
		r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky", LoadedOnly: true}))
		r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_a": {"coder-a-up"}}}) // only A is loaded

		if got := resolveVoice(t, r, voiceReq("tts-group", "")); got.ServerID != "srv_a" {
			t.Fatalf("control: no voice served by %q, want the loaded srv_a", got.ServerID)
		}
		if got := resolveVoice(t, r, voiceReq("tts-group", "bella")); got.ServerID != "srv_b" {
			t.Fatalf("server = %q, want srv_b (loaded-only relaxed to reach the voice's cold member)", got.ServerID)
		}
	})
	t.Run("the speed floor is relaxed, the voice is not", func(t *testing.T) {
		mem := voiceGroupStore(t, []string{"alba"}, []string{"bella"})
		setGenTPS(t, mem, "map_a", 50)
		setGenTPS(t, mem, "map_b", 5) // under the 20 tok/s floor
		r := NewResolver(mem, voiceClock(), nil)
		r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky", MinTokensPerSecond: 20, MinSpeedFallback: MinSpeedFallbackIgnore}))

		if got := resolveVoice(t, r, voiceReq("tts-group", "")); got.ServerID != "srv_a" {
			t.Fatalf("control: no voice served by %q, want the fast srv_a", got.ServerID)
		}
		if got := resolveVoice(t, r, voiceReq("tts-group", "bella")); got.ServerID != "srv_b" {
			t.Fatalf("server = %q, want srv_b (floor relaxed to reach the voice's slow member)", got.ServerID)
		}
	})
	t.Run("with the voice's member down it is refused, never served by the member without it", func(t *testing.T) {
		mem := voiceGroupStore(t, []string{"alba"}, []string{"bella"})
		r := NewResolver(mem, voiceClock(), fakeReachability{unreachable: map[string]bool{"app_b": true}})
		r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky", LoadedOnly: true}))
		r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_a": {"coder-a-up"}}})

		target, err := r.Resolve(ctx, voicePrincipal, voiceReq("tts-group", "bella"))
		if !errors.Is(err, ErrVoiceUnavailable) {
			t.Fatalf("err = %v (target %+v), want ErrVoiceUnavailable", err, target)
		}
	})
}

// Group failure mapping: the voice's member being down is ErrVoiceUnavailable (the other
// member is healthy and merely lacks the voice); when every member lists the voice the
// voice narrowed nothing and a dead group stays ErrNoHealthyHost.
func TestVoiceGroupFailureMapping(t *testing.T) {
	ctx := context.Background()
	t.Run("the voice's only member is down", func(t *testing.T) {
		r := NewResolver(voiceGroupStore(t, []string{"alba"}, []string{"bella"}), voiceClock(), fakeReachability{unreachable: map[string]bool{"app_b": true}})
		r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
		_, err := r.Resolve(ctx, voicePrincipal, voiceReq("tts-group", "bella"))
		if !errors.Is(err, ErrVoiceUnavailable) || errors.Is(err, ErrNoHealthyHost) {
			t.Fatalf("err = %v, want ErrVoiceUnavailable", err)
		}
	})
	t.Run("the voice's only member is busy", func(t *testing.T) {
		r := NewResolver(voiceGroupStore(t, []string{"alba"}, []string{"bella"}), voiceClock(), nil)
		r.SetServerBusyChecker(fakeBusy{busy: map[string]bool{"srv_b": true}})
		r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
		if _, err := r.Resolve(ctx, voicePrincipal, voiceReq("tts-group", "bella")); !errors.Is(err, ErrVoiceUnavailable) {
			t.Fatalf("err = %v, want ErrVoiceUnavailable", err)
		}
	})
	t.Run("every member lists the voice and all are down", func(t *testing.T) {
		r := NewResolver(voiceGroupStore(t, []string{"bella"}, []string{"bella"}), voiceClock(), fakeReachability{unreachable: map[string]bool{"app_a": true, "app_b": true}})
		r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
		_, err := r.Resolve(ctx, voicePrincipal, voiceReq("tts-group", "bella"))
		if !errors.Is(err, ErrNoHealthyHost) || errors.Is(err, ErrVoiceUnavailable) {
			t.Fatalf("err = %v, want ErrNoHealthyHost and not ErrVoiceUnavailable", err)
		}
	})
}

// A request without a voice reads no voices cache on the group path either.
func TestVoiceGroupNoVoiceNeverReadsTheVoicesCache(t *testing.T) {
	counting := &voiceCacheStore{resolverStore: voiceGroupStore(t, []string{"alba"}, []string{"bella"})}
	r := NewResolver(counting, voiceClock(), nil)
	r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky", MemberOrder: MemberOrderSpeed}))
	resolveVoice(t, r, voiceReq("tts-group", ""))
	if counting.cacheReads != 0 {
		t.Fatalf("cache reads for a group request without a voice = %d, want 0", counting.cacheReads)
	}
	resolveVoice(t, r, voiceReq("tts-group", "bella"))
	if counting.cacheReads != 1 {
		t.Fatalf("cache reads for a group request with a voice = %d, want exactly 1: the pre-pass reads once, not per attempt, member or walk step", counting.cacheReads)
	}
}

// The sentinel's text is API surface (the gateway maps it to a response code).
func TestErrVoiceUnavailableText(t *testing.T) {
	if got, want := ErrVoiceUnavailable.Error(), "routing.voice_unavailable"; got != want {
		t.Fatalf("ErrVoiceUnavailable = %q, want %q", got, want)
	}
}
