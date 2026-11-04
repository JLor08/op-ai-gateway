// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  RUNTIME_INSTANCES_FEATURE,
  RUNTIME_STOP_FEATURE,
  highestLiveInstance,
  manualStopProtectedCount,
  runtimeRowPlan,
  startInstanceLimit,
  startRoomLeft,
  type RowPlanSpec,
} from './runtimeActions';
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

function spec(overrides: Partial<RowPlanSpec> = {}): RowPlanSpec {
  return {
    replicable: false,
    min_instances: 0,
    max_instances: 0,
    pinned: false,
    admin_state: '',
    ...overrides,
  };
}

// An agent that declares both features the plan reads.
const declared = [RUNTIME_STOP_FEATURE, RUNTIME_INSTANCES_FEATURE];

describe('highestLiveInstance', () => {
  it('is the highest running or starting index of the spec, ignoring other specs', () => {
    const rows = [
      row('s', 0),
      row('s', 1, 'starting'),
      row('s', 2, 'crashed'),
      row('s', 3, 'stopped'),
      row('other', 5),
    ];
    expect(highestLiveInstance(rows, 's')).toBe(1);
  });

  it('is undefined while no instance of the spec runs or starts', () => {
    expect(highestLiveInstance([row('s', 0, 'stopped'), row('s', 1, 'draining')], 's')).toBe(
      undefined,
    );
  });
});

describe('manualStopProtectedCount', () => {
  it('is 0 for a spec that keeps nothing up', () => {
    expect(manualStopProtectedCount(spec())).toBe(0);
    // min_instances means nothing on a spec that is not replicable.
    expect(manualStopProtectedCount(spec({ min_instances: 3 }))).toBe(0);
  });

  it('is min_instances for a replicable spec', () => {
    expect(manualStopProtectedCount(spec({ replicable: true, min_instances: 2 }))).toBe(2);
  });

  it('is at least 1 for a pinned or force_running spec', () => {
    expect(manualStopProtectedCount(spec({ pinned: true }))).toBe(1);
    expect(manualStopProtectedCount(spec({ admin_state: 'force_running' }))).toBe(1);
    expect(
      manualStopProtectedCount(spec({ replicable: true, min_instances: 3, pinned: true })),
    ).toBe(3);
    // force_stopped protects nothing: everything drains under it.
    expect(manualStopProtectedCount(spec({ admin_state: 'force_stopped' }))).toBe(0);
  });
});

describe('startInstanceLimit', () => {
  it('is max_instances, capped at the 64 indexes an address can carry, and 64 for 0', () => {
    expect(startInstanceLimit({ max_instances: 3 })).toBe(3);
    expect(startInstanceLimit({ max_instances: 0 })).toBe(64);
    expect(startInstanceLimit({ max_instances: 100 })).toBe(64);
  });
});

describe('startRoomLeft', () => {
  it('has room below max_instances while an index is free', () => {
    const s = spec({ replicable: true, max_instances: 3 });
    expect(startRoomLeft(s, [row('s', 0), row('s', 1)], 's')).toBe(true);
  });

  it('has no room once every index below max_instances is taken', () => {
    const s = spec({ replicable: true, max_instances: 2 });
    expect(startRoomLeft(s, [row('s', 0), row('s', 1, 'starting')], 's')).toBe(false);
  });

  it('counts an instance 0 at rest as free, and a burst index resting after a failure', () => {
    const s = spec({ replicable: true, max_instances: 2 });
    expect(startRoomLeft(s, [row('s', 0, 'stopped'), row('s', 1)], 's')).toBe(true);
    for (const state of ['crashed', 'start_failed', 'not_permitted']) {
      expect(startRoomLeft(s, [row('s', 0), row('s', 1, state)], 's')).toBe(true);
    }
  });

  it('counts a burst index in any other listed state as taken, as the gateway does', () => {
    const s = spec({ replicable: true, max_instances: 2 });
    for (const state of ['stopped', 'draining', 'backoff', 'pending_vram_unknown']) {
      expect(startRoomLeft(s, [row('s', 0), row('s', 1, state)], 's')).toBe(false);
    }
  });

  it('caps an unlimited maximum at the 64 indexes an address can carry', () => {
    const s = spec({ replicable: true, max_instances: 0 });
    const rows = Array.from({ length: 63 }, (_, k) => row('s', k));
    expect(startRoomLeft(s, rows, 's')).toBe(true);
    expect(startRoomLeft(s, [...rows, row('s', 63)], 's')).toBe(false);
  });
});

