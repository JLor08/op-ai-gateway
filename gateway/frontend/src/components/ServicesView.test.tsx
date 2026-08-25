// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ServicesView } from './ServicesView';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import { EMPTY_LIMIT_CONFIG } from '../api';
import type {
  AdminGroupCandidate,
  CreateServiceRequest,
  ModelOption,
  PortalService,
  ServiceTokenDTO,
  UpdateServiceRequest,
} from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

const models: ModelOption[] = [
  { id: 'gpt-oss-20b', display_name: 'gpt-oss-20b', flavors: ['openai'] },
  { id: 'qwen-coder', display_name: 'qwen-coder', flavors: ['openai'] },
];

const adminCandidates = [
  {
    id: 'usr_full',
    email: 'full@example.test',
    display_name: 'Full Delegate',
    role: 'user',
    status: 'active',
    preferred_language: 'de',
    created_at: '2026-01-01T00:00:00Z',
    totp_enabled: false,
  },
  {
    id: 'usr_token',
    email: 'token@example.test',
    display_name: 'Token Delegate',
    role: 'user',
    status: 'active',
    preferred_language: 'de',
    created_at: '2026-01-01T00:00:00Z',
    totp_enabled: false,
  },
];

// A single admin-group candidate under a single system group (Phase C, spec
// 2026-08-10, mirrors ServerList.test.tsx's defaultAdminGroupCandidates) --
// the common case where the create form's picker auto-selects with no extra
// step, so every pre-existing create-flow test (which doesn't care about
// admin-group linkage) keeps the "Dienst erstellen" action reachable.
const defaultAdminGroupCandidates: AdminGroupCandidate[] = [
  {
    id: 'ag_default',
    name: 'Default Admin Group',
    parent_group_id: 'sg_default',
    parent_group_name: 'Default System Group',
  },
];

function makeService(overrides: Partial<PortalService> = {}): PortalService {
  return {
    id: 'svc_1',
    name: 'Nightly Batch',
    description: '',
    status: 'active',
    delegates: [],
    allowed_models: [],
    token_count: 0,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    limits: EMPTY_LIMIT_CONFIG,
    limits_usage: { requests_this_period: 0, tokens_this_period: 0, cost_this_period: 0 },
    admin_groups: [],
    system_group_id: '',
    system_group_name: '',
    ...overrides,
  };
}

function makeServiceToken(overrides: Partial<ServiceTokenDTO> = {}): ServiceTokenDTO {
  return {
    id: 'tok_new',
    service_id: 'svc_1',
    name: 'New Token',
    secret_prefix: 'svctok_new',
    status: 'active',
    scopes: ['llm:invoke'],
    expires_at: null,
    last_used_at: null,
    created_at: '2026-01-01T00:00:00Z',
    model_override: '',
    log_communication: false,
    secret: false,
    ...overrides,
  };
}

function renderServicesView(
  opts: {
    services?: PortalService[];
    role?: string;
    userId?: string;
    overrides?: Partial<PortalApi>;
    // Override the default two-model list — used by the unknown-model
    // redirect's fallback-picker test, which needs a group entry (is_group)
    // alongside a plain model in the SAME list (Task 8).
    models?: ModelOption[];
  } = {},
) {
  const services = opts.services ?? [makeService()];
  const fakeApi = {
    services: vi.fn(async () => ({ data: services })),
    createService: vi.fn(async (body: CreateServiceRequest) =>
      makeService({
        id: 'svc_created',
        name: body.name,
        description: body.description ?? '',
        status: (body.status as PortalService['status']) ?? 'active',
      }),
    ),
    updateService: vi.fn(async (id: string, body: UpdateServiceRequest) =>
      makeService({
        id,
        name: body.name ?? services[0].name,
        status: (body.status as PortalService['status']) ?? services[0].status,
      }),
    ),
    deleteService: vi.fn(async () => ({ ok: true })),
    serviceTokens: vi.fn(async () => ({ data: [] })),
    createServiceToken: vi.fn(),
    rotateServiceToken: vi.fn(),
    deleteServiceToken: vi.fn(),
    adminUsers: vi.fn(async () => ({ data: adminCandidates })),
    // Phase C admin-group linkage: the create form + the detail view's
    // admin-groups editor both fetch this (gated on isAdmin). Default to a
    // single auto-selecting candidate so every pre-existing test (none of
    // which care about admin-group linkage) keeps working unchanged.
    serviceAdminGroupCandidates: vi.fn(async () => defaultAdminGroupCandidates),
    setServiceAdminGroups: vi.fn(),
  };
  Object.assign(fakeApi, opts.overrides ?? {});

  render(
    <ToastProvider>
      <ServicesView
        t={t}
        api={fakeApi}
        models={opts.models ?? models}
        role={opts.role ?? 'admin'}
        userId={opts.userId ?? 'usr_admin'}
      />
    </ToastProvider>,
  );
  return { fakeApi };
}

