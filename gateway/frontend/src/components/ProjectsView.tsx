// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState, type SubmitEvent, type ReactNode } from 'react';
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Chip,
  FormControlLabel,
  Radio,
  RadioGroup,
  Stack,
  Tooltip,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import EditIcon from '@mui/icons-material/Edit';
import DeleteIcon from '@mui/icons-material/Delete';
import GroupIcon from '@mui/icons-material/Group';
import VpnKeyIcon from '@mui/icons-material/VpnKey';
import InfoOutlinedIcon from '@mui/icons-material/InfoOutlined';
import SwapHorizIcon from '@mui/icons-material/SwapHoriz';
import PersonRemoveIcon from '@mui/icons-material/PersonRemove';
import GroupRemoveIcon from '@mui/icons-material/GroupRemove';
import LinkOffIcon from '@mui/icons-material/LinkOff';
import type {
  Project,
  ProjectGroupRef,
  ProjectToken,
  ProjectTokenUsageTotal,
  UserGroup,
  UserRef,
} from '../api';
import type { Translation, MessageKey, PortalApi } from './shared/types';
import { formatPortalError, formatDate } from './shared/format';
import { useResource } from './shared/useResource';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { MultiSelectField } from './shared/MultiSelectField';
import { SearchableSelect } from './shared/SearchableSelect';
import { Breadcrumbs } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { StatusChip } from './shared/StatusChip';

// Mode is a discriminated union mirroring GroupsView's -- "create"/"edit" are
// plain rename forms, "members" is the manage sub-view (roster + candidate
// pickers + owner-only transfer), "tokens" is the separate assigned-tokens
// sub-view (split out from "members" so member management and token
// assignment are two distinct row actions/sub-views).
type Mode =
  | 'list'
  | { kind: 'create' }
  | { kind: 'edit'; project: Project }
  | { kind: 'members'; project: Project }
  | { kind: 'tokens'; project: Project };

/** Truncates a raw id for display, keeping the full value in a `title` tooltip. */
function ShortId({ id }: Readonly<{ id: string }>) {
  if (!id) return <>{'–'}</>;
  return <span title={id}>{id.length > 12 ? `${id.slice(0, 10)}…` : id}</span>;
}

function findProjectById(list: Project[], id: string): Project | undefined {
  return list.find((p) => p.id === id);
}

// Mirrors TokenList's file-local tokenStatusLabelByKey -- the shared owner/admin
// token status vocabulary, so a project's assigned-tokens list reads the same
// as the owner's own token list.
const projectTokenStatusLabelByKey: Record<ProjectToken['status'], MessageKey> = {
  active: 'statusActive',
  disabled: 'statusDisabled',
  expired: 'statusExpired',
};

function projectTokenStatusLabel(t: Translation, status: ProjectToken['status']): string {
  return t[projectTokenStatusLabelByKey[status] ?? 'statusActive'];
}

/**
 * Project management: every authenticated user sees the projects they own
 * PLUS the projects they are a member of (direct or via a group), via
 * ListProjects (spec §9 GET /api/portal/projects) -- a single flat list, no
 * tiers (projects are user-owned, not hierarchical like user-groups).
 * Manage actions (rename/members/delete) are gated on `can_manage` (owner OR
 * admin scope, spec §5); ownership transfer is further gated on
 * `my_role === "owner"` inside the members sub-view -- an admin managing
 * someone else's project can add/remove members/groups and delete, but
 * cannot transfer ownership away from the actual owner.
 */
