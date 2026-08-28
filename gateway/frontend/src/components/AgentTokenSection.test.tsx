// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/react';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { AgentTokenSection, buildServerAgentConfig } from './AgentTokenSection';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { PortalServer } from '../api';

const t = messages.de;

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const server = { id: 'srv-1', name: 'GPU-Box' } as unknown as PortalServer;
const configMaterial = {
  gateway_url: 'https://gateway.mesh.test:9443',
  ca_file: '',
  ca_cache_file: 'server-agent-ca.pem',
  ca_pem: '-----BEGIN CERTIFICATE-----\nPUBLIC-ROOT\n-----END CERTIFICATE-----\n',
};
const configDownloadBase = 'https://gateway.mesh.test:9443';

function makeApi(overrides: Record<string, unknown> = {}) {
  return {
    agentTokenStatus: vi.fn(async () => ({
      exists: false,
      config: configMaterial,
      agent_download_base: configDownloadBase,
    })),
    generateAgentToken: vi.fn(async () => ({
      secret: 'sk-agent-XYZ',
      token: {
        exists: true,
        secret_prefix: 'sk-agen',
        config: configMaterial,
        agent_download_base: configDownloadBase,
      },
    })),
    revokeAgentToken: vi.fn(async () => ({ ok: true })),
    // Always mocked: the per-server agent-presence-timeout field fetches the
    // live system default + saves via updateServer on every mount.
    agentPresenceTimeout: vi.fn(async () => ({ seconds: 15 })),
    updateServer: vi.fn(
      async (id: string, body: Record<string, unknown>) =>
        ({ id, ...body }) as unknown as PortalServer,
    ),
    // Always mocked: the download section + reveal-dialog curl fetch the agent
    // binary manifest on every mount. Empty by default (no download section
    // rendered) — tests that need it pass an override.
    agentBinaries: vi.fn(async () => ({
      agent_version: '',
      go_version: '',
      built_at: '',
      binaries: [],
      netbird_agent_download_only: false,
      agent_download_base: '',
    })),
    downloadAgentBinary: vi.fn(async () => new Blob()),
    ...overrides,
  };
}

function renderSection(api: ReturnType<typeof makeApi>) {
  return render(
    <ToastProvider>
      <AgentTokenSection t={t} api={api} server={server} />
    </ToastProvider>,
  );
}

describe('AgentTokenSection copy button', () => {
  it('has no copy button before a token is generated', async () => {
    const api = makeApi();
    renderSection(api);
    await waitFor(() => expect(api.agentTokenStatus).toHaveBeenCalled());
    expect(screen.queryByLabelText(t.agentTokenCopy)).toBeNull();
  });

  it('reveals the secret and a copy button after generate', async () => {
    const api = makeApi();
    renderSection(api);
    await waitFor(() => expect(api.agentTokenStatus).toHaveBeenCalled());

    fireEvent.click(screen.getByText(t.agentTokenGenerate));

    expect(await screen.findByText('sk-agent-XYZ')).toBeInTheDocument();
    expect(screen.getByLabelText(t.agentTokenCopy)).toBeInTheDocument();
  });

  it('copies the revealed secret to the clipboard', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });

    const api = makeApi();
    renderSection(api);
    await waitFor(() => expect(api.agentTokenStatus).toHaveBeenCalled());

    fireEvent.click(screen.getByText(t.agentTokenGenerate));
    expect(await screen.findByLabelText(t.agentTokenCopy)).toBeInTheDocument();

    fireEvent.click(screen.getByLabelText(t.agentTokenCopy));
    expect(writeText).toHaveBeenCalledWith('sk-agent-XYZ');
  });
});

// The mode field is a non-native MUI Select (combobox), not a <select>; options
// render in a portal on document.body (mirrors ApplicationSection's health-mode
// picker helper).
async function selectPresenceMode(optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: t.serverAgentPresenceTimeoutLabel }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
}

