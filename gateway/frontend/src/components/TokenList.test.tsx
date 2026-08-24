// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TokenList } from './TokenList';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import {
  PortalApiError,
  type CreateTokenRequest,
  type ModelOption,
  type PortalServer,
  type PortalToken,
  type ProjectRef,
  type ServerModelOption,
} from '../api';

const t = messages.de;

const models = [{ id: 'gpt-oss-20b', display_name: 'gpt-oss-20b', flavors: ['openai'] }];
const defaultProjects: ProjectRef[] = [
  { id: 'proj_a', name: 'Project A' },
  { id: 'proj_b', name: 'Project B' },
];

function makeServer(overrides: Partial<PortalServer> = {}): PortalServer {
  return {
    id: 'srv_a',
    name: 'Server A',
    domain: 'srv-a.example.test',
    server_path_suffix: '',
    status: 'active',
    health_status: 'healthy',
    owners: [],
    last_seen_at: null,
    created_at: '2026-08-12T12:00:00Z',
    netbird_enabled: false,
    netbird_setup_key_id: '',
    netbird_group_id: '',
    netbird_peer_id: '',
    netbird_connected: false,
    netbird_group_ids: [],
    netbird_peer_managed: false,
    netbird_policy_override: '',
    netbird_allow_ping: false,
    netbird_ping_exclude: false,
    agent_status: 'unconfigured',
    agent_presence_timeout_seconds: 0,
    estimated_watts: 0,
    idle_watts: 0,
    price_per_kwh: 0,
    pue: 0,
    price_unit: 'eur_cent',
    admin_groups: [],
    system_group_id: '',
    system_group_name: '',
    ...overrides,
  };
}

function makeToken(overrides: Partial<PortalToken> = {}): PortalToken {
  return {
    id: 'tok_1',
    name: 'Dev Token',
    secret_prefix: 'dev-secr',
    status: 'active',
    scopes: ['gateway:use'],
    expires_at: null,
    last_used_at: null,
    created_at: '2026-07-10T12:00:00Z',
    model_override: '',
    log_communication: false,
    secret: false,
    is_chat_session: false,
    deletable: true,
    ...overrides,
  };
}

function makeChatSessionRow(overrides: Partial<PortalToken> = {}): PortalToken {
  return makeToken({
    id: 'chat-session',
    name: 'chat-session',
    secret_prefix: '',
    scopes: [],
    model_override: '',
    is_chat_session: true,
    deletable: false,
    ...overrides,
  });
}

function renderTokenList(opts: {
  tokens?: PortalToken[];
  created?: CreateTokenRequest[];
  myProjects?: ProjectRef[];
  servers?: PortalServer[];
  serverModelsByServer?: Record<string, ServerModelOption[]>;
  // Override the default single-model list — used by the unknown-model
  // redirect's fallback-picker test, which needs a group entry (is_group)
  // alongside a plain model in the SAME list (Task 8).
  models?: ModelOption[];
}) {
  const created = opts.created ?? [];
  const tokens = opts.tokens ?? [];
  const servers = opts.servers ?? [];
  const setTokens = vi.fn();
  const fakeApi = {
    createToken: vi.fn(async (body: CreateTokenRequest) => {
      created.push(body);
      const token = makeToken({
        id: 'tok_created',
        name: body.name,
        scopes: body.scopes,
        model_override: body.model_override ?? '',
        log_communication: body.log_communication ?? false,
      });
      return { token, secret: 'opaigw_created_secret' };
    }),
    updateToken: vi.fn(async (id: string, body: Record<string, unknown>) =>
      makeToken({ id, name: (body.name as string) ?? '', ...body }),
    ),
    deleteToken: vi.fn(),
    rotateToken: vi.fn(async (id: string) => ({
      token: makeToken({ id, secret_prefix: 'opaigw_new' }),
      secret: 'opaigw_rotated_secret',
    })),
    updateChatSettings: vi.fn(async () => ({
      id: 'u',
      email: 'e',
      display_name: 'd',
      role: 'admin',
      preferred_language: 'de',
      totp_enabled: false,
      totp_mode: '',
      system_admin_mode: false,
      system_admin_mode_expires_at: '',
      system_admin_mode_require_password: false,
    })),
    // The token form's project picker (Task 8, spec §6) loads the caller's own
    // eligible projects each time the form opens; default to a fixed set so
    // pre-existing tests (which never touch the picker) stay stable.
    myProjects: vi.fn(async () => opts.myProjects ?? defaultProjects),
    // The server-override picker's filtered-model fetch (Task 6, step 2).
    serverModels: vi.fn(async (serverId: string) => opts.serverModelsByServer?.[serverId] ?? []),
  };

  render(
    <ToastProvider>
      <TokenList
        t={t}
        api={fakeApi}
        tokens={tokens}
        setTokens={setTokens}
        role="admin"
        models={opts.models ?? models}
        servers={servers}
      />
    </ToastProvider>,
  );

  return { fakeApi, created, setTokens };
}