afterEach(cleanup);

describe('ServicesView list', () => {
  it('renders the service list with name/status/token-count columns', async () => {
    renderServicesView({ services: [makeService({ name: 'Nightly Batch', token_count: 3 })] });
    expect(await screen.findByText('Nightly Batch')).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: '3' })).toBeInTheDocument();
  });

  it('shows the create action for an admin', async () => {
    renderServicesView({ role: 'admin' });
    await screen.findByText(t.services);
    expect(screen.getByRole('button', { name: t.serviceCreate })).toBeInTheDocument();
  });

  it('hides the create action for a non-admin', async () => {
    renderServicesView({
      role: 'user',
      userId: 'usr_delegate',
      services: [
        makeService({
          delegates: [{ user_id: 'usr_delegate', user_name: 'Me', can_manage_settings: true }],
        }),
      ],
    });
    await screen.findByText(t.services);
    expect(screen.queryByRole('button', { name: t.serviceCreate })).not.toBeInTheDocument();
  });
});

describe('ServicesView create form', () => {
  it('shows the model allowlist and both delegate groups', async () => {
    renderServicesView();
    fireEvent.click(await screen.findByRole('button', { name: t.serviceCreate }));
    expect(await screen.findByLabelText(t.serviceAllowedModelsLabel)).toBeInTheDocument();
    // Group headings render both as a section label AND (for "token", the
    // default add-level) as the closed add-level select's own display text —
    // assert via the unique per-group help text instead.
    expect(screen.getByText(t.serviceDelegatesFullHelp)).toBeInTheDocument();
    expect(screen.getByText(t.serviceDelegatesTokenHelp)).toBeInTheDocument();
  });

  it('creates a service with the given name, status, allowlist entry, and a full delegate', async () => {
    const { fakeApi } = renderServicesView();
    fireEvent.click(await screen.findByRole('button', { name: t.serviceCreate }));

    fireEvent.change(screen.getByLabelText(t.serviceNameLabel), {
      target: { value: 'New Service' },
    });

    // Add one allowlist model via the searchable multi-select.
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.serviceAllowedModelsLabel }));
    fireEvent.click(await screen.findByRole('option', { name: 'gpt-oss-20b' }));

    // Add a full delegate.
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.serviceDelegatesAddLabel }));
    fireEvent.click(await screen.findByRole('option', { name: 'Full Delegate' }));
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.serviceDelegatesLabel }));
    fireEvent.click(await screen.findByRole('option', { name: t.serviceDelegatesFullGroup }));
    fireEvent.click(screen.getByRole('button', { name: t.serviceDelegatesAdd }));

    // Let the single default admin-group candidate auto-select (Phase C)
    // before submitting.
    await screen.findByText(t.serviceAdminGroupAuto('Default Admin Group'));

    fireEvent.click(screen.getByRole('button', { name: t.serviceCreate }));

    await waitFor(() => expect(fakeApi.createService).toHaveBeenCalled());
    const body = (fakeApi.createService as unknown as { mock: { calls: [CreateServiceRequest][] } })
      .mock.calls[0][0];
    expect(body.name).toBe('New Service');
    expect(body.allowed_models).toEqual(['gpt-oss-20b']);
    expect(body.delegates).toEqual([{ user_id: 'usr_full', can_manage_settings: true }]);
    expect(body.admin_group_ids).toEqual(['ag_default']);
  });
});

describe('ServicesView create form — limits', () => {
  it('submits the entered rate-limit as part of the create request', async () => {
    const { fakeApi } = renderServicesView();
    fireEvent.click(await screen.findByRole('button', { name: t.serviceCreate }));

    fireEvent.change(screen.getByLabelText(t.serviceNameLabel), {
      target: { value: 'New Service' },
    });
    fireEvent.change(screen.getByLabelText(t.limitRateRequestsLabel), { target: { value: '50' } });
    fireEvent.change(screen.getByLabelText(t.limitRateWindowLabel), { target: { value: '60' } });

    // Let the single default admin-group candidate auto-select (Phase C)
    // before submitting.
    await screen.findByText(t.serviceAdminGroupAuto('Default Admin Group'));

    fireEvent.click(screen.getByRole('button', { name: t.serviceCreate }));

    await waitFor(() => expect(fakeApi.createService).toHaveBeenCalled());
    const body = (fakeApi.createService as unknown as { mock: { calls: [CreateServiceRequest][] } })
      .mock.calls[0][0];
    expect(body.limits).toEqual({
      ...EMPTY_LIMIT_CONFIG,
      rate_requests: 50,
      rate_window_seconds: 60,
    });
  });
});

