// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

// UnwrapService recovers the concrete *Service underneath an API value that
// may be wrapped by NewAPIWithTracing (api_tracing_gen.go's generated
// decorator, which embeds the wrapped API as its exported API field -- this
// reads straight through that embed rather than needing an Unwrap method
// added to the generated type, which "DO NOT EDIT" rules out touching
// directly).
//
// cmd/gateway needs this for exactly one purpose: it must build the gateway
// Server -- which owns the real runtime-config push, Server.
// PushRuntimeConfig -- AFTER wrapping the portal Service for
// ServerDeps.Portal, so ServerDeps.Portal is the only reference to the
// concrete Service left once gateway.New returns; UnwrapService lets it
// recover that pointer to call the exported setter,
// Service.SetRuntimeConfigChangedHook (see that method's doc for why the
// hook cannot be wired in at portal.NewService construction time instead).
//
// Returns nil when api is neither *Service nor an *APIWithTracing wrapping
// one (e.g. a test double, or a nil interface value).
func UnwrapService(api API) *Service {
	if traced, ok := api.(*APIWithTracing); ok {
		api = traced.API
	}
	svc, _ := api.(*Service)
	return svc
}
