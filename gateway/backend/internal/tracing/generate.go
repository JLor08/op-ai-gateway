// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package tracing

// Tracing decorators are generated with gowrap using templates/tracing.gowrap.tmpl.
// Regenerate with `go generate ./internal/tracing/...` after changing an interface.
// gowrap is a dev-time tool (MIT); the generated code does not import it.
//
// gowrap does not preserve per-file license headers, so after regenerating run
// `scripts/add-license-headers.sh` (idempotent) to re-apply the SPDX header to the
// generated *_gen.go files; that keeps `go generate` output diff-clean.

//go:generate gowrap gen -p op-ai-gateway/internal/routing -i Store -t ./templates/tracing.gowrap.tmpl -o routingstore_gen.go -v Prefix=routing.Store -v DecoratorName=RoutingStoreWithTracing

// account.API and portal.API are decorated with the OTel-GLOBAL template so the
// generated code lives IN package account / portal and opens spans via
// otel.Tracer(...) — it must NOT import internal/tracing (portal imports provider,
// which imports internal/tracing; a same-package tracing import would form a cycle).
// The output paths are relative to internal/tracing, landing the files in the
// account / portal packages so they compile there.
//go:generate gowrap gen -p op-ai-gateway/internal/account -i API -t ./templates/tracing-otelglobal.gowrap.tmpl -o ../account/api_tracing_gen.go -v Prefix=account.Service -v DecoratorName=APIWithTracing
//go:generate gowrap gen -p op-ai-gateway/internal/portal -i API -t ./templates/tracing-otelglobal.gowrap.tmpl -o ../portal/api_tracing_gen.go -v Prefix=portal.Service -v DecoratorName=APIWithTracing