describe('runtimeRowPlan, a spec that is not replicable', () => {
  it('offers Start and Pin as stopped on the stopped row, and no Stop', () => {
    const rows = [row('s', 0, 'stopped')];
    expect(runtimeRowPlan(rows[0], spec(), rows, declared)).toEqual({
      start: { more: false },
      pin: 'force_stopped',
    });
  });

  it('offers Stop and Pin as running on the running row, and no Start', () => {
    const rows = [row('s', 0)];
    expect(runtimeRowPlan(rows[0], spec(), rows, declared)).toEqual({
      stop: {},
      pin: 'force_running',
    });
  });

  it('treats a starting row as live: Stop, no Start, Pin as running', () => {
    const rows = [row('s', 0, 'starting')];
    expect(runtimeRowPlan(rows[0], spec(), rows, declared)).toEqual({
      stop: {},
      pin: 'force_running',
    });
  });

  it('disables Stop on a pinned or force_running instance 0', () => {
    const rows = [row('s', 0)];
    expect(runtimeRowPlan(rows[0], spec({ pinned: true }), rows, declared).stop).toEqual({
      disabled: 'protected',
    });
    const forced = runtimeRowPlan(rows[0], spec({ admin_state: 'force_running' }), rows, declared);
    expect(forced.stop).toEqual({ disabled: 'protected' });
    // An override is set, so there is nothing to pin.
    expect(forced.pin).toBeUndefined();
  });

  it('disables Stop when the agent lacks runtime_stop', () => {
    const rows = [row('s', 0)];
    expect(runtimeRowPlan(rows[0], spec(), rows, []).stop).toEqual({ disabled: 'feature' });
  });

  it('names the missing feature over the protected floor: no setting lifts it', () => {
    const rows = [row('s', 0)];
    expect(runtimeRowPlan(rows[0], spec({ pinned: true }), rows, []).stop).toEqual({
      disabled: 'feature',
    });
  });

  it('needs no runtime_instances to start instance 0', () => {
    const rows = [row('s', 0, 'stopped')];
    expect(runtimeRowPlan(rows[0], spec(), rows, [RUNTIME_STOP_FEATURE]).start).toEqual({
      more: false,
    });
  });

  it('disables Start while the spec is force_stopped, and offers no Pin', () => {
    const rows = [row('s', 0, 'stopped')];
    expect(runtimeRowPlan(rows[0], spec({ admin_state: 'force_stopped' }), rows, declared)).toEqual(
      { start: { disabled: 'force_stopped', more: false } },
    );
  });
});

