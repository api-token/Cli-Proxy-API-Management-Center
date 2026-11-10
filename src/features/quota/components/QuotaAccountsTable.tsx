/**
 * Accounts table: one row per credential showing its plan, short window, long
 * window, and tightest remaining limit. Expanding a row shows the provider's
 * full quota body and the actions that spend a reset.
 * Claude observations remain visible while refreshing or after a failed read.
 */

import { Fragment, useId, useMemo, useState, type MouseEvent } from 'react';
import { useTranslation } from 'react-i18next';
import {
  IconAlertTriangle,
  IconChevronDown,
  IconChevronUp,
  IconRefreshCw,
} from '@/components/ui/icons';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/Table';
import { useNow } from '@/hooks/useNow';
import type { ClaudeQuotaState, ResolvedTheme } from '@/types';
import { buildResetDisplay, resolveQuotaErrorMessage } from '@/utils/quota';
import { getQuotaCacheKey, getQuotaTitle } from '@/utils/quota/identity';
import {
  getAuthFileIcon,
  getThemeSurfaceIconBackground,
  getTypeLabel,
  isThemeSurfaceIconProvider,
} from '@/features/authFiles/constants';
import {
  hasAccountReading,
  periodParts,
  splitAccountLimits,
  summarizeAccount,
  tightestLimit,
  type AccountLimit,
  type AccountPlan,
} from '../accountSummary';
import { bindQuotaClasses } from '../types';
import { QUOTA_ADAPTERS, type QuotaCardState } from '../providers';
import type { QuotaSortColumn } from '../constants';
import { isQuotaRefreshDisabled, type QuotaFileEntry, type QuotaSort } from '../logic';
import { useClaudeResetGrants } from '../providers/claude/ClaudeResetGrants';
import { ClaudeResetGrantDetails } from '../providers/claude/ClaudeResetGrantDetails';
import { collectQuotaRowInstants, pickUrgentRowId } from '../resetSchedule';
import { QuotaMeter } from './QuotaMeter';
import bodyStyles from './QuotaBody.module.scss';
import styles from './QuotaAccountsTable.module.scss';

/** Bind the quota-page styles to the shared rendering contract. */
const quotaClasses = bindQuotaClasses(bodyStyles, 'QuotaBody.module.scss');

/** The same contract for the expanded detail, where groups flow into columns. */
const detailClasses = {
  ...quotaClasses,
  antigravityQuotaGroup: `${quotaClasses.antigravityQuotaGroup} ${styles.detailGroup}`,
};

const planClass = (tier: AccountPlan['tier']): string =>
  tier === 'elite'
    ? quotaClasses.elitePlanValue
    : tier === 'premium'
      ? quotaClasses.premiumPlanValue
      : quotaClasses.codexPlanValue;

export interface QuotaAccountsTableProps {
  entries: QuotaFileEntry[];
  /** False when no listed account has a short window, which drops that column. */
  showShortColumn: boolean;
  getQuota: (entry: QuotaFileEntry) => QuotaCardState | undefined;
  resolvedTheme: ResolvedTheme;
  canUseActions: boolean;
  /** Cache key of the credential whose quota reset is in flight, if any. */
  resettingQuotaName: string | null;
  /** When false, email addresses in account names are masked. */
  showEmails: boolean;
  sort: QuotaSort;
  /** A header was clicked; the page decides the next direction. */
  onSortColumn: (column: QuotaSortColumn) => void;
  onRefresh: (entry: QuotaFileEntry) => void;
  onReset: (entry: QuotaFileEntry) => void;
}

