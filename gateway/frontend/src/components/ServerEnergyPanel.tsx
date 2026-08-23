// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import {
  forwardRef,
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
  type ForwardedRef,
} from 'react';
import { Box, Button } from '@mui/material';
import type { PortalServer } from '../api';
import { availableUnits, fromEur, toEur, type CurrencyUnit } from '../currency';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { useToast } from './shared/ToastProvider';

// Renders a price for display without float noise (e.g. 0.3*100 must read
// "30", not "30.000000000000004"), rounded to 6 decimal places. 0/non-finite
// -> "" (matches the existing "blank = unset" convention for the other three
// energy fields: `server.price_per_kwh ? String(...) : ""`).
function fmtPrice(n: number): string {
  if (!Number.isFinite(n)) return '';
  const rounded = Math.round(n * 1e6) / 1e6;
  return rounded === 0 ? '' : String(rounded);
}

// Option label for a currency-unit dropdown entry.
function unitLabel(t: Translation, u: CurrencyUnit): string {
  switch (u) {
    case 'eur':
      return t.currencyUnitEur;
    case 'eur_cent':
      return t.currencyUnitEurCent;
    case 'usd':
      return t.currencyUnitUsd;
    case 'usd_cent':
      return t.currencyUnitUsdCent;
  }
}

// Parse a free-text numeric input into a non-negative number; blank/invalid → 0
// (the backend treats 0 as "unset / use default").
function num(s: string): number {
  const n = Number(s.trim());
  return s.trim() === '' || Number.isNaN(n) || n < 0 ? 0 : n;
}

// The values the CREATE form's single combined POST body needs (already
// num()-parsed / EUR-converted, exactly mirroring what the dedicated
// setServerEnergy save sends for an EDIT). Exposed via ref so the parent
// create-submit handler can fold them into its one CreateServerRequest —
// unlike every other server-edit save, create has no separate energy
// endpoint call.
export type ServerEnergyPanelHandle = {
  getCreatePayload: () => {
    estimated_watts: number;
    idle_watts: number;
    price_per_kwh: number;
    price_unit: CurrencyUnit;
    pue: number;
  };
};

type ServerEnergyPanelProps = {
  t: Translation;
  api: Pick<PortalApi, 'setServerEnergy'>;
  // The server being edited, or null on create (no dedicated Save button then —
  // the fields are still rendered/collected for the combined create POST).
  server: PortalServer | null;
  // USD-per-EUR conversion factor (fetched once, at the ServerList level, so
  // every open of this form sees the SAME warm value instead of refetching on
  // every mount) — drives the price-unit dropdown's options + every
  // EUR<->unit conversion.
  currencyFactor: number;
  // Called after a successful dedicated energy save with the fresh server DTO,
  // so the parent can update its `servers` list + edit-mode context.
  onSaved?: (updated: PortalServer) => void;
};

