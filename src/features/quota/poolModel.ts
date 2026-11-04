/**
 * Pooled quota capacity: recorded history and burn-rate projection.
 *
 * A provider's pool is the sum of each credential's remaining share of its
 * pooled window (normally the weekly one), so six Claude credentials make a
 * 600% pool. The router spreads load across credentials, so the pool, not any
 * single card, answers "how long does this provider last at the current pace?"
 *
 * Pure functions only: `now` is always passed in.
 */

import { HOUR_MS } from '@/utils/time/durations';
import type { QuotaHistorySample } from '@/services/api/quotaHistory';

/** Longest window a pool is built from. */
export const POOL_MAX_PERIOD_HOURS = 7 * 24;
/** How far ahead pool-empty / first-empty are searched, independent of the view. */
export const POOL_STATS_HORIZON_MS = 14 * 24 * HOUR_MS;
/** Projection step. Resets are hit exactly; this only bounds drain granularity. */
const PROJECTION_STEP_MS = 15 * 60 * 1000;
/** Below this much observed history a credential's burn rate is unknown. */
const MIN_BURN_COVERAGE_MS = 15 * 60 * 1000;
/** A rise of at least this much between two readings is a reset, even without reset_at. */
const RESET_JUMP = 5;
/** Recorded series resolution across the visible span. */
const RECORDED_GRID_POINTS = 360;

export const BURN_WINDOWS_HOURS = [1, 3, 6] as const;
export type BurnWindowHours = (typeof BURN_WINDOWS_HOURS)[number];

export const FUTURE_USAGE_MODES = ['same', 'lighter', 'heavier', 'none'] as const;
export type FutureUsageMode = (typeof FUTURE_USAGE_MODES)[number];

export const FUTURE_USAGE_MULTIPLIER: Record<FutureUsageMode, number> = {
  same: 1,
  lighter: 0.5,
  heavier: 2,
  none: 0,
};

export interface PoolPoint {
  t: number;
  v: number;
}

/** One credential's membership in a pool. */
export interface PoolMember {
  key: string;
  label: string;
  /** Readings, oldest first. Always non-empty for a member. */
  samples: readonly Pick<QuotaHistorySample, 'observedAtMs' | 'remainingPercent' | 'resetAtMs'>[];
  /** Window length used to schedule future resets. */
  periodHours: number | null;
}

/* ------------------------------------------------------- credential series */

/**
 * Piecewise-linear remaining% for one credential, from its first reading to
 * `now`. A reset is drawn as a vertical jump: two points at the same instant.
 *
 * Between readings the line is interpolated, except across a known reset,
 * where the old value is held until the reset and the fresh window then ramps
 * to the next reading. After the last reading the value is held, applying a
 * reset that has since passed.
 */
export function credentialSeries(member: PoolMember, now: number): PoolPoint[] {
  const samples = member.samples.filter((sample) => sample.observedAtMs <= now);
  if (samples.length === 0) return [];
  const points: PoolPoint[] = [];
  let previous = samples[0];
  points.push({ t: previous.observedAtMs, v: previous.remainingPercent });

  for (let index = 1; index < samples.length; index += 1) {
    const sample = samples[index];
    const resetAt = previous.resetAtMs;
    if (resetAt !== null && resetAt > previous.observedAtMs && resetAt <= sample.observedAtMs) {
      points.push({ t: resetAt, v: previous.remainingPercent }, { t: resetAt, v: 100 });
    } else if (sample.remainingPercent >= previous.remainingPercent + RESET_JUMP) {
      points.push({ t: sample.observedAtMs, v: previous.remainingPercent });
    }
    points.push({ t: sample.observedAtMs, v: sample.remainingPercent });
    previous = sample;
  }

  const last = points[points.length - 1];
  const resetAt = previous.resetAtMs;
  if (resetAt !== null && resetAt > last.t && resetAt <= now) {
    points.push({ t: resetAt, v: last.v }, { t: resetAt, v: 100 });
  }
  if (points[points.length - 1].t < now) points.push({ t: now, v: points[points.length - 1].v });
  return points;
}

/** Index of the last point with `t <= at` (or `< at` when `strict`); -1 if none. */
function lastIndexAtOrBefore(points: readonly PoolPoint[], at: number, strict: boolean): number {
  let lo = 0;
  let hi = points.length - 1;
  let found = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (strict ? points[mid].t < at : points[mid].t <= at) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return found;
}

/**
 * Value at `at`. Right-limit by default (after a jump at `at`); `left` gives
 * the value just before it. Before the first point the first value is used so
 * a credential with shorter history doesn't drop out of the pool sum.
 */
