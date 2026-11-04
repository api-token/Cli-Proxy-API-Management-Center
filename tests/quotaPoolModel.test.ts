import { describe, expect, test } from 'bun:test';
import {
  buildPool,
  buildRecordedPool,
  burnRate,
  clipSeries,
  credentialSeries,
  nextResetAfter,
  poolSpan,
  projectPool,
  valueAt,
  type PoolMember,
} from '@/features/quota/poolModel';
import { normalizeQuotaHistorySample } from '@/services/api/quotaHistory';
import { buildPoolMembers, groupSamples } from '@/features/quota/poolMembers';

const HOUR = 60 * 60 * 1000;
const NOW = Date.UTC(2026, 10, 4, 12);

const sample = (hoursAgo: number, remaining: number, resetAtMs: number | null = null) => ({
  observedAtMs: NOW - hoursAgo * HOUR,
  remainingPercent: remaining,
  resetAtMs,
});

const member = (key: string, samples: PoolMember['samples'], periodHours = 168): PoolMember => ({
  key,
  label: key,
  samples,
  periodHours,
});

describe('credentialSeries', () => {
  test('interpolates readings and holds the last value until now', () => {
    const points = credentialSeries(member('a', [sample(4, 80), sample(2, 60)]), NOW);
    expect(points).toEqual([
      { t: NOW - 4 * HOUR, v: 80 },
      { t: NOW - 2 * HOUR, v: 60 },
      { t: NOW, v: 60 },
    ]);
    expect(valueAt(points, NOW - 3 * HOUR)).toBe(70);
  });

  test('draws a known reset as a vertical jump at reset_at', () => {
    const resetAt = NOW - 3 * HOUR;
    const points = credentialSeries(member('a', [sample(4, 10, resetAt), sample(2, 90)]), NOW);
    expect(points.slice(0, 3)).toEqual([
      { t: NOW - 4 * HOUR, v: 10 },
      { t: resetAt, v: 10 },
      { t: resetAt, v: 100 },
    ]);
    expect(valueAt(points, resetAt, true)).toBe(10);
    expect(valueAt(points, resetAt)).toBe(100);
  });

  test('treats a large rise without reset_at as a reset step', () => {
    const points = credentialSeries(member('a', [sample(4, 20), sample(2, 95)]), NOW);
    expect(points[1]).toEqual({ t: NOW - 2 * HOUR, v: 20 });
    expect(points[2]).toEqual({ t: NOW - 2 * HOUR, v: 95 });
  });

  test('applies a reset that passed after the last reading', () => {
    const points = credentialSeries(member('a', [sample(4, 5, NOW - HOUR)]), NOW);
    expect(valueAt(points, NOW)).toBe(100);
  });
});

describe('burnRate', () => {
  test('averages consumption across the window and ignores resets', () => {
    const resetAt = NOW - 2 * HOUR;
    const points = credentialSeries(
      member('a', [sample(3, 20, resetAt), sample(2.5, 20, resetAt), sample(0, 94)]),
      NOW
    );
    // 0 burned before the reset; 100 → 94 after it: 6 points over 3h.
    expect(burnRate(points, NOW, 3 * HOUR)).toBeCloseTo(2, 5);
  });

  test('is unknown with under 15 minutes of history', () => {
    const points = credentialSeries(member('a', [sample(0.1, 50)]), NOW);
    expect(burnRate(points, NOW, 3 * HOUR)).toBeNull();
  });
});

describe('nextResetAfter', () => {
  test('rolls a past anchor forward by whole periods', () => {
    expect(nextResetAfter(NOW + HOUR, 168, NOW)).toBe(NOW + HOUR);
    expect(nextResetAfter(NOW - HOUR, 5, NOW)).toBe(NOW + 4 * HOUR);
    expect(nextResetAfter(NOW - HOUR, null, NOW)).toBeNull();
    expect(nextResetAfter(null, 168, NOW)).toBeNull();
  });
});

