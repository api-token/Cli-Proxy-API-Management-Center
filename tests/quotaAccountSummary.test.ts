/**
 * What an accounts-table row shows for a credential: its plan, and which limit
 * lands in the short, long, and "other" columns.
 */

import { describe, expect, test } from 'bun:test';
import type { TFunction } from 'i18next';
import i18n from '@/i18n';
import {
  accountSortValue,
  hasAccountReading,
  periodParts,
  splitAccountLimits,
  summarizeAccount,
  tightestLimit,
  type AccountLimit,
} from '@/features/quota/accountSummary';
import {
  XAI_WEEKLY_ROW_ID,
  collectQuotaRowInstants,
  pickUrgentRowId,
} from '@/features/quota/resetSchedule';
import { DAY_MS, HOUR_MS, MINUTE_MS } from '@/utils/time/durations';

const NOW = new Date(2026, 10, 9, 12).getTime();
// A fixed-language t, so the shared i18n instance other suites read is left alone.
const t: TFunction = i18n.getFixedT('en');

const columnsOf = (provider: Parameters<typeof summarizeAccount>[0], quota: unknown) => {
  const summary = summarizeAccount(provider, quota, t);
  const columns = splitAccountLimits(summary.limits);
  return { summary, ...columns, otherIds: columns.others.map((limit) => limit.id) };
};

