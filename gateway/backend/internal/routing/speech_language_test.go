// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

// SpeechLanguagePayload and ParseSpeechLanguage are the write and read halves of one
// persisted shape: what the health loop stores for "does this model take a `language`
// request param, and its default" is exactly what the composer gate reads back. A write
// the parser could not read back would leave a mapping reading as unknown (no control)
// although the model was probed, and a "no" that parsed as unusable would be
// indistinguishable from never having been probed.
func TestSpeechLanguagePayloadRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		accepts bool
		def     string
	}{
		{"accepts with a declared default", true, "zh"},
		{"accepts with no declared default", true, ""},
		{"a real no: the model declares no language param", false, ""},
		{"a default that needs JSON escaping", true, `a"b\c`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accepts, def, ok := ParseSpeechLanguage(SpeechLanguagePayload(tc.accepts, tc.def))
			if accepts != tc.accepts || def != tc.def || !ok {
				t.Fatalf("round trip of (%v, %q) = (%v, %q, %v), want (%v, %q, true)", tc.accepts, tc.def, accepts, def, ok, tc.accepts, tc.def)
			}
		})
	}
}

// The payload is a persisted shape, so it is pinned literally: the key names are what
// every already-written row carries, and a rename would silently read them all as unusable.
func TestSpeechLanguagePayloadShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		accepts bool
		def     string
		want    string
	}{
		{"accepts with a default", true, "zh", `{"accepts":true,"default":"zh"}`},
		{"a real no", false, "", `{"accepts":false,"default":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SpeechLanguagePayload(tc.accepts, tc.def); got != tc.want {
				t.Fatalf("SpeechLanguagePayload(%v, %q) = %s, want %s", tc.accepts, tc.def, got, tc.want)
			}
		})
	}
}

// ParseSpeechLanguage is the one definition of "a usable cached language fact", shared by
// every reader. Only an unusable payload is no fact: a `{accepts:false}` row is a usable
// fact (a model that declares no language param), and must not be confused with the
// unusable ones below, which the reader treats like an unprobed mapping.
func TestParseSpeechLanguage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		payload     string
		wantAccepts bool
		wantDef     string
		wantOK      bool
	}{
		{"accepts with a default", `{"accepts":true,"default":"zh"}`, true, "zh", true},
		{"a real no is a usable fact", `{"accepts":false,"default":""}`, false, "", true},
		{"a missing default reads as empty", `{"accepts":true}`, true, "", true},
		{"surrounding whitespace is tolerated", " \n{\"accepts\":true,\"default\":\"de\"}\n ", true, "de", true},
		{"unknown fields are ignored", `{"accepts":true,"default":"en","extra":1}`, true, "en", true},
		{"empty is no fact", "", false, "", false},
		{"whitespace only is no fact", "  \n", false, "", false},
		{"malformed JSON", "{bad", false, "", false},
		{"trailing garbage", `{"accepts":true,"default":"zh"} x`, false, "", false},
		{"a JSON array", `["zh"]`, false, "", false},
		{"a JSON null", "null", false, "", false},
		{"an object without accepts is not a fact", `{"default":"zh"}`, false, "", false},
		{"an empty object", "{}", false, "", false},
		{"accepts of the wrong type", `{"accepts":"yes","default":"zh"}`, false, "", false},
		{"default of the wrong type", `{"accepts":true,"default":7}`, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accepts, def, ok := ParseSpeechLanguage(tc.payload)
			if accepts != tc.wantAccepts || def != tc.wantDef || ok != tc.wantOK {
				t.Fatalf("ParseSpeechLanguage(%q) = (%v, %q, %v), want (%v, %q, %v)", tc.payload, accepts, def, ok, tc.wantAccepts, tc.wantDef, tc.wantOK)
			}
		})
	}
}

// The cache type is a persisted key, so renaming it would orphan every row already
// written. It differs from the other audio cache types, whose payloads are read by
// parsers of their own.
func TestModelMappingCacheSpeechLanguageType(t *testing.T) {
	if ModelMappingCacheSpeechLanguage != "speech_language" {
		t.Fatalf("ModelMappingCacheSpeechLanguage = %q, want %q", ModelMappingCacheSpeechLanguage, "speech_language")
	}
	for _, other := range []string{ModelMappingCacheVoices, ModelMappingCacheOutputByteRate, ModelMappingCacheOutputByteRateModel} {
		if ModelMappingCacheSpeechLanguage == other {
			t.Fatalf("the speech language fact must be its own cache type, not %q", other)
		}
	}
}
