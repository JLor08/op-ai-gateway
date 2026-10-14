// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// recordUsage must record the path the request was RELAYED to. It does not read
// the relay's path: it recomputes ProviderPath from the target and the request's
// flavor, and generations and edits share one flavor (openai_images), so the
// endpoint has to reach that computation or an edit is recorded as
// /v1/images/generations -- a wrong value in the usage row's provider_path column
// for every edit. The zero-value row pins that a call site that sets no endpoint
// keeps recording what it always did.
func TestRecordUsageProviderPathFollowsTheEndpoint(t *testing.T) {
	cases := []struct {
		name string
		meta usageMeta
		want string
	}{
		{"edits endpoint", usageMeta{Endpoint: endpointImagesEdit}, "/v1/images/edits"},
		{"generations endpoint", usageMeta{Endpoint: endpointImages}, "/v1/images/generations"},
		{"no endpoint set keeps the generations path", usageMeta{}, "/v1/images/generations"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewTestServer()
			tc.meta.ReqPath = "/v1/images/anything"
			tc.meta.HTTPStatus = http.StatusOK

			srv.recordUsage(
				time.Now().Add(-time.Second),
				auth.Token{ID: "tok_user", UserID: "usr_edit_path"},
				inference.Request{Model: "sd-turbo", APIFlavor: apiFlavorImages},
				routing.Target{Provider: routing.ProviderVLLM, ServerID: "mock-host-comp"},
				provider.Response{},
				"", "success", tc.meta, "req_edit_path", nil, nil,
			)

			events := srv.Usage.ByUser("usr_edit_path")
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if got := events[0].ProviderPath; got != tc.want {
				t.Fatalf("ProviderPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProxyNativeRecordsTheRelaysEndpointPath drives the REAL proxyNative for an
// images relay and pins that every usage row it writes carries the path of the
// endpoint the relay was built for. proxyNative has four recordUsage call sites
// an images request can reach (the provider is not a NativeProxyClient; the
// upstream call failed before a response; a non-2xx the images error relay
// normalises; a clean success), each of which has to hand rel.endpoint on --
// the unit test above only proves recordUsage honours it once it is there.
func TestProxyNativeRecordsTheRelaysEndpointPath(t *testing.T) {
	const okBody = `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`
	branches := []struct {
		name string
		prov provider.Client
	}{
		{"provider not a NativeProxyClient", nonProxyCapableProvider{}},
		{"upstream call fails before a response", erroringProxyProvider{err: errors.New("dial tcp: connection refused")}},
		{"upstream answers non-2xx", plainTextErrorProxyProvider{status: http.StatusInternalServerError, contentType: "application/json", body: `{"error":"out of memory"}`}},
		{"upstream answers 2xx", plainTextErrorProxyProvider{status: http.StatusOK, contentType: "application/json", body: okBody}},
	}
	endpoints := []struct {
		name     string
		endpoint sessionEndpoint
		path     string
	}{
		{"edits", endpointImagesEdit, "/v1/images/edits"},
		{"generations", endpointImages, "/v1/images/generations"},
	}
	for _, br := range branches {
		for _, ep := range endpoints {
			t.Run(br.name+"/"+ep.name, func(t *testing.T) {
				srv := newImageCapableTestServerWithProvider(t, br.prov)
				target := routing.Target{Provider: routing.ProviderVLLM, ServerID: "srv-sd-turbo-fake", Model: "sd-turbo"}
				req := httptest.NewRequest(http.MethodPost, ep.path, strings.NewReader(`{}`))
				rec := httptest.NewRecorder()

				srv.proxyNative(rec, req, nativeRelay{
					token:    auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true},
					target:   target,
					path:     upstreamPath(target, apiFlavorImages, ep.endpoint),
					raw:      []byte(`{}`),
					pfReq:    inference.Request{Model: "sd-turbo", RequestedModel: "sd-turbo", APIFlavor: apiFlavorImages},
					endpoint: ep.endpoint,
				})

				got := lastUsageEvent(t, srv)
				if got.ProviderPath != ep.path {
					t.Fatalf("ProviderPath = %q, want %q", got.ProviderPath, ep.path)
				}
			})
		}
	}
}
