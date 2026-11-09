/**
 * One credential reduced to what a table row can show: its plan and a flat
 * list of percent limits.
 *
 * The provider bodies remain the complete rendering (credits, balances,
 * groups); this is the at-a-glance view beside them. Limit ids match the row
 * ids in resetSchedule.ts, so `pickUrgentRowId` can point at a table cell.
 *
 * Read structurally per provider, like resetSchedule.ts and
 * quotaTimelineModel.ts: the state shapes disagree about where a window lives
 * and whether its percentage means used or remaining.
 */

import type { TFunction } from 'i18next';
import type {
  AntigravityQuotaState,
  ClaudeQuotaState,
  CodexQuotaState,
  DevinQuotaState,
  KimiQuotaState,
  MetaQuotaState,
  XaiBillingSummary,
  XaiQuotaState,
} from '@/types';
import { parseIsoToMs, resolvePlanTier } from '@/utils/quota';
import {
  resolveAntigravityBucketLabelKey,
  resolveAntigravityGroupLabelKey,
} from './providers/antigravity/labels';
import {
  getAntigravityPlanLabel,
  getCodexPlanLabel,
  getMetaWindowLabel,
  resolveXaiPlan,
} from './providers/labels';
import type { QuotaProviderType } from './providers/types';
import { XAI_WEEKLY_ROW_ID } from './resetSchedule';

export interface AccountLimit {
  id: string;
  label: string;
  /** Remaining percent, 0..100; null when the provider reports no usage. */
  remaining: number | null;
  resetAtMs: number | null;
  /** Absolute reset text baked at fetch time, for providers that have one. */
  resetLabel: string | null;
  periodHours: number | null;
  /** Limited to a model or feature rather than the whole account. */
  scoped: boolean;
}

export interface AccountPlan {
  label: string;
  tier: 'elite' | 'premium' | 'plain';
}

export interface AccountSummary {
  plan: AccountPlan | null;
  limits: AccountLimit[];
}

const WEEK_HOURS = 24 * 7;
const MONTH_HOURS = 24 * 30;
/** Windows up to a day are "short"; anything longer is "long". */
const SHORT_WINDOW_MAX_HOURS = 24;

const clampPercent = (value: number) => Math.min(100, Math.max(0, value));

const remainingFromUsed = (used: number | null | undefined): number | null =>
  typeof used === 'number' && Number.isFinite(used) ? clampPercent(100 - used) : null;

const plainPlan = (label: string | null | undefined): AccountPlan | null =>
  label ? { label, tier: 'plain' } : null;

/** Claude and Codex list the account-wide windows under these ids. */
const ACCOUNT_WINDOW_IDS = new Set(['five-hour', 'seven-day', 'weekly', 'monthly']);

/** Period implied by a Codex window id, for legacy payloads that state no duration. */
const codexPeriodFromId = (id: string): number | null =>
  id.endsWith('five-hour')
    ? 5
    : id.endsWith('weekly')
      ? WEEK_HOURS
      : id.endsWith('monthly')
        ? MONTH_HOURS
        : null;

function xaiPlan(billing: XaiBillingSummary, t: TFunction): AccountPlan | null {
  if (billing.planLabel) {
    const tier = billing.planTier;
    return {
      label: billing.planLabel,
      tier: tier === 'elite' || tier === 'premium' ? tier : 'plain',
    };
  }
  if (billing.mode === 'paid-health') return { label: t('xai_quota.plan_paid'), tier: 'premium' };
  const plan = resolveXaiPlan(billing.monthlyLimitCents);
  return plan
    ? { label: t(`xai_quota.${plan.labelKey}`), tier: plan.premium ? 'premium' : 'plain' }
    : null;
}

