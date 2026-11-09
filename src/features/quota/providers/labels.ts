/**
 * Plan and window names shared by the provider bodies and the accounts table.
 * Pure: `t` is passed in, and nothing here imports React or the store.
 */

import type { TFunction } from 'i18next';
import type { AntigravityQuotaSubscription, MetaQuotaWindow } from '@/types';
import { PREMIUM_CODEX_PLAN_TYPES, normalizePlanType } from '@/utils/quota';

/** Display name of a Codex plan; unknown plan types fall back to the raw value. */
export const getCodexPlanLabel = (pt: string | null | undefined, t: TFunction): string | null => {
  const normalized = normalizePlanType(pt);
  if (!normalized) return null;
  if (normalized === 'self_serve_business_prolite') {
    return t('codex_quota.plan_business_premium');
  }
  if (normalized === 'pro') return t('codex_quota.plan_pro');
  if (PREMIUM_CODEX_PLAN_TYPES.has(normalized) && normalized !== 'pro') {
    return t('codex_quota.plan_prolite');
  }
  if (normalized === 'plus') return t('codex_quota.plan_plus');
  if (normalized === 'team') return t('codex_quota.plan_team');
  if (normalized === 'free') return t('codex_quota.plan_free');
  return pt || normalized;
};

/** Display name of an Antigravity subscription, or null when it reports none. */
export const getAntigravityPlanLabel = (
  subscription: AntigravityQuotaSubscription | null | undefined,
  t: TFunction
): string | null => {
  if (!subscription) return null;
  if (subscription.plan === 'free') return t('antigravity_subscription.plan_free');
  if (subscription.plan === 'pro') return t('antigravity_subscription.plan_pro');
  if (subscription.plan === 'ultra') return t('antigravity_subscription.plan_ultra');
  if (subscription.plan === 'ultra-lite') return t('antigravity_subscription.plan_ultra_lite');
  return (
    subscription.tierName ||
    subscription.tierId ||
    (subscription.plan === 'unknown' ? t('antigravity_subscription.plan_unknown') : null)
  );
};

const XAI_SUPERGROK_LIMIT_CENTS = 15_000;
const XAI_SUPERGROK_HEAVY_LIMIT_CENTS = 150_000;

/** The xAI plan implied by its monthly credit limit, when no plan name is reported. */
export const resolveXaiPlan = (
  monthlyLimitCents: number | null
): { labelKey: string; premium: boolean } | null => {
  if (monthlyLimitCents === XAI_SUPERGROK_LIMIT_CENTS) {
    return { labelKey: 'plan_supergrok', premium: false };
  }
  if (monthlyLimitCents === XAI_SUPERGROK_HEAVY_LIMIT_CENTS) {
    return { labelKey: 'plan_supergrok_heavy', premium: true };
  }
  return null;
};

/** Name of a Meta window: its length when stated, else its scope. */
export const getMetaWindowLabel = (window: MetaQuotaWindow, t: TFunction): string =>
  window.id === 'window' && window.durationMinutes
    ? window.durationMinutes % 60 === 0
      ? t('meta_quota.window_duration_hours', { hours: window.durationMinutes / 60 })
      : t('meta_quota.window_duration', { minutes: window.durationMinutes })
    : t(`meta_quota.${window.id}`);
