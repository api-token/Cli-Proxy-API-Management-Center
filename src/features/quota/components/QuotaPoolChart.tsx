/**
 * One provider's pooled capacity: recorded remaining (solid) up to now, the
 * burn-rate projection (dashed) after it, with reset jumps labelled.
 *
 * The SVG is stretched to the plot box (preserveAspectRatio="none") with
 * non-scaling strokes; every text label is HTML positioned in percent, so
 * nothing is distorted at any width.
 */

import { useMemo } from 'react';
import type { CSSProperties } from 'react';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import {
  getAuthFileIcon,
  getThemeSurfaceIconBackground,
  getTypeLabel,
  isThemeSurfaceIconProvider,
} from '@/features/authFiles/constants';
import type { ResolvedTheme, ThemeColors } from '@/types';
import { TYPE_COLORS } from '@/utils/quota';
import { DAY_MS } from '@/utils/time/durations';
import {
  clipSeries,
  valueAt,
  POOL_STATS_HORIZON_MS,
  type PoolPoint,
  type PoolSummary,
} from '../poolModel';
import type { QuotaProviderType } from '../providers/types';
import styles from './QuotaPoolChart.module.scss';

export type PoolViewMode = 'remaining' | 'drain';

export interface QuotaPoolChartProps {
  provider: QuotaProviderType;
  accountCount: number;
  summary: PoolSummary;
  viewMode: PoolViewMode;
  span: { startMs: number; endMs: number };
  now: number;
  burnWindowHours: number;
  resolvedTheme: ResolvedTheme;
}

const VIEW_WIDTH = 1000;
const VIEW_HEIGHT = 240;
/** Reset labels closer than this (in % of width) to the previous one are skipped. */
const LABEL_MIN_GAP_PERCENT = 3.2;

const formatPercent = (value: number) => `${Math.round(value)}%`;

function formatPoolDuration(ms: number, t: TFunction): string {
  const minutes = Math.max(0, Math.round(ms / 60_000));
  const d = Math.floor(minutes / (24 * 60));
  const h = Math.floor((minutes % (24 * 60)) / 60);
  const m = minutes % 60;
  if (d > 0) return t('quota_management.pool_duration_dh', { d, h });
  if (h > 0) return t('quota_management.pool_duration_hm', { h, m });
  return t('quota_management.pool_duration_m', { m });
}

