// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState } from 'react';
import { Box, Button, IconButton, Paper, Typography } from '@mui/material';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import type { AgentBinaryEntry, AgentConfigMaterial, PortalServer } from '../api';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError, formatDate } from './shared/format';
import { Panel } from './shared/Panel';
import { SecretReveal } from './shared/SecretReveal';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { useToast } from './shared/ToastProvider';
import { useResource } from './shared/useResource';
import { useLatestFetch } from './shared/useLatestFetch';

// Seeded into the "Custom" seconds input when the server has no override yet;
// the actual custom value replaces it once loaded. 0 is never a valid custom
// value (0 means "follow the system setting").
const defaultCustomAgentPresenceTimeoutSeconds = 30;

// os-arch label + curl command builders (pure).
const OS_LABEL: Record<string, string> = { linux: 'Linux', darwin: 'macOS', windows: 'Windows' };
const ARCH_LABEL: Record<string, string> = { amd64: 'x86-64', arm64: 'ARM64' };
function targetKey(b: { os: string; arch: string }) {
  return `${b.os}-${b.arch}`;
}
function targetLabel(b: { os: string; arch: string }) {
  return `${OS_LABEL[b.os] ?? b.os} (${ARCH_LABEL[b.arch] ?? b.arch})`;
}
function curlCommand(base: string, token: string, b: { os: string; arch: string }) {
  const url = `${base}/api/agent/v1/download/${targetKey(b)}`;
  if (b.os === 'windows')
    return `curl -fL -H "Authorization: Bearer ${token}" ${url} -o server-agent.exe`;
  return `curl -fL -H "Authorization: Bearer ${token}" ${url} -o server-agent && chmod +x server-agent`;
}