// The create form is now a sub-view: click the page-level "Token erstellen"
// button to open the input mask, then interact with the form fields.
function openCreate() {
  fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));
}

// The edit form is the same sub-view mask, opened via a row's inline Edit icon.
function openEdit() {
  fireEvent.click(screen.getByRole('button', { name: t.tokenActionEdit }));
}

// SearchableSelect renders a non-native MUI Select (role="combobox"): open the
// named combobox, then click the option (options render in a portal). Used for the
// catch-all select and each per-row gateway-model select.
async function selectOption(comboName: string | RegExp, optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: comboName }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
}

afterEach(cleanup);

describe('TokenList model override map + catch-all + log communication', () => {
  it('starts with no mapping rows and can add one (a requested field + a gateway select)', async () => {
    renderTokenList({});
    openCreate();
    // No per-model rows initially.
    expect(
      screen.queryByRole('textbox', { name: `${t.tokenOverrideFromLabel} 1` }),
    ).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.tokenOverrideAddRow }));
    expect(
      screen.getByRole('textbox', { name: `${t.tokenOverrideFromLabel} 1` }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('combobox', { name: `${t.tokenOverrideToLabel} 1` }),
    ).toBeInTheDocument();
  });

  it('disables submit while a mapping row is half-filled and re-enables once completed', async () => {
    renderTokenList({});
    openCreate();
    const submit = screen.getByRole('button', { name: t.tokenCreate });
    expect(submit).not.toBeDisabled();

    // A row with only the requested side filled is incomplete -> block submit.
    fireEvent.click(screen.getByRole('button', { name: t.tokenOverrideAddRow }));
    fireEvent.change(screen.getByRole('textbox', { name: `${t.tokenOverrideFromLabel} 1` }), {
      target: { value: 'gpt-4o' },
    });
    expect(submit).toBeDisabled();

    // Completing the row (pick a gateway model) re-enables submit.
    await selectOption(`${t.tokenOverrideToLabel} 1`, 'gpt-oss-20b');
    expect(submit).not.toBeDisabled();
  });

  it('submits model_override_map + catch-all + log_communication when creating a token', async () => {
    const { fakeApi, created } = renderTokenList({});
    openCreate();

    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), { target: { value: 'New Token' } });
    // One mapping row: gpt-4o -> gpt-oss-20b, offered as its own model name.
    fireEvent.click(screen.getByRole('button', { name: t.tokenOverrideAddRow }));
    fireEvent.change(screen.getByRole('textbox', { name: `${t.tokenOverrideFromLabel} 1` }), {
      target: { value: 'gpt-4o' },
    });
    await selectOption(`${t.tokenOverrideToLabel} 1`, 'gpt-oss-20b');
    fireEvent.click(screen.getByRole('checkbox', { name: t.tokenOverrideOffer }));
    // Catch-all + log communication.
    await selectOption(t.tokenOverrideCatchAllLabel, 'gpt-oss-20b');
    fireEvent.click(screen.getByLabelText(t.tokenLogCommunicationLabel));

    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
    expect(created[0]).toMatchObject({
      model_override: 'gpt-oss-20b',
      model_override_map: {
        'gpt-4o': { to: 'gpt-oss-20b', offer: true, hide_target: false },
      },
      log_communication: true,
    });
  });

  it('drops fully-empty rows from the submitted map', async () => {
    const { fakeApi, created } = renderTokenList({});
    openCreate();
    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), { target: { value: 'T' } });
    // Add an empty row and leave it empty -> should be dropped (map stays empty).
    fireEvent.click(screen.getByRole('button', { name: t.tokenOverrideAddRow }));
    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
    expect(created[0].model_override_map).toEqual({});
  });

  it('renders the override summary (mappings + catch-all) or a dash in the read-only column', () => {
    renderTokenList({
      tokens: [
        makeToken({
          id: 'tok_a',
          name: 'A',
          model_override: 'gpt-oss-20b',
          model_override_map: {
            'gpt-4o': { to: 'gpt-oss-20b', offer: false, hide_target: false },
          },
        }),
        makeToken({ id: 'tok_b', name: 'B', model_override: '', model_override_map: {} }),
      ],
    });

    // "gpt-4o→gpt-oss-20b, Rest→gpt-oss-20b" (Rest = catch-all short label).
    expect(
      screen.getByRole('cell', {
        name: `gpt-4o→gpt-oss-20b, ${t.tokenOverrideCatchAllShort}→gpt-oss-20b`,
      }),
    ).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: '-' })).toBeInTheDocument();
  });

  it('submits secret:true when creating a token with Geheim checked (and false by default)', async () => {
    const { fakeApi, created } = renderTokenList({});
    openCreate();

    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), {
      target: { value: 'Secret Token' },
    });
    fireEvent.click(screen.getByLabelText(t.tokenSecretLabel));

    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
    expect(created[0]).toMatchObject({ secret: true });
  });

  it('defaults secret to false when creating a token without checking Geheim', async () => {
    const { fakeApi, created } = renderTokenList({});
    openCreate();

    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), { target: { value: 'Plain Token' } });
    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
    expect(created[0]).toMatchObject({ secret: false });
  });

  it('submits secret when editing a token and seeds the current value', async () => {
    const { fakeApi } = renderTokenList({
      tokens: [makeToken({ id: 'tok_edit', name: 'Edit Me', secret: false })],
    });

    openEdit();

    // In the edit sub-view the form fields carry their plain labels (the mask
    // fully replaces the list, so there is no clash to disambiguate).
    fireEvent.click(screen.getByLabelText(t.tokenSecretLabel));
    fireEvent.click(screen.getByRole('button', { name: t.tokenActionSave }));

    await waitFor(() => expect(fakeApi.updateToken).toHaveBeenCalled());
    const [, body] = (fakeApi.updateToken as unknown as { mock: { calls: unknown[][] } }).mock
      .calls[0];
    expect(body).toMatchObject({ secret: true });
  });

  it('seeds existing map rows (incl. both switches) on edit and submits the updated map + catch-all', async () => {
    const { fakeApi } = renderTokenList({
      tokens: [
        makeToken({
          id: 'tok_edit',
          name: 'Edit Me',
          model_override: '',
          model_override_map: {
            claude: { to: 'gpt-oss-20b', offer: false, hide_target: true },
          },
          log_communication: false,
        }),
      ],
    });

    openEdit();

    // The existing map entry is seeded as row 1, hide-target checked and
    // offer unchecked per the seeded wire value.
    expect(screen.getByRole('textbox', { name: `${t.tokenOverrideFromLabel} 1` })).toHaveValue(
      'claude',
    );
    expect(screen.getAllByRole('checkbox', { name: t.tokenOverrideOffer })[0]).not.toBeChecked();
    expect(screen.getAllByRole('checkbox', { name: t.tokenOverrideHideTarget })[0]).toBeChecked();

    // Add a second mapping + a catch-all + toggle log communication.
    fireEvent.click(screen.getByRole('button', { name: t.tokenOverrideAddRow }));
    fireEvent.change(screen.getByRole('textbox', { name: `${t.tokenOverrideFromLabel} 2` }), {
      target: { value: 'gpt-4o' },
    });
    await selectOption(`${t.tokenOverrideToLabel} 2`, 'gpt-oss-20b');
    await selectOption(t.tokenOverrideCatchAllLabel, 'gpt-oss-20b');
    fireEvent.click(screen.getByLabelText(t.tokenLogCommunicationLabel));

    fireEvent.click(screen.getByRole('button', { name: t.tokenActionSave }));

    await waitFor(() => expect(fakeApi.updateToken).toHaveBeenCalled());
    const [, body] = (fakeApi.updateToken as unknown as { mock: { calls: unknown[][] } }).mock
      .calls[0];
    expect(body).toMatchObject({
      model_override: 'gpt-oss-20b',
      model_override_map: {
        claude: { to: 'gpt-oss-20b', offer: false, hide_target: true },
        'gpt-4o': { to: 'gpt-oss-20b', offer: false, hide_target: false },
      },
      log_communication: true,
    });
  });

  it('treats a missing switch on a seeded row as false, both on screen and on the next save', async () => {
    // A hand-written or pre-migration response might carry only `to`. The
    // editor must not crash, and re-saving the untouched row must resubmit
    // explicit `false`s rather than silently dropping the keys (which is what
    // would happen if the missing switches were passed through as `undefined`
    // instead of being defaulted).
    const legacyEntry = { to: 'gpt-oss-20b' } as unknown as {
      to: string;
      offer: boolean;
      hide_target: boolean;
    };
    const { fakeApi } = renderTokenList({
      tokens: [
        makeToken({
          id: 'tok_legacy',
          name: 'Legacy',
          model_override: '',
          model_override_map: { claude: legacyEntry },
        }),
      ],
    });

    openEdit();

    expect(screen.getAllByRole('checkbox', { name: t.tokenOverrideOffer })[0]).not.toBeChecked();
    expect(
      screen.getAllByRole('checkbox', { name: t.tokenOverrideHideTarget })[0],
    ).not.toBeChecked();

    fireEvent.click(screen.getByRole('button', { name: t.tokenActionSave }));

    await waitFor(() => expect(fakeApi.updateToken).toHaveBeenCalled());
    const [, body] = (fakeApi.updateToken as unknown as { mock: { calls: unknown[][] } }).mock
      .calls[0];
    expect(body).toMatchObject({
      model_override_map: { claude: { to: 'gpt-oss-20b', offer: false, hide_target: false } },
    });
  });
});