describe('AgentTokenSection agent-presence-timeout field', () => {
  function makeFullApi(overrides: Record<string, unknown> = {}) {
    return {
      ...makeApi(),
      agentPresenceTimeout: vi.fn(async () => ({ seconds: 15 })),
      updateServer: vi.fn(
        async (id: string, body: Record<string, unknown>) =>
          ({ id, ...body }) as unknown as PortalServer,
      ),
      ...overrides,
    };
  }

  function renderWithServer(srv: PortalServer, api: ReturnType<typeof makeFullApi>) {
    return render(
      <ToastProvider>
        <AgentTokenSection t={t} api={api} server={srv} />
      </ToastProvider>,
    );
  }

  it("defaults to 'Default' and shows the live system value when the server has no override", async () => {
    const api = makeFullApi();
    const srv = { ...server, agent_presence_timeout_seconds: 0 } as PortalServer;
    renderWithServer(srv, api);

    await waitFor(() => expect(api.agentPresenceTimeout).toHaveBeenCalled());
    expect(
      screen.getByRole('combobox', { name: t.serverAgentPresenceTimeoutLabel }),
    ).toHaveTextContent(t.serverAgentPresenceTimeoutDefault);
    expect(
      screen.queryByLabelText(t.serverAgentPresenceTimeoutSecondsLabel),
    ).not.toBeInTheDocument();
    // The note paragraph carries the live system value; match on its own text
    // content + tag (its ancestors also "contain" the substring via bubbling).
    expect(
      await screen.findByText(
        (_content, el) =>
          el?.tagName.toLowerCase() === 'p' &&
          (el.textContent ?? '').includes(`${t.serverAgentPresenceTimeoutCurrent}: 15`),
      ),
    ).toBeTruthy();
  });

  it("shows 'Custom' with the stored value when the server has an override", async () => {
    const api = makeFullApi();
    const srv = { ...server, agent_presence_timeout_seconds: 42 } as PortalServer;
    renderWithServer(srv, api);

    expect(
      await screen.findByRole('combobox', { name: t.serverAgentPresenceTimeoutLabel }),
    ).toHaveTextContent(t.serverAgentPresenceTimeoutCustom);
    const secondsField = screen.getByLabelText(
      t.serverAgentPresenceTimeoutSecondsLabel,
    ) as HTMLInputElement;
    expect(secondsField.value).toBe('42');
  });

  it('saves the custom seconds via updateServer', async () => {
    const api = makeFullApi();
    const srv = { ...server, agent_presence_timeout_seconds: 0 } as PortalServer;
    renderWithServer(srv, api);

    await selectPresenceMode(t.serverAgentPresenceTimeoutCustom);
    const secondsField = screen.getByLabelText(
      t.serverAgentPresenceTimeoutSecondsLabel,
    ) as HTMLInputElement;
    fireEvent.change(secondsField, { target: { value: '42' } });

    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() =>
      expect(api.updateServer).toHaveBeenCalledWith('srv-1', {
        agent_presence_timeout_seconds: 42,
      }),
    );
  });

  it('saves 0 (follow system) when switched back to Default', async () => {
    const api = makeFullApi();
    const srv = { ...server, agent_presence_timeout_seconds: 42 } as PortalServer;
    renderWithServer(srv, api);

    await selectPresenceMode(t.serverAgentPresenceTimeoutDefault);
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() =>
      expect(api.updateServer).toHaveBeenCalledWith('srv-1', { agent_presence_timeout_seconds: 0 }),
    );
  });
});