// buildServerAgentConfig produces a ready-to-use, ANNOTATED server-agent.json:
// gateway_url + token filled in and every other key pre-set to the agent's own
// default, each preceded by an English comment explaining it. Output is JSONC — the
// agent's config loader strips whole-line `//` comments before parsing (see
// server-agent/internal/config/config.go stripJSONLineComments). The comments are
// the operator's primary documentation for this file: whoever clicks Download
// reads nothing else before starting the agent, so every key states what its
// value does AND what its empty value means — which differs per key, and is the
// point (an empty runtime_allowed_binaries starts NOTHING; an empty
// runtime_allowed_dirs accepts ANY work_dir).
//
// One template, duplicated unavoidably (two languages, two Go modules): the Go
// backend's buildAgentConfigJSON serves the same document over
// /api/agent/v1/download/config — its curl sits one row above this file's
// download button, so a key missing here is a visible disagreement on one
// screen — server-agent's fileConfig defines what the agent will actually read,
// and the server-agent README documents it for the operator.
//
// Both PRODUCING copies are now held to one generated golden,
// server-agent/testdata/server-agent.config.jsonc, byte for byte (this side in
// AgentTokenSection.test.tsx, the Go side in
// TestBuildAgentConfigJSONMatchesSharedGolden), and that golden's key set is
// checked against every json tag on fileConfig by reflection. Before that, each
// side pinned only its OWN key set and neither test could see the other
// template — which is how this copy came to carry a comment about five runtime
// settings while emitting none of them. Change this function and the golden
// test fails until the Go copy and the golden are updated to match.
export function buildServerAgentConfig(config: AgentConfigMaterial, token: string): string {
  return `{
  // The gateway base URL the agent sends telemetry to (origin only, no path).
  // Required. Example: https://gateway.example.com. Under a NetBird-only
  // restriction, use the gateway's mesh URL here instead.
  "gateway_url": ${JSON.stringify(config.gateway_url)},

  // The per-server agent bearer token, shown once when generated in the portal.
  // It identifies this server to the gateway. Required. Keep this file private
  // (e.g. chmod 600) because it holds the token.
  "token": ${JSON.stringify(token)},

  // Telemetry transport: "websocket" (default; one persistent connection, cheap
  // for high-frequency sending) or "post" (one HTTP POST per sample).
  "transport": "websocket",

  // Collection cadence as a Go duration, e.g. "500ms", "1s", "5s". Clamped up to
  // a 250ms floor. Default "1s".
  "interval": "1s",

  // POST-mode re-send cadence for the static hardware inventory (self-heals a
  // gateway restart). Floored at "1m"; ignored under the websocket transport.
  // Default "30m".
  "system_report_interval": "30m",

  // Optional inference /metrics (Prometheus) URL to scrape for active/queued
  // request counts. Empty disables it.
  "metrics_url": "",

  // Optional endpoint polled each cycle for currently-loaded models, e.g.
  // "/running" for llama-swap, "/props" for llama.cpp, "/v1/models" for vLLM.
  // Empty disables it.
  "model_status_url": "",

  // How to parse model_status_url: "openai" | "llama_swap" | "llama_cpp" |
  // "litellm" | "" or "auto" (tolerant union of all shapes). Empty = auto.
  "model_status_format": "",

  // Optional LibreHardwareMonitor Remote Web Server /data.json URL for CPU (and
  // best-effort system) power watts. Empty disables it.
  "lhm_url": "",

  // Certificate installation mode: "off" (default, never fetch a certificate),
  // "files" (write fullchain/cert/chain/ca/privkey into cert_dir and run
  // cert_reload_command on change), or "proxy" (everything "files" does, and
  // additionally runs the agent-side TLS-terminating reverse proxy: it serves
  // the routes the gateway publishes -- merged with cert_proxy_routes below --
  // with the installed leaf). cert_dir is required when this is not "off".
  "cert_mode": "off",

  // Directory certificate files are written to. Required when cert_mode is not
  // "off".
  "cert_dir": "",

  // Local command run after a changed certificate is fully installed on disk.
  // This value comes ONLY from this local file -- the gateway can never deliver
  // a command to run. On Windows, keep the value quote-free (no embedded quotes).
  "cert_reload_command": "",

  // Certificate poll cadence as a Go duration, e.g. "15m". Empty or "0"/"0s" means
  // automatic (websocket transport polls every 6h, post every 15m). A configured
  // positive value below "1m" is clamped up to "1m".
  "cert_poll_interval": "",

  // Local routes for the cert_mode "proxy" listener, each an object
  // {"listen": <port>, "upstream": "http://host:port"}. Config-file only: no
  // env var, no flag. Empty (the default) means the agent serves only the
  // routes the gateway publishes. Ignored unless cert_mode is "proxy".
  "cert_proxy_routes": [],

  // How a cert_proxy_routes entry is merged with a gateway-published route on
  // the SAME listen port: "fallback" (the default; the local route fills only
  // a port the gateway did not publish) or "override" (the local route wins).
  // Empty resolves to "fallback".
  "cert_proxy_routes_mode": "fallback",

  // Optional operator-managed CA bundle. Generated configs leave this empty;
  // the agent never writes this file.
  "ca_file": ${JSON.stringify(config.ca_file)},

  // Optional agent-managed CA cache, relative to this config file when not
  // absolute. Self-signed gateway configs use "server-agent-ca.pem".
  "ca_cache_file": ${JSON.stringify(config.ca_cache_file)},

  // Optional inline CA bootstrap bundle. Present only when the gateway's
  // currently served leaf is signed by the internal CA.
  "ca_pem": ${JSON.stringify(config.ca_pem)},

  // Where the agent-managed model runtime's launch specs come from: "gateway"
  // (the default -- fetched from the portal-maintained runtime-config
  // endpoint) or "file" (read from runtime_config below and reported upward
  // read-only, so the portal shows them but cannot change them).
  "runtime_source": "gateway",

  // Path to the local runtime-config JSON file. REQUIRED when runtime_source
  // is "file"; never read otherwise. Relative to this config file when not
  // absolute.
  "runtime_config": "",

  // Absolute paths a launch spec's "binary" must match EXACTLY -- compared
  // byte for byte, so spell each one as the spec does:
  // "/opt/llama/llama-server" on POSIX, "C:/llama/llama-server.exe" on
  // Windows (forward slashes; a backslash would need JSON escaping).
  // EMPTY (the default) MEANS NOTHING MAY START -- a deliberate hard refusal,
  // not a permissive default: every spec reports not_permitted until its
  // binary is listed here. Comes ONLY from this local file, and it is the
  // operator's half of the contract: the gateway decides when and how a model
  // process runs, this list decides whether it may run at all.
  "runtime_allowed_binaries": [],

  // Permitted "work_dir" prefixes for launch specs. Empty (the default) means
  // ANY work_dir is accepted -- deliberately the inverse of the list above,
  // so an operator who does not care need not enumerate the filesystem. Once
  // non-empty, a spec that sets no work_dir is refused too. Containment is a
  // lexical path-prefix check that does not resolve symlinks: defense in
  // depth, not the boundary.
  "runtime_allowed_dirs": [],

  // Where the last known-good runtime-config document from the gateway is
  // cached, so the agent can start (and keep) model processes before its first
  // successful gateway contact. Empty (the default) means
  // "server-agent-runtime.cache.json" next to the agent binary; a relative
  // value is resolved beside this config file.
  "runtime_cache": "",

  // Bind host for the agent-managed model runtime's router port -- the port the
  // gateway sends inference requests to for a server_agent application. This
  // value comes ONLY from this local file: the gateway supplies the router's
  // PORT, never its bind host. Empty (the default) means derive one from the
  // mesh leaf installed in cert_dir, and ALL INTERFACES -- with a warning in
  // the agent log -- when there is none, which is what the empty cert_dir this
  // file ships with gives you. Set it explicitly (the mesh IP, or
  // "127.0.0.1") on any host that is not mesh-only.
  "runtime_router_bind": "",

  // Skip TLS certificate verification. Self-signed dev gateways only. Default false.
  "tls_insecure": false,

  // Verbose mode: emit detailed debug logs to stderr. Default false.
  "verbose": false
}
`;
}