describe('projectPool', () => {
  test('drains at the pool rate, moves load to survivors, and finds empties', () => {
    const projection = projectPool(
      [
        { key: 'a', label: 'A', remaining: 10, burn: 5, nextResetMs: null, periodHours: null },
        { key: 'b', label: 'B', remaining: 50, burn: 5, nextResetMs: null, periodHours: null },
      ],
      10,
      1,
      NOW,
      NOW + 48 * HOUR
    );
    // A runs dry after 2h at 5%/h; then B takes all 10%/h: 40 left → 4h more.
    expect(projection.firstEmpty?.key).toBe('a');
    expect(projection.firstEmpty!.at).toBeCloseTo(NOW + 2 * HOUR, -3);
    expect(projection.poolEmptyAt).not.toBeNull();
    expect(Math.abs(projection.poolEmptyAt! - (NOW + 6 * HOUR))).toBeLessThan(16 * 60 * 1000);
    expect(projection.points[projection.points.length - 1].v).toBe(0);
  });

  test('refills at each reset and repeats it every period', () => {
    const projection = projectPool(
      [{ key: 'a', label: 'A', remaining: 40, burn: 1, nextResetMs: NOW + HOUR, periodHours: 24 }],
      1,
      1,
      NOW,
      NOW + 50 * HOUR
    );
    expect(projection.resets.map((reset) => reset.t)).toEqual([
      NOW + HOUR,
      NOW + 25 * HOUR,
      NOW + 49 * HOUR,
    ]);
    // 40 → 39 after an hour at 1%/h, so the first reset restores 61; later
    // resets restore the 24 points burned in each full day.
    expect(projection.resets[0].amount).toBeCloseTo(61, 5);
    expect(projection.resets[1].amount).toBeCloseTo(24, 5);
    expect(projection.poolEmptyAt).toBeNull();
  });

  test('a zero multiplier holds the pool flat', () => {
    const projection = projectPool(
      [{ key: 'a', label: 'A', remaining: 40, burn: 9, nextResetMs: null, periodHours: null }],
      9,
      0,
      NOW,
      NOW + 10 * HOUR
    );
    expect(projection.points.every((point) => point.v === 40)).toBe(true);
    expect(projection.firstEmpty).toBeNull();
  });
});

describe('buildRecordedPool', () => {
  test('starts at the first reading and keeps reset jumps vertical', () => {
    const resetAt = NOW - 2 * HOUR;
    const series = [
      credentialSeries(member('a', [sample(4, 50, resetAt), sample(1, 95)]), NOW),
      credentialSeries(member('b', [sample(3, 70)]), NOW),
    ];
    const recorded = buildRecordedPool(series, NOW - 24 * HOUR, NOW + 24 * HOUR, NOW, 3 * HOUR);
    expect(recorded.points[0].t).toBe(NOW - 4 * HOUR);
    expect(recorded.points[recorded.points.length - 1].t).toBe(NOW);
    const atReset = recorded.points.filter((point) => point.t === resetAt);
    expect(atReset.map((point) => point.v)).toEqual([120, 170]);
    expect(recorded.resets).toEqual([{ t: resetAt, amount: 50, projected: false }]);
  });
});

describe('buildPool', () => {
  test('summarizes capacity, remaining, burn and next reset', () => {
    const summary = buildPool(
      [
        member('a', [sample(3, 90, NOW + 5 * HOUR), sample(0, 84, NOW + 5 * HOUR)]),
        member('b', [sample(0, 40, NOW + 30 * HOUR)]),
      ],
      {
        now: NOW,
        spanStartMs: NOW - 72 * HOUR,
        spanEndMs: NOW + 264 * HOUR,
        burnWindowHours: 3,
        futureUsage: 'same',
      }
    );
    expect(summary.capacity).toBe(200);
    expect(summary.remaining).toBe(124);
    expect(summary.burn).toBeCloseTo(2, 5);
    expect(summary.nextReset?.t).toBe(NOW + 5 * HOUR);
  });

  test('burn is unknown when no credential has history', () => {
    const summary = buildPool([member('a', [sample(0, 50)])], {
      now: NOW,
      spanStartMs: NOW - 72 * HOUR,
      spanEndMs: NOW + 264 * HOUR,
      burnWindowHours: 1,
      futureUsage: 'same',
    });
    expect(summary.burn).toBeNull();
    expect(summary.projection.firstEmpty).toBeNull();
  });
});

