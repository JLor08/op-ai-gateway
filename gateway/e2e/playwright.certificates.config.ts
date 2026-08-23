// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { defineConfig } from "@playwright/test";

// e2e:certificates — proves the TLS-certificate management feature end to end
// through the real portal UI, in BOTH issuer modes (self_signed then acme),
// against a REAL SQLITE-BACKED gateway. Cloned from playwright.servers.config.ts
// with one extra webServer: a standalone fake ACME directory
// (e2e-certificates/fakeacme), the same protocol logic as the test-only
// fixtures in internal/certissue/fakedir_test.go and
// cmd/gateway/acme_fakedir_test.go, ported into a long-running program so a
// REAL, separately-running gateway process can be pointed at it via
// `acme_directory_url`.
//
// Ports: the fake ACME on 8093 (its HTTP-01 challenge fetch calls back into
// the gateway's public listener on 8091 — see FAKEACME_CHALLENGE_BASE below),
// the gateway on 8091, the frontend preview on 4173 — same layout as every
// other suite; suites never run concurrently. Fresh gateway + fresh sqlite
// file + a fresh in-memory fake-ACME CA/order-map every run (rm -f before the
// gateway starts, and the fake ACME program is itself re-built + re-launched
// fresh each run, so it never carries state between runs).
//
// The reconcile-loop cadence is floored at its production minimum (60s, see
// config.Config.CertReconcileIntervalSeconds) so the suite does not have to
// wait out the 900s default; the suite itself still needs several real
// reconcile passes (incl. an explicit "wait at least two intervals, confirm
// nothing changed" step in scenario 2), so the per-test timeout is generous.

// Known test-only values (never used in production). The bootstrap password
// must satisfy the >= 10 char policy in internal/auth.
export const CERTS_ADMIN_EMAIL = "certs-admin@example.test";
export const CERTS_ADMIN_PASSWORD = "Certs-E2E-Pass-1";
// The bootstrap admin's display name — the accessible name of the user-menu
// trigger button the spec clicks to open the dropdown (see
// enterSystemAdminMode in certificates.spec.ts).
export const CERTS_ADMIN_NAME = "Certs Admin";

const SQLITE_PATH = "/tmp/op-ai-gateway-certificates-e2e.db";
const GATEWAY_BIN = "/tmp/op-ai-gateway-certificates-e2e";
const FAKEACME_BIN = "/tmp/op-ai-gateway-fakeacme-e2e";
// Scenario 3 (the gateway's OWN edge/nginx certificate): a real, writable
// directory so OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR is genuinely exercised (the
// edge certificate/key/CA files really land here on disk, exactly as they
// would on the compose/k8s deployment), and -- the part Scenario 3 actually
// asserts on -- so `EdgeDeliveryCapable()` reads true, which is what makes
// `GET /api/system/certificates/edge/key` refuse with 409 rather than hand
// back the key. Fresh (rm -rf, then mkdir -p) on every run, mirroring the
// `rm -f ${SQLITE_PATH}` fresh-state pattern below. Harmless for Scenario 1/2
// (they never turn the edge feature on, so the edge row is never wanted and
// this directory is never written to).
const CERT_EDGE_OUTPUT_DIR = "/tmp/op-ai-gateway-certificates-e2e-edge";
// 64 hex chars, verified distinct from every other suite's bootstrap token/key
// via `grep -rn "BOOTSTRAP_API_TOKEN\s*=\|CAPTURE_ENCRYPTION_KEY\s*=\|CAPTURE_KEY\s*=\|CERT_ENCRYPTION_KEY\s*="
// playwright.*.config.ts` (fix round 2 — the original value here collided
// byte-for-byte with playwright.projects.config.ts's bootstrap token).
export const CERTS_BOOTSTRAP_API_TOKEN = "d42fff1257f7b73d2486d461fd88e8dbaf0a15a55720de03c82fdfd110927e78";

export const FAKEACME_ADDR = "127.0.0.1:8093";
export const FAKEACME_DIRECTORY_URL = `http://${FAKEACME_ADDR}/directory`;

// The sqlite driver is a real disk store, so every certificate private key
// (leaf keys, the ACME account key, and — the one this suite actually
// exercises — the internal CA's private key) is REJECTED without the
// CERTIFICATE encryption key configured (see [[secret-at-rest-enc-plain-scheme]]
// in the repo memory: sealed with a key / plaintext only in the volatile
// memory store / rejected on disk without a key). Certificates use their OWN
// key with NO fallback to OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY, so setting the
// capture key here would NOT help: without this variable, self_signed mode's
// `ensureCA` fails every reconcile pass ("internal CA unavailable; skipping
// certificate pass", err=system.cert_key_required) and NO certificate is ever
// issued — both scenarios of this suite depend on it. The capture key is
// deliberately NOT set: nothing in this suite exercises payload capture or
// chat transcripts. 64 hex chars; verified distinct from every other suite's
// BOOTSTRAP_API_TOKEN/CAPTURE_KEY/CERT_ENCRYPTION_KEY via the grep above.
const CERT_ENCRYPTION_KEY = "f8a95df9542790ca411521898e3df321895dc7fb794ba6bc32d81af33b49c5eb";