describe('TokenList project picker (spec: 2026-08-08-projects-design.md §6)', () => {
  it("loads the caller's own projects and sends the picked project_id on create", async () => {
    const { fakeApi, created } = renderTokenList({});
    openCreate();
    await waitFor(() => expect(fakeApi.myProjects).toHaveBeenCalled());

    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), {
      target: { value: 'Project Token' },
    });
    await selectOption(t.tokenProjectLabel, 'Project A');
    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
    expect(created[0]).toMatchObject({ project_id: 'proj_a' });
  });

  it("submits an empty project_id when the picker is left at its default '(no project)' option", async () => {
    const { fakeApi, created } = renderTokenList({});
    openCreate();
    await waitFor(() => expect(fakeApi.myProjects).toHaveBeenCalled());

    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), {
      target: { value: 'No Project Token' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
    expect(created[0]).toMatchObject({ project_id: '' });
  });

  it("seeds the picker from the token's current project on edit and submits the new pick", async () => {
    const { fakeApi } = renderTokenList({
      tokens: [
        makeToken({
          id: 'tok_proj',
          name: 'Has Project',
          project_id: 'proj_a',
          project_name: 'Project A',
        }),
      ],
    });

    openEdit();
    await waitFor(() => expect(fakeApi.myProjects).toHaveBeenCalled());
    expect(screen.getByRole('combobox', { name: t.tokenProjectLabel })).toHaveValue('Project A');

    await selectOption(t.tokenProjectLabel, 'Project B');
    fireEvent.click(screen.getByRole('button', { name: t.tokenActionSave }));

    await waitFor(() => expect(fakeApi.updateToken).toHaveBeenCalled());
    const [, body] = (fakeApi.updateToken as unknown as { mock: { calls: unknown[][] } }).mock
      .calls[0];
    expect(body).toMatchObject({ project_id: 'proj_b' });
  });

  it("clears the project attribution when picking '(no project)' on edit", async () => {
    const { fakeApi } = renderTokenList({
      tokens: [
        makeToken({
          id: 'tok_proj',
          name: 'Has Project',
          project_id: 'proj_a',
          project_name: 'Project A',
        }),
      ],
    });

    openEdit();
    await waitFor(() => expect(fakeApi.myProjects).toHaveBeenCalled());
    await selectOption(t.tokenProjectLabel, t.tokenProjectNone);
    fireEvent.click(screen.getByRole('button', { name: t.tokenActionSave }));

    await waitFor(() => expect(fakeApi.updateToken).toHaveBeenCalled());
    const [, body] = (fakeApi.updateToken as unknown as { mock: { calls: unknown[][] } }).mock
      .calls[0];
    expect(body).toMatchObject({ project_id: '' });
  });

  it('keeps a stale project attribution selectable/visible even when it dropped out of myProjects()', async () => {
    renderTokenList({
      tokens: [
        makeToken({
          id: 'tok_stale',
          name: 'Stale Project',
          project_id: 'proj_gone',
          project_name: 'Departed Project',
        }),
      ],
      myProjects: [{ id: 'proj_a', name: 'Project A' }],
    });

    openEdit();
    await screen.findByRole('combobox', { name: t.tokenProjectLabel });
    // The field still shows the current (now-foreign) attribution rather than
    // silently blanking it out.
    expect(screen.getByRole('combobox', { name: t.tokenProjectLabel })).toHaveValue(
      'Departed Project',
    );
  });

  it('shows a specific toast on a token.project_not_member 403', async () => {
    const fakeApi = {
      createToken: vi.fn(async () => {
        throw new PortalApiError(403, 'token.project_not_member', 'forbidden');
      }),
      myProjects: vi.fn(async () => defaultProjects),
      deleteToken: vi.fn(),
      rotateToken: vi.fn(),
      serverModels: vi.fn(async () => []),
      updateChatSettings: vi.fn(),
      updateToken: vi.fn(),
    };

    render(
      <ToastProvider>
        <TokenList
          t={t}
          api={fakeApi}
          tokens={[]}
          setTokens={vi.fn()}
          role="admin"
          models={models}
        />
      </ToastProvider>,
    );
    openCreate();
    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), { target: { value: 'T' } });
    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    expect(await screen.findByText(new RegExp(t.errorTokenProjectNotMember))).toBeInTheDocument();
  });
});

