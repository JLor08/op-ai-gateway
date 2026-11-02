// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// embeddingRow and rerankRow are one embedding / rerank capability row of the
// given verdict, the twins of speechRow and transcriptionRow.
func embeddingRow(verdict string) routing.CapabilityRow {
	return routing.CapabilityRow{Capability: routing.CapabilityEmbedding, Verdict: verdict, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime}
}

func rerankRow(verdict string) routing.CapabilityRow {
	return routing.CapabilityRow{Capability: routing.CapabilityRerank, Verdict: verdict, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime}
}

// The two capability names are part of the stored vocabulary: they are written
// verbatim into model_mapping_capabilities by the operator's verdict controls,
// so the strings are a persisted contract, not an implementation detail.
func TestEmbeddingAndRerankCapabilityNames(t *testing.T) {
	if routing.CapabilityEmbedding != "embedding" {
		t.Errorf("CapabilityEmbedding = %q, want %q", routing.CapabilityEmbedding, "embedding")
	}
	if routing.CapabilityRerank != "rerank" {
		t.Errorf("CapabilityRerank = %q, want %q", routing.CapabilityRerank, "rerank")
	}
}

// embeddingRerankOfferingStore seeds one model per shape the two folds tell
// apart, plus two groups. Neither capability has a gateway endpoint, so there is
// no flavor half to the fold (compare transcriptionOfferingStore): the verdict
// row alone decides, and an application's flavors play no part.
func embeddingRerankOfferingStore(t *testing.T) *routing.MemoryStore {
	t.Helper()
	text := []string{routing.APIFlavorOpenAI}
	audio := []string{routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderVLLM, text, nil, "emb-yes", embeddingRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderVLLM, text, nil, "emb-no", embeddingRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderVLLM, text, nil, "emb-unprobed")
	offerSpeechModel(t, rs, routing.ProviderVLLM, text, nil, "emb-speech-verdict-only", speechRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "emb-odd-flavor", embeddingRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderVLLM, text, nil, "rr-yes", rerankRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderVLLM, text, nil, "rr-no", rerankRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderVLLM, text, nil, "both-yes", embeddingRow(routing.CapabilityYes), rerankRow(routing.CapabilityYes))
	offerGroup(t, rs, "grp_pure_emb", "pure-emb-group", "emb-yes", "both-yes")
	offerGroup(t, rs, "grp_mixed_emb", "mixed-emb-group", "emb-yes", "emb-no")
	offerGroup(t, rs, "grp_pure_rr", "pure-rr-group", "rr-yes", "both-yes")
	offerGroup(t, rs, "grp_mixed_rr", "mixed-rr-group", "rr-yes", "rr-no")
	return rs
}

// ModelDTO.Embedding and ModelDTO.Rerank are the listing's embedding / rerank
// flags: a (capability, yes) verdict ANDed across a model's mappings, fail-closed
// like every other capability flag, and a group's flag the AND of its members.
// A "no" row and a never-probed model are both false, another capability's yes
// never sets either flag, and the two flags are independent of each other.
func TestModelsResponseEmbeddingAndRerankFlags(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(embeddingRerankOfferingStore(t), nil)
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

	for _, tc := range []struct {
		name              string
		embedding, rerank bool
	}{
		{"emb-yes", true, false},
		{"emb-no", false, false},                  // an explicit (embedding, no) verdict
		{"emb-unprobed", false, false},            // never probed: unknown is not capable
		{"emb-speech-verdict-only", false, false}, // another capability's yes is neither
		{"emb-odd-flavor", true, false},           // no flavor half: the verdict alone decides
		{"rr-yes", false, true},
		{"rr-no", false, false},
		{"both-yes", true, true}, // the two flags are independent axes
		{"pure-emb-group", true, false},
		{"mixed-emb-group", false, false}, // one member lacks the verdict
		{"pure-rr-group", false, true},
		{"mixed-rr-group", false, false},
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.Embedding != tc.embedding || got.Rerank != tc.rerank {
			t.Errorf("%s embedding=%v rerank=%v, want embedding=%v rerank=%v", tc.name, got.Embedding, got.Rerank, tc.embedding, tc.rerank)
		}
	}
}

// The flags are an AND across a model's mappings, fail-closed: a model on server
// A (yes) and server B (no, or never probed) is advertised false. "both-yes" pins
// the fail-closed SEED: two yes mappings only reach true because the accumulator
// starts true on a model's first view.
func TestModelsResponseEmbeddingAndRerankAndAcrossMappings(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerSharedMapping := func(srvID, appID, mappingID, gateway string, rows ...routing.CapabilityRow) {
		t.Helper()
		if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", srvID, err)
		}
		if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
			t.Fatalf("CreateApplication %s: %v", appID, err)
		}
		if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mappingID, err)
		}
		if len(rows) > 0 {
			if err := rs.UpsertMappingCapabilities(ctx, mappingID, rows); err != nil {
				t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
			}
		}
	}

	offerSharedMapping("srv_a", "app_a", "map_a", "split", embeddingRow(routing.CapabilityYes), rerankRow(routing.CapabilityYes))
	offerSharedMapping("srv_b", "app_b", "map_b", "split", embeddingRow(routing.CapabilityNo), rerankRow(routing.CapabilityNo))
	offerSharedMapping("srv_c", "app_c", "map_c", "unprobed-second", embeddingRow(routing.CapabilityYes), rerankRow(routing.CapabilityYes))
	offerSharedMapping("srv_d", "app_d", "map_d", "unprobed-second")
	offerSharedMapping("srv_e", "app_e", "map_e", "both-yes", embeddingRow(routing.CapabilityYes), rerankRow(routing.CapabilityYes))
	offerSharedMapping("srv_f", "app_f", "map_f", "both-yes", embeddingRow(routing.CapabilityYes), rerankRow(routing.CapabilityYes))
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	for _, tc := range []struct {
		name string
		want bool
		why  string
	}{
		{"split", false, "one mapping says no, and the fold is an AND"},
		{"unprobed-second", false, "a never-probed mapping ANDs in as false"},
		{"both-yes", true, "the fail-closed seed must let two yes mappings reach true"},
	} {
		if got := byID[tc.name]; got.Embedding != tc.want || got.Rerank != tc.want {
			t.Errorf("%s embedding=%v rerank=%v, want both %v -- %s", tc.name, got.Embedding, got.Rerank, tc.want, tc.why)
		}
	}
}

// An override alias carries its target's Embedding and Rerank flags under the
// alias name, like every sibling flag: the alias redirect (inheritTargetRow in
// service.go) wires each flag in on its own, so a build that wired only the older
// flags would leave both silently false behind every alias.
func TestModelsResponseEmbeddingAndRerankAliasRedirect(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(embeddingRerankOfferingStore(t), nil)
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"alias-embed":   {To: "emb-yes", Offer: true},
		"alias-rank":    {To: "rr-yes", Offer: true},
		"alias-nothing": {To: "emb-no", Offer: true},
	})
	byID := modelsByID(svc.Models(ctx, token))

	for _, tc := range []struct {
		name              string
		embedding, rerank bool
	}{
		{"alias-embed", true, false},
		{"alias-rank", false, true},
		{"alias-nothing", false, false},
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.Embedding != tc.embedding || got.Rerank != tc.rerank {
			t.Errorf("%s embedding=%v rerank=%v, want its target's flags embedding=%v rerank=%v", tc.name, got.Embedding, got.Rerank, tc.embedding, tc.rerank)
		}
	}
}