export default defineConfig({
  testDir: "./e2e-certificates",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  // Scenario 2 deliberately waits out at least two 60s reconcile intervals
  // (plus setup, plus several bounded polls for a reconcile pass to land) —
  // comfortably past the 30s default, and past every other suite's budget.
  timeout: 900000,
  use: {
    baseURL: "http://127.0.0.1:4173",
    trace: "on-first-retry"
  },
  webServer: [
    {
      command: `rm -f ${FAKEACME_BIN} && go build -o ${FAKEACME_BIN} . && ${FAKEACME_BIN}`,
      cwd: "e2e-certificates/fakeacme",
      url: `http://${FAKEACME_ADDR}/healthz`,
      reuseExistingServer: false,
      timeout: 120000,
      env: {
        FAKEACME_ADDR,
        FAKEACME_CHALLENGE_BASE: "http://127.0.0.1:8091",
        GOCACHE: "/private/tmp/op-ai-gateway-go-build-cache"
      }
    },
    {
      command: `rm -f ${SQLITE_PATH} && rm -rf ${CERT_EDGE_OUTPUT_DIR} && mkdir -p ${CERT_EDGE_OUTPUT_DIR} && go build -o ${GATEWAY_BIN} ./cmd/gateway && ${GATEWAY_BIN}`,
      cwd: "../backend",
      url: "http://127.0.0.1:8091/healthz",
      reuseExistingServer: false,
      timeout: 120000,
      env: {
        OP_AI_GATEWAY_ADDR: "127.0.0.1:8091",
        // Scenario 6 only (P3 mesh TLS): an explicit second listener on a fixed
        // loopback address so the REAL server-agent can dial it over wss. With this
        // set, AgentBindHost is 127.0.0.1, so the self_signed gateway leaf carries
        // 127.0.0.1 as an IP-SAN and the agent verifies the connection against the
        // internal CA bundle (no tls_insecure). Scenarios 1-5 do not use it.
        OP_AI_GATEWAY_AGENT_ADDR: "127.0.0.1:8094",
        // Scenario 8 only (the separate encrypted agent-port mode): an explicit,
        // bindable loopback address for the DEDICATED TLS-only listener that
        // cert_mesh_tls_mode=separate brings up. Distinct from the plaintext
        // AGENT_ADDR above (8094) by port only, mirroring the production
        // "same host, TLS port differs" topology. Setting the *_ADDR (not just
        // *_PORT) means resolveAgentTLSAddr returns a fixed address without needing
        // a NetBird peer (there is none in this stack), so the TLS bind comes up on
        // a mode toggle within one reconcile tick. effectiveAgentTLSPort takes its
        // port component (8095), which the panel then shows read-only as
        // cert_mesh_tls_port. Scenarios 1-7 keep cert_mesh_tls_mode="" (combined,
        // the env default), so this address is never bound for them.
        OP_AI_GATEWAY_AGENT_TLS_ADDR: "127.0.0.1:8095",
        OP_AI_GATEWAY_PUBLIC_URL: "http://127.0.0.1:4173/portal",
        OP_AI_GATEWAY_DB_DRIVER: "sqlite",
        OP_AI_GATEWAY_SQLITE_PATH: SQLITE_PATH,
        OP_AI_GATEWAY_AUTO_MIGRATE: "true",
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL: CERTS_ADMIN_EMAIL,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME: CERTS_ADMIN_NAME,
        OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN: CERTS_BOOTSTRAP_API_TOKEN,
        OP_AI_GATEWAY_BOOTSTRAP_ADMIN_PASSWORD: CERTS_ADMIN_PASSWORD,
        // Floor (60s, see config.Config.CertReconcileIntervalSeconds) instead
        // of the 900s production default, so the suite's several real
        // reconcile passes land within a practical wall-clock budget.
        OP_AI_GATEWAY_CERT_RECONCILE_INTERVAL_SECONDS: "60",
        // Scenario 6: the gateway-peer reconcile loop is what upgrades the mesh
        // listener to TLS once the self_signed gateway leaf exists; floor it (30s)
        // so that upgrade lands within the test budget instead of the 60s default.
        OP_AI_GATEWAY_NETBIRD_SYNC_INTERVAL_SECONDS: "30",
        OP_AI_GATEWAY_CERT_ENCRYPTION_KEY: CERT_ENCRYPTION_KEY,
        // Scenario 3 only -- see CERT_EDGE_OUTPUT_DIR's own doc comment above.
        OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR: CERT_EDGE_OUTPUT_DIR,
        // Scenario 4 only (the plaintext-refusal gate) -- see
        // cmd/gateway/main.go's certEdgeGateTestRemoteAddrEnv doc comment for the
        // full explanation. Short version: this whole e2e stack runs on ONE
        // machine with NO nginx in front, so every real connection between the
        // test client (through the frontend's vite proxy) and this gateway
        // process is genuinely loopback -- which internal/gateway/edge_scheme.go
        // PERMANENTLY exempts (the gateway's own background chat runs depend on
        // exactly that exemption). Without this override, scenario 4 could not
        // exercise EITHER half of the gate: the arming precondition's
        // observation and the refusal itself both read the identical RemoteAddr
        // and would both silently no-op. A genuine internal caller (chat runs)
        // is exempted by its own header FIRST, independent of RemoteAddr, so it
        // is unaffected -- and scenarios 1-3 never touch cert_edge_require_https,
        // so this override is a no-op for them (the gate stays disarmed).
        OP_AI_GATEWAY_CERT_EDGE_GATE_TEST_REMOTE_ADDR: "203.0.113.90:1",
        GOCACHE: "/private/tmp/op-ai-gateway-go-build-cache"
      }
    },
    {
      command: "npm run build && npm run preview",
      cwd: "../frontend",
      url: "http://127.0.0.1:4173/portal/",
      reuseExistingServer: false,
      timeout: 120000
    }
  ]
});