export function ProjectsView({
  t,
  api,
  userId,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'addProjectGroups'
    | 'addProjectMembers'
    | 'createProject'
    | 'deleteProject'
    | 'detachProjectToken'
    | 'groups'
    | 'projectCandidates'
    | 'projectMembers'
    | 'projectTokens'
    | 'projects'
    | 'removeProjectGroup'
    | 'removeProjectMember'
    | 'renameProject'
    | 'transferProject'
  >;
  /** "user" | "admin" | "system_admin" -- unused here (unlike GroupsView's
      tiered sections, projects have no role-gated sections; every action is
      driven by the per-project can_manage/my_role fields), kept for a
      call-site signature consistent with the other role-scoped views. */
  role: string;
  userId: string;
}>) {
  const { showError, showSuccess } = useToast();
  const {
    data: projectsData,
    setData: setProjectsData,
    error: projectsError,
    loading: projectsLoading,
  } = useResource(() => api.projects(), [api, t], t, { trackLoading: false });
  useEffect(() => {
    if (projectsError) showError(projectsError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectsError]);
  const projects = projectsData ?? [];

  async function reloadProjects(): Promise<Project[]> {
    const next = await api.projects();
    setProjectsData(next);
    return next;
  }

  const [mode, setMode] = useState<Mode>('list');
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [deleteTarget, setDeleteTarget] = useState<Project | null>(null);
  const [transferTarget, setTransferTarget] = useState<Project | null>(null);

  // Coupled-project create form (spec 2026-08-09): "none" = a normal project
  // (unchanged); "select" couples to an existing user-tier group the caller
  // owns; "create" creates a new user-tier group (optionally under a chosen
  // admin-group parent) then couples to it. Options are loaded lazily each
  // time the create form opens (mirrors loadCandidates/loadRoster's
  // latest-wins pattern) so a slow/stale response from a previous open can't
  // clobber a newer one.
  const [coupleMode, setCoupleMode] = useState<'none' | 'select' | 'create'>('none');
  const [coupleGroupId, setCoupleGroupId] = useState('');
  const [newGroupName, setNewGroupName] = useState('');
  const [newGroupParent, setNewGroupParent] = useState('');
  const [ownedGroups, setOwnedGroups] = useState<UserGroup[]>([]);
  const [adminGroups, setAdminGroups] = useState<UserGroup[]>([]);
  const coupleGroupsReqRef = useRef(0);

  // "Manage members" sub-view state.
  const [candidateUsers, setCandidateUsers] = useState<UserRef[] | null>(null);
  const [candidateGroups, setCandidateGroups] = useState<ProjectGroupRef[] | null>(null);
  const [candidatesLoading, setCandidatesLoading] = useState(false);
  const [selectedUsers, setSelectedUsers] = useState<string[]>([]);
  const [selectedGroups, setSelectedGroups] = useState<string[]>([]);
  const [memberBusy, setMemberBusy] = useState(false);
  const [transferUserId, setTransferUserId] = useState('');

  // The project's real current roster (users + groups) -- feeds both the
  // member-list display AND the transfer picker (as opposed to
  // `candidateUsers`/`candidateGroups`, the ADD-able lists, i.e. NOT current
  // members). Latest-wins token (mirrors GroupsView's rosterReqRef): a slow
  // response for a since-switched project can't clobber a newer one.
  const [roster, setRoster] = useState<{
    users: UserRef[];
    groups: ProjectGroupRef[];
    transfer_candidates: UserRef[];
  } | null>(null);
  const [rosterLoading, setRosterLoading] = useState(false);
  const rosterReqRef = useRef(0);

  // The project's assigned API tokens (owner/admin, read-only + detach) --
  // fetched alongside the roster (same open/refresh lifecycle), own
  // latest-wins token so a slow response for a since-switched project can't
  // clobber a newer one. `tokens` are the CURRENTLY-ATTACHED tokens' own usage
  // (all-time); `total` is the project's TRUE all-time total, which may exceed
  // the sum of `tokens` (it also counts usage from tokens since detached).
  const [projectTokens, setProjectTokens] = useState<{
    tokens: ProjectToken[];
    total: ProjectTokenUsageTotal;
  } | null>(null);
  const [tokensLoading, setTokensLoading] = useState(false);
  const tokensReqRef = useRef(0);
  const [detachTarget, setDetachTarget] = useState<{
    project: Project;
    token: ProjectToken;
  } | null>(null);

  function openCreate() {
    setName('');
    setDescription('');
    setCoupleMode('none');
    setCoupleGroupId('');
    setNewGroupName('');
    setNewGroupParent('');
    loadCoupleGroups();
    setMode({ kind: 'create' });
  }

  // Loads the caller's owned user-tier groups (the "select existing group"
  // picker options) and the caller's admin-tier groups (the "create new
  // group" parent picker options), via the same ListGroups landscape
  // GroupsView reads. Latest-wins token so a slow/stale response from a
  // previous create-form open can't clobber a newer one.
  function loadCoupleGroups() {
    const token = ++coupleGroupsReqRef.current;
    setOwnedGroups([]);
    setAdminGroups([]);
    api
      .groups()
      .then((landscape) => {
        if (coupleGroupsReqRef.current !== token) return;
        setOwnedGroups(landscape.user.filter((g) => g.owner_user_id === userId));
        setAdminGroups(landscape.admin);
        if (landscape.admin.length === 1) setNewGroupParent(landscape.admin[0].id);
      })
      .catch(() => {
        // Best-effort: an unavailable group list just leaves the couple
        // controls showing no options (the toggle can still be turned off).
      });
  }

  function openEdit(project: Project) {
    setName(project.name);
    setDescription(project.description);
    setMode({ kind: 'edit', project });
  }

  function loadCandidates(id: string) {
    setCandidatesLoading(true);
    api
      .projectCandidates(id)
      .then((res) => {
        setCandidateUsers(res.users);
        setCandidateGroups(res.groups);
      })
      .catch((err) => {
        showError(formatPortalError(err, t));
        setCandidateUsers([]);
        setCandidateGroups([]);
      })
      .finally(() => setCandidatesLoading(false));
  }

  function loadRoster(id: string) {
    const token = ++rosterReqRef.current;
    setRosterLoading(true);
    api
      .projectMembers(id)
      .then((res) => {
        if (rosterReqRef.current === token) setRoster(res);
      })
      .catch((err) => {
        if (rosterReqRef.current === token) {
          showError(formatPortalError(err, t));
          setRoster({ users: [], groups: [], transfer_candidates: [] });
        }
      })
      .finally(() => {
        if (rosterReqRef.current === token) setRosterLoading(false);
      });
  }

  function loadProjectTokens(id: string) {
    const token = ++tokensReqRef.current;
    setTokensLoading(true);
    api
      .projectTokens(id)
      .then((res) => {
        if (tokensReqRef.current === token) setProjectTokens(res);
      })
      .catch((err) => {
        if (tokensReqRef.current === token) {
          showError(formatPortalError(err, t));
          setProjectTokens({
            tokens: [],
            total: { request_count: 0, input_tokens: 0, output_tokens: 0, total_tokens: 0 },
          });
        }
      })
      .finally(() => {
        if (tokensReqRef.current === token) setTokensLoading(false);
      });
  }

  function openMembers(project: Project) {
    setSelectedUsers([]);
    setSelectedGroups([]);
    setTransferUserId('');
    setCandidateUsers(null);
    setCandidateGroups(null);
    setRoster(null);
    loadCandidates(project.id);
    loadRoster(project.id);
    setMode({ kind: 'members', project });
  }

  // Separate sub-view for the project's assigned API tokens (split out from
  // "members" -- see the Mode comment). Own fetch lifecycle, independent of
  // the roster/candidates state above.
  function openTokens(project: Project) {
    setProjectTokens(null);
    setDetachTarget(null);
    loadProjectTokens(project.id);
    setMode({ kind: 'tokens', project });
  }

  // Re-fetch the list after a membership/ownership/token-assignment change;
  // if the members or tokens sub-view is still open on that project, refresh
  // its snapshot in place (and, for members, its candidate list + roster,
  // since who's addable/current just changed) -- falling back to the list if
  // the project vanished (deleted).
  async function afterProjectChanged(id: string) {
    const next = await reloadProjects();
    const updated = findProjectById(next, id);
    if (updated) {
      setMode((current) => {
        if (current === 'list') return current;
        if (current.kind === 'members') return { kind: 'members', project: updated };
        if (current.kind === 'tokens') return { kind: 'tokens', project: updated };
        return current;
      });
      if (mode !== 'list' && mode.kind === 'members') {
        loadCandidates(id);
        loadRoster(id);
      } else if (mode !== 'list' && mode.kind === 'tokens') {
        loadProjectTokens(id);
      }
    } else {
      setMode('list');
    }
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    try {
      let body;
      if (coupleMode === 'select') {
        body = { name, description, coupled_group_id: coupleGroupId };
      } else if (coupleMode === 'create') {
        body = {
          name,
          description,
          create_coupled_group: {
            name: newGroupName,
            parent_group_id: newGroupParent || undefined,
          },
        };
      } else {
        body = { name, description };
      }
      await api.createProject(body);
      setMode('list');
      await reloadProjects();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function submitEdit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (mode === 'list' || mode.kind !== 'edit') return;
    setBusy(true);
    try {
      await api.renameProject(mode.project.id, name, description);
      setMode('list');
      await reloadProjects();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function submitAddMembers(project: Project, event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (selectedUsers.length === 0) return;
    setMemberBusy(true);
    try {
      await api.addProjectMembers(project.id, selectedUsers);
      setSelectedUsers([]);
      await afterProjectChanged(project.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function removeMember(project: Project, uid: string) {
    setMemberBusy(true);
    try {
      await api.removeProjectMember(project.id, uid);
      await afterProjectChanged(project.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function submitAddGroups(project: Project, event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (selectedGroups.length === 0) return;
    setMemberBusy(true);
    try {
      await api.addProjectGroups(project.id, selectedGroups);
      setSelectedGroups([]);
      await afterProjectChanged(project.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function removeGroup(project: Project, gid: string) {
    setMemberBusy(true);
    try {
      await api.removeProjectGroup(project.id, gid);
      await afterProjectChanged(project.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function confirmDetachToken() {
    if (!detachTarget) return;
    const { project, token } = detachTarget;
    setDetachTarget(null);
    setMemberBusy(true);
    try {
      await api.detachProjectToken(project.id, token.id);
      showSuccess(t.projectsTokenDetached);
      loadProjectTokens(project.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function confirmTransfer() {
    if (!transferTarget) return;
    const uid = transferUserId;
    const project = transferTarget;
    setTransferTarget(null);
    if (!uid) return;
    setMemberBusy(true);
    try {
      await api.transferProject(project.id, uid);
      setTransferUserId('');
      await afterProjectChanged(project.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function confirmDelete() {
    if (!deleteTarget) return;
    const project = deleteTarget;
    setDeleteTarget(null);
    try {
      await api.deleteProject(project.id);
      await reloadProjects();
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  function roleLabel(value: string): string {
    if (value === 'owner') return t.projectsRoleOwner;
    if (value === 'member') return t.projectsRoleMember;
    return t.projectsRoleNone;
  }

  function ownerCell(project: Project): ReactNode {
    if (!project.owner_user_id) return '–';
    if (project.owner_user_id === userId) return t.projectsOwnerSelf;
    return <ShortId id={project.owner_user_id} />;
  }

  function rowActions(project: Project): RowAction[] {
    const actions: RowAction[] = [];
    if (project.can_manage) {
      actions.push(
        {
          key: 'rename',
          label: t.projectsActionRename,
          icon: <EditIcon fontSize="small" />,
          onClick: () => openEdit(project),
        },
        {
          key: 'members',
          label: t.projectsActionMembers,
          icon: <GroupIcon fontSize="small" />,
          onClick: () => openMembers(project),
        },
        {
          key: 'tokens',
          label: t.projectsActionTokens,
          icon: <VpnKeyIcon fontSize="small" />,
          onClick: () => openTokens(project),
        },
        {
          key: 'delete',
          label: t.projectsActionDelete,
          icon: <DeleteIcon fontSize="small" />,
          color: 'error',
          onClick: () => setDeleteTarget(project),
        },
      );
    }
    return actions;
  }

  const columns: ListColumn<Project>[] = [
    {
      id: 'name',
      label: t.tableName,
      value: (p) => p.name,
      filter: 'text',
      render: (p) =>
        p.coupled_group_id ? (
          <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
            <span>{p.name}</span>
            <Chip size="small" label={t.projectsCoupledChip(p.coupled_group_name || '')} />
          </Box>
        ) : (
          p.name
        ),
    },
    { id: 'description', label: t.projectDescription, value: (p) => p.description, filter: 'text' },
    {
      id: 'owner',
      label: t.projectsColOwner,
      value: (p) => p.owner_user_id,
      render: (p) => ownerCell(p),
    },
    {
      id: 'role',
      label: t.projectsColMyRole,
      value: (p) => p.my_role,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => roleLabel(v),
      render: (p) => roleLabel(p.my_role),
    },
    {
      id: 'members',
      label: t.projectsColMembers,
      value: (p) => String(p.member_count),
      numeric: true,
    },
    {
      id: 'groups',
      label: t.projectsColGroups,
      value: (p) => String(p.group_count),
      numeric: true,
    },
    {
      id: 'total_tokens',
      label: t.projectsColTotalTokens,
      value: (p) => String(p.total_tokens ?? 0),
      numeric: true,
      render: (p) => (p.total_tokens ?? 0).toLocaleString(),
    },
  ];

  const listLabels = listTableLabels(t);

  // --- Create / edit / members sub-views (replace the list in place) -------

  if (mode !== 'list' && mode.kind === 'create') {
    // "create a new group" needs an admin-tier parent (mirrors GroupsView's own
    // user-group create form: parentUnavailable = the caller is in 0 admin
    // groups -> can't create one at all; parentUnresolved = in >1, none picked
    // yet). Both disable Save + parentUnavailable shows GroupsView's existing
    // warning (no redundant i18n key).
    const parentUnavailable = coupleMode === 'create' && adminGroups.length === 0;
    const parentUnresolved =
      coupleMode === 'create' && adminGroups.length > 1 && newGroupParent === '';
    // The couple toggle needs a group chosen (select mode) or a non-blank new
    // name + a resolved parent (create mode) before it can submit; coupling
    // off is never invalid.
    const coupleInvalid =
      (coupleMode === 'select' && coupleGroupId === '') ||
      (coupleMode === 'create' &&
        (newGroupName.trim() === '' || parentUnavailable || parentUnresolved));
    let newGroupParentField: ReactNode;
    if (parentUnavailable) {
      newGroupParentField = <Alert severity="warning">{t.groupsNoAdminGroupHint}</Alert>;
    } else if (adminGroups.length === 1) {
      newGroupParentField = (
        <Typography variant="body2" color="text.secondary">
          {t.groupsParentAuto(adminGroups[0].name)}
        </Typography>
      );
    } else {
      newGroupParentField = (
        <SearchableSelect
          id="project-couple-parent"
          label={t.groupsParentLabel}
          value={newGroupParent}
          onChange={setNewGroupParent}
          options={adminGroups.map((g) => ({ value: g.id, label: g.name }))}
        />
      );
    }
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.projects, onClick: () => setMode('list') },
            { label: t.projectsCreateTitle },
          ]}
        />
        <Panel titleId="projects-create-heading" title={t.projectsCreateTitle}>
          <Box
            component="form"
            onSubmit={(event) => void submitCreate(event)}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(220px, 480px)', gap: 2.25 }}
          >
            <Field
              id="project-name"
              label={t.projectName}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
            <Field
              id="project-description"
              label={t.projectDescription}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              multiline
              minRows={2}
            />
            <FormControlLabel
              control={
                <Checkbox
                  checked={coupleMode !== 'none'}
                  onChange={(e) => setCoupleMode(e.target.checked ? 'select' : 'none')}
                />
              }
              label={t.projectsCoupleToggle}
            />
            {coupleMode !== 'none' && (
              <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
                <RadioGroup
                  row
                  value={coupleMode}
                  onChange={(e) => setCoupleMode(e.target.value === 'create' ? 'create' : 'select')}
                >
                  <FormControlLabel
                    value="select"
                    control={<Radio />}
                    label={t.projectsCoupleModeSelect}
                  />
                  <FormControlLabel
                    value="create"
                    control={<Radio />}
                    label={t.projectsCoupleModeCreate}
                  />
                </RadioGroup>
                {coupleMode === 'select' ? (
                  <SearchableSelect
                    id="project-couple-group"
                    label={t.projectsCoupleSelectLabel}
                    value={coupleGroupId}
                    onChange={setCoupleGroupId}
                    options={ownedGroups.map((g) => ({ value: g.id, label: g.name }))}
                  />
                ) : (
                  <>
                    <Field
                      id="project-couple-new-name"
                      label={t.projectsCoupleNewName}
                      value={newGroupName}
                      onChange={(e) => setNewGroupName(e.target.value)}
                      required
                    />
                    {newGroupParentField}
                  </>
                )}
              </Box>
            )}
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy || coupleInvalid}>
                {t.save}
              </Button>
              <Button
                type="button"
                variant="text"
                color="secondary"
                onClick={() => setMode('list')}
              >
                {t.cancel}
              </Button>
            </Box>
          </Box>
        </Panel>
      </>
    );
  }

  if (mode !== 'list' && mode.kind === 'edit') {
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.projects, onClick: () => setMode('list') },
            { label: t.projectsEditTitle },
          ]}
        />
        <Panel titleId="projects-edit-heading" title={t.projectsEditTitle}>
          <Box
            component="form"
            onSubmit={submitEdit}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(220px, 480px)', gap: 2.25 }}
          >
            <Field
              id="project-edit-name"
              label={t.projectName}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
            <Field
              id="project-edit-description"
              label={t.projectDescription}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              multiline
              minRows={2}
            />
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy}>
                {t.save}
              </Button>
              <Button
                type="button"
                variant="text"
                color="secondary"
                onClick={() => setMode('list')}
              >
                {t.cancel}
              </Button>
            </Box>
          </Box>
        </Panel>
      </>
    );
  }

  if (mode !== 'list' && mode.kind === 'members') {
    const project = mode.project;
    const userCandidateOptions = (candidateUsers ?? []).map((c) => ({
      value: c.id,
      label: c.display_name || c.email,
      sublabel: c.email,
    }));
    const groupCandidateOptions = (candidateGroups ?? []).map((g) => ({
      value: g.id,
      label: g.name,
    }));
    const rosterGroups = roster?.groups ?? [];
    // Coupled projects (spec 2026-08-09): membership is managed via the
    // coupled group, not directly -- add/remove is backend-refused
    // (project.coupled) so the forms/actions are hidden entirely, and
    // ownership transfer stays with the group (not this project).
    const isCoupled = !!project.coupled_group_id;
    // The members table ALWAYS renders the EFFECTIVE member set
    // (transfer_candidates = direct-users ∪ group-resolved members, the same
    // set TransferProject accepts) -- for both coupled AND non-coupled
    // projects. This is required so a group-resolved (non-direct) member can
    // still be offered as an "Eigentuemer wechseln" target (the standalone
    // picker this replaced fed from transfer_candidates independently of the
    // visible roster; folding transfer into a row action means the target
    // must have a row, so the table itself must include group-resolved
    // members -- see the 2026-08-09 "transfer_candidates" follow-up fix this
    // must not regress).
    const displayedMembers = roster?.transfer_candidates ?? [];
    // The set of user ids that are DIRECT members (a real project_members
    // row) -- used to gate Entfernen: a group-resolved-only member has no
    // direct row to remove (you'd remove the group, not the user).
    const directMemberIds = new Set((roster?.users ?? []).map((u) => u.id));
    // Transfer is owner-only (spec §5) and never targets the owner
    // themselves -- projects have no manager tier, so unlike GroupsView's
    // roster the only member-row actions are transfer (owner-only, on a
    // non-owner row, INCLUDING a group-resolved member) and remove (DIRECT
    // members only).
    const canTransfer = project.my_role === 'owner' && !isCoupled;
    const rosterStillLoading = rosterLoading && roster === null;

    const memberColumns: ListColumn<UserRef>[] = [
      {
        id: 'name',
        label: t.tableName,
        value: (u) => u.display_name || u.email || u.id,
        filter: 'text',
        render: (u) => (
          <Box sx={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
            <Typography sx={{ fontWeight: 600 }}>{u.display_name || u.email || u.id}</Typography>
            {u.display_name && u.email && (
              <Typography variant="caption" color="text.secondary">
                {u.email}
              </Typography>
            )}
          </Box>
        ),
      },
    ];

    // Row actions mirror GroupsView's member roster: a coupled project's
    // roster is read-only (isCoupled gates the whole `actions` prop off, not
    // just each row's array, so no "row menu" column renders at all).
    function memberRowActions(u: UserRef): RowAction[] {
      const actions: RowAction[] = [];
      // Eigentuemer wechseln: any EFFECTIVE member other than the owner --
      // including a group-resolved (non-direct) member, since
      // TransferProject accepts any isProjectMember.
      if (canTransfer && u.id !== project.owner_user_id) {
        actions.push({
          key: 'transfer',
          label: t.projectsActionTransfer,
          icon: <SwapHorizIcon fontSize="small" />,
          disabled: memberBusy,
          onClick: () => {
            setTransferUserId(u.id);
            setTransferTarget(project);
          },
        });
      }
      // Entfernen: DIRECT members only -- a group-resolved-only member has no
      // project_members row to remove (removing them means removing the
      // assigned group, from the groups table below).
      if (directMemberIds.has(u.id)) {
        actions.push({
          key: 'remove',
          label: t.projectsActionRemoveMember,
          icon: <PersonRemoveIcon fontSize="small" />,
          color: 'error',
          disabled: memberBusy,
          onClick: () => void removeMember(project, u.id),
        });
      }
      return actions;
    }

    const groupColumns: ListColumn<ProjectGroupRef>[] = [
      { id: 'name', label: t.tableName, value: (g) => g.name, filter: 'text' },
    ];

    function groupRowActions(g: ProjectGroupRef): RowAction[] {
      return [
        {
          key: 'remove',
          label: t.projectsActionRemoveGroup,
          icon: <GroupRemoveIcon fontSize="small" />,
          color: 'error',
          disabled: memberBusy,
          onClick: () => void removeGroup(project, g.id),
        },
      ];
    }

    const memberListLabels = { ...listLabels, empty: t.projectsMembersEmpty };
    const groupListLabels = { ...listLabels, empty: t.projectsGroupsEmpty };

    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.projects, onClick: () => setMode('list') },
            { label: t.projectsMembersTitle(project.name) },
          ]}
        />
        <Panel titleId="projects-members-heading" title={t.projectsMembersTitle(project.name)}>
          <Stack spacing={3}>
            {isCoupled && <Alert severity="info">{t.projectsCoupledNote}</Alert>}
            <Stack direction="row" spacing={4}>
              <Typography variant="body2">
                {t.projectsColMembers}: {project.member_count}
              </Typography>
              <Typography variant="body2">
                {t.projectsColGroups}: {project.group_count}
              </Typography>
            </Stack>
            <Box>
              <Typography variant="subtitle2" gutterBottom>
                {t.projectsMembersLabel}
              </Typography>
              <ListTable
                rows={displayedMembers}
                columns={memberColumns}
                rowKey={(u) => u.id}
                actions={isCoupled ? undefined : memberRowActions}
                storageKey="op.project-members"
                labels={memberListLabels}
                loading={rosterStillLoading}
              />
            </Box>
            {!isCoupled && (
              <Box component="form" onSubmit={(event) => void submitAddMembers(project, event)}>
                <Stack spacing={1.25}>
                  <MultiSelectField
                    id="project-candidate-users"
                    label={t.projectsAddMembersLabel}
                    options={userCandidateOptions}
                    selected={selectedUsers}
                    onChange={setSelectedUsers}
                    disabled={candidatesLoading}
                  />
                  <Box>
                    <Button
                      type="submit"
                      variant="contained"
                      disabled={memberBusy || selectedUsers.length === 0}
                    >
                      {t.projectsActionAdd}
                    </Button>
                  </Box>
                </Stack>
              </Box>
            )}
            <Box>
              <Typography variant="subtitle2" gutterBottom>
                {t.projectsGroupsLabel}
              </Typography>
              <ListTable
                rows={rosterGroups}
                columns={groupColumns}
                rowKey={(g) => g.id}
                actions={isCoupled ? undefined : groupRowActions}
                storageKey="op.project-groups"
                labels={groupListLabels}
                loading={rosterStillLoading}
              />
            </Box>
            {!isCoupled && (
              <Box component="form" onSubmit={(event) => void submitAddGroups(project, event)}>
                <Stack spacing={1.25}>
                  <MultiSelectField
                    id="project-candidate-groups"
                    label={t.projectsAddGroupsLabel}
                    options={groupCandidateOptions}
                    selected={selectedGroups}
                    onChange={setSelectedGroups}
                    disabled={candidatesLoading}
                  />
                  <Box>
                    <Button
                      type="submit"
                      variant="contained"
                      disabled={memberBusy || selectedGroups.length === 0}
                    >
                      {t.projectsActionAdd}
                    </Button>
                  </Box>
                </Stack>
              </Box>
            )}
          </Stack>
        </Panel>
        <ConfirmDialog
          open={transferTarget !== null}
          title={t.projectsTransferConfirmTitle}
          body={t.projectsTransferConfirmBody}
          confirmLabel={t.projectsActionTransfer}
          cancelLabel={t.cancel}
          onConfirm={() => void confirmTransfer()}
          onCancel={() => setTransferTarget(null)}
        />
      </>
    );
  }

  if (mode !== 'list' && mode.kind === 'tokens') {
    const project = mode.project;
    const tokenRows = projectTokens?.tokens ?? [];
    const tokenStillLoading = tokensLoading && projectTokens === null;

    const tokenColumns: ListColumn<ProjectToken>[] = [
      { id: 'name', label: t.tableName, value: (tok) => tok.name, filter: 'text' },
      {
        id: 'owner',
        label: t.projectsTokenOwner,
        value: (tok) => tok.owner_name || tok.owner_user_id,
        filter: 'text',
      },
      {
        id: 'status',
        label: t.tableStatus,
        value: (tok) => tok.status,
        filter: 'enum',
        searchable: false,
        enumLabel: (v) => projectTokenStatusLabel(t, v as ProjectToken['status']),
        render: (tok) => (
          <StatusChip status={tok.status} label={projectTokenStatusLabel(t, tok.status)} />
        ),
      },
      { id: 'created', label: t.captureCreatedAt, value: (tok) => formatDate(tok.created_at, '–') },
      {
        id: 'requests',
        label: t.projectsTokensColRequests,
        value: (tok) => String(tok.request_count),
        numeric: true,
        render: (tok) => tok.request_count.toLocaleString(),
      },
      {
        id: 'prompt',
        label: t.projectsTokensColPrompt,
        value: (tok) => String(tok.input_tokens),
        numeric: true,
        render: (tok) => tok.input_tokens.toLocaleString(),
      },
      {
        id: 'generated',
        label: t.projectsTokensColGenerated,
        value: (tok) => String(tok.output_tokens),
        numeric: true,
        render: (tok) => tok.output_tokens.toLocaleString(),
      },
      {
        id: 'total',
        label: t.projectsTokensColTotal,
        value: (tok) => String(tok.total_tokens),
        numeric: true,
        render: (tok) => tok.total_tokens.toLocaleString(),
      },
    ];

    function tokenRowActions(tok: ProjectToken): RowAction[] {
      return [
        {
          key: 'detach',
          label: t.projectsTokenDetach,
          icon: <LinkOffIcon fontSize="small" />,
          color: 'error',
          disabled: memberBusy,
          onClick: () => setDetachTarget({ project, token: tok }),
        },
      ];
    }

    const tokenListLabels = { ...listLabels, empty: t.projectsTokensEmpty };

    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.projects, onClick: () => setMode('list') },
            { label: t.projectsTokensTitle(project.name) },
          ]}
        />
        <Panel titleId="projects-tokens-heading" title={t.projectsTokensTitle(project.name)}>
          <Box>
            <Typography variant="subtitle2" gutterBottom>
              {t.projectsTokensLabel}
            </Typography>
            <ListTable
              rows={tokenRows}
              columns={tokenColumns}
              rowKey={(tok) => tok.id}
              actions={project.can_manage ? tokenRowActions : undefined}
              storageKey="op.project-token-usage"
              labels={tokenListLabels}
              loading={tokenStillLoading}
            />
            {projectTokens && (
              <Box sx={{ mt: 1.5, pt: 1, borderTop: '1px solid var(--line)' }}>
                <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
                  <Typography variant="subtitle2">{t.projectsTokensTotalLabel}</Typography>
                  <Tooltip title={t.projectsTokensTotalNote}>
                    <InfoOutlinedIcon fontSize="inherit" sx={{ color: 'text.secondary' }} />
                  </Tooltip>
                </Box>
                <Box sx={{ display: 'flex', gap: 3, flexWrap: 'wrap', mt: 0.5 }}>
                  <Typography variant="body2" color="text.secondary">
                    {t.projectsTokensColRequests}:{' '}
                    {projectTokens.total.request_count.toLocaleString()}
                  </Typography>
                  <Typography variant="body2" color="text.secondary">
                    {t.projectsTokensColPrompt}: {projectTokens.total.input_tokens.toLocaleString()}
                  </Typography>
                  <Typography variant="body2" color="text.secondary">
                    {t.projectsTokensColGenerated}:{' '}
                    {projectTokens.total.output_tokens.toLocaleString()}
                  </Typography>
                  <Typography variant="body2" color="text.secondary">
                    {t.projectsTokensColTotal}: {projectTokens.total.total_tokens.toLocaleString()}
                  </Typography>
                </Box>
              </Box>
            )}
          </Box>
        </Panel>
        <ConfirmDialog
          open={detachTarget !== null}
          title={t.projectsTokenDetachConfirmTitle}
          body={t.projectsTokenDetachConfirmBody}
          confirmLabel={t.projectsTokenDetach}
          cancelLabel={t.cancel}
          onConfirm={() => void confirmDetachToken()}
          onCancel={() => setDetachTarget(null)}
        />
      </>
    );
  }

  // --- List view -------------------------------------------------------------

  return (
    <>
      <PageTitle title={t.projects} subtitle={t.projectsIntro} />
      <Panel
        titleId="projects-heading"
        title={t.projects}
        actions={
          <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
            {t.projectsCreateTitle}
          </Button>
        }
      >
        <ListTable
          rows={projects}
          columns={columns}
          rowKey={(p) => p.id}
          actions={rowActions}
          storageKey="op.projects"
          labels={listLabels}
          loading={projectsLoading}
        />
      </Panel>
      <ConfirmDialog
        open={deleteTarget !== null}
        title={t.projectsDeleteConfirmTitle}
        body={t.projectsDeleteConfirmBody}
        confirmLabel={t.projectsActionDelete}
        cancelLabel={t.cancel}
        onConfirm={() => void confirmDelete()}
        onCancel={() => setDeleteTarget(null)}
      />
    </>
  );
}