function xaiLimits(billing: XaiBillingSummary, t: TFunction): AccountLimit[] {
  // A paid API credential reports chat health only, no quota totals.
  if (billing.mode === 'paid-health') return [];

  const limits: AccountLimit[] = [];
  const hasWeekly =
    billing.periodType === 'weekly' &&
    (billing.usagePercent !== null ||
      Boolean(billing.periodEnd) ||
      billing.productUsage.length > 0);
  if (hasWeekly) {
    limits.push({
      id: XAI_WEEKLY_ROW_ID,
      label: t('xai_quota.weekly_limit'),
      remaining: remainingFromUsed(billing.usagePercent),
      resetAtMs: billing.resetAtMs ?? null,
      resetLabel: null,
      periodHours: billing.periodHours ?? WEEK_HOURS,
      scoped: false,
    });
  }
  // Same visibility rule as XaiQuotaBody: an all-zero monthly figure beside a
  // weekly limit is a placeholder, not a second limit.
  const hasMonthly =
    (billing.monthlyLimitCents !== null ||
      billing.usedCents !== null ||
      Boolean(billing.billingPeriodEnd)) &&
    !(hasWeekly && billing.monthlyLimitCents === 0 && billing.usedCents === 0);
  if (hasMonthly) {
    limits.push({
      id: 'xai:monthly',
      label: t('xai_quota.monthly_credits'),
      remaining: remainingFromUsed(billing.usedPercent),
      resetAtMs: parseIsoToMs(billing.billingPeriodEnd),
      resetLabel: null,
      periodHours: MONTH_HOURS,
      scoped: false,
    });
  }
  if ((billing.onDemandCapCents ?? 0) > 0) {
    limits.push({
      id: 'xai:on-demand',
      label: t('xai_quota.pay_as_you_go_label'),
      remaining: remainingFromUsed(billing.onDemandUsedPercent),
      resetAtMs: null,
      resetLabel: null,
      periodHours: null,
      scoped: true,
    });
  }
  return limits;
}

/**
 * Plan and limits for one credential. Reads whatever the state holds whatever
 * its status, so retained Claude usage still summarizes while a refresh is
 * loading or has failed; the caller decides when a summary is worth showing.
 */
export function summarizeAccount(
  provider: QuotaProviderType,
  quota: unknown,
  t: TFunction
): AccountSummary {
  if (!quota) return { plan: null, limits: [] };

  if (provider === 'claude') {
    const state = quota as ClaudeQuotaState;
    return {
      plan: plainPlan(state.planType ? t(`claude_quota.${state.planType}`) : null),
      limits: (state.windows ?? []).map((window) => ({
        id: window.id,
        label: window.labelKey ? t(window.labelKey) : window.label,
        remaining: remainingFromUsed(window.usedPercent),
        resetAtMs: window.resetAtMs ?? null,
        resetLabel: window.resetLabel || null,
        periodHours: window.periodHours ?? null,
        scoped: !ACCOUNT_WINDOW_IDS.has(window.id),
      })),
    };
  }

  if (provider === 'codex') {
    const state = quota as CodexQuotaState;
    const label = getCodexPlanLabel(state.planType, t);
    return {
      plan: label ? { label, tier: resolvePlanTier(state.planType) } : null,
      limits: (state.windows ?? []).map((window) => ({
        id: window.id,
        label: window.labelKey
          ? t(window.labelKey, (window.labelParams ?? {}) as Record<string, string | number>)
          : window.label,
        remaining: remainingFromUsed(window.usedPercent),
        resetAtMs: window.resetAtMs ?? null,
        resetLabel: window.resetLabel || null,
        periodHours: window.periodHours ?? codexPeriodFromId(window.id),
        scoped: !ACCOUNT_WINDOW_IDS.has(window.id),
      })),
    };
  }

  if (provider === 'devin') {
    const state = quota as DevinQuotaState;
    return {
      plan: plainPlan(state.plan),
      limits: (state.windows ?? []).map((window) => ({
        id: window.id,
        label: t(`devin_quota.${window.id}`),
        remaining: window.remainingPercent,
        resetAtMs: window.resetAtMs,
        resetLabel: null,
        periodHours: window.periodHours,
        scoped: false,
      })),
    };
  }

  if (provider === 'kimi') {
    const rows = (quota as KimiQuotaState).rows ?? [];
    return {
      plan: null,
      limits: rows.map((row) => ({
        id: row.id,
        label: row.labelKey
          ? t(row.labelKey, (row.labelParams ?? {}) as Record<string, string | number>)
          : (row.label ?? ''),
        // Kimi reports raw counts; remaining is derived.
        remaining:
          row.limit > 0
            ? clampPercent(((row.limit - row.used) / row.limit) * 100)
            : row.used > 0
              ? 0
              : null,
        resetAtMs: row.resetAtMs ?? null,
        resetLabel: null,
        periodHours: row.periodHours ?? null,
        scoped: false,
      })),
    };
  }

  if (provider === 'meta') {
    const data = (quota as MetaQuotaState).data;
    return {
      plan: plainPlan(data?.planName),
      limits: (data?.windows ?? []).map((window) => ({
        id: window.id,
        label: getMetaWindowLabel(window, t),
        remaining: remainingFromUsed(window.usedPercent),
        resetAtMs: typeof window.resetAt === 'number' ? window.resetAt * 1000 : null,
        resetLabel: null,
        // The weekly bucket has no duration field; its scope defines seven days.
        periodHours:
          window.id === 'weekly'
            ? WEEK_HOURS
            : window.durationMinutes
              ? window.durationMinutes / 60
              : null,
        scoped: false,
      })),
    };
  }

  if (provider === 'xai') {
    const billing = (quota as XaiQuotaState).billing;
    if (!billing) return { plan: null, limits: [] };
    return { plan: xaiPlan(billing, t), limits: xaiLimits(billing, t) };
  }

  // Antigravity and plugin providers share the grouped-bucket shape. Only
  // Antigravity's upstream labels have translations.
  const state = quota as AntigravityQuotaState;
  const groups = state.groups ?? [];
  const translate = (value: string, key: string | null) =>
    provider === 'antigravity' && key ? t(`antigravity_quota.${key}`) : value;
  const subscription = state.subscription;
  const plan: AccountPlan | null =
    provider === 'antigravity'
      ? (() => {
          const label = getAntigravityPlanLabel(subscription, t);
          const premium = subscription?.plan === 'ultra' || subscription?.plan === 'ultra-lite';
          return label ? { label, tier: premium ? 'premium' : 'plain' } : null;
        })()
      : plainPlan(subscription?.tierName || subscription?.plan || subscription?.tierId);

  return {
    plan,
    limits: groups.flatMap((group) => {
      const groupLabel = translate(group.label, resolveAntigravityGroupLabelKey(group.label));
      return (group.buckets ?? []).map((bucket) => {
        const bucketLabel = translate(
          bucket.label,
          resolveAntigravityBucketLabelKey(bucket.label, bucket.window)
        );
        return {
          id: bucket.id,
          // Several groups repeat the same bucket names, so name the group too.
          label: groups.length > 1 ? `${groupLabel} · ${bucketLabel}` : bucketLabel,
          remaining: clampPercent(bucket.remainingFraction * 100),
          resetAtMs: bucket.resetAtMs ?? parseIsoToMs(bucket.resetTime),
          resetLabel: null,
          periodHours: bucket.periodHours ?? null,
          scoped: false,
        };
      });
    }),
  };
}