describe('AgentTokenSection agent-binary downloads', () => {
  const binariesManifest = {
    agent_version: '0.1.0',
    go_version: 'go1.26',
    built_at: '2026-08-07T00:00:00Z',
    binaries: [
      {
        os: 'linux',
        arch: 'amd64',
        filename: 'server-agent-linux-amd64',
        size: 7000000,
        sha256: 'aa',
      },
      {
        os: 'windows',
        arch: 'amd64',
        filename: 'server-agent-windows-amd64.exe',
        size: 7400000,
        sha256: 'bb',
      },
    ],
    netbird_agent_download_only: false,
    agent_download_base: '',
  };

  it('lists downloads and builds a curl command for the picked system', async () => {
    const api = makeApi({
      agentBinaries: vi.fn(async () => binariesManifest),
      generateAgentToken: vi.fn(async () => ({
        secret: 'op_agt_secret',
        token: {
          exists: true,
          secret_prefix: 'op_agt',
          config: configMaterial,
          agent_download_base: configDownloadBase,
        },
      })),
    });
    renderSection(api);

    // download section lists both targets (by filename)
    expect(await screen.findByText('server-agent-linux-amd64')).toBeInTheDocument();
    expect(screen.getByText('server-agent-windows-amd64.exe')).toBeInTheDocument();

    // generate → reveal dialog shows a curl command with the real token + origin
    fireEvent.click(screen.getByText(t.agentTokenGenerate));
    const curl = await screen.findByTestId('agent-curl');
    expect(curl.textContent).toContain('Bearer op_agt_secret');
    expect(curl.textContent).toContain('/api/agent/v1/download/linux-amd64');
    expect(curl.textContent).toContain('-o server-agent && chmod +x server-agent');

    // switch the OS dropdown to windows → .exe form, no chmod. The picker is a
    // non-native MUI Select (combobox), not a <select> — mirrors the
    // selectPresenceMode helper above (mouseDown → click the rendered option).
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.agentDownloadSelectSystem }));
    fireEvent.click(await screen.findByRole('option', { name: 'Windows (x86-64)' }));
    expect(screen.getByTestId('agent-curl').textContent).toContain('-o server-agent.exe');
    expect(screen.getByTestId('agent-curl').textContent).not.toContain('chmod');
  });

  it('offers a server-agent.json download when binaries exist', async () => {
    const api = makeApi({ agentBinaries: vi.fn(async () => binariesManifest) });
    renderSection(api);
    expect(await screen.findByText(t.agentDownloadConfig)).toBeInTheDocument();
  });

  it('shows copyable curl commands for the binary and the config', async () => {
    const meshDownloadBase = 'http://gateway.mesh.test:8081';
    const api = makeApi({
      agentTokenStatus: vi.fn(async () => ({
        exists: false,
        config: configMaterial,
        agent_download_base: meshDownloadBase,
      })),
      agentBinaries: vi.fn(async () => ({
        ...binariesManifest,
        agent_download_base: meshDownloadBase,
      })),
      generateAgentToken: vi.fn(async () => ({
        secret: 'op_agt_secret',
        token: {
          exists: true,
          secret_prefix: 'op_agt',
          config: configMaterial,
          agent_download_base: meshDownloadBase,
        },
      })),
    });
    renderSection(api);

    // Persistent section (no token yet): a config-download curl with the placeholder,
    // and each curl carries a copy button (the generic binary curl + the config curl).
    const persistentConfig = await screen.findAllByText((c) =>
      c.includes('/api/agent/v1/download/config'),
    );
    expect(persistentConfig.some((el) => el.textContent?.includes('<AGENT_TOKEN>'))).toBe(true);
    expect(persistentConfig.some((el) => el.textContent?.includes(meshDownloadBase))).toBe(true);
    expect(screen.getAllByLabelText(t.agentDownloadCopyCurl).length).toBeGreaterThanOrEqual(2);

    // Generate → the reveal dialog's config curl carries the REAL token.
    fireEvent.click(screen.getByText(t.agentTokenGenerate));
    await screen.findByTestId('agent-curl');
    const allConfig = screen.getAllByText((c) => c.includes('/api/agent/v1/download/config'));
    expect(allConfig.some((el) => el.textContent?.includes('Bearer op_agt_secret'))).toBe(true);
    expect(allConfig.some((el) => el.textContent?.includes(meshDownloadBase))).toBe(true);
  });

  it('buildServerAgentConfig is annotated JSONC with defaults + websocket transport', () => {
    const raw = buildServerAgentConfig(
      { ...configMaterial, gateway_url: 'https://gw.example' },
      'sk-abc',
    );
    // It carries English // comments (JSONC) — the agent strips whole-line comments.
    expect(raw).toContain('// Telemetry transport:');
    expect(raw).toContain('// The gateway base URL');
    // Stripping whole-line // comments (as the agent does) must yield valid JSON,
    // and the // inside the https:// URL value must survive.
    const stripped = raw
      .split('\n')
      .filter((l) => !l.trim().startsWith('//'))
      .join('\n');
    const cfg = JSON.parse(stripped);
    expect(cfg.gateway_url).toBe('https://gw.example');
    expect(cfg.token).toBe('sk-abc');
    expect(cfg.transport).toBe('websocket');
    expect(cfg.interval).toBe('1s');
    expect(cfg.system_report_interval).toBe('30m');
    expect(cfg.tls_insecure).toBe(false);
    expect(cfg.verbose).toBe(false);
    // optional endpoints are present but empty (disabled by default)
    expect(cfg.metrics_url).toBe('');
    expect(cfg.model_status_url).toBe('');
    expect(cfg.model_status_format).toBe('');
    expect(cfg.lhm_url).toBe('');
    // Phase 2 certificate-installation keys: present, defaulted to off/empty (a
    // no-op configuration — an agent that never touches this stanza behaves
    // exactly as before).
    expect(cfg.cert_mode).toBe('off');
    expect(cfg.cert_dir).toBe('');
    expect(cfg.cert_reload_command).toBe('');
    expect(cfg.cert_poll_interval).toBe('');
    // The cert_reload_command comment must state the hard boundary explicitly:
    // this value comes only from the local file, never from the gateway.
    expect(raw).toContain('comes ONLY from this local file');
    expect(raw).toContain('the gateway can never deliver');
    // Windows guidance for the reload command.
    expect(raw).toContain('quote-free');
    // The operator-only half of the managed-runtime router bind contract: the
    // gateway supplies the router PORT, this file decides its bind host, and an
    // empty value means the agent falls back to ALL INTERFACES. A generated
    // config that never mentions the key leaves that decision invisible to the
    // one person making it.
    expect(cfg.runtime_router_bind).toBe('');
    expect(raw).toContain('the gateway supplies the router');
    // The other five managed-runtime keys. Every one of them used to be
    // MISSING while this template's runtime_router_bind comment referred the
    // operator to the README for them — the worst possible split, because the
    // key that was present is the one whose empty value is identical to being
    // absent, and the one most conspicuously absent (the binary allowlist) is
    // the one whose absence stops every model process by design.
    expect(cfg.runtime_source).toBe('gateway');
    expect(cfg.runtime_config).toBe('');
    expect(cfg.runtime_allowed_binaries).toEqual([]);
    expect(cfg.runtime_allowed_dirs).toEqual([]);
    expect(cfg.runtime_cache).toBe('');
    // The two empty-value semantics are opposite, and the comments — not the
    // values, which a generated template cannot guess — are what carry that.
    expect(raw).toContain('EMPTY (the default) MEANS NOTHING MAY START');
    expect(raw).toContain('ANY work_dir is accepted');
    // A user hit this while configuring a WINDOWS server, so the binary
    // allowlist names an example for both platforms (the same reason the
    // portal's own binary-path error message was reworded).
    expect(raw).toContain('/opt/llama/llama-server');
    expect(raw).toContain('C:/llama/llama-server.exe');
    // cert_mode "proxy" is implemented and wired (server-agent/main.go builds
    // proxy.New/NewRoutesClient/NewDriver for it); this template used to tell
    // the operator it was not.
    expect(raw).not.toContain('NOT IMPLEMENTED YET');
    expect(cfg.cert_proxy_routes).toEqual([]);
    expect(cfg.cert_proxy_routes_mode).toBe('fallback');
  });

  // The frontend half of the cross-copy drift guard. One JSONC template is
  // duplicated in two producers that cannot share code — this function and the
  // Go backend's buildAgentConfigJSON
  // (gateway/backend/internal/gateway/agent_binaries.go) — plus two consumers
  // that must agree with it: server-agent's fileConfig, which decides what the
  // agent can actually read, and the server-agent README, which documents it.
  //
  // Each producer used to pin only its OWN key set, so neither test could see
  // the other template and the two could disagree indefinitely. They did: this
  // copy carried a comment naming five runtime settings while emitting none of
  // them, and the one runtime key it did emit was the only one whose absence is
  // indistinguishable from its empty value.
  //
  // Both producers are now compared byte for byte against ONE generated golden,
  // server-agent/testdata/server-agent.config.jsonc — so a change on either
  // side fails on the other until they match. The golden's key set is in turn
  // checked against every `json` tag on fileConfig by reflection
  // (server-agent/internal/config/config_test.go), which is what catches a
  // setting the agent can read but neither template mentions. No hand-
  // maintained key list survives anywhere in that chain.
  //
  // Regenerate the golden after a deliberate template change:
  //   cd gateway/backend && go test ./internal/gateway \
  //     -run TestBuildAgentConfigJSONMatchesSharedGolden -update-agent-config-golden
  it('buildServerAgentConfig is byte-for-byte the shared golden (cross-copy drift guard)', () => {
    // The golden's fixed inputs, identical to the Go side's: plain ASCII with
    // none of `<`, `>` or `&`, the only characters Go's json.Marshal escapes
    // and JSON.stringify does not — which is what makes a byte comparison
    // between a Go and a TypeScript producer meaningful at all.
    const raw = buildServerAgentConfig(
      { gateway_url: 'https://gw.example.test', ca_file: '', ca_cache_file: '', ca_pem: '' },
      'fixture-token',
    );
    const goldenPath = path.resolve(
      path.dirname(fileURLToPath(import.meta.url)),
      '../../../../server-agent/testdata/server-agent.config.jsonc',
    );
    const golden = fs.readFileSync(goldenPath, 'utf8');
    // Reported as a line-by-line first difference rather than a whole-file
    // diff: the template is ~140 lines and a raw mismatch dump buries which
    // key drifted.
    const got = raw.split('\n');
    const want = golden.split('\n');
    for (let i = 0; i < Math.max(got.length, want.length); i++) {
      expect(
        got[i] ?? '<end of file>',
        `buildServerAgentConfig has drifted from ${goldenPath} at line ${i + 1}. If the change is deliberate, update the Go copy in gateway/backend/internal/gateway/agent_binaries.go and regenerate the golden (go test ./internal/gateway -run TestBuildAgentConfigJSONMatchesSharedGolden -update-agent-config-golden).`,
      ).toBe(want[i] ?? '<end of file>');
    }
    // And the whole document, so a difference the loop cannot express (a
    // trailing newline) still fails.
    expect(raw).toBe(golden);
  });

  it('uses token.config even when the binary manifest is unavailable', async () => {
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    const createObjectURL = vi.fn((_blob: Blob) => 'blob:agent-config');
    Object.defineProperty(window.URL, 'createObjectURL', {
      configurable: true,
      value: createObjectURL,
    });
    Object.defineProperty(window.URL, 'revokeObjectURL', { configurable: true, value: vi.fn() });
    const api = makeApi({
      agentTokenStatus: vi.fn(async () => ({
        exists: true,
        secret_prefix: 'op_agt',
        config: configMaterial,
        agent_download_base: configDownloadBase,
      })),
      agentBinaries: vi.fn(async () => {
        throw new Error('404 agent.binaries_unavailable');
      }),
    });
    renderSection(api);

    await waitFor(() => expect(api.agentTokenStatus).toHaveBeenCalled());
    const configCurls = await screen.findAllByText((content) =>
      content.includes('/api/agent/v1/download/config'),
    );
    expect(configCurls.some((element) => element.textContent?.includes(configDownloadBase))).toBe(
      true,
    );
    fireEvent.click(await screen.findByRole('button', { name: t.agentDownloadConfig }));
    expect(createObjectURL).toHaveBeenCalledTimes(1);
    const blob = createObjectURL.mock.calls[0]?.[0] as Blob;
    const raw = await new Promise<string>((resolve, reject) => {
      const reader = new FileReader();
      reader.onerror = () => reject(reader.error);
      reader.onload = () => resolve(String(reader.result));
      reader.readAsText(blob);
    });
    const cfg = JSON.parse(
      raw
        .split('\n')
        .filter((line) => !line.trim().startsWith('//'))
        .join('\n'),
    );
    expect(cfg.gateway_url).toBe(configMaterial.gateway_url);
    expect(cfg.ca_cache_file).toBe(configMaterial.ca_cache_file);
    expect(cfg.ca_pem).toBe(configMaterial.ca_pem);
  });

  it('emits ca_file, ca_cache_file and ca_pem from one config object', () => {
    const raw = buildServerAgentConfig(configMaterial, 'sk-abc');
    const cfg = JSON.parse(
      raw
        .split('\n')
        .filter((line) => !line.trim().startsWith('//'))
        .join('\n'),
    );
    expect(cfg.ca_file).toBe(configMaterial.ca_file);
    expect(cfg.ca_cache_file).toBe(configMaterial.ca_cache_file);
    expect(cfg.ca_pem).toBe(configMaterial.ca_pem);
  });
});