describe('TokenList synthetic ChatSession row', () => {
  // The ChatSession pseudo-token is now its own settings panel (a labelled
  // region), not a table row.
  function chatPanel() {
    return within(screen.getByRole('region', { name: t.chatSessionName }));
  }

  it('renders the localized name and hint, never the raw marker', () => {
    renderTokenList({
      tokens: [makeChatSessionRow(), makeToken({ id: 'tok_real', name: 'Real Token' })],
    });

    expect(screen.getByText(t.chatSessionName)).toBeInTheDocument();
    expect(screen.getByText(t.chatSessionHint)).toBeInTheDocument();
    // The stable backend marker "chat-session" must never surface as visible text.
    expect(screen.queryByText('chat-session')).not.toBeInTheDocument();
  });

  it('shows no edit / disable / delete actions for the ChatSession row', () => {
    renderTokenList({
      tokens: [makeChatSessionRow(), makeToken({ id: 'tok_real', name: 'Real Token' })],
    });

    const chat = chatPanel();
    expect(chat.queryByRole('button', { name: t.tokenActionEdit })).not.toBeInTheDocument();
    expect(chat.queryByRole('button', { name: t.tokenActionDelete })).not.toBeInTheDocument();
    expect(chat.queryByRole('button', { name: t.tokenActionDisable })).not.toBeInTheDocument();
    expect(chat.queryByRole('button', { name: t.tokenActionEnable })).not.toBeInTheDocument();

    // Real rows keep their normal actions (proves the branch is row-scoped).
    expect(screen.getByRole('button', { name: t.tokenActionEdit })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.tokenActionDelete })).toBeInTheDocument();
  });

  it('saves the two profile flags via updateChatSettings, not updateToken', async () => {
    const { fakeApi } = renderTokenList({
      tokens: [makeChatSessionRow({ log_communication: false, secret: false })],
    });

    const chat = chatPanel();
    fireEvent.click(chat.getByLabelText(`${t.tokenLogCommunicationLabel} ${t.chatSessionName}`));
    fireEvent.click(chat.getByLabelText(`${t.tokenSecretLabel} ${t.chatSessionName}`));
    fireEvent.click(chat.getByRole('button', { name: t.tokenActionSave }));

    await waitFor(() => expect(fakeApi.updateChatSettings).toHaveBeenCalled());
    expect(fakeApi.updateChatSettings).toHaveBeenCalledWith({
      log_communication: true,
      secret: true,
    });
    expect(fakeApi.updateToken).not.toHaveBeenCalled();
    expect(fakeApi.deleteToken).not.toHaveBeenCalled();
  });

  it("seeds the checkboxes from the row's current flags", () => {
    renderTokenList({
      tokens: [makeChatSessionRow({ log_communication: true, secret: false })],
    });

    const chat = chatPanel();
    expect(
      chat.getByLabelText(`${t.tokenLogCommunicationLabel} ${t.chatSessionName}`),
    ).toBeChecked();
    expect(chat.getByLabelText(`${t.tokenSecretLabel} ${t.chatSessionName}`)).not.toBeChecked();
  });
});