export function QuotaPoolChart({
  provider,
  accountCount,
  summary,
  viewMode,
  span,
  now,
  burnWindowHours,
  resolvedTheme,
}: QuotaPoolChartProps) {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const formats = useMemo(
    () => ({
      day: new Intl.DateTimeFormat(locale, { weekday: 'short', day: 'numeric' }),
      instant: new Intl.DateTimeFormat(locale, {
        weekday: 'short',
        hour: 'numeric',
        minute: '2-digit',
      }),
      date: new Intl.DateTimeFormat(locale, { month: 'short', day: 'numeric' }),
    }),
    [locale]
  );

  const colorSet = TYPE_COLORS[provider] || TYPE_COLORS.unknown;
  const color: ThemeColors =
    resolvedTheme === 'dark' && colorSet.dark ? colorSet.dark : colorSet.light;
  const providerLabel = getTypeLabel(t, provider);
  const iconSrc = getAuthFileIcon(provider, resolvedTheme);

  const { capacity, remaining, burn, recorded, projection, nextReset } = summary;
  const isDrain = viewMode === 'drain';

  const geometry = useMemo(() => {
    const recordedPoints = clipSeries(
      isDrain ? recorded.drain : recorded.points,
      span.startMs,
      span.endMs
    );
    const projectedPoints = clipSeries(
      isDrain ? projection.drain : projection.points,
      span.startMs,
      span.endMs
    );
    const peak = Math.max(
      0,
      ...recordedPoints.map((point) => point.v),
      ...projectedPoints.map((point) => point.v)
    );
    const yMax = isDrain ? Math.max(1, peak * 1.2) : Math.max(100, capacity);
    const width = span.endMs - span.startMs;
    const x = (ms: number) => ((ms - span.startMs) / width) * VIEW_WIDTH;
    // Keep a little headroom so a full pool's line isn't drawn on the border.
    const y = (v: number) => VIEW_HEIGHT - (v / yMax) * VIEW_HEIGHT * 0.94;
    const line = (points: PoolPoint[]) =>
      points
        .map(
          (point, index) =>
            `${index === 0 ? 'M' : 'L'}${x(point.t).toFixed(2)} ${y(point.v).toFixed(2)}`
        )
        .join(' ');
    const area = (points: PoolPoint[]) =>
      points.length < 2
        ? ''
        : `${line(points)} L${x(points[points.length - 1].t).toFixed(2)} ${VIEW_HEIGHT} L${x(points[0].t).toFixed(2)} ${VIEW_HEIGHT} Z`;
    const xPercent = (ms: number) => (x(ms) / VIEW_WIDTH) * 100;
    const yPercent = (v: number) => (y(v) / VIEW_HEIGHT) * 100;
    return { recordedPoints, projectedPoints, yMax, line, area, xPercent, yPercent, y };
  }, [capacity, isDrain, projection, recorded, span]);

  const inSpan = (ms: number) => ms >= span.startMs && ms <= span.endMs;
  const nowVisible = inSpan(now);

  const resetLabels = useMemo(() => {
    if (isDrain) return [];
    const labels: { t: number; amount: number; v: number }[] = [];
    let lastLeft = -Infinity;
    [...recorded.resets, ...projection.resets]
      .filter((reset) => reset.amount >= 1 && reset.t >= span.startMs && reset.t <= span.endMs)
      .sort((a, b) => a.t - b.t)
      .forEach((reset) => {
        const left = geometry.xPercent(reset.t);
        if (left - lastLeft < LABEL_MIN_GAP_PERCENT) return;
        lastLeft = left;
        const source = reset.projected ? projection.points : recorded.points;
        labels.push({ t: reset.t, amount: reset.amount, v: valueAt(source, reset.t) });
      });
    return labels;
  }, [geometry, isDrain, projection, recorded, span]);

  const days = useMemo(() => {
    const out: number[] = [];
    const cursor = new Date(span.startMs);
    while (cursor.getTime() < span.endMs) {
      out.push(cursor.getTime());
      cursor.setDate(cursor.getDate() + 1);
    }
    return out;
  }, [span]);

  const horizonEnd = Math.max(span.endMs, now + POOL_STATS_HORIZON_MS);
  const formatInstant = (ms: number) => formats.instant.format(ms);
  const dash = t('quota_management.pool_unknown');

  const poolEmptyText =
    projection.poolEmptyAt !== null
      ? formatInstant(projection.poolEmptyAt)
      : burn === null
        ? dash
        : t('quota_management.pool_not_by', { date: formats.date.format(horizonEnd) });
  const firstEmptyText = projection.firstEmpty
    ? projection.firstEmpty.at <= now
      ? t('quota_management.pool_first_empty_now', { name: projection.firstEmpty.label })
      : t('quota_management.pool_first_empty_in', {
          name: projection.firstEmpty.label,
          duration: formatPoolDuration(projection.firstEmpty.at - now, t),
        })
    : burn === null
      ? dash
      : t('quota_management.pool_not_by', { date: formats.date.format(horizonEnd) });
  const nextResetText = nextReset
    ? `+${formatPercent(nextReset.amount)} ${formatInstant(nextReset.t)}`
    : dash;
  const burnText =
    burn === null ? dash : t('quota_management.pool_rate', { value: burn.toFixed(1) });

  const yTicks = [geometry.yMax, geometry.yMax / 2, 0].map((value) => ({
    value,
    label: isDrain
      ? t('quota_management.pool_rate', { value: value.toFixed(value >= 10 ? 0 : 1) })
      : formatPercent(value),
  }));

  const emptyAt = projection.poolEmptyAt;
  const summaryLabel = t('quota_management.pool_aria', {
    provider: providerLabel,
    remaining: formatPercent(remaining),
    capacity: formatPercent(capacity),
    burn: burnText,
    empty: poolEmptyText,
  });

  return (
    <article
      className={styles.pool}
      style={{ '--pool-accent': color.text } as CSSProperties}
      aria-label={providerLabel}
    >
      <header className={styles.head}>
        <div className={styles.identity}>
          <div className={styles.titleRow}>
            <span
              className={styles.icon}
              style={
                isThemeSurfaceIconProvider(provider)
                  ? { background: getThemeSurfaceIconBackground(resolvedTheme) }
                  : undefined
              }
            >
              {iconSrc ? (
                <img src={iconSrc} alt="" />
              ) : (
                <span>{providerLabel.slice(0, 1).toUpperCase()}</span>
              )}
            </span>
            <h2 className={styles.title}>{providerLabel}</h2>
            <span className={styles.accounts}>
              {t('quota_management.pool_accounts', { count: accountCount })}
            </span>
          </div>
          <p className={styles.headline}>
            <span className={styles.headlineValue}>{formatPercent(remaining)}</span>
            <span className={styles.of}>
              {t('quota_management.pool_of', { capacity: formatPercent(capacity) })}
            </span>
          </p>
        </div>

        <dl className={styles.stats}>
          <div>
            <dt>{t('quota_management.pool_empty')}</dt>
            <dd>{poolEmptyText}</dd>
          </div>
          <div>
            <dt>{t('quota_management.pool_first_empty')}</dt>
            <dd>{firstEmptyText}</dd>
          </div>
          <div>
            <dt>{t('quota_management.pool_next_reset')}</dt>
            <dd>{nextResetText}</dd>
          </div>
          <div>
            <dt>{t('quota_management.pool_burn', { hours: burnWindowHours })}</dt>
            <dd>{burnText}</dd>
          </div>
        </dl>
      </header>

      <figure className={styles.figure} role="img" aria-label={summaryLabel}>
        <div className={styles.yAxis} aria-hidden="true">
          {yTicks.map((tick) => (
            <span key={tick.value} style={{ top: `${geometry.yPercent(tick.value)}%` }}>
              {tick.label}
            </span>
          ))}
        </div>

        <div className={styles.plot} aria-hidden="true">
          <svg
            className={styles.svg}
            viewBox={`0 0 ${VIEW_WIDTH} ${VIEW_HEIGHT}`}
            preserveAspectRatio="none"
          >
            {yTicks.map((tick) => (
              <line
                key={tick.value}
                className={styles.gridLine}
                x1={0}
                x2={VIEW_WIDTH}
                y1={geometry.y(tick.value)}
                y2={geometry.y(tick.value)}
                vectorEffect="non-scaling-stroke"
              />
            ))}
            <path className={styles.areaRecorded} d={geometry.area(geometry.recordedPoints)} />
            <path className={styles.areaProjected} d={geometry.area(geometry.projectedPoints)} />
            <path
              className={styles.lineRecorded}
              d={geometry.line(geometry.recordedPoints)}
              vectorEffect="non-scaling-stroke"
            />
            <path
              className={styles.lineProjected}
              d={geometry.line(geometry.projectedPoints)}
              vectorEffect="non-scaling-stroke"
            />
          </svg>

          {nowVisible && (
            <div className={styles.now} style={{ left: `${geometry.xPercent(now)}%` }}>
              <span>{t('quota_management.pool_now')}</span>
            </div>
          )}

          {resetLabels.map((label) => (
            <span
              key={label.t}
              className={styles.resetLabel}
              style={{
                left: `${geometry.xPercent(label.t)}%`,
                top: `${geometry.yPercent(label.v)}%`,
              }}
            >
              +{formatPercent(label.amount)}
            </span>
          ))}

          {!isDrain && emptyAt !== null && inSpan(emptyAt) && (
            <div className={styles.emptyMarker} style={{ left: `${geometry.xPercent(emptyAt)}%` }}>
              <span className={styles.emptyDot} />
              <span className={styles.emptyLabel}>
                {t('quota_management.pool_empty_at', { time: formatInstant(emptyAt) })}
              </span>
            </div>
          )}
        </div>

        <div className={styles.xAxis} aria-hidden="true">
          {days.map((day, index) => (
            <span
              key={day}
              data-odd={index % 2 === 1 ? 1 : 0}
              style={{ left: `${geometry.xPercent(day + DAY_MS / 2)}%` }}
            >
              {formats.day.format(day)}
            </span>
          ))}
        </div>
      </figure>

      <footer className={styles.legend}>
        <span className={styles.legendTitle}>{t('quota_management.pool_legend_pool')}</span>
        <span className={styles.legendItem}>
          <span className={styles.swatchRecorded} />
          {t('quota_management.pool_legend_recorded')}
        </span>
        <span className={styles.legendItem}>
          <span className={styles.swatchProjected} />
          {t('quota_management.pool_legend_projected')}
        </span>
      </footer>
    </article>
  );
}