describe('span and clipping', () => {
  test('span shows three days back and eleven ahead, stepping by weeks', () => {
    const now = new Date(2026, 10, 4, 15).getTime();
    const span = poolSpan(now, 0);
    expect(new Date(span.startMs).getDate()).toBe(1);
    expect(new Date(span.endMs).getDate()).toBe(15);
    expect(new Date(poolSpan(now, -1).startMs).getDate()).toBe(25);
  });

  test('clipSeries interpolates the boundaries', () => {
    const points = [
      { t: 0, v: 100 },
      { t: 10, v: 0 },
    ];
    expect(clipSeries(points, 5, 8)).toEqual([
      { t: 5, v: 50 },
      { t: 8, v: 20 },
    ]);
    expect(clipSeries(points, 20, 30)).toEqual([]);
  });
});

describe('normalizeQuotaHistorySample', () => {
  test('maps snake_case fields and clamps remaining', () => {
    expect(
      normalizeQuotaHistorySample({
        name: 'a.json',
        auth_index: ' 7 ',
        provider: 'Claude',
        observed_at: '2026-11-04T12:00:00Z',
        period_hours: 168,
        remaining_percent: 120,
        reset_at: '2026-11-05T12:00:00Z',
        source: 'observed',
      })
    ).toEqual({
      name: 'a.json',
      authIndex: '7',
      provider: 'claude',
      observedAtMs: Date.UTC(2026, 10, 4, 12),
      windowId: null,
      periodHours: 168,
      remainingPercent: 100,
      resetAtMs: Date.UTC(2026, 10, 5, 12),
      source: 'observed',
    });
    expect(normalizeQuotaHistorySample({ name: 'a', provider: 'x' })).toBeNull();
  });
});

describe('projectPool redistribution', () => {
  test('demand a drained credential cannot absorb moves to the others in the same step', () => {
    const projection = projectPool(
      [
        { key: 'a', label: 'A', remaining: 0.5, burn: 10, nextResetMs: null, periodHours: null },
        { key: 'b', label: 'B', remaining: 90, burn: 0, nextResetMs: null, periodHours: null },
      ],
      10,
      1,
      NOW,
      NOW + 2 * HOUR
    );
    // The pool keeps draining at the full 10%/h even after A runs dry.
    projection.drain.forEach((point) => expect(point.v).toBeCloseTo(10, 5));
    expect(projection.points[projection.points.length - 1].v).toBeCloseTo(90.5 - 20, 5);
  });
});

describe('buildPoolMembers history matching', () => {
  const sampleFor = (name: string, authIndex: string | null, provider = 'claude') => ({
    name,
    authIndex,
    provider,
    observedAtMs: NOW - HOUR,
    windowId: null,
    periodHours: 168,
    remainingPercent: 50,
    resetAtMs: null,
    source: 'observed',
  });
  const entry = (name: string, type: 'claude' | 'devin', authIndex?: string) => ({
    file: { name, type, authIndex },
    type,
  });

  test('matches plugin readings by auth_index even when the name differs', () => {
    const samples = groupSamples([
      sampleFor('claude:oauth:internal-id', 'idx-1'),
      sampleFor('claude-a.json', null),
    ]);
    const members = buildPoolMembers(
      [entry('claude-a.json', 'claude', 'idx-1')],
      'claude',
      samples,
      new Map(),
      (item) => item.file.name
    );
    expect(members).toHaveLength(1);
    expect(members[0].samples).toHaveLength(2);
  });

  test('never falls back to the shared file name for multi-identity providers', () => {
    const samples = groupSamples([sampleFor('devin.json', null, 'devin')]);
    const members = buildPoolMembers(
      [entry('devin.json', 'devin', 'idx-2')],
      'devin',
      samples,
      new Map(),
      (item) => item.file.name
    );
    expect(members).toHaveLength(0);
  });
});