describe('summarizeAccount', () => {
  test('Claude: account windows fill the columns, model windows are other limits', () => {
    const quota = {
      status: 'success',
      planType: 'plan_max20',
      windows: [
        {
          id: 'five-hour',
          label: '5h',
          labelKey: 'claude_quota.five_hour',
          usedPercent: 18,
          resetLabel: '11-09 15:00',
          resetAtMs: NOW + 3 * HOUR_MS,
          periodHours: 5,
        },
        {
          id: 'seven-day',
          label: '7d',
          labelKey: 'claude_quota.seven_day',
          usedPercent: 59,
          resetLabel: '11-13 12:00',
          resetAtMs: NOW + 4 * DAY_MS,
          periodHours: 168,
        },
        {
          id: 'seven-day-opus',
          label: 'opus',
          labelKey: 'claude_quota.seven_day_opus',
          usedPercent: 91,
          resetLabel: '11-13 12:00',
          resetAtMs: NOW + 4 * DAY_MS,
          periodHours: 168,
        },
      ],
    };
    const { summary, short, long, otherIds } = columnsOf('claude', quota);

    expect(summary.plan).toEqual({ label: 'Max 20x', tier: 'plain' });
    expect(short).toMatchObject({ id: 'five-hour', remaining: 82, label: '5-hour limit' });
    expect(long).toMatchObject({ id: 'seven-day', remaining: 41, resetLabel: '11-13 12:00' });
    expect(otherIds).toEqual(['seven-day-opus']);
  });

  test('Claude: retained usage still summarizes while a refresh is loading', () => {
    const quota = {
      status: 'loading',
      observedAt: new Date(NOW).toISOString(),
      windows: [{ id: 'five-hour', label: '5h', usedPercent: 40, resetLabel: '', periodHours: 5 }],
    };
    expect(columnsOf('claude', quota).short).toMatchObject({ remaining: 60, resetLabel: null });
  });

  test('Codex: a code-review window never stands in for the account window', () => {
    const quota = {
      status: 'success',
      planType: 'pro',
      windows: [
        // A plan with no account-level 5-hour window.
        { id: 'weekly', label: 'Weekly', usedPercent: 30, resetLabel: '-', periodHours: 168 },
        {
          id: 'code-review-five-hour',
          label: 'Code review 5h',
          usedPercent: 5,
          resetLabel: '-',
          periodHours: 5,
        },
      ],
    };
    const { summary, short, long, otherIds } = columnsOf('codex', quota);

    expect(summary.plan).toEqual({ label: t('codex_quota.plan_pro'), tier: 'elite' });
    expect(short).toBeNull();
    expect(long).toMatchObject({ id: 'weekly', remaining: 70 });
    expect(otherIds).toEqual(['code-review-five-hour']);
  });

  test('Codex: legacy payloads without a duration are placed by window id', () => {
    const quota = {
      status: 'success',
      windows: [
        { id: 'five-hour', label: '5h', usedPercent: 10, resetLabel: '-' },
        { id: 'monthly', label: 'Monthly', usedPercent: 20, resetLabel: '-' },
      ],
    };
    const { short, long } = columnsOf('codex', quota);

    expect(short).toMatchObject({ id: 'five-hour', periodHours: 5 });
    expect(long).toMatchObject({ id: 'monthly', periodHours: 720 });
  });

  test('Antigravity: the first group fills the columns; labels name the group', () => {
    const bucket = (id: string, label: string, remainingFraction: number, periodHours: number) => ({
      id,
      label,
      remainingFraction,
      periodHours,
      resetAtMs: NOW + periodHours * HOUR_MS,
    });
    const quota = {
      status: 'success',
      subscription: { plan: 'ultra', tierName: null, tierId: null },
      groups: [
        {
          id: 'gemini',
          label: 'Gemini Models',
          buckets: [
            bucket('g-5h', 'Five Hour Limit Remaining', 1, 5),
            bucket('g-week', 'Weekly Limit', 0.5, 168),
          ],
        },
        {
          id: 'claude',
          label: 'Claude and GPT Models',
          buckets: [
            bucket('c-5h', '5 hour limit', 0.9, 5),
            bucket('c-week', 'Weekly limit', 0.2, 168),
          ],
        },
      ],
    };
    const { summary, short, long, others } = columnsOf('antigravity', quota);

    expect(summary.plan?.tier).toBe('premium');
    expect(short).toMatchObject({
      id: 'g-5h',
      remaining: 100,
      label: 'Gemini models · 5-hour limit',
    });
    expect(long).toMatchObject({ id: 'g-week', remaining: 50 });
    expect(tightestLimit(others)).toMatchObject({
      id: 'c-week',
      remaining: 20,
      label: 'Claude and GPT models · Weekly limit',
    });
  });

  test('Kimi: remaining is derived from raw counts', () => {
    const quota = {
      status: 'success',
      rows: [
        { id: 'row-1', label: 'Weekly limit', used: 25, limit: 100, periodHours: 168 },
        { id: 'row-2', label: 'Exhausted', used: 3, limit: 0 },
        { id: 'row-3', label: 'Unknown', used: 0, limit: 0 },
      ],
    };
    const { summary, long, otherIds } = columnsOf('kimi', quota);

    expect(summary.plan).toBeNull();
    expect(long).toMatchObject({ id: 'row-1', remaining: 75 });
    expect(summary.limits.map((limit) => limit.remaining)).toEqual([75, 0, null]);
    expect(otherIds).toEqual(['row-2', 'row-3']);
  });

  test('Meta: the weekly bucket is seven days; reset seconds become ms', () => {
    const quota = {
      status: 'success',
      data: {
        planName: 'Muse Pro',
        windows: [
          { id: 'window', usedPercent: 12.5, resetAt: NOW / 1000 + 3600, durationMinutes: 300 },
          { id: 'weekly', usedPercent: null },
        ],
      },
    };
    const { summary, short, long } = columnsOf('meta', quota);

    expect(summary.plan).toEqual({ label: 'Muse Pro', tier: 'plain' });
    expect(short).toMatchObject({
      remaining: 87.5,
      resetAtMs: NOW + HOUR_MS,
      periodHours: 5,
      label: '5-hour window',
    });
    expect(long).toMatchObject({ id: 'weekly', remaining: null, periodHours: 168 });
  });

  test('xAI: weekly limit is the long window; pay-as-you-go is another limit', () => {
    const billing = {
      mode: 'billing',
      periodType: 'weekly',
      usagePercent: 64,
      periodEnd: new Date(NOW + 2 * DAY_MS).toISOString(),
      productUsage: [],
      monthlyLimitCents: 0,
      usedCents: 0,
      includedUsedCents: 0,
      onDemandCapCents: 5000,
      onDemandUsedCents: 1000,
      onDemandUsedPercent: 20,
      usedPercent: null,
      planLabel: 'SuperGrok Heavy',
      planTier: 'elite',
      resetAtMs: NOW + 2 * DAY_MS,
      periodHours: 168,
    };
    const { summary, short, long, otherIds } = columnsOf('xai', { status: 'success', billing });

    expect(summary.plan).toEqual({ label: 'SuperGrok Heavy', tier: 'elite' });
    expect(short).toBeNull();
    expect(long).toMatchObject({ id: XAI_WEEKLY_ROW_ID, remaining: 36 });
    // The all-zero monthly figure beside a weekly limit is not a second limit.
    expect(otherIds).toEqual(['xai:on-demand']);
  });

  test('xAI: a paid API credential has a plan but no limits', () => {
    const billing = { mode: 'paid-health', periodType: 'unknown', productUsage: [] };
    expect(summarizeAccount('xai', { status: 'success', billing }, t)).toEqual({
      plan: { label: t('xai_quota.plan_paid'), tier: 'premium' },
      limits: [],
    });
  });

  test('limit ids match the reset schedule, so the urgent row maps to a cell', () => {
    const quota = {
      status: 'success',
      windows: [
        { id: 'daily', remainingPercent: 10, resetAtMs: NOW + 20 * MINUTE_MS, periodHours: 24 },
        { id: 'weekly', remainingPercent: 80, resetAtMs: NOW + 3 * DAY_MS, periodHours: 168 },
      ],
    };
    const urgent = pickUrgentRowId(collectQuotaRowInstants('devin', quota), NOW);
    expect(columnsOf('devin', quota).short?.id).toBe(urgent as string);
  });

  test('a missing or empty state summarizes to nothing', () => {
    expect(summarizeAccount('claude', undefined, t)).toEqual({ plan: null, limits: [] });
    expect(summarizeAccount('plugin', { status: 'error' }, t)).toEqual({ plan: null, limits: [] });
    expect(summarizeAccount('meta', { status: 'error' }, t)).toEqual({ plan: null, limits: [] });
  });
});