describe('TokenList one-time-secret reveal modal', () => {
  it('shows the created secret in a widened (md) dialog', async () => {
    renderTokenList({});
    openCreate();
    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), { target: { value: 'Reveal Me' } });
    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveClass('MuiDialog-paperWidthMd');
    expect(within(dialog).getByText('opaigw_created_secret')).toBeInTheDocument();
  });
});

describe('TokenList rotate action', () => {
  it('requires confirmation, then rotates and reveals the new secret', async () => {
    const { fakeApi } = renderTokenList({
      tokens: [makeToken({ id: 'tok_rot', name: 'Rotate Me' })],
    });

    // The row rotate icon is unambiguous before the dialog opens.
    fireEvent.click(screen.getByRole('button', { name: t.tokenActionRotate }));

    // Confirm is required: nothing fired yet, the confirm dialog is shown.
    expect(fakeApi.rotateToken).not.toHaveBeenCalled();
    const dialog = within(screen.getByRole('dialog'));
    expect(screen.getByText(t.tokenRotateConfirmTitle)).toBeInTheDocument();

    fireEvent.click(dialog.getByRole('button', { name: t.tokenRotateConfirm }));

    await waitFor(() => expect(fakeApi.rotateToken).toHaveBeenCalledWith('tok_rot'));
    // The existing one-time-secret reveal now shows the freshly rotated secret.
    expect(await screen.findByText('opaigw_rotated_secret')).toBeInTheDocument();
  });

  it('does not call rotateToken until the dialog is confirmed', () => {
    const { fakeApi } = renderTokenList({
      tokens: [makeToken({ id: 'tok_rot', name: 'Rotate Me' })],
    });
    fireEvent.click(screen.getByRole('button', { name: t.tokenActionRotate }));
    expect(fakeApi.rotateToken).not.toHaveBeenCalled();
  });

  it('shows no rotate action for the ChatSession pseudo row', () => {
    renderTokenList({
      tokens: [makeChatSessionRow(), makeToken({ id: 'tok_real', name: 'Real Token' })],
    });
    const chat = within(screen.getByRole('region', { name: t.chatSessionName }));
    expect(chat.queryByRole('button', { name: t.tokenActionRotate })).not.toBeInTheDocument();
    // The real row keeps its rotate action (proves the pseudo-row exclusion is real).
    expect(screen.getByRole('button', { name: t.tokenActionRotate })).toBeInTheDocument();
  });
});

