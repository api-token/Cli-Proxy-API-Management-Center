/**
 * Glue between the quota page's entries/cache and the pool model: what a
 * loaded credential contributes to its pool, and which history belongs to it.
 */

import type { QuotaHistorySample, QuotaHistorySampleInput } from '@/services/api/quotaHistory';
import { getQuotaCacheKey } from '@/utils/quota/identity';
import { normalizeRecentRequestAuthIndex } from '@/utils/recentRequests';
import type { QuotaFileEntry } from './logic';
import { POOL_MAX_PERIOD_HOURS, type PoolMember } from './poolModel';
import type { QuotaCardState } from './providers';
import type { QuotaProviderType } from './providers/types';
import { buildTimelineLane } from './quotaTimelineModel';

/** Providers whose credentials share a file name and differ by auth_index. */
const INDEXED_PROVIDERS = new Set<string>(['devin', 'plugin']);

export function historySeriesKey(
  provider: string,
  name: string,
  authIndex: string | null | undefined
): string {
  return INDEXED_PROVIDERS.has(provider)
    ? `${provider}\0${name}\0${authIndex ?? ''}`
    : `${provider}\0${name}`;
}

/** A loaded credential's current reading of its pooled window. */
export interface PoolReading extends QuotaHistorySampleInput {
  key: string;
  provider: QuotaProviderType;
}

export function readingFor(
  entry: QuotaFileEntry,
  quota: QuotaCardState | undefined,
  observedAtMs: number
): PoolReading | null {
  const lane = buildTimelineLane({
    name: getQuotaCacheKey(entry.file),
    displayName: entry.file.name,
    provider: entry.type,
    quota,
    maxPeriodHours: POOL_MAX_PERIOD_HOURS,
  });
  if (lane.remaining === null) return null;
  return {
    key: getQuotaCacheKey(entry.file),
    name: entry.file.name,
    authIndex: normalizeRecentRequestAuthIndex(entry.file.authIndex),
    provider: entry.type,
    observedAtMs,
    windowId: null,
    periodHours: lane.periodHours,
    remainingPercent: lane.remaining,
    resetAtMs: lane.anchorMs,
  };
}

/** Same window if both lengths are known and within 10% of each other. */
const samePeriod = (a: number | null, b: number | null) =>
  a === null || b === null || Math.abs(a - b) <= Math.max(a, b) * 0.1;

/**
 * Pool members for one provider: every listed credential that has at least
 * one reading, live or recorded. History for credentials no longer listed is
 * ignored, and readings of a different window length (a 5-hour sample next to
 * a weekly pool) are dropped rather than mixed in.
 */
export function buildPoolMembers(
  entries: readonly QuotaFileEntry[],
  provider: QuotaProviderType,
  samplesBySeries: ReadonlyMap<string, readonly QuotaHistorySample[]>,
  liveByKey: ReadonlyMap<string, PoolReading>,
  labelFor: (entry: QuotaFileEntry) => string
): PoolMember[] {
  const members: PoolMember[] = [];
  entries.forEach((entry) => {
    if (entry.type !== provider) return;
    const key = getQuotaCacheKey(entry.file);
    const live = liveByKey.get(key) ?? null;
    const series =
      samplesBySeries.get(
        historySeriesKey(
          provider,
          entry.file.name,
          normalizeRecentRequestAuthIndex(entry.file.authIndex)
        )
      ) ?? [];
    const periodHours =
      live?.periodHours ?? [...series].reverse().find((s) => s.periodHours)?.periodHours ?? null;
    const samples = series.filter((s) => samePeriod(s.periodHours, periodHours));
    if (samples.length === 0) return;
    members.push({ key, label: labelFor(entry), samples, periodHours });
  });
  return members;
}

/** Index samples by series, oldest first, dropping exact duplicates. */
export function groupSamples(
  samples: readonly QuotaHistorySample[]
): Map<string, QuotaHistorySample[]> {
  const bySeries = new Map<string, QuotaHistorySample[]>();
  const seen = new Set<string>();
  samples.forEach((sample) => {
    const series = historySeriesKey(sample.provider, sample.name, sample.authIndex);
    const id = `${series}\0${sample.observedAtMs}`;
    if (seen.has(id)) return;
    seen.add(id);
    const list = bySeries.get(series) ?? [];
    list.push(sample);
    bySeries.set(series, list);
  });
  bySeries.forEach((list) => list.sort((a, b) => a.observedAtMs - b.observedAtMs));
  return bySeries;
}
