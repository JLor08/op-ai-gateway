// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  INSTANCES_MAX,
  instanceAddress,
  instanceCountFromText,
  instanceCountText,
  instanceIndex,
  primaryStatusRows,
  runningInstancesBySpec,
  specStopState,
} from './runtimeInstances';
import type { RuntimeStatus } from '../../api';

function row(specId: string, instance: number, state = 'running'): RuntimeStatus {
  return {
    spec_id: specId,
    instance,
    model: instance === 0 ? 'm' : `m#${instance}`,
    state,
    since: '2026-11-01T12:00:00Z',
    in_flight: 0,
    restarts: 0,
    context_size: 0,
    active_requests: 0,
    queue_depth: 0,
    metrics_probe: '',
    context_probe: '',
  };
}

describe('instanceIndex', () => {
  it('reads the row instance, and a row without a usable one as instance 0', () => {
    expect(instanceIndex(row('s', 0))).toBe(0);
    expect(instanceIndex(row('s', 3))).toBe(3);
    // A row from a gateway that predates instances carries no field at all.
    const { instance: _dropped, ...legacy } = row('s', 0);
    expect(instanceIndex(legacy as RuntimeStatus)).toBe(0);
    expect(instanceIndex({ ...row('s', 0), instance: -1 })).toBe(0);
    expect(instanceIndex({ ...row('s', 0), instance: 1.5 })).toBe(0);
  });
});

describe('primaryStatusRows', () => {
  it('picks instance 0 wherever the stream lists it', () => {
    const picked = primaryStatusRows([row('s', 1), row('s', 2), row('s', 0, 'starting')]);
    expect(picked.get('s')).toMatchObject({ instance: 0, state: 'starting' });
  });

  it('without an instance-0 row, picks the first running instance', () => {
    const picked = primaryStatusRows([row('s', 1, 'starting'), row('s', 2), row('s', 3)]);
    expect(picked.get('s')?.instance).toBe(2);
  });

  it('with neither, falls back to the first row, so the spec still has a state to show', () => {
    const picked = primaryStatusRows([row('s', 2, 'starting'), row('s', 1, 'crashed')]);
    expect(picked.get('s')?.instance).toBe(2);
  });

  it('picks per spec id', () => {
    const picked = primaryStatusRows([row('a', 1), row('b', 0), row('a', 0)]);
    expect(picked.get('a')?.instance).toBe(0);
    expect(picked.get('b')?.instance).toBe(0);
    expect(picked.size).toBe(2);
  });
});

// The restart sequence's wait: under force_stopped every instance of the spec
// drains at once, in any order, so the spec is down only once every row is.
describe('specStopState', () => {
  it('is absent while the stream lists no row of the spec', () => {
    expect(specStopState([row('other', 0, 'stopped')], 's')).toBe('absent');
  });

  it('is pending while any instance is not stopped, whichever it is', () => {
    expect(specStopState([row('s', 0, 'stopped'), row('s', 1, 'draining')], 's')).toBe('pending');
    expect(specStopState([row('s', 0, 'draining'), row('s', 1, 'stopped')], 's')).toBe('pending');
  });

  it('is stopped once every listed row of the spec is, a retired instance being no row', () => {
    expect(specStopState([row('s', 0, 'stopped')], 's')).toBe('stopped');
    expect(specStopState([row('s', 1, 'stopped'), row('s', 2, 'stopped')], 's')).toBe('stopped');
    expect(specStopState([row('s', 0, 'stopped'), row('s', 1, 'stopped'), row('t', 0)], 's')).toBe(
      'stopped',
    );
  });
});

describe('instanceAddress', () => {
  it('is the mapping name for instance 0 and name#k for instance k', () => {
    expect(instanceAddress('m', row('s', 0))).toBe('m');
    expect(instanceAddress('m', row('s', 2))).toBe('m#2');
  });
});

describe('runningInstancesBySpec', () => {
  it('counts only running rows, per spec id', () => {
    const counts = runningInstancesBySpec([
      row('a', 0),
      row('a', 1, 'starting'),
      row('a', 2),
      row('b', 0, 'stopped'),
    ]);
    expect(counts.get('a')).toBe(2);
    expect(counts.get('b')).toBeUndefined();
  });
});

describe('instance count text', () => {
  it('reads an empty field as 0 and shows a stored 0 as the empty field', () => {
    expect(instanceCountFromText('')).toBe(0);
    expect(instanceCountFromText('  ')).toBe(0);
    expect(instanceCountFromText('3')).toBe(3);
    expect(instanceCountText(0)).toBe('');
    expect(instanceCountText(4)).toBe('4');
  });

  it("caps a spec's instances at the agent's 64", () => {
    expect(INSTANCES_MAX).toBe(64);
  });
});
