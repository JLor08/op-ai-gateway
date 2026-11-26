// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"net/http"
)

// probeAuth is the credential the agent attaches to its OWN probes of a
// managed child (metrics scrape, context probe, capability probes): the
// header name and its full value, built by the caller from the spec's
// decrypted api_token. It lives only in memory. String/GoString redact it,
// so a context or struct that ends up in a log line never prints the token.
type probeAuth struct {
	header string
	value  string
}

func (probeAuth) String() string   { return "[redacted probe credential]" }
func (probeAuth) GoString() string { return "[redacted probe credential]" }

type probeAuthKey struct{}

// WithProbeAuth returns ctx carrying the credential header the child probes
// started under it (ProbeContext/ProbeContextInfo, ProbePropsVerdicts,
// ProbeOllamaVerdicts, ProbeSdcppVerdicts, ProbeAudioCppVerdicts) attach to
// their request. An empty header or value leaves ctx unchanged, so those
// probes send no credential -- the behaviour for a spec without a token.
func WithProbeAuth(ctx context.Context, header, value string) context.Context {
	if header == "" || value == "" {
		return ctx
	}
	return context.WithValue(ctx, probeAuthKey{}, probeAuth{header: header, value: value})
}

func probeAuthFrom(ctx context.Context) (probeAuth, bool) {
	a, ok := ctx.Value(probeAuthKey{}).(probeAuth)
	return a, ok
}

// doProbe sends req through client, attaching auth when it is set. An
// authenticated request does not follow redirects: the redirect itself is the
// answer (a non-2xx the caller rejects), so the credential never travels to a
// location the child named rather than the loopback URL the agent built. The
// client is copied for that, never modified, since it is shared across
// children.
func doProbe(client *http.Client, req *http.Request, auth probeAuth, authed bool) (*http.Response, error) {
	if !authed {
		return client.Do(req)
	}
	req.Header.Set(auth.header, auth.value)
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c.Do(req)
}