export function QuotaAccountsTable({
  entries,
  showShortColumn,
  getQuota,
  resolvedTheme,
  canUseActions,
  resettingQuotaName,
  showEmails,
  sort,
  onSortColumn,
  onRefresh,
  onReset,
}: QuotaAccountsTableProps) {
  const { t } = useTranslation();
  const sortHeader = (column: QuotaSortColumn, className?: string) => {
    const active = sort.mode === column;
    const SortIcon = sort.descending ? IconChevronDown : IconChevronUp;
    return (
      <TableHead
        scope="col"
        className={className}
        aria-sort={active ? (sort.descending ? 'descending' : 'ascending') : 'none'}
      >
        <button
          type="button"
          className={active ? styles.sortButtonActive : styles.sortButton}
          onClick={() => onSortColumn(column)}
        >
          {t(`quota_management.accounts_col_${column}`)}
          {active && <SortIcon size={13} aria-hidden="true" />}
        </button>
      </TableHead>
    );
  };

  return (
    // The wrapper is the size container the table's responsive tiers query.
    <div className={styles.wrap}>
      <Table
        className={styles.table}
        cols={
          <>
            <col />
            <col className={styles.colPlan} />
            {showShortColumn && <col className={styles.colLimit} />}
            <col className={styles.colLimit} />
            <col className={styles.colOther} />
            <col className={styles.colActions} />
          </>
        }
      >
        <TableHeader>
          <tr>
            {sortHeader('account')}
            {sortHeader('plan')}
            {showShortColumn && sortHeader('short')}
            {sortHeader('long')}
            {sortHeader('other', styles.cellOther)}
            <TableHead scope="col">
              <span className={styles.srOnly}>{t('quota_management.accounts_col_actions')}</span>
            </TableHead>
          </tr>
        </TableHeader>
        <TableBody>
          {entries.map((entry) => {
            const key = getQuotaCacheKey(entry.file);
            return (
              <AccountRow
                key={`${entry.type}:${key}`}
                entry={entry}
                showShortColumn={showShortColumn}
                quota={getQuota(entry)}
                resolvedTheme={resolvedTheme}
                canRefresh={canUseActions && !entry.file.disabled}
                resetting={resettingQuotaName === key}
                showEmails={showEmails}
                onRefresh={() => onRefresh(entry)}
                onReset={() => onReset(entry)}
              />
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}

interface LimitCellProps {
  limit: AccountLimit | null;
  /** Names the limit on the second line; defaults to its window length. */
  showLabel?: boolean;
  /** Further limits this cell stands in for. */
  moreCount?: number;
  /** Tooltip override; defaults to the limit's label and reset time. */
  title?: string;
  urgent: boolean;
  now: number;
}

function LimitCell({
  limit,
  showLabel = false,
  moreCount = 0,
  title,
  urgent,
  now,
}: LimitCellProps) {
  const { t, i18n } = useTranslation();
  if (!limit) return <span className={styles.none}>{t('quota_management.pool_unknown')}</span>;

  const percent = limit.remaining === null ? null : Math.round(limit.remaining);
  const reset = buildResetDisplay(limit.resetLabel, limit.resetAtMs, now, i18n.resolvedLanguage);
  const period = limit.periodHours === null ? null : periodParts(limit.periodHours);
  const periodTag =
    period === null
      ? null
      : period.unit === 'hours'
        ? t('quota_management.pool_hours', { count: period.count })
        : t('quota_management.accounts_period_days', { count: period.count });
  const meter = <QuotaMeter percent={limit.remaining} classes={quotaClasses} />;

  return (
    <div
      className={styles.limit}
      title={title ?? [limit.label, reset?.absolute].filter(Boolean).join(' · ')}
    >
      <div className={styles.limitTop}>
        <span className={styles.limitPercent}>
          {percent === null ? t('quota_management.pool_unknown') : `${percent}%`}
        </span>
        {percent === null ? (
          <div className={styles.limitMeter}>{meter}</div>
        ) : (
          <div
            className={styles.limitMeter}
            role="meter"
            aria-label={limit.label}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={percent}
          >
            {meter}
          </div>
        )}
      </div>
      <div className={styles.limitMeta}>
        {showLabel ? (
          <>
            <span className={styles.limitLabel}>{limit.label}</span>
            {moreCount > 0 && (
              <span className={styles.limitMore}>
                {t('quota_management.accounts_more', { count: moreCount })}
              </span>
            )}
          </>
        ) : (
          <>
            {periodTag && <span className={styles.limitTag}>{periodTag}</span>}
            {reset && (
              <span className={urgent ? styles.limitResetSoon : styles.limitReset}>
                {reset.relative ?? reset.absolute}
              </span>
            )}
          </>
        )}
      </div>
    </div>
  );
}

interface AccountRowProps {
  entry: QuotaFileEntry;
  showShortColumn: boolean;
  quota?: QuotaCardState;
  resolvedTheme: ResolvedTheme;
  canRefresh: boolean;
  resetting: boolean;
  showEmails: boolean;
  onRefresh: () => void;
  onReset: () => void;
}

function AccountRow({
  entry,
  showShortColumn,
  quota,
  resolvedTheme,
  canRefresh,
  resetting,
  showEmails,
  onRefresh,
  onReset,
}: AccountRowProps) {
  const { t, i18n } = useTranslation();
  const now = useNow();
  const detailId = useId();
  const [expanded, setExpanded] = useState(false);
  const adapter = QUOTA_ADAPTERS[entry.type];
  const file = entry.file;
  const displayName = getQuotaTitle(file, showEmails);

  const status = quota?.status ?? 'idle';
  const loading = status === 'loading';
  const hasData = hasAccountReading(entry.type, quota);
  const open = expanded && hasData;

  const claudeReset = useClaudeResetGrants(
    file,
    entry.type === 'claude' && status !== 'idle',
    !canRefresh || loading || resetting,
    quota,
    onRefresh
  );

  const summary = useMemo(
    () => (hasData ? summarizeAccount(entry.type, quota, t) : null),
    [entry.type, hasData, quota, t]
  );
  const columns = useMemo(() => (summary ? splitAccountLimits(summary.limits) : null), [summary]);
  const otherLimit = columns ? tightestLimit(columns.others) : null;
  // Same rule as the detail body: only the first recovery, and only in its final hour.
  const urgentRowId = useMemo(
    () => pickUrgentRowId(collectQuotaRowInstants(entry.type, quota), now),
    [entry.type, quota, now]
  );

  const providerType =
    entry.type === 'plugin'
      ? String(
          file.quotaProvider ?? file['quota_provider'] ?? file.provider ?? file.type ?? 'plugin'
        )
      : entry.type;
  const iconSrc = getAuthFileIcon(providerType, resolvedTheme);
  const typeLabel = getTypeLabel(t, providerType);
  const errorText =
    status === 'error'
      ? t(`${adapter.i18nPrefix}.load_failed`, {
          message: resolveQuotaErrorMessage(
            t,
            quota?.errorStatus,
            quota?.error || t('common.unknown_error')
          ),
        })
      : '';

  // What the collapsed row would otherwise hide: a failed refresh behind
  // retained data, a stale snapshot, or an unresolved reset.
  const claudeState = entry.type === 'claude' ? (quota as ClaudeQuotaState | undefined) : undefined;
  const notice = !hasData
    ? ''
    : errorText ||
      (claudeReset.message
        ? t(`claude_reset.${claudeReset.message}`)
        : claudeState?.stale
          ? t('claude_quota.stale_snapshot', {
              time: claudeState.observedAt
                ? new Date(claudeState.observedAt).toLocaleString(i18n.resolvedLanguage)
                : '--',
            })
          : '');

  // Claude banked resets join the plan chip row; their expiry list follows it.
  const showClaudeReset = entry.type === 'claude' && status === 'success';
  const claudePlanItems = showClaudeReset ? (
    <span className={quotaClasses.codexPlanItem}>
      <span className={quotaClasses.codexPlanLabel}>{t('claude_reset.remaining')}</span>
      <span className={quotaClasses.codexPlanValue}>{claudeReset.count ?? '--'}</span>
    </span>
  ) : undefined;
  const claudeAfterPlan = showClaudeReset ? (
    <>
      <ClaudeResetGrantDetails grants={claudeReset.grants} classes={quotaClasses} />
      {claudeReset.message && (
        <div role="status" className={quotaClasses.codexResetCreditsError}>
          {t(`claude_reset.${claudeReset.message}`)}
        </div>
      )}
    </>
  ) : undefined;
  const showReset =
    status === 'success' &&
    Boolean(adapter.resetQuota) &&
    quota !== undefined &&
    Boolean(adapter.canResetQuota?.(quota));

  // The whole row toggles the detail, except its own buttons and text selections.
  const handleRowClick = (event: MouseEvent<HTMLTableRowElement>) => {
    if (!hasData || (event.target as HTMLElement).closest('button')) return;
    if (window.getSelection()?.toString()) return;
    setExpanded((value) => !value);
  };

  const detailsLabel = t(
    open ? 'quota_management.accounts_hide_details' : 'quota_management.accounts_show_details'
  );

  return (
    <Fragment>
      <TableRow
        className={[styles.row, hasData ? styles.rowExpandable : '', open ? styles.rowOpen : '']
          .filter(Boolean)
          .join(' ')}
        onClick={handleRowClick}
      >
        <TableCell className={styles.cellAccount}>
          <div className={styles.account}>
            <span
              className={styles.iconWrap}
              title={typeLabel}
              style={
                isThemeSurfaceIconProvider(providerType)
                  ? { background: getThemeSurfaceIconBackground(resolvedTheme) }
                  : undefined
              }
            >
              {iconSrc ? (
                <img src={iconSrc} alt="" className={styles.icon} />
              ) : (
                <span className={styles.iconFallback}>{typeLabel.slice(0, 1).toUpperCase()}</span>
              )}
            </span>
            <span className={styles.fileName} title={displayName}>
              {displayName}
            </span>
            {notice && (
              <span className={styles.notice} role="img" aria-label={notice} title={notice}>
                <IconAlertTriangle size={14} aria-hidden="true" />
              </span>
            )}
          </div>
        </TableCell>
        <TableCell className={styles.cellPlan}>
          {summary?.plan ? (
            <span className={planClass(summary.plan.tier)}>{summary.plan.label}</span>
          ) : (
            <span className={styles.none}>{t('quota_management.pool_unknown')}</span>
          )}
        </TableCell>
        {columns ? (
          <>
            {showShortColumn && (
              <TableCell>
                <LimitCell
                  limit={columns.short}
                  urgent={columns.short?.id === urgentRowId}
                  now={now}
                />
              </TableCell>
            )}
            <TableCell className={showShortColumn ? undefined : styles.cellSoleLimit}>
              <LimitCell limit={columns.long} urgent={columns.long?.id === urgentRowId} now={now} />
            </TableCell>
            <TableCell className={styles.cellOther}>
              <LimitCell
                limit={otherLimit}
                showLabel
                moreCount={columns.others.length - 1}
                title={columns.others
                  .map((limit) =>
                    limit.remaining === null
                      ? limit.label
                      : `${limit.label} ${Math.round(limit.remaining)}%`
                  )
                  .join('\n')}
                urgent={false}
                now={now}
              />
            </TableCell>
          </>
        ) : (
          <TableCell colSpan={showShortColumn ? 3 : 2} className={styles.cellState}>
            {status === 'error' ? (
              <div className={styles.errorStrip} role="alert">
                {errorText}
              </div>
            ) : loading ? (
              <div className={styles.skeleton} aria-busy="true">
                <span className={styles.srOnly}>{t(`${adapter.i18nPrefix}.loading`)}</span>
                <span className={styles.skeletonTrack} aria-hidden="true" />
              </div>
            ) : (
              <button
                type="button"
                className={styles.idle}
                onClick={onRefresh}
                disabled={!canRefresh}
              >
                <IconRefreshCw size={13} aria-hidden="true" />
                {t(`${adapter.i18nPrefix}.idle`)}
              </button>
            )}
          </TableCell>
        )}
        <TableCell alignRight className={styles.cellActions}>
          <div className={styles.actions}>
            <button
              type="button"
              className={styles.iconButton}
              onClick={onRefresh}
              disabled={isQuotaRefreshDisabled(canRefresh, loading, resetting || claudeReset.busy)}
              aria-label={t('auth_files.quota_refresh_single')}
              title={t('auth_files.quota_refresh_hint')}
            >
              <IconRefreshCw
                size={14}
                aria-hidden="true"
                className={loading ? styles.spinning : undefined}
              />
            </button>
            <button
              type="button"
              className={styles.iconButton}
              onClick={() => setExpanded((value) => !value)}
              disabled={!hasData}
              aria-expanded={open}
              aria-controls={open ? detailId : undefined}
              aria-label={detailsLabel}
              title={detailsLabel}
            >
              <IconChevronDown
                size={14}
                aria-hidden="true"
                className={open ? styles.chevronOpen : styles.chevron}
              />
            </button>
          </div>
        </TableCell>
      </TableRow>
      {open && quota && (
        <tr className={styles.detailRow}>
          <td colSpan={showShortColumn ? 6 : 5} id={detailId}>
            <div className={styles.detail}>
              {status === 'error' && (
                <div className={styles.errorStrip} role="alert">
                  {errorText}
                </div>
              )}
              {loading && (
                <div role="status" aria-busy="true" className={quotaClasses.quotaMessage}>
                  {t(`${adapter.i18nPrefix}.loading`)}
                </div>
              )}
              <div className={styles.detailBody}>
                <adapter.Body
                  quota={quota}
                  classes={detailClasses}
                  planItems={claudePlanItems}
                  afterPlan={claudeAfterPlan}
                />
              </div>
              {(entry.type === 'claude' || showReset) && (
                <div className={styles.detailActions}>
                  {entry.type === 'claude' && (
                    <button
                      type="button"
                      className={styles.actionPill}
                      disabled={claudeReset.blocked}
                      onClick={claudeReset.confirm}
                      title={t(`claude_reset.${claudeReset.buttonLabel}`)}
                    >
                      <IconRefreshCw
                        size={13}
                        className={claudeReset.busy ? styles.spinning : undefined}
                      />
                      {t(`claude_reset.${claudeReset.buttonLabel}`)}
                    </button>
                  )}
                  {showReset && (
                    <button
                      type="button"
                      className={styles.actionPill}
                      onClick={onReset}
                      disabled={!canRefresh || loading || resetting}
                      title={t('codex_quota.reset_button')}
                    >
                      <IconRefreshCw
                        size={13}
                        className={resetting ? styles.spinning : undefined}
                      />
                      {t('codex_quota.reset_button')}
                    </button>
                  )}
                </div>
              )}
            </div>
          </td>
        </tr>
      )}
    </Fragment>
  );
}
