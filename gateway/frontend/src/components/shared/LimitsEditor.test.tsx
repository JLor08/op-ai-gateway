// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { LimitsEditor } from './LimitsEditor';
import { messages } from '../../i18n';
import { EMPTY_LIMIT_CONFIG, type LimitConfig } from '../../api';

const t = messages.de;

afterEach(cleanup);

// Drives a period SelectField (non-native MUI Select) at the given index
// among the (possibly several, same-labelled) period dropdowns: open it,
// then click the matching option.
function pickPeriod(index: number, optionLabel: string) {
  fireEvent.mouseDown(screen.getAllByLabelText(t.limitPeriodLabel)[index]);
  fireEvent.click(screen.getByRole('option', { name: optionLabel }));
}

describe('LimitsEditor', () => {
  it('renders the four optional limit blocks with their fields', () => {
    render(<LimitsEditor t={t} idPrefix="svc" value={EMPTY_LIMIT_CONFIG} onChange={() => {}} />);
    expect(screen.getByText(t.limitRateTitle)).toBeInTheDocument();
    expect(screen.getByLabelText(t.limitRateRequestsLabel)).toBeInTheDocument();
    expect(screen.getByLabelText(t.limitRateWindowLabel)).toBeInTheDocument();

    expect(screen.getByText(t.limitRequestQuotaTitle)).toBeInTheDocument();
    expect(screen.getByLabelText(t.limitRequestQuotaLabel)).toBeInTheDocument();

    expect(screen.getByText(t.limitTokenQuotaTitle)).toBeInTheDocument();
    expect(screen.getByLabelText(t.limitTokenQuotaLabel)).toBeInTheDocument();

    expect(screen.getByText(t.limitCostBudgetTitle)).toBeInTheDocument();
    expect(screen.getByLabelText(t.limitCostBudgetLabel)).toBeInTheDocument();

    // 3 period dropdowns (rate has none — it uses a window in seconds, not a
    // calendar period).
    expect(screen.getAllByLabelText(t.limitPeriodLabel)).toHaveLength(3);
  });

  it('reports a rate-limit edit via onChange', () => {
    const onChange = vi.fn();
    render(<LimitsEditor t={t} idPrefix="svc" value={EMPTY_LIMIT_CONFIG} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText(t.limitRateRequestsLabel), { target: { value: '100' } });
    expect(onChange).toHaveBeenCalledWith({ ...EMPTY_LIMIT_CONFIG, rate_requests: 100 });
  });

  it('reports the request-quota number and its own paired period independently', () => {
    const onChange = vi.fn();
    render(<LimitsEditor t={t} idPrefix="svc" value={EMPTY_LIMIT_CONFIG} onChange={onChange} />);

    fireEvent.change(screen.getByLabelText(t.limitRequestQuotaLabel), {
      target: { value: '10000' },
    });
    expect(onChange).toHaveBeenCalledWith({ ...EMPTY_LIMIT_CONFIG, request_quota: 10000 });

    // The period dropdowns render in declaration order: request-/token-quota,
    // cost-budget — so index 0 is the request-quota period.
    pickPeriod(0, t.limitPeriodDay);
    expect(onChange).toHaveBeenCalledWith({ ...EMPTY_LIMIT_CONFIG, request_quota_period: 'day' });
  });

  it('reports the token-quota and cost-budget periods independently (index 1 / 2)', () => {
    const onChange = vi.fn();
    render(<LimitsEditor t={t} idPrefix="svc" value={EMPTY_LIMIT_CONFIG} onChange={onChange} />);

    pickPeriod(1, t.limitPeriodWeek);
    expect(onChange).toHaveBeenLastCalledWith({
      ...EMPTY_LIMIT_CONFIG,
      token_quota_period: 'week',
    });

    pickPeriod(2, t.limitPeriodMonth);
    expect(onChange).toHaveBeenLastCalledWith({
      ...EMPTY_LIMIT_CONFIG,
      cost_budget_period: 'month',
    });
  });

  it('shows a read-only usage line only for a configured (period-set) limit', () => {
    const value: LimitConfig = {
      ...EMPTY_LIMIT_CONFIG,
      request_quota: 10000,
      request_quota_period: 'day',
    };
    render(
      <LimitsEditor
        t={t}
        idPrefix="svc"
        value={value}
        onChange={() => {}}
        usage={{ requests_this_period: 8000, tokens_this_period: 5, cost_this_period: 0 }}
      />,
    );
    expect(screen.getByText(t.limitUsageRequestsLine(8000, 10000))).toBeInTheDocument();
    // Token quota is unconfigured (period ""), so no usage line for it even
    // though the usage DTO happens to carry a nonzero tokens_this_period.
    expect(screen.queryByText(t.limitUsageTokensLine(5, 0))).not.toBeInTheDocument();
  });

  it('shows no usage lines at all when no usage prop is supplied', () => {
    const value: LimitConfig = {
      ...EMPTY_LIMIT_CONFIG,
      cost_budget: 50,
      cost_budget_period: 'month',
    };
    render(<LimitsEditor t={t} idPrefix="svc" value={value} onChange={() => {}} />);
    expect(screen.queryByText(t.limitUsageCostLine(0, 50))).not.toBeInTheDocument();
  });

  it('disables every numeric field and blocks opening the period dropdowns when disabled', () => {
    render(
      <LimitsEditor t={t} idPrefix="svc" value={EMPTY_LIMIT_CONFIG} onChange={() => {}} disabled />,
    );
    expect(screen.getByLabelText(t.limitRateRequestsLabel)).toBeDisabled();
    expect(screen.getByLabelText(t.limitRateWindowLabel)).toBeDisabled();
    expect(screen.getByLabelText(t.limitRequestQuotaLabel)).toBeDisabled();
    expect(screen.getByLabelText(t.limitTokenQuotaLabel)).toBeDisabled();
    expect(screen.getByLabelText(t.limitCostBudgetLabel)).toBeDisabled();

    fireEvent.mouseDown(screen.getAllByLabelText(t.limitPeriodLabel)[0]);
    expect(screen.queryByRole('option', { name: t.limitPeriodDay })).not.toBeInTheDocument();
  });
});
