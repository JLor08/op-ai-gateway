// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { PortalApiError } from '../../api';
import type { MessageKey, Translation } from './types';

export const errorLabelByCode: Partial<Record<string, MessageKey>> = {
  'auth.invalid_token': 'errorAuthInvalidToken',
  'auth.invalid_credentials': 'errorAuthInvalidCredentials',
  'auth.csrf_required': 'errorAuthCsrfRequired',
  'auth.session_invalid': 'errorAuthSessionInvalid',
  'auth.set_password_token_invalid': 'errorAuthSetPasswordTokenInvalid',
  'auth.password_too_weak': 'errorAuthPasswordTooWeak',
  'auth.totp_invalid': 'errorAuthTotpInvalid',
  'auth.totp_disable_forbidden': 'errorTotpDisableForbidden',
  'admin.user_conflict': 'errorAdminUserConflict',
  'admin.cannot_disable_last_admin': 'errorAdminCannotDisableLastAdmin',
  'admin.user_not_found': 'errorAdminUserNotFound',
  'portal.token_required': 'errorPortalTokenRequired',
  'portal.token_forbidden': 'errorPortalTokenForbidden',
  'portal.token_name_required': 'errorPortalTokenNameRequired',
  'portal.token_name_conflict': 'errorPortalTokenNameConflict',
  'portal.token_status_invalid': 'errorPortalTokenStatusInvalid',
  'portal.token_not_found': 'errorPortalTokenNotFound',
  'portal.token_scope_invalid': 'errorPortalTokenScopeInvalid',
  'portal.token_scope_forbidden': 'errorPortalTokenScopeForbidden',
  'portal.token_model_override_invalid': 'errorPortalTokenModelOverrideInvalid',
  'token.project_not_member': 'errorTokenProjectNotMember',
  'token.not_found': 'errorTokenNotFound',
  'server.not_found': 'errorServerNotFound',
  'server.forbidden': 'errorServerForbidden',
  'server.name_required': 'errorServerNameRequired',
  'server.domain_required': 'errorServerDomainRequired',
  'server.status_invalid': 'errorServerStatusInvalid',
  'server.owner_invalid': 'errorServerOwnerInvalid',
  'agent_token.conflict': 'errorAgentTokenConflict',
  'agent_token.request_failed': 'errorAgentTokenRequestFailed',
  'application.not_found': 'errorApplicationNotFound',
  'application.type_invalid': 'errorApplicationTypeInvalid',
  'application.scheme_invalid': 'errorApplicationSchemeInvalid',
  'application.port_invalid': 'errorApplicationPortInvalid',
  'application.flavor_invalid': 'errorApplicationFlavorInvalid',
  'application.status_invalid': 'errorApplicationStatusInvalid',
  'application.port_conflict': 'errorApplicationConflict',
  'application.sync_failed': 'errorApplicationSyncFailed',
  'mapping.not_found': 'errorMappingNotFound',
  'mapping.gateway_name_required': 'errorMappingGatewayNameRequired',
  'mapping.app_name_required': 'errorMappingAppNameRequired',
  'mapping.gateway_name_conflict': 'errorMappingGatewayNameConflict',
  'mapping.status_invalid': 'errorMappingStatusInvalid',
  'benchmark.already_running': 'errorBenchmarkAlreadyRunning',
  'benchmark.server_in_use': 'errorBenchmarkServerInUse',
  'benchmark.no_models': 'errorBenchmarkNoModels',
  'limit.validation_failed': 'errorLimitValidationFailed',
  'limit.user_not_found': 'errorLimitUserNotFound',
  'group.not_found': 'errorGroupNotFound',
  'group.name_conflict': 'errorGroupNameConflict',
  'group.name_invalid': 'errorGroupNameInvalid',
  'group.parent_invalid': 'errorGroupParentInvalid',
  'group.tier_invalid': 'errorGroupTierInvalid',
  'group.member_not_visible': 'errorGroupMemberNotVisible',
  'group.not_parent_member': 'errorGroupNotParentMember',
  'group.candidate_invalid': 'errorGroupCandidateInvalid',
  'group.forbidden': 'errorGroupForbidden',
  'user.no_system_group': 'errorUserNoSystemGroup',
  'user.system_group_required': 'errorUserSystemGroupRequired',
  'user.system_group_invalid': 'errorUserSystemGroupInvalid',
  'project.not_found': 'errorProjectNotFound',
  'project.name_conflict': 'errorProjectNameConflict',
  'project.transfer_not_member': 'errorProjectTransferNotMember',
  'project.member_not_visible': 'errorProjectMemberNotVisible',
  'project.group_not_visible': 'errorProjectGroupNotVisible',
  'project.forbidden': 'errorProjectForbidden',
  'project.coupled': 'errorProjectCoupled',
  'project.couple_group_invalid': 'errorProjectCoupleGroupInvalid',
  'project.couple_ambiguous': 'errorProjectCoupleAmbiguous',
  'resource_group.provision_target_invalid': 'errorResourceGroupProvisionTargetInvalid',
  // The plaintext gate's two arming-precondition refusals. Both are mapped, not
  // just the newer one: an operator seeing one translated and its sibling in raw
  // English would read the pair as inconsistent.
  'certificate.edge_https_not_observed': 'errorCertificateEdgeHttpsNotObserved',
  'certificate.edge_arm_requires_https': 'errorCertificateEdgeArmRequiresHttps',
  'server_override.forbidden': 'errorServerOverrideForbidden',
  'server_override.server_unavailable': 'errorServerOverrideServerUnavailable',
  'server_override.model_unavailable': 'errorServerOverrideModelUnavailable',
  'request.failed': 'errorRequestFailed',
  'request.invalid_response': 'errorRequestFailed',
};

export function formatPortalError(err: unknown, t: Translation): string {
  if (err instanceof PortalApiError) {
    const labelKey = errorLabelByCode[err.code];
    return `${err.code}: ${labelKey ? t[labelKey] : err.message}`;
  }
  return err instanceof Error ? err.message : String(err);
}

/**
 * Render a measured metric for a list cell: a fixed number of decimals so the
 * column reads as one scale, and the em-dash placeholder when the metric was
 * never measured (the backend reports that as 0).
 *
 * The fixed decimals also make the cell text sortable as a number — see
 * `ListTable`'s `numeric` columns, which parse this string back.
 */
export function formatMetric(
  value: number | null | undefined,
  decimals: number,
  placeholder = '—',
): string {
  if (!value) {
    return placeholder;
  }
  return value.toFixed(decimals);
}

export function formatDate(value: string | null | undefined, fallback: string): string {
  if (!value) {
    return fallback;
  }
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    return fallback;
  }
  return parsed.toLocaleString();
}