export interface AccountLimitColumns {
  short: AccountLimit | null;
  long: AccountLimit | null;
  others: AccountLimit[];
}

/**
 * Sort a credential's limits into the table's columns.
 *
 * The short and long columns each take the first account-wide window of that
 * length, in the provider's own order — providers list the main window first.
 * Model- and feature-scoped limits, windows of unknown length, and any further
 * windows of a length already taken all land in `others`.
 */
export function splitAccountLimits(limits: readonly AccountLimit[]): AccountLimitColumns {
  const primary = limits.filter((limit) => !limit.scoped && limit.periodHours !== null);
  const short =
    primary.find((limit) => (limit.periodHours as number) <= SHORT_WINDOW_MAX_HOURS) ?? null;
  const long =
    primary.find((limit) => (limit.periodHours as number) > SHORT_WINDOW_MAX_HOURS) ?? null;
  return { short, long, others: limits.filter((limit) => limit !== short && limit !== long) };
}

/** The limit with the least remaining. Unknown usage ranks last; the first wins a tie. */
export function tightestLimit(limits: readonly AccountLimit[]): AccountLimit | null {
  let best: AccountLimit | null = null;
  for (const limit of limits) {
    if (best === null || (limit.remaining ?? Infinity) < (best.remaining ?? Infinity)) best = limit;
  }
  return best;
}

/** A window length as a compact tag: hours below a day, whole days from there. */
export function periodParts(periodHours: number): { unit: 'hours' | 'days'; count: number } {
  return periodHours < 24
    ? { unit: 'hours', count: Number(periodHours.toFixed(1)) }
    : { unit: 'days', count: Math.round(periodHours / 24) };
}