describe('TokenList server override (Task 6)', () => {
  it('hides the whole control when the caller manages zero servers', () => {
    renderTokenList({});
    openCreate();
    expect(screen.queryByRole('combobox', { name: t.serverOverrideLabel })).not.toBeInTheDocument();
  });

  it('shows the picker (and no force checkbox yet) when the caller manages at least one server', () => {
    renderTokenList({ servers: [makeServer({ id: 'srv_a', name: 'Server A' })] });
    openCreate();
    expect(screen.getByRole('combobox', { name: t.serverOverrideLabel })).toBeInTheDocument();
    // The force checkbox only appears once a server is actually picked.
    expect(screen.queryByLabelText(t.serverOverrideForceLabel)).not.toBeInTheDocument();
  });

  it('labels a maintenance-status server, filters the model dropdown to its offered models once picked, and submits both fields', async () => {
    const { fakeApi, created } = renderTokenList({
      servers: [
        makeServer({ id: 'srv_a', name: 'Server A', status: 'active' }),
        makeServer({ id: 'srv_b', name: 'Server B', status: 'maintenance' }),
      ],
      serverModelsByServer: {
        srv_a: [{ id: 'server-only-model', display_name: 'server-only-model' }],
      },
    });
    openCreate();

    // The maintenance server is annotated in the option list.
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.serverOverrideLabel }));
    expect(
      await screen.findByRole('option', { name: `Server B (${t.statusMaintenance})` }),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole('option', { name: 'Server A' }));

    await waitFor(() => expect(fakeApi.serverModels).toHaveBeenCalledWith('srv_a'));
    // The force checkbox now renders (server override is set).
    expect(screen.getByLabelText(t.serverOverrideForceLabel)).toBeInTheDocument();

    // The model-override map's per-row "to" dropdown now offers ONLY the
    // override server's own model, not the full model list.
    fireEvent.click(screen.getByRole('button', { name: t.tokenOverrideAddRow }));
    fireEvent.mouseDown(screen.getByRole('combobox', { name: `${t.tokenOverrideToLabel} 1` }));
    expect(await screen.findByRole('option', { name: 'server-only-model' })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'gpt-oss-20b' })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('option', { name: 'server-only-model' }));
    fireEvent.change(screen.getByRole('textbox', { name: `${t.tokenOverrideFromLabel} 1` }), {
      target: { value: 'req' },
    });

    fireEvent.click(screen.getByLabelText(t.serverOverrideForceLabel));
    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), { target: { value: 'Overridden' } });
    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
    expect(created[0]).toMatchObject({
      server_override: 'srv_a',
      server_override_force_unreachable: true,
      model_override_map: { req: { to: 'server-only-model', offer: false, hide_target: false } },
    });
  });

  it("seeds the picker + checkbox from the token's current override on edit", async () => {
    const { fakeApi } = renderTokenList({
      servers: [makeServer({ id: 'srv_a', name: 'Server A' })],
      tokens: [
        makeToken({
          id: 'tok_edit',
          name: 'Edit Me',
          server_override: 'srv_a',
          server_override_force_unreachable: true,
        }),
      ],
    });

    openEdit();
    expect(screen.getByRole('combobox', { name: t.serverOverrideLabel })).toHaveValue('Server A');
    expect(screen.getByLabelText(t.serverOverrideForceLabel)).toBeChecked();

    fireEvent.click(screen.getByRole('button', { name: t.tokenActionSave }));

    await waitFor(() => expect(fakeApi.updateToken).toHaveBeenCalled());
    const [, body] = (fakeApi.updateToken as unknown as { mock: { calls: unknown[][] } }).mock
      .calls[0];
    expect(body).toMatchObject({
      server_override: 'srv_a',
      server_override_force_unreachable: true,
    });
  });
});

