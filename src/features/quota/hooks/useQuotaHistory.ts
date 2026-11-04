/**
 * Quota history for the pool charts.
 *
 * Fetches recorded readings from the backend and reports every freshly loaded
 * quota reading back to it, so history accumulates across sessions and
 * devices. Fresh readings are also kept locally, so the chart reflects a load
 * immediately and still works against a backend without history (404).
 *
 * Session isolation: a connection switch bumps the quota cache generation;
 * all history is dropped and late responses for the old session are ignored.
 */

import { useEffect, useMemo, useRef, useState } from 'react';
import { quotaHistoryApi, type QuotaHistorySample } from '@/services/api/quotaHistory';
import { useQuotaStore } from '@/stores';
import { getStatusFromError } from '@/utils/quota';
import type { QuotaFileEntry } from '../logic';
import { groupSamples, readingFor, type PoolReading } from '../poolMembers';
import type { QuotaCardState } from '../providers';

export interface UseQuotaHistoryOptions {
  enabled: boolean;
  sinceMs: number;
  entries: readonly QuotaFileEntry[];
  getQuota: (entry: QuotaFileEntry) => QuotaCardState | undefined;
  /** Bump to refetch (e.g. after "refresh all"). */
  refreshToken: number;
}

export interface QuotaHistoryState {
  samplesBySeries: Map<string, QuotaHistorySample[]>;
  /** Latest live reading per quota cache key. */
  liveByKey: Map<string, PoolReading>;
  /** The backend predates quota history; only this visit's readings are charted. */
  unsupported: boolean;
}

const toSample = ({ key: _key, ...reading }: PoolReading): QuotaHistorySample => ({
  ...reading,
  source: 'management',
});

export function useQuotaHistory({
  enabled,
  sinceMs,
  entries,
  getQuota,
  refreshToken,
}: UseQuotaHistoryOptions): QuotaHistoryState {
  const session = useQuotaStore((state) => state.cacheGeneration);
  const [remote, setRemote] = useState<QuotaHistorySample[]>([]);
  const [local, setLocal] = useState<PoolReading[]>([]);
  const [unsupported, setUnsupported] = useState(false);
  const unsupportedRef = useRef(false);
  const recordedStates = useRef(new WeakSet<object>());
  const requestRef = useRef(0);

  const markUnsupported = (err: unknown) => {
    if (getStatusFromError(err) !== 404) return;
    unsupportedRef.current = true;
    setUnsupported(true);
  };

  useEffect(() => {
    setRemote([]);
    setLocal([]);
    setUnsupported(false);
    unsupportedRef.current = false;
    recordedStates.current = new WeakSet();
  }, [session]);

  useEffect(() => {
    if (!enabled) return;
    const requestId = ++requestRef.current;
    const isCurrent = () =>
      requestId === requestRef.current && session === useQuotaStore.getState().cacheGeneration;
    quotaHistoryApi
      .list(sinceMs)
      .then((samples) => {
        if (!isCurrent()) return;
        setRemote(samples);
      })
      .catch((err: unknown) => {
        if (isCurrent()) markUnsupported(err);
      });
    return () => {
      requestRef.current += 1;
    };
  }, [enabled, refreshToken, session, sinceMs]);

  useEffect(() => {
    if (!enabled) return;
    const observedAtMs = Date.now();
    const fresh: PoolReading[] = [];
    entries.forEach((entry) => {
      const quota = getQuota(entry);
      if (!quota || quota.status !== 'success' || recordedStates.current.has(quota)) return;
      recordedStates.current.add(quota);
      const reading = readingFor(entry, quota, observedAtMs);
      if (reading) fresh.push(reading);
    });
    if (fresh.length === 0) return;
    setLocal((prev) => [...prev, ...fresh]);
    if (unsupportedRef.current) return;
    const generation = session;
    void quotaHistoryApi.record(fresh.map(toSample)).catch((err: unknown) => {
      if (generation === useQuotaStore.getState().cacheGeneration) markUnsupported(err);
    });
  }, [enabled, entries, getQuota, session]);

  const samplesBySeries = useMemo(
    () => groupSamples([...remote, ...local.map(toSample)]),
    [remote, local]
  );
  const liveByKey = useMemo(() => {
    const byKey = new Map<string, PoolReading>();
    local.forEach((reading) => byKey.set(reading.key, reading));
    return byKey;
  }, [local]);

  return { samplesBySeries, liveByKey, unsupported };
}