describe('ServicesView admin-group picker (Phase C, spec 2026-08-10)', () => {
  function renderCreate(
    candidates: AdminGroupCandidate[],
    createService?: PortalApi['createService'],
  ) {
    const create =
      createService ??
      vi.fn(async (body: CreateServiceRequest) => makeService({ id: 'svc_new', name: body.name }));
    const fakeApi = {
      services: vi.fn(async () => ({ data: [] })),
      adminUsers: vi.fn(async () => ({ data: [] })),
      serviceAdminGroupCandidates: vi.fn(async () => candidates),
      createService: create,
      createServiceToken: vi.fn(),
      deleteService: vi.fn(),
      deleteServiceToken: vi.fn(),
      rotateServiceToken: vi.fn(),
      serviceTokens: vi.fn(async () => ({ data: [] })),
      setServiceAdminGroups: vi.fn(),
      updateService: vi.fn(),
    };
    render(
      <ToastProvider>
        <ServicesView t={t} api={fakeApi} models={models} role="admin" userId="usr_admin" />
      </ToastProvider>,
    );
    return { createService: create as ReturnType<typeof vi.fn> };
  }

  it('auto-selects the single admin-group candidate (no field) and submits its id', async () => {
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_1', name: 'Ops Admins', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
    ];
    const { createService } = renderCreate(candidates);

    fireEvent.click(await screen.findByRole('button', { name: t.serviceCreate }));
    // The auto note names the single candidate + derives the system group;
    // no picker of any kind renders.
    await screen.findByText(t.serviceAdminGroupAuto('Ops Admins'));
    expect(screen.getByText(t.serviceAdminGroupSystemGroupAuto('Ops System'))).toBeInTheDocument();
    expect(screen.queryByLabelText(t.serviceAdminGroupLabel)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.serviceAdminGroupSystemGroupLabel)).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(t.serviceNameLabel), { target: { value: 'svc' } });
    fireEvent.click(screen.getByRole('button', { name: t.serviceCreate }));
    await waitFor(() => expect(createService).toHaveBeenCalledTimes(1));
    expect(createService.mock.calls[0][0].admin_group_ids).toEqual(['ag_1']);
  });

  it('shows a required multi-select when there are several candidates under ONE system group, and submits the chosen ids', async () => {
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
      { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
    ];
    const { createService } = renderCreate(candidates);

    fireEvent.click(await screen.findByRole('button', { name: t.serviceCreate }));
    // A single shared parent -> no system-group step, just the derived note.
    await screen.findByText(t.serviceAdminGroupSystemGroupAuto('Ops System'));
    expect(screen.queryByLabelText(t.serviceAdminGroupSystemGroupLabel)).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(t.serviceNameLabel), { target: { value: 'svc' } });

    // Nothing picked yet -> submit stays disabled.
    expect(screen.getByRole('button', { name: t.serviceCreate })).toBeDisabled();

    fireEvent.mouseDown(screen.getByLabelText(t.serviceAdminGroupLabel));
    fireEvent.click(await screen.findByRole('option', { name: 'Group B' }));

    fireEvent.click(screen.getByRole('button', { name: t.serviceCreate }));
    await waitFor(() => expect(createService).toHaveBeenCalledTimes(1));
    expect(createService.mock.calls[0][0].admin_group_ids).toEqual(['ag_b']);
  });

  it('requires a system-group choice first when candidates span MORE THAN ONE parent, then narrows the admin-group picker to its children', async () => {
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'System One' },
      { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'System One' },
      { id: 'ag_c', name: 'Group C', parent_group_id: 'sg_2', parent_group_name: 'System Two' },
    ];
    const { createService } = renderCreate(candidates);

    fireEvent.click(await screen.findByRole('button', { name: t.serviceCreate }));
    await screen.findByLabelText(t.serviceAdminGroupSystemGroupLabel);
    // No admin-group picker of any kind before a system group is chosen.
    expect(screen.queryByLabelText(t.serviceAdminGroupLabel)).not.toBeInTheDocument();
    expect(screen.queryByText(t.serviceAdminGroupAuto('Group C'))).not.toBeInTheDocument();

    fireEvent.mouseDown(screen.getByLabelText(t.serviceAdminGroupSystemGroupLabel));
    fireEvent.click(await screen.findByRole('option', { name: 'System Two' }));

    // Narrowed to System Two's single child -> auto-selected.
    await screen.findByText(t.serviceAdminGroupAuto('Group C'));
    expect(screen.queryByLabelText(t.serviceAdminGroupLabel)).not.toBeInTheDocument();
    expect(screen.queryByText('Group A')).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(t.serviceNameLabel), { target: { value: 'svc' } });
    fireEvent.click(screen.getByRole('button', { name: t.serviceCreate }));
    await waitFor(() => expect(createService).toHaveBeenCalledTimes(1));
    expect(createService.mock.calls[0][0].admin_group_ids).toEqual(['ag_c']);
  });

  it('shows a hint and keeps the submit action disabled when the caller has no admin-group candidate', async () => {
    const createService = vi.fn(async (body: CreateServiceRequest) =>
      makeService({ id: 'svc_new', name: body.name }),
    );
    renderCreate([], createService as unknown as PortalApi['createService']);

    fireEvent.click(await screen.findByRole('button', { name: t.serviceCreate }));
    await screen.findByText(t.serviceNoAdminGroupHint);
    fireEvent.change(screen.getByLabelText(t.serviceNameLabel), { target: { value: 'svc' } });

    expect(screen.getByRole('button', { name: t.serviceCreate })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: t.serviceCreate }));
    expect(createService).not.toHaveBeenCalled();
  });
});