describe('TokenList unknown-model redirect (Task 8)', () => {
  it('keeps the sub-settings disabled until the redirect is on', () => {
    renderTokenList({});
    openCreate();
    expect(screen.getByRole('checkbox', { name: t.tokenUnknownRedirectBlocked })).toBeDisabled();
    expect(screen.getByLabelText(t.tokenUnknownFallback)).toBeDisabled();

    fireEvent.click(screen.getByRole('checkbox', { name: t.tokenUnknownRedirect }));

    expect(
      screen.getByRole('checkbox', { name: t.tokenUnknownRedirectBlocked }),
    ).not.toBeDisabled();
    expect(screen.getByLabelText(t.tokenUnknownFallback)).not.toBeDisabled();
  });

  it('shows the last used model in the edit form', () => {
    renderTokenList({ tokens: [makeToken({ last_used_model: 'qwen3-32b' })] });
    openEdit();
    expect(screen.getByText('qwen3-32b')).toBeInTheDocument();
  });

  it('shows a placeholder when the token has never been used', () => {
    renderTokenList({ tokens: [makeToken({ last_used_model: '' })] });
    openEdit();
    expect(screen.getByText(t.tokenLastUsedModelNone)).toBeInTheDocument();
  });

  it('always shows the placeholder in the create form (no last-used value yet)', () => {
    renderTokenList({});
    openCreate();
    expect(screen.getByText(t.tokenLastUsedModelNone)).toBeInTheDocument();
  });

  it('offers models and groups in one fallback picker', async () => {
    renderTokenList({
      models: [
        { id: 'qwen3-32b', display_name: 'qwen3-32b', flavors: [] },
        { id: 'fast-group', display_name: 'fast-group', flavors: [], is_group: true },
      ],
    });
    openCreate();
    fireEvent.click(screen.getByRole('checkbox', { name: t.tokenUnknownRedirect }));
    fireEvent.mouseDown(screen.getByLabelText(t.tokenUnknownFallback));
    expect(await screen.findByRole('option', { name: 'qwen3-32b' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'fast-group' })).toBeInTheDocument();
  });

  it('sends the redirect settings on submit', async () => {
    const { fakeApi, created } = renderTokenList({});
    openCreate();
    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), { target: { value: 'neu' } });
    fireEvent.click(screen.getByRole('checkbox', { name: t.tokenUnknownRedirect }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.tokenUnknownRedirectBlocked }));
    await selectOption(t.tokenUnknownFallback, 'gpt-oss-20b');
    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
    expect(created[0]).toMatchObject({
      unknown_model_redirect: true,
      unknown_model_redirect_blocked: true,
      unknown_model_fallback: 'gpt-oss-20b',
    });
  });

  it('never sends last_used_model on create, even blank', async () => {
    const { fakeApi, created } = renderTokenList({});
    openCreate();
    fireEvent.change(screen.getByLabelText(t.tokenNameLabel), { target: { value: 'neu' } });
    fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));

    await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
    expect(created[0]).not.toHaveProperty('last_used_model');
  });

  it('never resends last_used_model when saving an edit', async () => {
    const { fakeApi } = renderTokenList({
      tokens: [makeToken({ id: 'tok_edit', last_used_model: 'qwen3-32b' })],
    });
    openEdit();
    fireEvent.click(screen.getByRole('button', { name: t.tokenActionSave }));

    await waitFor(() => expect(fakeApi.updateToken).toHaveBeenCalled());
    const [, body] = (fakeApi.updateToken as unknown as { mock: { calls: unknown[][] } }).mock
      .calls[0];
    expect(body).not.toHaveProperty('last_used_model');
  });

  it("seeds the redirect settings from the token's current values on edit", async () => {
    const { fakeApi } = renderTokenList({
      tokens: [
        makeToken({
          id: 'tok_edit',
          unknown_model_redirect: true,
          unknown_model_redirect_blocked: true,
          unknown_model_fallback: 'gpt-oss-20b',
        }),
      ],
    });

    openEdit();
    expect(screen.getByRole('checkbox', { name: t.tokenUnknownRedirect })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: t.tokenUnknownRedirectBlocked })).toBeChecked();
    expect(screen.getByLabelText(t.tokenUnknownFallback)).toHaveValue('gpt-oss-20b');

    fireEvent.click(screen.getByRole('button', { name: t.tokenActionSave }));

    await waitFor(() => expect(fakeApi.updateToken).toHaveBeenCalled());
    const [, body] = (fakeApi.updateToken as unknown as { mock: { calls: unknown[][] } }).mock
      .calls[0];
    expect(body).toMatchObject({
      unknown_model_redirect: true,
      unknown_model_redirect_blocked: true,
      unknown_model_fallback: 'gpt-oss-20b',
    });
  });
});