export function valueAt(points: readonly PoolPoint[], at: number, left = false): number {
  if (points.length === 0) return 0;
  const index = lastIndexAtOrBefore(points, at, left);
  if (index < 0) return points[0].v;
  if (index === points.length - 1) return points[index].v;
  const a = points[index];
  const b = points[index + 1];
  if (b.t === a.t) return b.v;
  return a.v + ((b.v - a.v) * (at - a.t)) / (b.t - a.t);
}

/** Cumulative consumption (sum of decreases) along a series, for fast window queries. */
function cumulativeUse(points: readonly PoolPoint[]): number[] {
  const cumulative = [0];
  for (let index = 1; index < points.length; index += 1) {
    const drop = points[index].t > points[index - 1].t ? points[index - 1].v - points[index].v : 0;
    cumulative.push(cumulative[index - 1] + Math.max(0, drop));
  }
  return cumulative;
}

function consumedBy(
  points: readonly PoolPoint[],
  cumulative: readonly number[],
  at: number
): number {
  const index = lastIndexAtOrBefore(points, at, false);
  if (index < 0) return 0;
  if (index === points.length - 1) return cumulative[index];
  const a = points[index];
  const b = points[index + 1];
  if (b.t === a.t) return cumulative[index];
  const drop = Math.max(0, a.v - b.v);
  return cumulative[index] + (drop * (at - a.t)) / (b.t - a.t);
}

/**
 * Average consumption in %/h over `[now - windowMs, now]`, or null when the
 * credential has under 15 minutes of history in that window.
 */
export function burnRate(
  points: readonly PoolPoint[],
  now: number,
  windowMs: number
): number | null {
  if (points.length === 0) return null;
  const from = Math.max(now - windowMs, points[0].t);
  const covered = now - from;
  if (covered < MIN_BURN_COVERAGE_MS) return null;
  const cumulative = cumulativeUse(points);
  const used = consumedBy(points, cumulative, now) - consumedBy(points, cumulative, from);
  return (used / covered) * HOUR_MS;
}

/* ------------------------------------------------------------ pool series */

export interface PoolReset {
  t: number;
  /** Percentage points restored across the pool at this instant. */
  amount: number;
  projected: boolean;
}

export interface PoolRecorded {
  points: PoolPoint[];
  /** Consumption rate (%/h) over the trailing burn window at each recorded instant. */
  drain: PoolPoint[];
  resets: PoolReset[];
}

/** Group simultaneous jumps (same minute) into one labelled reset. */
function mergeResets(resets: PoolReset[]): PoolReset[] {
  const byMinute = new Map<number, PoolReset>();
  resets.forEach((reset) => {
    const minute = Math.round(reset.t / 60_000);
    const existing = byMinute.get(minute);
    if (existing) existing.amount += reset.amount;
    else byMinute.set(minute, { ...reset });
  });
  return [...byMinute.values()].sort((a, b) => a.t - b.t);
}

/**
 * Pool remaining over `[fromMs, min(toMs, now)]`, sampled on a fixed grid plus
 * every reset instant so jumps stay vertical. The series starts at the first
 * recorded reading, never earlier: no history is drawn as no line.
 */
export function buildRecordedPool(
  series: readonly PoolPoint[][],
  fromMs: number,
  toMs: number,
  now: number,
  burnWindowMs: number
): PoolRecorded {
  const nonEmpty = series.filter((points) => points.length > 0);
  if (nonEmpty.length === 0) return { points: [], drain: [], resets: [] };
  const historyStart = Math.min(...nonEmpty.map((points) => points[0].t));
  const start = Math.max(fromMs, historyStart);
  const end = Math.min(toMs, now);
  if (!(end >= start)) return { points: [], drain: [], resets: [] };

  const rawResets: PoolReset[] = [];
  const jumpTimes = new Set<number>();
  nonEmpty.forEach((points) => {
    for (let index = 1; index < points.length; index += 1) {
      const a = points[index - 1];
      const b = points[index];
      if (a.t === b.t && b.v > a.v && b.t >= start && b.t <= end) {
        jumpTimes.add(b.t);
        rawResets.push({ t: b.t, amount: b.v - a.v, projected: false });
      }
    }
  });

  const times = new Set<number>(jumpTimes);
  const step = (end - start) / RECORDED_GRID_POINTS;
  for (let index = 0; index <= RECORDED_GRID_POINTS; index += 1) {
    times.add(step > 0 ? start + index * step : start);
  }
  const sorted = [...times].sort((a, b) => a - b);
  const sum = (at: number, left: boolean) =>
    nonEmpty.reduce((total, points) => total + valueAt(points, at, left), 0);

  const cumulatives = nonEmpty.map(cumulativeUse);
  const poolConsumedBy = (at: number) =>
    nonEmpty.reduce(
      (total, points, index) => total + consumedBy(points, cumulatives[index], at),
      0
    );

  const points: PoolPoint[] = [];
  const drain: PoolPoint[] = [];
  sorted.forEach((at) => {
    if (jumpTimes.has(at)) points.push({ t: at, v: sum(at, true) });
    points.push({ t: at, v: sum(at, false) });
    const windowStart = Math.max(at - burnWindowMs, historyStart);
    const covered = at - windowStart;
    // Too little history for a rate yet: leave a gap rather than draw a spike.
    if (covered < MIN_BURN_COVERAGE_MS) return;
    drain.push({
      t: at,
      v: ((poolConsumedBy(at) - poolConsumedBy(windowStart)) / covered) * HOUR_MS,
    });
  });

  return { points, drain, resets: mergeResets(rawResets) };
}