// Energy-attribution config panel (edit form's "Energy & cost" section, also
// rendered on create so the same four fields seed the combined create POST).
// Purely additive -- no engine consumes these yet; free-text numeric fields,
// blank/0 = "unset / use default".
function ServerEnergyPanelImpl(
  { t, api, server, currencyFactor, onSaved }: ServerEnergyPanelProps,
  ref: ForwardedRef<ServerEnergyPanelHandle>,
) {
  const { showError, showSuccess } = useToast();
  const [estimatedWatts, setEstimatedWatts] = useState(() =>
    server?.estimated_watts ? String(server.estimated_watts) : '',
  );
  const [idleWatts, setIdleWatts] = useState(() =>
    server?.idle_watts ? String(server.idle_watts) : '',
  );
  const [priceUnit, setPriceUnit] = useState<CurrencyUnit>(() => server?.price_unit || 'eur_cent');
  // pricePerKwh holds the price DISPLAY string in the currently selected
  // priceUnit, never raw EUR — see the seed/convert/save derivation below.
  const [pricePerKwh, setPricePerKwh] = useState(() => {
    if (!server) return '';
    const unit = server.price_unit || 'eur_cent';
    const displayUnit: CurrencyUnit = availableUnits(currencyFactor).includes(unit)
      ? unit
      : 'eur_cent';
    return fmtPrice(fromEur(server.price_per_kwh, displayUnit, currencyFactor));
  });
  const [pue, setPue] = useState(() => (server?.pue ? String(server.pue) : ''));
  // Whether the price field has been hand-edited since mount — while pristine,
  // a currencyFactor that arrives AFTER the initial seed re-derives the
  // displayed price (matters only for a USD unit; eur/eur_cent are
  // factor-independent, so re-seeding them is a harmless no-op).
  const pricePristineRef = useRef(true);
  const [energyBusy, setEnergyBusy] = useState(false);

  // A stored/pending USD unit degrades to eur_cent for DISPLAY/SAVE when the
  // conversion factor is 0 (USD unavailable) — never mutate the raw `priceUnit`
  // state itself, only derive this for rendering/saving (mirrors Activity.tsx's
  // `effectiveCostUnit`). Without this, fromEur(x,"usd",0)===0 would show a
  // blank price, the unit Select would sit out of its own option set, AND a
  // save would silently zero the stored EUR price.
  const effectiveUnit: CurrencyUnit = availableUnits(currencyFactor).includes(priceUnit)
    ? priceUnit
    : 'eur_cent';

  // A factor that arrives AFTER the price field was seeded re-derives the
  // displayed price, but ONLY while the field is still pristine (untouched) —
  // a hand-edited value must never be silently reinterpreted. No-op for create
  // (no stored server value to reseed from) and for eur/eur_cent units (their
  // conversion doesn't use the factor).
  useEffect(() => {
    if (!pricePristineRef.current) return;
    if (!server) return;
    setPricePerKwh(fmtPrice(fromEur(server.price_per_kwh, effectiveUnit, currencyFactor)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currencyFactor]);

  // Unit-dropdown change: convert the CURRENTLY SHOWN price from the old unit to
  // EUR and back into the new unit, so the underlying price is preserved exactly
  // (never reinterpreted as a raw number in the new unit). Does not affect the
  // pristine flag — the operator hasn't touched the NUMBER, only its unit.
  function changePriceUnit(newUnit: CurrencyUnit) {
    const eur = toEur(num(pricePerKwh), effectiveUnit, currencyFactor);
    setPricePerKwh(fmtPrice(fromEur(eur, newUnit, currencyFactor)));
    setPriceUnit(newUnit);
  }

  // Dedicated "Energy & cost" save (edit only): a full-replace of the four energy
  // columns via the owner/admin-scoped endpoint, independent of the main form's
  // Save.
  async function saveServerEnergy() {
    if (!server) return;
    setEnergyBusy(true);
    try {
      const updated = await api.setServerEnergy(
        server.id,
        num(estimatedWatts),
        num(idleWatts),
        toEur(num(pricePerKwh), effectiveUnit, currencyFactor),
        num(pue),
        effectiveUnit,
      );
      onSaved?.(updated);
      setEstimatedWatts(updated.estimated_watts ? String(updated.estimated_watts) : '');
      setIdleWatts(updated.idle_watts ? String(updated.idle_watts) : '');
      const updatedUnit = updated.price_unit || 'eur_cent';
      setPriceUnit(updatedUnit);
      // Defensive degrade (mirrors the initial seed above): we just sent
      // effectiveUnit, so this is normally already in-range, but guards
      // against a stale currencyFactor between the request and this response.
      const updatedDisplayUnit: CurrencyUnit = availableUnits(currencyFactor).includes(updatedUnit)
        ? updatedUnit
        : 'eur_cent';
      setPricePerKwh(fmtPrice(fromEur(updated.price_per_kwh, updatedDisplayUnit, currencyFactor)));
      pricePristineRef.current = true;
      setPue(updated.pue ? String(updated.pue) : '');
      showSuccess(t.serverEnergySaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setEnergyBusy(false);
    }
  }

  useImperativeHandle(ref, () => ({
    getCreatePayload: () => ({
      estimated_watts: num(estimatedWatts),
      idle_watts: num(idleWatts),
      price_per_kwh: toEur(num(pricePerKwh), effectiveUnit, currencyFactor),
      price_unit: effectiveUnit,
      pue: num(pue),
    }),
  }));

  return (
    // Its own section (like the NetBird linkage), saved by the main form's
    // Save button on create (the create payload reads this state via the
    // imperative handle above) or by its own button on edit. Shown for both
    // create and edit.
    <Panel
      titleId="server-energy-heading"
      title={t.serverEnergySection}
      subtitle={t.serverEnergySectionIntro}
    >
      <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}>
        <Field
          id="server-estimated-watts"
          type="number"
          label={t.serverEstimatedWatts}
          value={estimatedWatts}
          onChange={(e) => setEstimatedWatts(e.target.value)}
          helperText={t.serverEstimatedWattsHelp}
          inputProps={{ min: 0, step: 'any' }}
        />
        <Field
          id="server-idle-watts"
          type="number"
          label={t.serverIdleWatts}
          value={idleWatts}
          onChange={(e) => setIdleWatts(e.target.value)}
          helperText={t.serverIdleWattsHelp}
          inputProps={{ min: 0, step: 'any' }}
        />
        <SelectField
          id="server-price-unit"
          label={t.priceUnitLabel}
          value={effectiveUnit}
          onChange={(e) => changePriceUnit(e.target.value as CurrencyUnit)}
        >
          {availableUnits(currencyFactor).map((u) => (
            <option key={u} value={u}>
              {unitLabel(t, u)}
            </option>
          ))}
        </SelectField>
        <Field
          id="server-price-per-kwh"
          type="number"
          label={t.serverPricePerKwh}
          value={pricePerKwh}
          onChange={(e) => {
            pricePristineRef.current = false;
            setPricePerKwh(e.target.value);
          }}
          inputProps={{ min: 0, step: 'any' }}
        />
        <Field
          id="server-pue"
          type="number"
          label={t.serverPue}
          value={pue}
          onChange={(e) => setPue(e.target.value)}
          helperText={t.serverPueHelp}
          inputProps={{ min: 0, step: 'any' }}
        />
        {server && (
          <Box>
            <Button
              type="button"
              variant="contained"
              disabled={energyBusy}
              onClick={saveServerEnergy}
            >
              {t.serverEnergySave}
            </Button>
          </Box>
        )}
      </Box>
    </Panel>
  );
}

export const ServerEnergyPanel = forwardRef(ServerEnergyPanelImpl);
