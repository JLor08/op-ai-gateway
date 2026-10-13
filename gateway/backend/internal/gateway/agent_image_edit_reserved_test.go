// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
)

// The agent trust boundary for the DERIVED image_edit capability.
// reservedAgentCapabilityNames must contain routing.CapabilityImageEdit, so a
// verdict carrying that name in an agent's OPEN verdict list never becomes a
// row.
//
// image_edit has no legitimate agent writer: it is derived at the gate from the
// image verdict (routing.ImpliedImageEdit), and the only row the capability
// ever carries is an OPERATOR's image_edit:no opt-out. That is the OPPOSITE of
// "image", which is deliberately kept OFF this list because the sd-server
// probe really does write it (ADR-042). A name with no honest writer is
// exactly what the list is for.
//
// The direction that matters is "no": an agent's rank-1 image_edit:no on a
// mapping that generates images would silently DISABLE edit for it, and the
// operator's way back (deleting the row, which is what "unknown" is) would be
// re-overwritten by the next sample. "yes" is inert to the gate but would still
// put a row nobody vetted in front of the operator, so it is refused too.
//
// Each subtest is falsifiable: the mapping is seeded image:yes (so a row that
// got through would FLIP ImpliedImageEdit, which is asserted), and the same
// sample carries a "vision" verdict that must still land -- the reserved rule
// drops rows, never the pass, and "nothing was written" cannot be satisfied by
// a write path that had simply stopped working.
//
// sdcpp_capabilities is not exercised: that probe is held to the image row by
// the notImage rule, which would drop image_edit whether or not the name were
// reserved, so a green result there could not tell the two rules apart. The
// two sources below can.
func TestIngestDropsAnAgentReportedImageEditVerdict(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, spec, source, verdict string
	}{
		{"a no would silently disable edit (llama.cpp props)", "rspec_edit_no", routing.CapabilitySourceLlamaCppProps, routing.CapabilityNo},
		{"a yes would put an unvetted grant in front of the operator (llama.cpp props)", "rspec_edit_yes", routing.CapabilitySourceLlamaCppProps, routing.CapabilityYes},
		{"a yes from the ollama probe, whose open list carries publisher strings", "rspec_edit_oll", routing.CapabilitySourceOllamaAPIShow, routing.CapabilityYes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := withCapturedSlogAtTheDefaultLevel(t)
			srv := NewTestServer()
			seedRuntimeIngestSpec(t, srv, tc.spec, false)
			mappingID := "map_" + tc.spec
			// The mapping generates images, so it derives edit before the
			// sample arrives.
			seedCapabilityRow(t, srv, mappingID, routing.CapabilityImage, routing.CapabilityYes, routing.CapabilitySourceManual)
			counting := countingRowStore(srv)

			req, raw := ingestReq(t, capabilitiesBody(tc.spec,
				`{"verdicts":[{"name":"`+routing.CapabilityImageEdit+`","verdict":"`+tc.verdict+`"},`+
					`{"name":"vision","verdict":"yes"}],"source":"`+tc.source+`"}`))
			if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
				t.Fatalf("ingest: %v", err)
			}

			if sent := counting.lastSent(); len(sent) != 1 || sent[0].Capability != routing.CapabilityVision {
				t.Fatalf("the write carried %+v, want exactly the vision row -- an agent-reported %q must never reach the store, and the rest of the pass must still land", sent, routing.CapabilityImageEdit)
			}
			if row, ok := capabilityRow(t, srv, mappingID, routing.CapabilityImageEdit); ok {
				t.Fatalf("a %q row exists (%+v), want none -- only an operator may state the opt-out", routing.CapabilityImageEdit, row)
			}
			assertCapabilityRow(t, srv, mappingID, routing.CapabilityVision, routing.CapabilityYes, tc.source)
			assertCapabilityRow(t, srv, mappingID, routing.CapabilityImage, routing.CapabilityYes, routing.CapabilitySourceManual)
			rows, err := srv.Routes.MappingCapabilities(ctx, mappingID)
			if err != nil {
				t.Fatalf("MappingCapabilities: %v", err)
			}
			if !routing.ImpliedImageEdit(rows) {
				t.Fatalf("ImpliedImageEdit = false after an agent-reported image_edit:%s, want the mapping still edit-capable (rows %+v)", tc.verdict, rows)
			}
			if recs := buf.Snapshot(); !findLogRecord(recs, "WARN", "reserved internal capability names") {
				t.Fatalf("no WARN record naming the reserved drop at the gateway's own default level (info); records = %+v", recs)
			}
		})
	}
}
