// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ModelOption } from '../../api';
import type { Translation } from './types';
import { SearchableSelect, type SearchableOption } from './SearchableSelect';

// A gateway model as a target picker shows it. Both ModelOption (the model
// listing) and ServerModelOption (one server's own listing, see
// api.serverModels) satisfy this.
export type OverrideModelOption = { id: string; display_name: string };

// The targets of every model-valued token setting: the catch-all override,
// each rule's target and the unknown-model fallback, on user and service
// tokens alike.
//
// `options` are the names the pickers offer. `refused` holds the listed
// names a save refuses, by id, with their display names: a Models() row
// whose `flavors` are [] is served under no API, and the backend's
// callable-name check leaves such a name out, so a save that targets it fails
// with portal.token_model_override_invalid.
export type OverrideTargets = {
  options: OverrideModelOption[];
  refused: ReadonlyMap<string, string>;
};

/**
 * overrideTargets derives one form's picker targets from the caller's
 * Models() listing. `serverModels` is the listing of the server override
 * while one is set (api.serverModels), else null. That listing carries no
 * flavors and keeps the hidden names, so only the ids whose Models() row has
 * `flavors` [] are subtracted from it.
 *
 * Models() drops hidden names, so the frontend cannot see whether a hidden
 * name has an empty served set: a saved value of that kind is never marked
 * unavailable, in any picker, and under a server override such a name is
 * also offered and the save refuses it (portal.token_model_override_invalid).
 * The server listing also ignores provisioning and group locking, so it can
 * offer other names the save refuses. Both are known limits.
 */
export function overrideTargets(
  models: readonly ModelOption[],
  serverModels: readonly OverrideModelOption[] | null,
): OverrideTargets {
  const refused = new Map<string, string>();
  for (const model of models) {
    if (model.flavors.length === 0) refused.set(model.id, model.display_name);
  }
  const listed: readonly OverrideModelOption[] = serverModels ?? models;
  return { options: listed.filter((model) => !refused.has(model.id)), refused };
}

/**
 * overrideTargetOptions is one picker's option list: the empty "-" choice and
 * every offered target. A current value in `refused` is appended, labelled
 * unavailable, so the field keeps showing the saved setting and the operator
 * sees which value the save fails on.
 */
export function overrideTargetOptions(
  t: Translation,
  targets: OverrideTargets,
  value: string,
): SearchableOption[] {
  const options: SearchableOption[] = [
    { value: '', label: '-' },
    ...targets.options.map((model) => ({ value: model.id, label: model.display_name })),
  ];
  const refusedName = targets.refused.get(value);
  if (refusedName !== undefined) {
    options.push({ value, label: `${refusedName} ${t.tokenOverrideTargetUnavailable}` });
  }
  return options;
}

/**
 * OverrideTargetSelect is the picker of every model-valued token setting. A
 * current value in `refused` also carries SearchableSelect's unavailable
 * marker, whose tooltip is the save's own refusal text.
 */
export function OverrideTargetSelect({
  id,
  label,
  value,
  onChange,
  targets,
  t,
  disabled,
  helperText,
}: Readonly<{
  id: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  targets: OverrideTargets;
  t: Translation;
  disabled?: boolean;
  helperText?: string;
}>) {
  return (
    <SearchableSelect
      id={id}
      label={label}
      value={value}
      onChange={onChange}
      disabled={disabled}
      helperText={helperText}
      options={overrideTargetOptions(t, targets, value)}
      unavailable={targets.refused.has(value)}
      unavailableTitle={t.errorPortalTokenModelOverrideInvalid}
    />
  );
}
