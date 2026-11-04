// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  INSTANCES_MAX,
  instanceAddress,
  instanceCountFromText,
  instanceCountText,
  instanceIndex,
  instanceSettingsBody,
  listedIdleTimeout,
  primaryStatusRows,
  runningInstanceCount,
  showsAllInstanceLogs,
  showsRunningInstances,
  runningInstancesBySpec,
  specStopState,
  validateInstances,
  type InstanceForm,
} from './runtimeInstances';
import type { RuntimeStatus } from '../../api';
import { messages } from '../../i18n';

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

describe('listedIdleTimeout', () => {
  const never = 'never unloaded';
  const stored = {
    configured: true,
    replicable: false,
    idle_timeout_seconds: 600,
    effective_idle_timeout_seconds: 600,
  };

  it('lists the idle timeout in effect for a spec stored as replicable, 0 as never', () => {
    expect(
      listedIdleTimeout(
        {
          ...stored,
          replicable: true,
          idle_timeout_seconds: 0,
          effective_idle_timeout_seconds: 150,
        },
        never,
      ),
    ).toBe('150');
    expect(
      listedIdleTimeout(
        { ...stored, replicable: true, idle_timeout_seconds: 0, effective_idle_timeout_seconds: 0 },
        never,
      ),
    ).toBe(never);
  });

  it('lists the stored value for any other spec, as before instances', () => {
    expect(listedIdleTimeout(stored, never)).toBe('600');
    expect(listedIdleTimeout({ ...stored, idle_timeout_seconds: 0 }, never)).toBe('—');
    expect(listedIdleTimeout(undefined, never)).toBe('—');
    // Not configured: nothing stored to be replicable.
    expect(listedIdleTimeout({ ...stored, configured: false, replicable: true }, never)).toBe(
      '600',
    );
  });
});

describe('showsAllInstanceLogs', () => {
  it('is true for a spec stored as replicable, or one the stream lists an instance k >= 1 of', () => {
    expect(showsAllInstanceLogs({ replicable: true }, [], 's')).toBe(true);
    expect(showsAllInstanceLogs({ replicable: false }, [row('s', 0), row('s', 1)], 's')).toBe(true);
    expect(showsAllInstanceLogs(undefined, [row('s', 2)], 's')).toBe(true);
  });

  it('is false for a single-process spec', () => {
    expect(showsAllInstanceLogs({ replicable: false }, [row('s', 0), row('t', 1)], 's')).toBe(
      false,
    );
    expect(showsAllInstanceLogs(undefined, [row('s', 0)], 's')).toBe(false);
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

function form(overrides: Partial<InstanceForm> = {}): InstanceForm {
  return { replicable: true, minText: '', maxText: '', parallelismText: '1', ...overrides };
}

describe('instanceSettingsBody', () => {
  it('sends the neutral 0/0/0 while "Multiple instances" is unticked, whatever the fields hold', () => {
    expect(
      instanceSettingsBody(
        form({ replicable: false, minText: '3', maxText: '2', parallelismText: '4' }),
      ),
    ).toEqual({ replicable: false, min_instances: 0, max_instances: 0, instance_parallelism: 0 });
  });

  it('sends the typed settings while ticked: empty counts as 0, an empty parallelism as 1', () => {
    expect(instanceSettingsBody(form())).toEqual({
      replicable: true,
      min_instances: 0,
      max_instances: 0,
      instance_parallelism: 1,
    });
    expect(
      instanceSettingsBody(form({ minText: '1', maxText: '3', parallelismText: '2' })),
    ).toEqual({ replicable: true, min_instances: 1, max_instances: 3, instance_parallelism: 2 });
    expect(instanceSettingsBody(form({ parallelismText: '' })).instance_parallelism).toBe(1);
  });
});

describe('validateInstances', () => {
  const t = messages.en;
  const invalid = t.errorRuntimeSpecInstancesInvalid;

  it('never checks the hidden fields of a spec that does not opt in', () => {
    for (const hidden of [
      { minText: '-1' },
      { maxText: '65' },
      { minText: '3', maxText: '2' },
      { minText: '1.5' },
      { parallelismText: '0' },
      { parallelismText: 'abc' },
    ]) {
      expect(validateInstances(form({ replicable: false, ...hidden }), 0, t)).toBeUndefined();
    }
    // A fixed listen port is fine for a spec that runs one process.
    expect(validateInstances(form({ replicable: false }), 9000, t)).toBeUndefined();
  });

  it.each([
    ['a negative minimum', { minText: '-1' }],
    ['a negative maximum', { maxText: '-1' }],
    ['a fractional minimum', { minText: '0.5' }],
    ['a fractional maximum', { maxText: '2.5' }],
    ['a minimum that is no number', { minText: 'abc' }],
    ['a minimum above 64', { minText: '65' }],
    ['a maximum above 64', { maxText: '65' }],
    ['a minimum above a set maximum', { minText: '3', maxText: '2' }],
    ['a parallelism of 0', { parallelismText: '0' }],
    ['a negative parallelism', { parallelismText: '-2' }],
    ['a fractional parallelism', { parallelismText: '1.5' }],
  ])('refuses %s while ticked', (_, fields) => {
    expect(validateInstances(form(fields), 0, t)).toBe(invalid);
  });

  it.each([
    ['empty bounds and the default parallelism', {}],
    ['an empty parallelism, read as its default 1', { parallelismText: '' }],
    ['a minimum with an unlimited maximum', { minText: '5' }],
    ['a maximum with no minimum', { maxText: '5' }],
    ['equal bounds at the cap', { minText: '64', maxText: '64' }],
    ['a minimum below the maximum', { minText: '1', maxText: '3', parallelismText: '8' }],
  ])('accepts %s while ticked', (_, fields) => {
    expect(validateInstances(form(fields), 0, t)).toBeUndefined();
  });

  it('refuses several instances on a fixed listen port, after the counts', () => {
    expect(validateInstances(form(), 9000, t)).toBe(t.errorRuntimeSpecInstancesNeedEphemeralPort);
    expect(validateInstances(form({ minText: '-1' }), 9000, t)).toBe(invalid);
  });
});

// The models catalog's per-server count (ModelServerRow).
describe('running instances on a model-server row', () => {
  it('reads a missing or unusable count as 0', () => {
    expect(runningInstanceCount({ running_instances: 3 })).toBe(3);
    expect(runningInstanceCount({})).toBe(0);
    expect(runningInstanceCount({ running_instances: -1 })).toBe(0);
    expect(runningInstanceCount({ running_instances: Number.NaN })).toBe(0);
  });

  it('is shown for a replicable spec whatever runs, otherwise once more than one runs', () => {
    expect(showsRunningInstances({ replicable: true, running_instances: 1 })).toBe(true);
    expect(showsRunningInstances({ replicable: true, running_instances: 0 })).toBe(true);
    expect(showsRunningInstances({ replicable: true })).toBe(true);
    expect(showsRunningInstances({ replicable: false, running_instances: 1 })).toBe(false);
    expect(showsRunningInstances({ running_instances: 1 })).toBe(false);
    expect(showsRunningInstances({})).toBe(false);
    // A spec turned off while its burst instances still drain.
    expect(showsRunningInstances({ replicable: false, running_instances: 2 })).toBe(true);
    expect(showsRunningInstances({ running_instances: 2 })).toBe(true);
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
