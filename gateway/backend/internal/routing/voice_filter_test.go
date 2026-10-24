// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"errors"
	"reflect"
	"testing"
)

// VoiceNamesFromCacheRows is the one reader of the voices cache's encoding: the voices
// endpoint and the router's voice filter both go through it.
func TestVoiceNamesFromCacheRows(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rows    []ModelMappingCacheRow
		want    []string
		wantErr bool
	}{
		{name: "no rows", rows: nil, want: nil},
		{name: "the scope-empty row", rows: []ModelMappingCacheRow{{Payload: `["alba","bella"]`}}, want: []string{"alba", "bella"}},
		{name: "another scope is not the voices list", rows: []ModelMappingCacheRow{{ScopeKey: "de", Payload: `["hans"]`}}, want: nil},
		{name: "the scope-empty row among others", rows: []ModelMappingCacheRow{{Payload: `["alba"]`}, {ScopeKey: "de", Payload: `["hans"]`}}, want: []string{"alba"}},
		{name: "an empty payload", rows: []ModelMappingCacheRow{{Payload: ``}}, want: nil},
		{name: "a blank payload", rows: []ModelMappingCacheRow{{Payload: "  \n"}}, want: nil},
		{name: "JSON null", rows: []ModelMappingCacheRow{{Payload: `null`}}, want: nil},
		{name: "a cached-empty list", rows: []ModelMappingCacheRow{{Payload: `[]`}}, want: []string{}},
		{name: "not JSON", rows: []ModelMappingCacheRow{{Payload: `{not json`}}, wantErr: true},
		{name: "JSON of the wrong shape", rows: []ModelMappingCacheRow{{Payload: `{"voices":["alba"]}`}}, wantErr: true},
		{name: "an array of the wrong element type", rows: []ModelMappingCacheRow{{Payload: `[1,2]`}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := VoiceNamesFromCacheRows(tc.rows)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("names = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// FirstPresetVoice is the one place a preset voice is picked for a model that has presets
// (the speech benchmark's voice for a preset clone, the health loop's "does it have any"):
// the first bytewise-sorted non-blank name of the voices cache, or "" when there is none.
func TestFirstPresetVoice(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []ModelMappingCacheRow
		want string
	}{
		{name: "no rows", rows: nil, want: ""},
		{name: "a cached-empty list", rows: []ModelMappingCacheRow{{Payload: `[]`}}, want: ""},
		{name: "the bytewise first of the writer's sorted list", rows: []ModelMappingCacheRow{{Payload: `["Frank","Thorsten"]`}}, want: "Frank"},
		{name: "the bytewise first, whatever order the row was written in", rows: []ModelMappingCacheRow{{Payload: `["Thorsten","Frank"]`}}, want: "Frank"},
		{name: "bytewise: an upper-case name sorts before a lower-case one", rows: []ModelMappingCacheRow{{Payload: `["alba","Zed"]`}}, want: "Zed"},
		{name: "blank names are skipped", rows: []ModelMappingCacheRow{{Payload: `[" ","","Frank"]`}}, want: "Frank"},
		{name: "only blank names", rows: []ModelMappingCacheRow{{Payload: `[" ",""]`}}, want: ""},
		{name: "the name is kept verbatim, not trimmed", rows: []ModelMappingCacheRow{{Payload: `[" a ","b"]`}}, want: " a "},
		{name: "another scope is not the voices list", rows: []ModelMappingCacheRow{{ScopeKey: "de", Payload: `["hans"]`}}, want: ""},
		{name: "an empty payload", rows: []ModelMappingCacheRow{{Payload: ``}}, want: ""},
		{name: "JSON null", rows: []ModelMappingCacheRow{{Payload: `null`}}, want: ""},
		{name: "an undecodable row is no presets", rows: []ModelMappingCacheRow{{Payload: `{not json`}}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstPresetVoice(tc.rows); got != tc.want {
				t.Fatalf("FirstPresetVoice(%+v) = %q, want %q", tc.rows, got, tc.want)
			}
		})
	}
}

// voiceScope's two methods are the identity on a nil scope, which is what a request
// without a known voice threads through the resolver.
func TestVoiceScopeNilIsTheIdentity(t *testing.T) {
	var none *voiceScope
	cands := []MappingCandidate{{Mapping: ModelMapping{ID: "m1"}}, {Mapping: ModelMapping{ID: "m2"}}}
	if got := none.restrict(cands); !reflect.DeepEqual(got, cands) {
		t.Fatalf("restrict = %v, want the candidates unchanged", got)
	}
	if got := none.refine(ErrNoHealthyHost); !errors.Is(got, ErrNoHealthyHost) || errors.Is(got, ErrVoiceUnavailable) {
		t.Fatalf("refine = %v, want ErrNoHealthyHost untouched", got)
	}
	if got := none.refine(nil); got != nil {
		t.Fatalf("refine(nil) = %v, want nil", got)
	}
}

// refine only ever rewrites the no-healthy-host outcome of a scope that narrowed.
func TestVoiceScopeRefine(t *testing.T) {
	narrowed := &voiceScope{allowed: map[string]struct{}{"m1": {}}, narrowed: true}
	notNarrowed := &voiceScope{allowed: map[string]struct{}{"m1": {}}, narrowed: false}
	other := errors.New("some other failure")
	for _, tc := range []struct {
		name  string
		scope *voiceScope
		in    error
		want  error
	}{
		{"narrowed + no healthy host", narrowed, ErrNoHealthyHost, ErrVoiceUnavailable},
		{"narrowed + no model route", narrowed, ErrNoModelRoute, ErrNoModelRoute},
		{"narrowed + a queue outcome", narrowed, ErrAdmissionQueueTimeout, ErrAdmissionQueueTimeout},
		{"narrowed + another failure", narrowed, other, other},
		{"narrowed + no error", narrowed, nil, nil},
		{"not narrowed + no healthy host", notNarrowed, ErrNoHealthyHost, ErrNoHealthyHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.scope.refine(tc.in); !errors.Is(got, tc.want) && !(got == nil && tc.want == nil) {
				t.Fatalf("refine(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