/* ------------------------------------------------------------- projection */

export interface ProjectionMember {
  key: string;
  label: string;
  remaining: number;
  /** %/h this credential burned recently; null when unknown. */
  burn: number | null;
  /** Next reset instant after `now`, or null. */
  nextResetMs: number | null;
  periodHours: number | null;
}

export interface PoolProjection {
  points: PoolPoint[];
  drain: PoolPoint[];
  resets: PoolReset[];
  /** First instant the whole pool is empty, within the horizon. */
  poolEmptyAt: number | null;
  /** First credential to run dry, within the horizon. */
  firstEmpty: { key: string; label: string; at: number } | null;
}

/** First reset instant strictly after `now` from a known anchor and period. */
export function nextResetAfter(
  anchorMs: number | null,
  periodHours: number | null,
  now: number
): number | null {
  if (anchorMs === null || !Number.isFinite(anchorMs)) return null;
  if (anchorMs > now) return anchorMs;
  if (!periodHours || periodHours <= 0) return null;
  const period = periodHours * HOUR_MS;
  return anchorMs + Math.ceil((now - anchorMs + 1) / period) * period;
}

/**
 * Simulate the pool forward from `now` to `untilMs`.
 *
 * Demand is the pool burn rate times `multiplier`. Each step it is shared
 * among credentials that still have quota, in proportion to how much each
 * burned recently (evenly when none did), so load shifts to the survivors as
 * credentials run dry, roughly as the router does. Resets refill a
 * credential to 100% and recur every period.
 */
export function projectPool(
  members: readonly ProjectionMember[],
  poolBurn: number,
  multiplier: number,
  now: number,
  untilMs: number
): PoolProjection {
  const state = members.map((member) => ({
    ...member,
    remaining: Math.min(100, Math.max(0, member.remaining)),
    nextResetMs: member.nextResetMs,
  }));
  const total = () => state.reduce((sum, member) => sum + member.remaining, 0);
  const demandPerHour = Math.max(0, poolBurn * multiplier);

  const points: PoolPoint[] = [{ t: now, v: total() }];
  const drain: PoolPoint[] = [];
  const resets: PoolReset[] = [];
  let poolEmptyAt: number | null = total() <= 0 && members.length > 0 ? now : null;
  let firstEmpty: PoolProjection['firstEmpty'] = null;
  const alreadyEmpty = state.find((member) => member.remaining <= 0);
  if (alreadyEmpty) firstEmpty = { key: alreadyEmpty.key, label: alreadyEmpty.label, at: now };

  let t = now;
  // Bounded: ~1350 steps for 14 days, plus one per reset.
  for (let guard = 0; t < untilMs && guard < 20_000; guard += 1) {
    const nextReset = state.reduce(
      (soonest, member) =>
        member.nextResetMs !== null && member.nextResetMs < soonest ? member.nextResetMs : soonest,
      Infinity
    );
    const tNext = Math.min(t + PROJECTION_STEP_MS, untilMs, nextReset);
    const hours = (tNext - t) / HOUR_MS;

    const weightOf = (member: (typeof state)[number]) => Math.max(0, member.burn ?? 0);
    let demandLeft = demandPerHour * hours;
    let consumed = 0;
    let lastCrossing = t;
    // Demand a credential can't absorb (it ran dry) moves to the survivors
    // within the same step, so emptying one credential doesn't dent the rate.
    let active = state.filter((member) => member.remaining > 0);
    while (demandLeft > 1e-9 && active.length > 0) {
      const weightTotal = active.reduce((sum, member) => sum + weightOf(member), 0);
      const round = demandLeft;
      const survivors: typeof active = [];
      active.forEach((member) => {
        const share =
          weightTotal > 0 ? (round * weightOf(member)) / weightTotal : round / active.length;
        if (share <= 0) {
          survivors.push(member);
          return;
        }
        if (share >= member.remaining) {
          const crossing = t + (member.remaining / share) * (tNext - t);
          lastCrossing = Math.max(lastCrossing, crossing);
          if (!firstEmpty || crossing < firstEmpty.at) {
            firstEmpty = { key: member.key, label: member.label, at: crossing };
          }
          consumed += member.remaining;
          demandLeft -= member.remaining;
          member.remaining = 0;
        } else {
          consumed += share;
          demandLeft -= share;
          member.remaining -= share;
          survivors.push(member);
        }
      });
      // Each round either absorbs the demand or empties someone; once no
      // survivor has a weight, the next round splits evenly.
      active = survivors;
    }
    if (hours > 0) drain.push({ t, v: consumed / hours }, { t: tNext, v: consumed / hours });

    t = tNext;
    const before = total();
    if (poolEmptyAt === null && before <= 1e-9 && members.length > 0) poolEmptyAt = lastCrossing;
    points.push({ t, v: before });

    let restored = 0;
    state.forEach((member) => {
      if (member.nextResetMs === null || member.nextResetMs > t) return;
      restored += 100 - member.remaining;
      member.remaining = 100;
      member.nextResetMs =
        member.periodHours && member.periodHours > 0
          ? member.nextResetMs + member.periodHours * HOUR_MS
          : null;
    });
    if (restored > 0) {
      points.push({ t, v: total() });
      resets.push({ t, amount: restored, projected: true });
    }
  }

  return { points, drain, resets: mergeResets(resets), poolEmptyAt, firstEmpty };
}

