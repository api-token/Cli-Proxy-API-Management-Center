import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { IconChevronLeft } from '@/components/ui/icons';
import { Select } from '@/components/ui/Select';
import {
  BURN_WINDOWS_HOURS,
  FUTURE_USAGE_MODES,
  type BurnWindowHours,
  type FutureUsageMode,
} from '../poolModel';
import type { PoolViewMode } from './QuotaPoolChart';
import styles from './QuotaPoolToolbar.module.scss';

export interface QuotaPoolToolbarProps {
  viewMode: PoolViewMode;
  onViewModeChange: (mode: PoolViewMode) => void;
  burnWindowHours: BurnWindowHours;
  onBurnWindowChange: (hours: BurnWindowHours) => void;
  futureUsage: FutureUsageMode;
  onFutureUsageChange: (mode: FutureUsageMode) => void;
  accountsOpen: boolean;
  onToggleAccounts: () => void;
  span: { startMs: number; endMs: number };
  offset: number;
  onOffsetChange: (offset: number) => void;
}

const VIEW_MODES: PoolViewMode[] = ['remaining', 'drain'];

export function QuotaPoolToolbar({
  viewMode,
  onViewModeChange,
  burnWindowHours,
  onBurnWindowChange,
  futureUsage,
  onFutureUsageChange,
  accountsOpen,
  onToggleAccounts,
  span,
  offset,
  onOffsetChange,
}: QuotaPoolToolbarProps) {
  const { t, i18n } = useTranslation();
  const dateFormat = useMemo(
    () => new Intl.DateTimeFormat(i18n.resolvedLanguage, { month: 'short', day: 'numeric' }),
    [i18n.resolvedLanguage]
  );
  const futureOptions = useMemo(
    () =>
      FUTURE_USAGE_MODES.map((mode) => ({
        value: mode,
        label: t(`quota_management.pool_future_${mode}`),
      })),
    [t]
  );
  // The span end is exclusive; show the last day it covers.
  const rangeLabel = t('quota_management.pool_range', {
    start: dateFormat.format(span.startMs),
    end: dateFormat.format(span.endMs - 1),
  });

  return (
    <div className={styles.toolbar}>
      <div className={styles.controls}>
        <div
          className={styles.segmented}
          role="group"
          aria-label={t('quota_management.pool_view_label')}
        >
          {VIEW_MODES.map((mode) => (
            <button
              key={mode}
              type="button"
              aria-pressed={viewMode === mode}
              onClick={() => onViewModeChange(mode)}
            >
              {t(`quota_management.pool_view_${mode}`)}
            </button>
          ))}
        </div>

        <div className={styles.field}>
          <span className={styles.fieldLabel} id="quota-pool-burn-label">
            {t('quota_management.pool_burn_rate')}
          </span>
          <div className={styles.segmented} role="group" aria-labelledby="quota-pool-burn-label">
            {BURN_WINDOWS_HOURS.map((hours) => (
              <button
                key={hours}
                type="button"
                aria-pressed={burnWindowHours === hours}
                onClick={() => onBurnWindowChange(hours)}
              >
                {t('quota_management.pool_hours', { count: hours })}
              </button>
            ))}
          </div>
        </div>

        <div className={styles.field}>
          <span className={styles.fieldLabel} id="quota-pool-future-label">
            {t('quota_management.pool_future_usage')}
          </span>
          <Select
            className={styles.select}
            value={futureUsage}
            options={futureOptions}
            onChange={(value) => onFutureUsageChange(value as FutureUsageMode)}
            ariaLabelledBy="quota-pool-future-label"
            size="sm"
          />
        </div>

        <button
          type="button"
          className={styles.toggle}
          aria-expanded={accountsOpen}
          aria-controls="quota-accounts"
          onClick={onToggleAccounts}
        >
          {t('quota_management.pool_accounts_toggle')}
        </button>
      </div>

      <div className={styles.range}>
        <span className={styles.rangeLabel}>{rangeLabel}</span>
        <button
          type="button"
          className={styles.iconButton}
          onClick={() => onOffsetChange(offset - 1)}
          aria-label={t('quota_management.windows_prev')}
        >
          <IconChevronLeft size={14} />
        </button>
        <button
          type="button"
          className={styles.iconButton}
          onClick={() => onOffsetChange(offset + 1)}
          aria-label={t('quota_management.windows_next')}
        >
          <IconChevronLeft size={14} className={styles.flip} />
        </button>
        <button
          type="button"
          className={styles.today}
          onClick={() => onOffsetChange(0)}
          disabled={offset === 0}
        >
          {t('quota_management.windows_today')}
        </button>
      </div>
    </div>
  );
}