// configCurlCommand downloads a ready-to-use server-agent.json via the agent-token
// endpoint. The backend fills token from the bearer + gateway_url from runtime
// state, so the saved file needs no manual editing (when a real token is used).
// A local Blob already carries the Self-Signed bootstrap CA; curl must establish
// HTTPS before it can download that file and therefore requires existing trust.
// Deliberately never add an insecure curl flag here.
function configCurlCommand(base: string, token: string) {
  return `curl -fL -H "Authorization: Bearer ${token}" ${base}/api/agent/v1/download/config -o server-agent.json`;
}

// CurlRow renders a copyable curl command: the command in a horizontally-scrolling
// <pre> (so a long line never overflows the page) plus a copy button beside it.
function CurlRow({
  command,
  copyLabel,
  testId,
}: Readonly<{
  command: string;
  copyLabel: string;
  testId?: string;
}>) {
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, minWidth: 0 }}>
      <Box
        data-testid={testId}
        component="pre"
        sx={{
          flexGrow: 1,
          minWidth: 0,
          m: 0,
          overflowX: 'auto',
          p: 1,
          bgcolor: 'action.hover',
          borderRadius: 1,
        }}
      >
        <code>{command}</code>
      </Box>
      <IconButton
        size="small"
        aria-label={copyLabel}
        onClick={() => navigator.clipboard?.writeText(command)}
      >
        <ContentCopyIcon fontSize="small" />
      </IconButton>
    </Box>
  );
}