/* ---------------------------------------------------------------- summary */

export interface PoolSummary {
  capacity: number;
  remaining: number;
  /** Pool burn over the selected window, %/h; null when no credential has enough history. */
  burn: number | null;
  recorded: PoolRecorded;
  projection: PoolProjection;
  nextReset: PoolReset | null;
}

export interface BuildPoolOptions {
  now: number;
  spanStartMs: number;
  spanEndMs: number;
  burnWindowHours: BurnWindowHours;
  futureUsage: FutureUsageMode;
}

/** Everything one pool chart needs. */
export function buildPool(members: readonly PoolMember[], options: BuildPoolOptions): PoolSummary {
  const { now, spanStartMs, spanEndMs, burnWindowHours, futureUsage } = options;
  const windowMs = burnWindowHours * HOUR_MS;
  const series = members.map((member) => credentialSeries(member, now));

  const projectionMembers: ProjectionMember[] = members.map((member, index) => {
    const points = series[index];
    const lastSample = member.samples[member.samples.length - 1];
    return {
      key: member.key,
      label: member.label,
      remaining: valueAt(points, now),
      burn: burnRate(points, now, windowMs),
      nextResetMs: nextResetAfter(lastSample?.resetAtMs ?? null, member.periodHours, now),
      periodHours: member.periodHours,
    };
  });

  const burns = projectionMembers.map((member) => member.burn);
  const burn = burns.some((value) => value !== null)
    ? burns.reduce<number>((sum, value) => sum + (value ?? 0), 0)
    : null;

  const projection = projectPool(
    projectionMembers,
    burn ?? 0,
    FUTURE_USAGE_MULTIPLIER[futureUsage],
    now,
    Math.max(spanEndMs, now + POOL_STATS_HORIZON_MS)
  );

  return {
    capacity: members.length * 100,
    remaining: projectionMembers.reduce((sum, member) => sum + member.remaining, 0),
    burn,
    recorded: buildRecordedPool(series, spanStartMs - windowMs, spanEndMs, now, windowMs),
    projection,
    nextReset: projection.resets[0] ?? null,
  };
}

/* ------------------------------------------------------------- rendering */

/**
 * Clip a series to `[fromMs, toMs]`, interpolating boundary points so the
 * line meets the plot edges instead of stopping short.
 */
export function clipSeries(
  points: readonly PoolPoint[],
  fromMs: number,
  toMs: number
): PoolPoint[] {
  if (points.length === 0 || toMs <= fromMs) return [];
  const out: PoolPoint[] = [];
  const first = points[0].t;
  const last = points[points.length - 1].t;
  if (last < fromMs || first > toMs) return [];
  if (first < fromMs) out.push({ t: fromMs, v: valueAt(points, fromMs) });
  points.forEach((point) => {
    if (point.t >= fromMs && point.t <= toMs) out.push(point);
  });
  if (last > toMs) out.push({ t: toMs, v: valueAt(points, toMs, true) });
  return out;
}

/** Visible span: three days of history, eleven ahead, stepped a week at a time. */
export const POOL_SPAN_DAYS = 14;
const POOL_HISTORY_DAYS = 3;

export function poolSpan(now: number, offsetWeeks: number): { startMs: number; endMs: number } {
  const start = new Date(now);
  start.setHours(0, 0, 0, 0);
  start.setDate(start.getDate() - POOL_HISTORY_DAYS + offsetWeeks * 7);
  const end = new Date(start);
  end.setDate(end.getDate() + POOL_SPAN_DAYS);
  return { startMs: start.getTime(), endMs: end.getTime() };
}