describe('runtimeRowPlan, a replicable spec', () => {
  const replicable = spec({ replicable: true, max_instances: 4 });

  it('keeps Start on the primary row while instances run, saying it starts one more', () => {
    const rows = [row('s', 0), row('s', 1), row('s', 2)];
    expect(runtimeRowPlan(rows[0], replicable, rows, declared)).toEqual({
      start: { more: true },
      pin: 'force_running',
    });
  });

  it('offers Stop only on the highest running or starting instance', () => {
    const rows = [row('s', 0), row('s', 1), row('s', 2, 'starting'), row('s', 3, 'crashed')];
    expect(runtimeRowPlan(rows[1], replicable, rows, declared)).toEqual({});
    expect(runtimeRowPlan(rows[2], replicable, rows, declared)).toEqual({ stop: {} });
    expect(runtimeRowPlan(rows[3], replicable, rows, declared)).toEqual({});
    // Instance 0 is the primary row: Start and Pin, but it is not the highest.
    expect(runtimeRowPlan(rows[0], replicable, rows, declared).stop).toBeUndefined();
  });

  it('disables Stop on the next instance down while a higher one still drains', () => {
    const rows = [row('s', 0), row('s', 1), row('s', 2, 'draining')];
    expect(runtimeRowPlan(rows[1], replicable, rows, declared).stop).toEqual({
      disabled: 'draining',
    });
    // The draining row is on its way out already, and instance 0 is not the
    // highest: neither carries Stop.
    expect(runtimeRowPlan(rows[2], replicable, rows, declared).stop).toBeUndefined();
    expect(runtimeRowPlan(rows[0], replicable, rows, declared).stop).toBeUndefined();
    // Once the drain is over, Stop is usable on #1.
    const after = [row('s', 0), row('s', 1), row('s', 2, 'stopped')];
    expect(runtimeRowPlan(after[1], replicable, after, declared).stop).toEqual({});
  });

  it('keeps Stop usable under force_stopped while a higher instance drains', () => {
    // The agent drains every instance of a force_stopped spec at once and
    // takes a stop of any of them.
    const s = spec({ replicable: true, admin_state: 'force_stopped' });
    const rows = [row('s', 0), row('s', 1, 'draining')];
    expect(runtimeRowPlan(rows[0], s, rows, declared).stop).toEqual({});
  });

  it('names the missing feature over the protected floor on a burst instance', () => {
    const s = spec({ replicable: true, min_instances: 2 });
    const rows = [row('s', 0), row('s', 1)];
    expect(runtimeRowPlan(rows[1], s, rows, [RUNTIME_INSTANCES_FEATURE]).stop).toEqual({
      disabled: 'feature',
    });
  });

  it('disables a Start of one more instance when the agent lacks runtime_instances', () => {
    const rows = [row('s', 0)];
    expect(runtimeRowPlan(rows[0], replicable, rows, [RUNTIME_STOP_FEATURE]).start).toEqual({
      disabled: 'feature',
      more: true,
    });
    // Instance 0 at rest is what the start picks then: no feature needed.
    const stopped = [row('s', 0, 'stopped')];
    expect(runtimeRowPlan(stopped[0], replicable, stopped, [RUNTIME_STOP_FEATURE]).start).toEqual({
      more: false,
    });
  });

  it('offers Stop on instance 0 once it is the only one left', () => {
    const rows = [row('s', 0), row('s', 1, 'stopped')];
    expect(runtimeRowPlan(rows[0], replicable, rows, declared).stop).toEqual({});
  });

  it('disables Stop on an instance below min_instances', () => {
    const s = spec({ replicable: true, min_instances: 2 });
    const rows = [row('s', 0), row('s', 1)];
    expect(runtimeRowPlan(rows[1], s, rows, declared).stop).toEqual({ disabled: 'protected' });
    const more = [...rows, row('s', 2)];
    expect(runtimeRowPlan(more[2], s, more, declared).stop).toEqual({});
  });

  it('disables Start at max_instances', () => {
    const s = spec({ replicable: true, max_instances: 2 });
    const rows = [row('s', 0), row('s', 1)];
    expect(runtimeRowPlan(rows[0], s, rows, declared).start).toEqual({
      disabled: 'max_instances',
      more: true,
    });
  });

  it('names force_stopped over max_instances', () => {
    const s = spec({ replicable: true, max_instances: 1, admin_state: 'force_stopped' });
    const rows = [row('s', 0, 'starting')];
    expect(runtimeRowPlan(rows[0], s, rows, declared).start).toEqual({
      disabled: 'force_stopped',
      more: true,
    });
  });

  it('puts the spec-wide actions on the primary row, a running burst instance before a stopped 0', () => {
    const rows = [row('s', 0, 'stopped'), row('s', 1)];
    const plan0 = runtimeRowPlan(rows[0], replicable, rows, declared);
    const plan1 = runtimeRowPlan(rows[1], replicable, rows, declared);
    expect(plan0).toEqual({});
    expect(plan1).toEqual({ start: { more: true }, stop: {}, pin: 'force_running' });
  });

  it('offers Start without "one more" while nothing of the spec runs', () => {
    const rows = [row('s', 0, 'stopped')];
    expect(runtimeRowPlan(rows[0], replicable, rows, declared)).toEqual({
      start: { more: false },
      pin: 'force_stopped',
    });
  });
});
