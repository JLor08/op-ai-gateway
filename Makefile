# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 OnPrem AI Gateway contributors

.PHONY: test test-go test-ui test-e2e dev run-gateway fmt lint lint-go lint-docs lint-fix \
	lint-install hooks \
	sonar-up sonar-coverage sonar-scan sonar-gate sonar-findings sonar-branch-findings sonar-down

test: test-go test-ui

# --- Formatting & linting (Go: golangci-lint v2 with gofumpt+goimports) ---

# Install the pinned linter (also used by CI). Requires Go on PATH.
lint-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1

# Format both Go modules in place (gofumpt + goimports via golangci-lint).
fmt:
	cd gateway/backend && golangci-lint fmt
	cd server-agent && golangci-lint fmt

# Everything lintable: both Go modules, then the documentation corpus.
lint: lint-go lint-docs

# Lint both Go modules against .golangci.yml.
lint-go:
	cd gateway/backend && golangci-lint run
	cd server-agent && golangci-lint run

# Docs consistency: intra-repo markdown links and anchors, the
# docs/architecture index, and the OpenAPI refs. bash + git + awk only;
# its own failure modes are pinned by scripts/check-docs.test.sh.
lint-docs:
	./scripts/check-docs.sh

# Lint with autofix for the fixable findings.
lint-fix:
	cd gateway/backend && golangci-lint run --fix
	cd server-agent && golangci-lint run --fix

# Install the repo's git hooks (pre-commit: fmt-check + lint).
hooks:
	git config core.hooksPath scripts/git-hooks

# Both Go modules, mirroring CI, which tests them in separate steps. The
# name says `go`, not `backend`: a target that silently skipped server-agent
# gave a local run false confidence about the module CI does cover.
test-go:
	cd gateway/backend && go test ./...
	cd server-agent && go test ./...

test-ui:
	cd gateway/frontend && npm test

test-e2e:
	cd gateway/e2e && npm install && npx playwright install chromium && npm run e2e

dev:
	./scripts/dev.sh

run-gateway:
	cd gateway/backend && go run ./cmd/gateway

# --- Local SonarQube quality gate (headless, agent-runnable) ---

sonar-up:
	./scripts/sonar/sonar.sh up

sonar-coverage:
	./scripts/sonar/sonar.sh coverage

sonar-scan:
	./scripts/sonar/sonar.sh scan

sonar-gate:
	./scripts/sonar/sonar.sh gate

sonar-findings:
	./scripts/sonar/sonar.sh findings

# Which of the exported findings sit on lines THIS branch changed (vs origin/main).
# Post-processing only: run sonar-findings first. Exits non-zero when the branch
# owns a finding, so it works as a pre-PR check.
sonar-branch-findings:
	./scripts/sonar/branch-findings.sh

sonar-down:
	./scripts/sonar/sonar.sh down