describe('accountSortValue', () => {
  const window = (id: string, usedPercent: number, periodHours: number) => ({
    id,
    label: id,
    usedPercent,
    resetLabel: '-',
    periodHours,
  });
  const quota = {
    status: 'success',
    planType: 'plus',
    windows: [
      window('five-hour', 10, 5),
      window('weekly', 70, 168),
      window('code-review-weekly', 20, 168),
      window('spark-weekly-0', 95, 168),
    ],
  };

  test('each column sorts by what its cell shows', () => {
    expect(accountSortValue('plan', 'codex', quota, t)).toBe(t('codex_quota.plan_plus'));
    expect(accountSortValue('short', 'codex', quota, t)).toBe(90);
    expect(accountSortValue('long', 'codex', quota, t)).toBe(30);
    // The tightest of the other limits, as the cell shows.
    expect(accountSortValue('other', 'codex', quota, t)).toBe(5);
  });

  test('a credential with no reading, or nothing in the column, has no value', () => {
    expect(accountSortValue('short', 'codex', undefined, t)).toBeNull();
    expect(accountSortValue('short', 'codex', { ...quota, status: 'error' }, t)).toBeNull();
    expect(accountSortValue('plan', 'kimi', { status: 'success', rows: [] }, t)).toBeNull();
    expect(accountSortValue('other', 'kimi', { status: 'success', rows: [] }, t)).toBeNull();
  });

  test('retained Claude usage still counts as a reading after a failed refresh', () => {
    const retained = {
      status: 'error',
      observedAt: new Date(NOW).toISOString(),
      windows: [window('five-hour', 25, 5)],
    };
    expect(hasAccountReading('claude', retained)).toBe(true);
    expect(accountSortValue('short', 'claude', retained, t)).toBe(75);
    expect(hasAccountReading('claude', { status: 'error', windows: [] })).toBe(false);
  });
});

describe('column helpers', () => {
  const limit = (
    id: string,
    remaining: number | null,
    periodHours: number | null
  ): AccountLimit => ({
    id,
    label: id,
    remaining,
    resetAtMs: null,
    resetLabel: null,
    periodHours,
    scoped: false,
  });

  test('a second window of a length already taken becomes an other limit', () => {
    const limits = [
      limit('a', 50, 5),
      limit('b', 40, 24),
      limit('c', 30, 168),
      limit('d', 20, null),
    ];
    const columns = splitAccountLimits(limits);
    expect(columns.short?.id).toBe('a');
    expect(columns.long?.id).toBe('c');
    expect(columns.others.map((item) => item.id)).toEqual(['b', 'd']);
  });

  test('tightestLimit ranks unknown usage last and keeps the first of a tie', () => {
    expect(tightestLimit([])).toBeNull();
    expect(tightestLimit([limit('a', null, 5), limit('b', 30, 5), limit('c', 30, 5)])?.id).toBe(
      'b'
    );
    expect(tightestLimit([limit('a', null, 5), limit('b', null, 5)])?.id).toBe('a');
  });

  test('periodParts shows hours below a day and whole days from there', () => {
    expect(periodParts(5)).toEqual({ unit: 'hours', count: 5 });
    expect(periodParts(1.5)).toEqual({ unit: 'hours', count: 1.5 });
    expect(periodParts(24)).toEqual({ unit: 'days', count: 1 });
    expect(periodParts(168)).toEqual({ unit: 'days', count: 7 });
    expect(periodParts(720)).toEqual({ unit: 'days', count: 30 });
  });
});