export function AgentTokenSection({
  t,
  api,
  server,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'agentBinaries'
    | 'agentPresenceTimeout'
    | 'agentTokenStatus'
    | 'downloadAgentBinary'
    | 'generateAgentToken'
    | 'revokeAgentToken'
    | 'updateServer'
  >;
  server: PortalServer;
}>) {
  const {
    data: status,
    setData: setStatus,
    loading,
    error,
  } = useResource(() => api.agentTokenStatus(server.id), [api, server.id, t], t);
  const { showError, showSuccess } = useToast();
  const [revealed, setRevealed] = useState('');
  const [busy, setBusy] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);

  // Per-server agent-presence-timeout override: "default" (0, follow the
  // system-wide setting) or "custom" (a fixed value). Mirrors the per-application
  // health-check-interval Default/Custom UI.
  const [presenceMode, setPresenceMode] = useState<'default' | 'custom'>(
    server.agent_presence_timeout_seconds > 0 ? 'custom' : 'default',
  );
  const [presenceSeconds, setPresenceSeconds] = useState(
    server.agent_presence_timeout_seconds > 0
      ? server.agent_presence_timeout_seconds
      : defaultCustomAgentPresenceTimeoutSeconds,
  );
  const presenceTimeoutFetch = useLatestFetch(() => api.agentPresenceTimeout(), [api]);
  const systemAgentPresenceTimeout = presenceTimeoutFetch.data?.seconds ?? null;
  const [presenceBusy, setPresenceBusy] = useState(false);

  // Agent-binary manifest for the download section + the reveal dialog's live
  // curl command. Loaded once on mount; a load failure just hides the download
  // section (binaries === null) — never blocks the token flows above.
  const binariesFetch = useLatestFetch(() => api.agentBinaries(), [api]);
  const binaries = binariesFetch.data;
  const [selectedTarget, setSelectedTarget] = useState('');
  useEffect(() => {
    if (binaries?.binaries[0]) setSelectedTarget(targetKey(binaries.binaries[0]));
  }, [binaries]);
  const base = binaries?.agent_download_base || window.location.origin;
  const configDownloadBase = status?.agent_download_base || window.location.origin;
  const selectedBinary =
    binaries?.binaries.find((b) => targetKey(b) === selectedTarget) ?? binaries?.binaries[0];
  async function download(b: AgentBinaryEntry) {
    try {
      const blob = await api.downloadAgentBinary(targetKey(b));
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = b.filename;
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 0);
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }
  // Download a ready-to-use server-agent.json (defaults pre-filled). In the reveal
  // dialog `token` is the real secret; in the persistent section it is the
  // <AGENT_TOKEN> placeholder.
  function downloadConfig(token: string) {
    if (!status?.config) return;
    const blob = new Blob([buildServerAgentConfig(status.config, token)], {
      type: 'application/json',
    });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'server-agent.json';
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 0);
  }

  // Reset the local mode/value when navigating to a different server (or when
  // the server's stored override changes underneath us).
  useEffect(() => {
    setPresenceMode(server.agent_presence_timeout_seconds > 0 ? 'custom' : 'default');
    setPresenceSeconds(
      server.agent_presence_timeout_seconds > 0
        ? server.agent_presence_timeout_seconds
        : defaultCustomAgentPresenceTimeoutSeconds,
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [server.id, server.agent_presence_timeout_seconds]);

  async function savePresenceTimeout() {
    setPresenceBusy(true);
    try {
      await api.updateServer(server.id, {
        agent_presence_timeout_seconds: presenceMode === 'custom' ? presenceSeconds : 0,
      });
      showSuccess(t.serverAgentPresenceTimeoutSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setPresenceBusy(false);
    }
  }

  async function generate() {
    setBusy(true);
    try {
      const resp = await api.generateAgentToken(server.id);
      setRevealed(resp.secret);
      setStatus(resp.token);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }
  async function confirmRevoke() {
    setConfirmOpen(false);
    setBusy(true);
    try {
      await api.revokeAgentToken(server.id);
      setRevealed('');
      setStatus((current) => (current ? { ...current, exists: false } : current));
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  const exists = status?.exists ?? false;
  return (
    <Panel titleId="agent-token-heading" title={t.agentToken} subtitle={t.agentTokenIntro}>
      {loading && <Typography component="p">{t.loading}</Typography>}
      {!loading && !exists && <Typography component="p">{t.agentTokenNone}</Typography>}
      {!loading && exists && (
        <Typography component="p">
          {t.agentTokenPrefix}: <code>{status?.secret_prefix}</code> · {t.agentTokenLastUsed}:{' '}
          {formatDate(status?.last_used_at, t.agentTokenNever)}
        </Typography>
      )}
      <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1, mt: 2 }}>
        <Button variant="contained" type="button" disabled={busy} onClick={generate}>
          {exists ? t.agentTokenRotate : t.agentTokenGenerate}
        </Button>
        {exists && (
          <Button
            variant="outlined"
            type="button"
            disabled={busy}
            onClick={() => setConfirmOpen(true)}
          >
            {t.agentTokenRevoke}
          </Button>
        )}
      </Box>
      {revealed && (
        <>
          <SecretReveal title={t.agentTokenReveal}>
            <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
              <code>{revealed}</code>
              <IconButton
                size="small"
                aria-label={t.agentTokenCopy}
                onClick={() => navigator.clipboard?.writeText(revealed)}
              >
                <ContentCopyIcon fontSize="small" />
              </IconButton>
            </Box>
            <p>
              {t.agentTokenEndpointHint} <code>/api/agent/v1/telemetry</code>
            </p>
          </SecretReveal>
          {(status?.config || (binaries && selectedBinary)) && (
            <Paper
              variant="outlined"
              sx={{ mt: 2, p: 2, display: 'grid', gridTemplateColumns: 'minmax(0, 1fr)', gap: 1 }}
            >
              {binaries && selectedBinary && (
                <>
                  <SelectField
                    id="agent-reveal-system"
                    label={t.agentDownloadSelectSystem}
                    value={selectedTarget}
                    onChange={(e) => setSelectedTarget(e.target.value)}
                  >
                    {binaries.binaries.map((b) => (
                      <option key={targetKey(b)} value={targetKey(b)}>
                        {targetLabel(b)}
                      </option>
                    ))}
                  </SelectField>
                  <CurlRow
                    command={curlCommand(base, revealed, selectedBinary)}
                    copyLabel={t.agentDownloadCopyCurl}
                    testId="agent-curl"
                  />
                  {binaries.agent_download_base && (
                    <Typography variant="caption" color="text.secondary">
                      {t.agentDownloadMeshNote}
                    </Typography>
                  )}
                </>
              )}
              {status?.config && (
                <>
                  <Box>
                    <Button
                      size="small"
                      variant="outlined"
                      onClick={() => downloadConfig(revealed)}
                    >
                      {t.agentDownloadConfig}
                    </Button>
                  </Box>
                  <CurlRow
                    command={configCurlCommand(configDownloadBase, revealed)}
                    copyLabel={t.agentDownloadCopyCurl}
                  />
                </>
              )}
            </Paper>
          )}
        </>
      )}
      <ConfirmDialog
        open={confirmOpen}
        title={t.agentTokenRevokeConfirm}
        confirmLabel={t.agentTokenRevoke}
        cancelLabel={t.serverActionCancel}
        onConfirm={confirmRevoke}
        onCancel={() => setConfirmOpen(false)}
      />
      <Box sx={{ mt: 3, pt: 3, borderTop: 1, borderColor: 'divider' }}>
        <SelectField
          id="agent-presence-timeout-mode"
          label={t.serverAgentPresenceTimeoutLabel}
          value={presenceMode}
          onChange={(event) => setPresenceMode(event.target.value as 'default' | 'custom')}
        >
          <option value="default">{t.serverAgentPresenceTimeoutDefault}</option>
          <option value="custom">{t.serverAgentPresenceTimeoutCustom}</option>
        </SelectField>
        {presenceMode === 'custom' ? (
          <Field
            id="agent-presence-timeout-seconds"
            label={t.serverAgentPresenceTimeoutSecondsLabel}
            type="number"
            value={String(presenceSeconds)}
            onChange={(event) =>
              setPresenceSeconds(event.target.value === '' ? 0 : Number(event.target.value))
            }
            inputProps={{ min: 3, max: 3600, step: 1 }}
            sx={{ mt: 2 }}
          />
        ) : (
          <Typography variant="caption" component="p" sx={{ mt: 1, color: 'text.secondary' }}>
            {t.serverAgentPresenceTimeoutNote}
            {systemAgentPresenceTimeout !== null &&
              ` (${t.serverAgentPresenceTimeoutCurrent}: ${systemAgentPresenceTimeout} s)`}
          </Typography>
        )}
        <Box sx={{ mt: 2 }}>
          <Button
            variant="outlined"
            type="button"
            disabled={presenceBusy}
            onClick={savePresenceTimeout}
          >
            {t.save}
          </Button>
        </Box>
      </Box>
      <Box sx={{ mt: 3, pt: 3, borderTop: 1, borderColor: 'divider' }}>
        <Typography component="h3" variant="subtitle1">
          {t.agentDownloadHeading}
        </Typography>
        {(!binaries || binaries.binaries.length === 0) && (
          <Typography variant="body2" color="text.secondary">
            {t.agentDownloadEmpty}
          </Typography>
        )}
        {binaries && binaries.binaries.length > 0 && (
          <>
            <Typography variant="caption" color="text.secondary">
              {t.agentDownloadVersion}: {binaries.agent_version} ·{' '}
              {formatDate(binaries.built_at, '')}
            </Typography>
            <Box sx={{ display: 'grid', gap: 1, mt: 1 }}>
              {binaries.binaries.map((b) => (
                <Box
                  key={targetKey(b)}
                  sx={{ display: 'flex', alignItems: 'center', gap: 2, flexWrap: 'wrap' }}
                >
                  <Typography sx={{ minWidth: 160 }}>{targetLabel(b)}</Typography>
                  <Typography variant="caption" color="text.secondary">
                    {b.filename}
                  </Typography>
                  <Typography variant="caption" color="text.secondary">
                    {Math.round(b.size / 1048576)} MB
                  </Typography>
                  <Button size="small" variant="outlined" onClick={() => download(b)}>
                    {t.agentDownloadButton}
                  </Button>
                  <Typography variant="caption" color="text.secondary" title={b.sha256}>
                    {t.agentDownloadChecksum}: {b.sha256.slice(0, 12)}…
                  </Typography>
                </Box>
              ))}
            </Box>
            {(selectedBinary ?? binaries.binaries[0]) && (
              <Box sx={{ mt: 1 }}>
                <CurlRow
                  command={curlCommand(
                    base,
                    '<AGENT_TOKEN>',
                    (selectedBinary ?? binaries.binaries[0])!,
                  )}
                  copyLabel={t.agentDownloadCopyCurl}
                  testId="agent-curl-generic"
                />
              </Box>
            )}
          </>
        )}
        {status?.config && (
          <>
            <Box sx={{ mt: 1 }}>
              <Button
                size="small"
                variant="outlined"
                onClick={() => downloadConfig('<AGENT_TOKEN>')}
              >
                {t.agentDownloadConfig}
              </Button>
            </Box>
            <Box sx={{ mt: 1 }}>
              <CurlRow
                command={configCurlCommand(configDownloadBase, '<AGENT_TOKEN>')}
                copyLabel={t.agentDownloadCopyCurl}
              />
            </Box>
          </>
        )}
      </Box>
    </Panel>
  );
}
