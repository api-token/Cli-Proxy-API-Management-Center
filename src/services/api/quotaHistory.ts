/**
 * Quota history: the backend keeps a 14-day series of per-credential readings
 * of each credential's pooled (weekly) quota window. The quota page reports
 * what it reads from provider usage endpoints, and the backend adds readings
 * it derives from proxied traffic on its own.
 */

import { apiClient } from './client';

const HISTORY_PATH = '/credentials/quota/history';
/** Mirrors the backend's per-request limit. */
export const QUOTA_HISTORY_MAX_BATCH = 500;

export interface QuotaHistorySample {
  name: string;
  authIndex: string | null;
  provider: string;
  observedAtMs: number;
  windowId: string | null;
  periodHours: number | null;
  /** 0..100, percent of the window remaining. */
  remainingPercent: number;
  resetAtMs: number | null;
  source: 'management' | 'observed' | string;
}

export type QuotaHistorySampleInput = Omit<QuotaHistorySample, 'source'>;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

const parseInstant = (value: unknown): number | null => {
  if (typeof value !== 'string' || !value) return null;
  const ms = Date.parse(value);
  return Number.isFinite(ms) ? ms : null;
};

const finiteOrNull = (value: unknown): number | null =>
  typeof value === 'number' && Number.isFinite(value) ? value : null;

/** Normalize one backend sample; null when it is unusable. */
export function normalizeQuotaHistorySample(raw: unknown): QuotaHistorySample | null {
  if (!isRecord(raw)) return null;
  const name = typeof raw.name === 'string' ? raw.name : '';
  const provider = typeof raw.provider === 'string' ? raw.provider.toLowerCase() : '';
  const observedAtMs = parseInstant(raw.observed_at);
  const remainingPercent = finiteOrNull(raw.remaining_percent);
  if (!name || !provider || observedAtMs === null || remainingPercent === null) return null;
  const authIndex =
    typeof raw.auth_index === 'string' && raw.auth_index.trim() ? raw.auth_index.trim() : null;
  const periodHours = finiteOrNull(raw.period_hours);
  return {
    name,
    authIndex,
    provider,
    observedAtMs,
    windowId: typeof raw.window_id === 'string' && raw.window_id ? raw.window_id : null,
    periodHours: periodHours !== null && periodHours > 0 ? periodHours : null,
    remainingPercent: Math.min(100, Math.max(0, remainingPercent)),
    resetAtMs: parseInstant(raw.reset_at),
    source: typeof raw.source === 'string' ? raw.source : 'management',
  };
}

const serializeSample = (sample: QuotaHistorySampleInput) => ({
  name: sample.name,
  ...(sample.authIndex ? { auth_index: sample.authIndex } : {}),
  provider: sample.provider,
  observed_at: new Date(sample.observedAtMs).toISOString(),
  ...(sample.windowId ? { window_id: sample.windowId } : {}),
  ...(sample.periodHours ? { period_hours: sample.periodHours } : {}),
  remaining_percent: sample.remainingPercent,
  ...(sample.resetAtMs !== null ? { reset_at: new Date(sample.resetAtMs).toISOString() } : {}),
});

export const quotaHistoryApi = {
  async list(sinceMs?: number): Promise<QuotaHistorySample[]> {
    const params = sinceMs !== undefined ? { since: new Date(sinceMs).toISOString() } : undefined;
    const data = await apiClient.get<unknown>(HISTORY_PATH, { params });
    const samples = isRecord(data) && Array.isArray(data.samples) ? data.samples : [];
    return samples
      .map(normalizeQuotaHistorySample)
      .filter((sample): sample is QuotaHistorySample => sample !== null);
  },

  async record(samples: QuotaHistorySampleInput[]): Promise<void> {
    for (let index = 0; index < samples.length; index += QUOTA_HISTORY_MAX_BATCH) {
      const batch = samples.slice(index, index + QUOTA_HISTORY_MAX_BATCH).map(serializeSample);
      await apiClient.post(HISTORY_PATH, { samples: batch });
    }
  },
};