describe('ServicesView edit-form admin-groups editor (Phase C, spec 2026-08-10)', () => {
  it("pre-fills the service's linked groups and saves the edited set via its own button (already-grouped service)", async () => {
    const svc = makeService({
      id: 'svc-ag',
      admin_groups: [{ id: 'ag_a', name: 'Group A' }],
      system_group_id: 'sg_1',
      system_group_name: 'Ops System',
    });
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
      { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
      // A candidate under a DIFFERENT system group must NOT be offered here.
      {
        id: 'ag_other',
        name: "Other System's Group",
        parent_group_id: 'sg_2',
        parent_group_name: 'Other System',
      },
    ];
    const setServiceAdminGroups = vi.fn(async () => ({
      ...svc,
      admin_groups: [{ id: 'ag_b', name: 'Group B' }],
    }));
    renderServicesView({
      services: [svc],
      overrides: {
        serviceAdminGroupCandidates: vi.fn(async () => candidates),
        setServiceAdminGroups,
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    await screen.findByText(t.serviceAdminGroupsSectionTitle);
    // Pre-filled with the service's own linked group.
    expect(screen.getByText('Group A')).toBeInTheDocument();
    expect(screen.queryByText("Other System's Group")).not.toBeInTheDocument();

    fireEvent.mouseDown(screen.getByLabelText(t.serviceAdminGroupLabel));
    fireEvent.click(await screen.findByRole('option', { name: 'Group B' }));
    fireEvent.click(screen.getByRole('button', { name: t.serviceAdminGroupsSave }));

    await waitFor(() =>
      expect(setServiceAdminGroups).toHaveBeenCalledWith('svc-ag', ['ag_a', 'ag_b']),
    );
    expect(await screen.findByText(t.serviceAdminGroupsSaved)).toBeInTheDocument();
  });

  // Migration recovery (mirrors the server fix `c73887c`): a service created
  // before Phase C has no containment root (system_group_id==""). The edit
  // editor must offer the create-style choose-a-system-group flow so an
  // admin can SET the root -- the fixed-root editAdminGroupOptions path
  // filters to the (empty) root's children and would offer nothing.
  it('offers the create-style flow for an ungrouped (migrated) service and sets its root on save', async () => {
    const svc = makeService({
      id: 'svc-ungrouped',
      admin_groups: [],
      system_group_id: '',
      system_group_name: '',
    });
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'System One' },
      { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'System One' },
      { id: 'ag_c', name: 'Group C', parent_group_id: 'sg_2', parent_group_name: 'System Two' },
    ];
    const setServiceAdminGroups = vi.fn(async () => ({
      ...svc,
      admin_groups: [{ id: 'ag_c', name: 'Group C' }],
      system_group_id: 'sg_2',
      system_group_name: 'System Two',
    }));
    renderServicesView({
      services: [svc],
      overrides: {
        serviceAdminGroupCandidates: vi.fn(async () => candidates),
        setServiceAdminGroups,
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    await screen.findByText(t.serviceAdminGroupsSectionTitle);
    // Candidates span two system groups -> the system-group step appears first,
    // and no admin-group picker until one is chosen. Save stays disabled.
    await screen.findByLabelText(t.serviceAdminGroupSystemGroupLabel);
    expect(screen.queryByLabelText(t.serviceAdminGroupLabel)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.serviceAdminGroupsSave })).toBeDisabled();

    fireEvent.mouseDown(screen.getByLabelText(t.serviceAdminGroupSystemGroupLabel));
    fireEvent.click(await screen.findByRole('option', { name: 'System Two' }));

    // Narrowed to System Two's single child -> auto-selected, Save enabled.
    await screen.findByText(t.serviceAdminGroupAuto('Group C'));
    expect(screen.getByRole('button', { name: t.serviceAdminGroupsSave })).toBeEnabled();

    fireEvent.click(screen.getByRole('button', { name: t.serviceAdminGroupsSave }));
    await waitFor(() =>
      expect(setServiceAdminGroups).toHaveBeenCalledWith('svc-ungrouped', ['ag_c']),
    );
    expect(await screen.findByText(t.serviceAdminGroupsSaved)).toBeInTheDocument();
  });

  // Fix-round 1: the linkage Panel must NOT be isAdmin-only -- a non-admin
  // Full-Delegate (can_manage_settings=true) already edits every other
  // setting on this service (canEditSettings), and the backend's
  // authorizeServiceSettings permits them to call SetServiceAdminGroups too.
  // This also proves the design spec's ungrouped-recovery path works for a
  // Full-Delegate, not just an admin.
  it('a non-admin Full-Delegate sees the linkage section for an ungrouped service they manage and can set its root', async () => {
    const svc = makeService({
      id: 'svc-fd-ungrouped',
      admin_groups: [],
      system_group_id: '',
      system_group_name: '',
      delegates: [
        { user_id: 'usr_full_delegate', user_name: 'Full Delegate', can_manage_settings: true },
      ],
    });
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_1', name: 'Ops Admins', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
    ];
    const setServiceAdminGroups = vi.fn(async () => ({
      ...svc,
      admin_groups: [{ id: 'ag_1', name: 'Ops Admins' }],
      system_group_id: 'sg_1',
      system_group_name: 'Ops System',
    }));
    renderServicesView({
      services: [svc],
      role: 'user',
      userId: 'usr_full_delegate',
      overrides: {
        serviceAdminGroupCandidates: vi.fn(async () => candidates),
        setServiceAdminGroups,
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    // The Full-Delegate can still edit every other setting (proves canEditSettings
    // is true here, i.e. this isn't accidentally the read-only path).
    expect(await screen.findByLabelText(t.serviceNameLabel)).not.toBeDisabled();
    // The linkage section is visible despite role="user" (not admin/system_admin).
    await screen.findByText(t.serviceAdminGroupsSectionTitle);
    // Single candidate under a single system group -> both auto-derived, Save enabled.
    await screen.findByText(t.serviceAdminGroupAuto('Ops Admins'));
    expect(screen.getByText(t.serviceAdminGroupSystemGroupAuto('Ops System'))).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.serviceAdminGroupsSave })).toBeEnabled();

    fireEvent.click(screen.getByRole('button', { name: t.serviceAdminGroupsSave }));
    await waitFor(() =>
      expect(setServiceAdminGroups).toHaveBeenCalledWith('svc-fd-ungrouped', ['ag_1']),
    );
    expect(await screen.findByText(t.serviceAdminGroupsSaved)).toBeInTheDocument();
  });

  // A plain member/regular user (no delegate row, not admin) must NOT see the
  // linkage section at all -- canEditSettings is false for them.
  it('hides the linkage section entirely for a non-admin, non-delegate viewer', async () => {
    const svc = makeService({
      id: 'svc-no-access',
      admin_groups: [],
      system_group_id: '',
      system_group_name: '',
    });
    renderServicesView({
      services: [svc],
      role: 'user',
      userId: 'usr_random',
      overrides: {
        serviceAdminGroupCandidates: vi.fn(async () => []),
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    await screen.findByLabelText(t.serviceNameLabel);
    expect(screen.queryByText(t.serviceAdminGroupsSectionTitle)).not.toBeInTheDocument();
  });
});

describe('ServicesView detail — settings editable for admin/full-delegate', () => {
  it('opens the detail view and can save an edited name', async () => {
    const svc = makeService({ id: 'svc_edit', name: 'Edit Me' });
    const { fakeApi } = renderServicesView({ services: [svc] });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    const nameField = await screen.findByLabelText(t.serviceNameLabel);
    expect(nameField).not.toBeDisabled();
    fireEvent.change(nameField, { target: { value: 'Renamed' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() =>
      expect(fakeApi.updateService).toHaveBeenCalledWith(
        'svc_edit',
        expect.objectContaining({ name: 'Renamed' }),
      ),
    );
  });

  it('saves an edited request-quota (threshold + period) for an admin/full-delegate', async () => {
    const svc = makeService({ id: 'svc_limits' });
    const { fakeApi } = renderServicesView({ services: [svc] });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    fireEvent.change(await screen.findByLabelText(t.limitRequestQuotaLabel), {
      target: { value: '10000' },
    });
    // Period selects are non-native MUI Selects — open + pick the option.
    fireEvent.mouseDown(screen.getAllByLabelText(t.limitPeriodLabel)[0]);
    fireEvent.click(await screen.findByRole('option', { name: t.limitPeriodDay }));
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() =>
      expect(fakeApi.updateService).toHaveBeenCalledWith(
        'svc_limits',
        expect.objectContaining({
          limits: expect.objectContaining({ request_quota: 10000, request_quota_period: 'day' }),
        }),
      ),
    );
  });

  it('toggles the service status from the list row action', async () => {
    const svc = makeService({ id: 'svc_toggle', status: 'active' });
    const { fakeApi } = renderServicesView({ services: [svc] });

    fireEvent.click(await screen.findByRole('button', { name: t.tokenActionDisable }));

    await waitFor(() =>
      expect(fakeApi.updateService).toHaveBeenCalledWith('svc_toggle', { status: 'disabled' }),
    );
  });

  it('deletes a service after confirmation', async () => {
    const svc = makeService({ id: 'svc_del' });
    const { fakeApi } = renderServicesView({ services: [svc] });

    fireEvent.click(await screen.findByRole('button', { name: t.serviceActionDelete }));
    expect(fakeApi.deleteService).not.toHaveBeenCalled();
    const dialog = within(screen.getByRole('dialog'));
    fireEvent.click(dialog.getByRole('button', { name: t.serviceActionDelete }));

    await waitFor(() => expect(fakeApi.deleteService).toHaveBeenCalledWith('svc_del'));
  });
});

describe('ServicesView detail — read-only for a token-delegate', () => {
  it('disables every settings field and hides Save, but still shows the token panel', async () => {
    const svc = makeService({
      id: 'svc_ro',
      name: 'RO Service',
      delegates: [
        { user_id: 'usr_token_delegate', user_name: 'Token Delegate', can_manage_settings: false },
      ],
    });
    renderServicesView({ services: [svc], role: 'user', userId: 'usr_token_delegate' });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    expect(await screen.findByLabelText(t.serviceNameLabel)).toBeDisabled();
    expect(screen.getByLabelText(t.serviceDescriptionLabel)).toBeDisabled();
    expect(screen.queryByRole('button', { name: t.save })).not.toBeInTheDocument();
    expect(screen.getByText(t.serviceSettingsReadOnlyNote)).toBeInTheDocument();

    // Token management stays available to any delegate.
    expect(screen.getByRole('button', { name: t.serviceTokenCreate })).toBeInTheDocument();
    // No delete/toggle row action for the list row either (only "Details").
  });

  it('disables the limits fields but still shows the read-only usage line for a configured limit', async () => {
    const svc = makeService({
      id: 'svc_ro_limits',
      delegates: [
        { user_id: 'usr_token_delegate', user_name: 'Token Delegate', can_manage_settings: false },
      ],
      limits: { ...EMPTY_LIMIT_CONFIG, cost_budget: 50, cost_budget_period: 'month' },
      limits_usage: { requests_this_period: 0, tokens_this_period: 0, cost_this_period: 12.5 },
    });
    renderServicesView({ services: [svc], role: 'user', userId: 'usr_token_delegate' });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    expect(await screen.findByLabelText(t.limitCostBudgetLabel)).toBeDisabled();
    expect(screen.getByLabelText(t.limitRateRequestsLabel)).toBeDisabled();
    expect(screen.getByText(t.limitUsageCostLine(12.5, 50))).toBeInTheDocument();
  });

  it('hides the enable/disable and delete row actions on the list for a token-delegate', async () => {
    const svc = makeService({
      id: 'svc_ro2',
      delegates: [
        { user_id: 'usr_token_delegate', user_name: 'Token Delegate', can_manage_settings: false },
      ],
    });
    renderServicesView({ services: [svc], role: 'user', userId: 'usr_token_delegate' });

    await screen.findByRole('button', { name: t.modelDetailsAction });
    expect(screen.queryByRole('button', { name: t.tokenActionDisable })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.serviceActionDelete })).not.toBeInTheDocument();
  });
});

describe('ServicesView token management', () => {
  it('creates a service token and reveals the one-time secret with a curl snippet', async () => {
    const svc = makeService({ id: 'svc_tok' });
    const { fakeApi } = renderServicesView({
      services: [svc],
      overrides: {
        createServiceToken: vi.fn(async () => ({
          token: {
            id: 'tok_1',
            service_id: 'svc_tok',
            name: 'Batch Token',
            secret_prefix: 'svctok_',
            status: 'active',
            scopes: ['llm:invoke'],
            expires_at: null,
            last_used_at: null,
            created_at: '2026-01-01T00:00:00Z',
            model_override: '',
            log_communication: false,
            secret: false,
          },
          secret: 'svctok_created_secret',
        })),
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    fireEvent.click(await screen.findByRole('button', { name: t.serviceTokenCreate }));
    // The create-token dialog's "Name" field is ambiguous with the settings
    // panel's own "Name" field still mounted behind it — scope to the dialog.
    const createDialog = within(screen.getByRole('dialog'));
    fireEvent.change(createDialog.getByLabelText(t.tokenNameLabel), {
      target: { value: 'Batch Token' },
    });
    fireEvent.click(createDialog.getByRole('button', { name: t.serviceTokenCreate }));

    await waitFor(() =>
      expect(fakeApi.createServiceToken).toHaveBeenCalledWith(
        'svc_tok',
        expect.objectContaining({ name: 'Batch Token' }),
      ),
    );
    const revealDialog = await screen.findByRole('dialog');
    expect(within(revealDialog).getByText('svctok_created_secret')).toBeInTheDocument();
    expect(within(revealDialog).getByText(/\/v1\/chat\/completions/)).toBeInTheDocument();
  });

  it('rotates a token after confirmation and reveals the new secret', async () => {
    const svc = makeService({ id: 'svc_rot' });
    const existingToken = {
      id: 'tok_rot',
      service_id: 'svc_rot',
      name: 'Rotate Me',
      secret_prefix: 'svctok_old',
      status: 'active',
      scopes: ['llm:invoke'],
      expires_at: null,
      last_used_at: null,
      created_at: '2026-01-01T00:00:00Z',
      model_override: '',
      log_communication: false,
      secret: false,
    };
    const { fakeApi } = renderServicesView({
      services: [svc],
      overrides: {
        serviceTokens: vi.fn(async () => ({ data: [existingToken] })),
        rotateServiceToken: vi.fn(async () => ({
          token: { ...existingToken, secret_prefix: 'svctok_new' },
          secret: 'svctok_rotated_secret',
        })),
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    fireEvent.click(await screen.findByRole('button', { name: t.tokenActionRotate }));
    expect(fakeApi.rotateServiceToken).not.toHaveBeenCalled();
    const confirmDialog = within(screen.getByRole('dialog'));
    fireEvent.click(confirmDialog.getByRole('button', { name: t.tokenRotateConfirm }));

    await waitFor(() =>
      expect(fakeApi.rotateServiceToken).toHaveBeenCalledWith('svc_rot', 'tok_rot'),
    );
    expect(await screen.findByText('svctok_rotated_secret')).toBeInTheDocument();
  });

  it('deletes a token after confirmation', async () => {
    const svc = makeService({ id: 'svc_del_tok' });
    const existingToken = {
      id: 'tok_del',
      service_id: 'svc_del_tok',
      name: 'Delete Me',
      secret_prefix: 'svctok_del',
      status: 'active',
      scopes: ['llm:invoke'],
      expires_at: null,
      last_used_at: null,
      created_at: '2026-01-01T00:00:00Z',
      model_override: '',
      log_communication: false,
      secret: false,
    };
    const { fakeApi } = renderServicesView({
      services: [svc],
      overrides: { serviceTokens: vi.fn(async () => ({ data: [existingToken] })) },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    await screen.findByText('Delete Me');
    fireEvent.click(screen.getByRole('button', { name: t.tokenActionDelete }));
    const confirmDialog = within(screen.getByRole('dialog'));
    fireEvent.click(confirmDialog.getByRole('button', { name: t.tokenActionDelete }));

    await waitFor(() =>
      expect(fakeApi.deleteServiceToken).toHaveBeenCalledWith('svc_del_tok', 'tok_del'),
    );
  });
});

describe('ServiceTokensSection unknown-model redirect (Task 8)', () => {
  it('sends the redirect settings when creating a service token', async () => {
    const svc = makeService({ id: 'svc_redirect' });
    const { fakeApi } = renderServicesView({
      services: [svc],
      overrides: {
        createServiceToken: vi.fn(async () => ({ token: makeServiceToken(), secret: 's' })),
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    fireEvent.click(await screen.findByRole('button', { name: t.serviceTokenCreate }));
    const createDialog = within(screen.getByRole('dialog'));
    fireEvent.change(createDialog.getByLabelText(t.tokenNameLabel), {
      target: { value: 'Batch Token' },
    });
    fireEvent.click(createDialog.getByRole('checkbox', { name: t.tokenUnknownRedirect }));
    fireEvent.click(createDialog.getByRole('checkbox', { name: t.tokenUnknownRedirectBlocked }));
    fireEvent.mouseDown(createDialog.getByLabelText(t.tokenUnknownFallback));
    fireEvent.click(await screen.findByRole('option', { name: 'gpt-oss-20b' }));
    fireEvent.click(createDialog.getByRole('button', { name: t.serviceTokenCreate }));

    await waitFor(() => expect(fakeApi.createServiceToken).toHaveBeenCalled());
    expect(fakeApi.createServiceToken).toHaveBeenCalledWith(
      'svc_redirect',
      expect.objectContaining({
        unknown_model_redirect: true,
        unknown_model_redirect_blocked: true,
        unknown_model_fallback: 'gpt-oss-20b',
      }),
    );
  });

  it('offers models and groups in one fallback picker', async () => {
    const svc = makeService({ id: 'svc_groups' });
    renderServicesView({
      services: [svc],
      models: [
        { id: 'qwen3-32b', display_name: 'qwen3-32b', flavors: [] },
        { id: 'fast-group', display_name: 'fast-group', flavors: [], is_group: true },
      ],
      overrides: {
        createServiceToken: vi.fn(async () => ({ token: makeServiceToken(), secret: 's' })),
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    fireEvent.click(await screen.findByRole('button', { name: t.serviceTokenCreate }));
    const createDialog = within(screen.getByRole('dialog'));
    fireEvent.click(createDialog.getByRole('checkbox', { name: t.tokenUnknownRedirect }));
    fireEvent.mouseDown(createDialog.getByLabelText(t.tokenUnknownFallback));

    expect(await screen.findByRole('option', { name: 'qwen3-32b' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'fast-group' })).toBeInTheDocument();
  });

  it('clears the sub-settings when the redirect is switched off before creating a token', async () => {
    const svc = makeService({ id: 'svc_clear' });
    const { fakeApi } = renderServicesView({
      services: [svc],
      overrides: {
        createServiceToken: vi.fn(async () => ({ token: makeServiceToken(), secret: 's' })),
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    fireEvent.click(await screen.findByRole('button', { name: t.serviceTokenCreate }));
    const createDialog = within(screen.getByRole('dialog'));
    fireEvent.change(createDialog.getByLabelText(t.tokenNameLabel), {
      target: { value: 'Batch Token' },
    });
    fireEvent.click(createDialog.getByRole('checkbox', { name: t.tokenUnknownRedirect }));
    fireEvent.click(createDialog.getByRole('checkbox', { name: t.tokenUnknownRedirectBlocked }));
    fireEvent.mouseDown(createDialog.getByLabelText(t.tokenUnknownFallback));
    fireEvent.click(await screen.findByRole('option', { name: 'gpt-oss-20b' }));
    // Turn the redirect back off before submitting — the sub-settings must
    // be cleared, not merely rendered disabled.
    fireEvent.click(createDialog.getByRole('checkbox', { name: t.tokenUnknownRedirect }));
    fireEvent.click(createDialog.getByRole('button', { name: t.serviceTokenCreate }));

    await waitFor(() => expect(fakeApi.createServiceToken).toHaveBeenCalled());
    expect(fakeApi.createServiceToken).toHaveBeenCalledWith(
      'svc_clear',
      expect.objectContaining({
        unknown_model_redirect: false,
        unknown_model_redirect_blocked: false,
        unknown_model_fallback: '',
      }),
    );
  });
});

describe('ServiceTokensSection last-used-model column (Task 9)', () => {
  // The column-visibility toggle persists to localStorage (usePreference), so
  // clear it between cases to keep each test starting from the hidden-by-default
  // state (see ServerList.test.tsx's Server-ID column tests for the same pattern).
  beforeEach(() => {
    try {
      window.localStorage.clear();
    } catch {
      /* jsdom/private-mode guard */
    }
  });

  it('hides the last-used-model column by default and shows it from the column menu', async () => {
    const svc = makeService({ id: 'svc_last_used' });
    const existingToken = makeServiceToken({
      id: 'tok_last_used',
      service_id: 'svc_last_used',
      name: 'Batch Token',
      last_used_model: 'qwen3-32b',
    });
    renderServicesView({
      services: [svc],
      overrides: { serviceTokens: vi.fn(async () => ({ data: [existingToken] })) },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    await screen.findByText('Batch Token');
    expect(screen.queryByText('qwen3-32b')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.tokenLastUsedModel }));
    expect(screen.getByText('qwen3-32b')).toBeInTheDocument();
  });

  it('renders the placeholder for a token that has never been used', async () => {
    const svc = makeService({ id: 'svc_never_used' });
    const existingToken = makeServiceToken({
      id: 'tok_never_used',
      service_id: 'svc_never_used',
      name: 'Fresh Token',
      last_used_model: '',
    });
    renderServicesView({
      services: [svc],
      overrides: { serviceTokens: vi.fn(async () => ({ data: [existingToken] })) },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
    await screen.findByText('Fresh Token');

    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.tokenLastUsedModel }));
    expect(screen.getByText('—')).toBeInTheDocument();
  });
});
