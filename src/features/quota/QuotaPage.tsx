/**
 * Quota page: per-provider pooled capacity charts, with the per-credential
 * card grid behind the "Accounts" toggle.
 *
 * Behavior contracts:
 * - every credential's quota loads once when the page opens (pools need all
 *   of them); no polling. "Refresh all" reloads every credential;
 * - cacheGeneration 会话隔离 + request-id 去重（见 useQuotaBatchLoader）；
 * - 文件列表变化后按 provider 剪枝额度缓存（已删文件不残留）；
 * - useHeaderRefresh 单槽位：本页唯一注册者，全局刷新 = 重取文件列表。
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { authFilesApi } from '@/services/api';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { IconSearch, IconX } from '@/components/ui/icons';
import { Select } from '@/components/ui/Select';
import { Skeleton } from '@/components/ui/Skeleton';
import { DAY_MS, HOUR_MS } from '@/utils/time/durations';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useNow } from '@/hooks/useNow';
import { useRevealGroup } from '@/hooks/motion';
import { useAuthStore, useQuotaStore, useThemeStore } from '@/stores';
import type { AuthFileItem, ResolvedTheme } from '@/types';
import { getQuotaCacheKey, getQuotaTitle } from '@/utils/quota/identity';
import { ProviderTabs } from '@/features/authFiles/components/ProviderTabs';
import { QuotaHeader } from './components/QuotaHeader';
import { QuotaCard } from './components/QuotaCard';
import { QuotaPoolChart, type PoolViewMode } from './components/QuotaPoolChart';
import { QuotaPoolToolbar } from './components/QuotaPoolToolbar';
import {
  CARD_ENTRANCE_BUDGET_MS,
  QUOTA_PAGE_SIZE,
  QUOTA_SORT_MODES,
  QUOTA_TAB_ORDER,
  type QuotaSortMode,
  type QuotaTabId,
} from './constants';
import {
  buildTabCounts,
  canRefreshQuotaAfterList,
  classifyQuotaFiles,
  filterEntriesByTab,
  filterEntriesBySearch,
  paginate,
  sortQuotaEntries,
  type QuotaFileEntry,
} from './logic';
import { nextRecoveryMs } from './resetSchedule';
import { buildPool, poolSpan, type BurnWindowHours, type FutureUsageMode } from './poolModel';
import { buildPoolMembers } from './poolMembers';
import { QUOTA_ADAPTERS, getQuotaSetter, type QuotaCardState } from './providers';
import type { QuotaProviderType } from './providers/types';
import { useQuotaActions } from './hooks/useQuotaActions';
import { useQuotaBatchLoader } from './hooks/useQuotaBatchLoader';
import { useQuotaHistory } from './hooks/useQuotaHistory';
import { readQuotaUiState, writeQuotaUiState, type QuotaUiState } from './uiState';
import styles from './QuotaPage.module.scss';

const TAB_IDS: string[] = ['all', ...QUOTA_TAB_ORDER];
const SKELETON_CARD_COUNT = 6;
/** History also covers the longest burn window before the visible span. */
const HISTORY_LEAD_MS = 6 * HOUR_MS;

export function QuotaPage() {
  const { t } = useTranslation();
  const connectionStatus = useAuthStore((state) => state.connectionStatus);
  const resolvedTheme: ResolvedTheme = useThemeStore((state) => state.resolvedTheme);

  const [files, setFiles] = useState<AuthFileItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [tab, setTab] = useState<QuotaTabId>(() => readQuotaUiState()?.tab ?? 'all');
  const [sortMode, setSortMode] = useState<QuotaSortMode>(
    () => readQuotaUiState()?.sortMode ?? 'default'
  );
  const [viewMode, setViewMode] = useState<PoolViewMode>(
    () => readQuotaUiState()?.viewMode ?? 'remaining'
  );
  const [burnWindowHours, setBurnWindowHours] = useState<BurnWindowHours>(
    () => readQuotaUiState()?.burnWindowHours ?? 3
  );
  const [futureUsage, setFutureUsage] = useState<FutureUsageMode>(
    () => readQuotaUiState()?.futureUsage ?? 'same'
  );
  const [accountsOpen, setAccountsOpen] = useState(() => readQuotaUiState()?.accountsOpen ?? false);
  const [showEmails, setShowEmails] = useState(() => readQuotaUiState()?.showEmails ?? false);
  const [spanOffset, setSpanOffset] = useState(0);
  const [historyRefresh, setHistoryRefresh] = useState(0);
  const [page, setPage] = useState(1);
  const [search, setSearch] = useState('');
  const searchInputRef = useRef<HTMLInputElement>(null);
  // Header + tabs entrance cascade (title → meta → actions → tabs, 70ms apart).
  const revealRef = useRevealGroup<HTMLDivElement>();

  const disableControls = connectionStatus !== 'connected';

  /* ---------- credential list ---------- */

  const sessionGeneration = useQuotaStore((state) => state.cacheGeneration);
  const [filesGeneration, setFilesGeneration] = useState<number | null>(null);
  const listRequestRef = useRef(0);
  const loadFiles = useCallback(async () => {
    const requestId = ++listRequestRef.current;
    if (connectionStatus !== 'connected') {
      setFiles([]);
      setFilesGeneration(null);
      setLoading(false);
      return;
    }
    const isCurrent = () =>
      requestId === listRequestRef.current &&
      sessionGeneration === useQuotaStore.getState().cacheGeneration;
    setLoading(true);
    setError('');
    try {
      const data = await authFilesApi.list();
      if (!isCurrent()) return;
      setFiles(data?.files || []);
      setFilesGeneration(sessionGeneration);
    } catch (err: unknown) {
      if (!isCurrent()) return;
      const message = err instanceof Error ? err.message : t('notification.refresh_failed');
      setError(message);
    } finally {
      if (isCurrent()) setLoading(false);
    }
  }, [connectionStatus, sessionGeneration, t]);

  useHeaderRefresh(loadFiles);

  useEffect(() => {
    void loadFiles();
    return () => {
      listRequestRef.current += 1;
    };
  }, [loadFiles]);

  /* ---------- quota cache ----------
   * Read before classification/sorting: "soonest recovery" sorts by it. */

  const antigravityQuota = useQuotaStore((state) => state.antigravityQuota);
  const claudeQuota = useQuotaStore((state) => state.claudeQuota);
  const codexQuota = useQuotaStore((state) => state.codexQuota);
  const devinQuota = useQuotaStore((state) => state.devinQuota);
  const kimiQuota = useQuotaStore((state) => state.kimiQuota);
  const metaQuota = useQuotaStore((state) => state.metaQuota);
  const pluginQuota = useQuotaStore((state) => state.pluginQuota);
  const xaiQuota = useQuotaStore((state) => state.xaiQuota);

  const quotaByType = useMemo<Record<QuotaProviderType, Record<string, QuotaCardState>>>(
    () =>
      ({
        antigravity: antigravityQuota,
        claude: claudeQuota,
        codex: codexQuota,
        devin: devinQuota,
        kimi: kimiQuota,
        meta: metaQuota,
        plugin: pluginQuota,
        xai: xaiQuota,
      }) as unknown as Record<QuotaProviderType, Record<string, QuotaCardState>>,
    [
      antigravityQuota,
      claudeQuota,
      codexQuota,
      devinQuota,
      kimiQuota,
      metaQuota,
      pluginQuota,
      xaiQuota,
    ]
  );

  const getQuota = useCallback(
    (entry: QuotaFileEntry): QuotaCardState | undefined =>
      quotaByType[entry.type][getQuotaCacheKey(entry.file)],
    [quotaByType]
  );

  /* ---------- classify / filter / sort / paginate ---------- */

  // The charts need a running clock. Sorting reads it only in "soonest" mode:
  // in the default order pageItems must keep its identity across ticks, or the
  // "refresh all" loading-edge effect below would spin every minute.
  const now = useNow();
  const sortNow = sortMode === 'default' ? 0 : now;

  const entries = useMemo(() => classifyQuotaFiles(files), [files]);
  const tabCounts = useMemo(() => buildTabCounts(entries), [entries]);
  const tabEntries = useMemo(() => filterEntriesByTab(entries, tab), [entries, tab]);
  const filteredEntries = useMemo(
    () => filterEntriesBySearch(tabEntries, search),
    [tabEntries, search]
  );
  const handleSearchChange = useCallback((value: string) => {
    setSearch(value);
    setPage(1);
  }, []);

  const resolveNextRecovery = useCallback(
    (entry: QuotaFileEntry) => nextRecoveryMs(entry.type, getQuota(entry), sortNow),
    [getQuota, sortNow]
  );
  // Sort before paginating, or "soonest" would only hold within a page.
  const sortedEntries = useMemo(
    () => sortQuotaEntries(filteredEntries, sortMode, resolveNextRecovery),
    [filteredEntries, sortMode, resolveNextRecovery]
  );

  const { pageItems, currentPage, totalPages } = useMemo(
    () => paginate(sortedEntries, page, QUOTA_PAGE_SIZE),
    [sortedEntries, page]
  );

  const handleTabChange = useCallback((next: string) => {
    setTab(next as QuotaTabId);
    setPage(1);
    writeQuotaUiState({ tab: next as QuotaTabId });
  }, []);

  const handleSortModeChange = useCallback((next: string) => {
    setSortMode(next as QuotaSortMode);
    setPage(1);
    writeQuotaUiState({ sortMode: next as QuotaSortMode });
  }, []);

  const sortOptions = useMemo(
    () =>
      QUOTA_SORT_MODES.map((mode) => ({ value: mode, label: t(`quota_management.sort_${mode}`) })),
    [t]
  );

  const { loadedCount, attentionCount } = useMemo(() => {
    let loaded = 0;
    let attention = 0;
    entries.forEach((entry) => {
      const status = quotaByType[entry.type][getQuotaCacheKey(entry.file)]?.status;
      if (status === 'success') loaded += 1;
      else if (status === 'error') attention += 1;
    });
    return { loadedCount: loaded, attentionCount: attention };
  }, [entries, quotaByType]);

  // Prune: once the list settles, keep only credentials that still exist.
  useEffect(() => {
    if (loading || error || filesGeneration !== sessionGeneration) return;
    const survivorsByType = new Map<QuotaProviderType, Set<string>>(
      QUOTA_TAB_ORDER.map((type) => [type, new Set<string>()])
    );
    entries.forEach((entry) => survivorsByType.get(entry.type)?.add(getQuotaCacheKey(entry.file)));

    QUOTA_TAB_ORDER.forEach((type) => {
      const survivors = survivorsByType.get(type) ?? new Set<string>();
      const setQuota = getQuotaSetter(QUOTA_ADAPTERS[type]);
      setQuota((prev) => {
        const staleKeys = Object.keys(prev).filter((name) => !survivors.has(name));
        if (staleKeys.length === 0) return prev;
        const next = { ...prev };
        staleKeys.forEach((name) => delete next[name]);
        return next;
      });
    });
  }, [entries, error, filesGeneration, loading, sessionGeneration]);

  /* ---------- loading and actions ---------- */

  const { batchLoading, loadQuota } = useQuotaBatchLoader();
  const { resettingQuotaName, refreshQuota, resetQuota } = useQuotaActions(disableControls);

  // Disabled credentials take no traffic: they are neither loaded nor pooled.
  const poolEntries = useMemo(() => entries.filter((entry) => !entry.file.disabled), [entries]);

  // Load in page-sized chunks so upstream concurrency stays bounded.
  const loadAllQuota = useCallback(
    async (targets: QuotaFileEntry[]) => {
      for (let index = 0; index < targets.length; index += QUOTA_PAGE_SIZE) {
        await loadQuota(targets.slice(index, index + QUOTA_PAGE_SIZE));
      }
    },
    [loadQuota]
  );

  // Auto-load once per visit and session, as soon as the list has settled.
  const autoLoadedSessionRef = useRef<number | null>(null);
  useEffect(() => {
    if (disableControls || loading || error || filesGeneration !== sessionGeneration) return;
    if (autoLoadedSessionRef.current === sessionGeneration) return;
    autoLoadedSessionRef.current = sessionGeneration;
    void loadAllQuota(poolEntries);
  }, [
    disableControls,
    error,
    filesGeneration,
    loadAllQuota,
    loading,
    poolEntries,
    sessionGeneration,
  ]);

  const pendingRefreshRef = useRef<number | null>(null);
  const prevLoadingRef = useRef(loading);

  // Refresh all: refetch the list, then (on its loading falling edge) reload every credential.
  const handleRefreshAll = useCallback(() => {
    if (disableControls) return;
    pendingRefreshRef.current = sessionGeneration;
    void loadFiles();
  }, [disableControls, loadFiles, sessionGeneration]);

  useEffect(() => {
    const wasLoading = prevLoadingRef.current;
    prevLoadingRef.current = loading;

    const requestedSession = pendingRefreshRef.current;
    if (requestedSession === null) return;
    if (requestedSession !== sessionGeneration) {
      pendingRefreshRef.current = null;
      return;
    }
    if (loading || !wasLoading) return;

    pendingRefreshRef.current = null;
    if (
      canRefreshQuotaAfterList(
        requestedSession,
        sessionGeneration,
        filesGeneration,
        Boolean(error),
        disableControls
      )
    ) {
      setHistoryRefresh((value) => value + 1);
      void loadAllQuota(poolEntries);
    }
  }, [
    disableControls,
    error,
    filesGeneration,
    loading,
    loadAllQuota,
    poolEntries,
    sessionGeneration,
  ]);

  const canUseActions = !disableControls && !loading && filesGeneration === sessionGeneration;

  /* ---------- pools ---------- */

  const dayStart = new Date(now).setHours(0, 0, 0, 0);
  const span = useMemo(() => poolSpan(dayStart, spanOffset), [dayStart, spanOffset]);
  const history = useQuotaHistory({
    enabled: !disableControls,
    sinceMs: Math.min(span.startMs, dayStart - DAY_MS) - HISTORY_LEAD_MS,
    entries: poolEntries,
    getQuota,
    refreshToken: historyRefresh,
  });

  const labelFor = useCallback(
    (entry: QuotaFileEntry) =>
      (showEmails && entry.file.email?.trim()) || getQuotaTitle(entry.file, showEmails),
    [showEmails]
  );

  const pools = useMemo(() => {
    const providers = tab === 'all' ? QUOTA_TAB_ORDER : [tab];
    return providers.flatMap((provider) => {
      const members = buildPoolMembers(
        poolEntries,
        provider,
        history.samplesBySeries,
        history.liveByKey,
        labelFor
      );
      if (members.length === 0) return [];
      const summary = buildPool(members, {
        now,
        spanStartMs: span.startMs,
        spanEndMs: span.endMs,
        burnWindowHours,
        futureUsage,
      });
      return [{ provider, accountCount: members.length, summary }];
    });
  }, [burnWindowHours, futureUsage, history, labelFor, now, poolEntries, span, tab]);

  const updateUi =
    <K extends keyof QuotaUiState>(key: K, setter: (value: NonNullable<QuotaUiState[K]>) => void) =>
    (value: NonNullable<QuotaUiState[K]>) => {
      setter(value);
      writeQuotaUiState({ [key]: value } as QuotaUiState);
    };

  /* ---------- one-time card entrance cascade ----------
   * Flip cardsAnimated right after the first batch renders. Mounted cards
   * captured their delay at mount (useState init in QuotaCard); cards mounted
   * later (tab/page/refresh) get null — no replay. */

  const [cardsAnimated, setCardsAnimated] = useState(false);
  const enableCardEntrance = !cardsAnimated && !loading && accountsOpen && pageItems.length > 0;
  useEffect(() => {
    if (enableCardEntrance) {
      setCardsAnimated(true);
    }
  }, [enableCardEntrance]);
  const cardEntranceDelay = (index: number): number | null => {
    if (!enableCardEntrance) return null;
    if (pageItems.length <= 1) return 0;
    return Math.round((index / (pageItems.length - 1)) * CARD_ENTRANCE_BUDGET_MS);
  };

  /* ---------- render ---------- */

  const noCredentials = !loading && tabEntries.length === 0;
  const isEmpty = !loading && filteredEntries.length === 0;

  return (
    <div className={styles.page} ref={revealRef}>
      <QuotaHeader
        totalCount={entries.length}
        loadedCount={loadedCount}
        attentionCount={attentionCount}
        refreshing={loading || batchLoading}
        disableControls={disableControls}
        onRefreshAll={handleRefreshAll}
        showEmails={showEmails}
        onToggleEmails={() => updateUi('showEmails', setShowEmails)(!showEmails)}
      />

      <section className={styles.workbench}>
        {/* Provider navigation stays on its own row, apart from the view controls. */}
        <div className={styles.tabsRow} data-reveal>
          <ProviderTabs
            types={TAB_IDS}
            counts={tabCounts}
            active={tab}
            resolvedTheme={resolvedTheme}
            onChange={handleTabChange}
          />
        </div>

        <QuotaPoolToolbar
          viewMode={viewMode}
          onViewModeChange={updateUi('viewMode', setViewMode)}
          burnWindowHours={burnWindowHours}
          onBurnWindowChange={updateUi('burnWindowHours', setBurnWindowHours)}
          futureUsage={futureUsage}
          onFutureUsageChange={updateUi('futureUsage', setFutureUsage)}
          accountsOpen={accountsOpen}
          onToggleAccounts={() => updateUi('accountsOpen', setAccountsOpen)(!accountsOpen)}
          span={span}
          offset={spanOffset}
          onOffsetChange={setSpanOffset}
        />

        {error && (
          <div className={styles.errorBanner} role="alert">
            {error}
          </div>
        )}

        {history.unsupported && (
          <p className={styles.historyNote} role="status">
            {t('quota_management.pool_history_unsupported')}
          </p>
        )}

        {noCredentials ? (
          <EmptyState
            title={
              tab === 'all'
                ? t('quota_management.empty_title')
                : t(`${QUOTA_ADAPTERS[tab].i18nPrefix}.empty_title`)
            }
            description={
              tab === 'all'
                ? t('quota_management.empty_desc')
                : t(`${QUOTA_ADAPTERS[tab].i18nPrefix}.empty_desc`)
            }
            action={
              tab === 'all' ? undefined : (
                <Button variant="secondary" size="sm" onClick={() => handleTabChange('all')}>
                  {t('auth_files.filter_all')}
                </Button>
              )
            }
          />
        ) : (
          <div className={styles.pools}>
            {pools.length > 0 ? (
              pools.map((pool) => (
                <QuotaPoolChart
                  key={pool.provider}
                  provider={pool.provider}
                  accountCount={pool.accountCount}
                  summary={pool.summary}
                  viewMode={viewMode}
                  span={span}
                  now={now}
                  burnWindowHours={burnWindowHours}
                  resolvedTheme={resolvedTheme}
                />
              ))
            ) : loading || batchLoading ? (
              <Skeleton height={360} rounded={14} />
            ) : (
              <p className={styles.poolsEmpty} role="status">
                {t('quota_management.pool_none')}
              </p>
            )}
          </div>
        )}

        {accountsOpen && !noCredentials && (
          <section
            id="quota-accounts"
            className={styles.accounts}
            aria-label={t('quota_management.pool_accounts_toggle')}
          >
            <div className={styles.toolbar}>
              <div className={styles.search}>
                <IconSearch size={16} className={styles.searchIcon} aria-hidden="true" />
                <input
                  ref={searchInputRef}
                  className={styles.searchInput}
                  type="search"
                  value={search}
                  onChange={(event) => handleSearchChange(event.target.value)}
                  placeholder={t('quota_management.search_placeholder')}
                  aria-label={t('quota_management.search_label')}
                />
                {search && (
                  <button
                    type="button"
                    className={styles.clearSearch}
                    aria-label={t('quota_management.search_clear')}
                    title={t('quota_management.search_clear')}
                    onClick={() => {
                      handleSearchChange('');
                      searchInputRef.current?.focus();
                    }}
                  >
                    <IconX size={14} aria-hidden="true" />
                  </button>
                )}
              </div>
              <div className={styles.sort}>
                <Select
                  value={sortMode}
                  options={sortOptions}
                  onChange={handleSortModeChange}
                  ariaLabel={t('quota_management.sort_label')}
                  size="sm"
                />
              </div>
            </div>

            {loading ? (
              <div className={styles.grid} aria-hidden="true">
                {Array.from({ length: SKELETON_CARD_COUNT }, (_, index) => (
                  <Skeleton key={index} height={168} rounded={14} />
                ))}
              </div>
            ) : isEmpty ? (
              <EmptyState
                title={t('quota_management.search_empty_title')}
                description={t('quota_management.search_empty_desc')}
                action={
                  <Button variant="secondary" size="sm" onClick={() => handleSearchChange('')}>
                    {t('quota_management.search_clear')}
                  </Button>
                }
              />
            ) : (
              <div className={styles.grid}>
                {pageItems.map((entry, index) => (
                  <QuotaCard
                    key={`${entry.type}:${getQuotaCacheKey(entry.file)}`}
                    entry={entry}
                    quota={getQuota(entry)}
                    resolvedTheme={resolvedTheme}
                    canRefresh={canUseActions && !entry.file.disabled}
                    resetting={resettingQuotaName === getQuotaCacheKey(entry.file)}
                    entranceDelayMs={cardEntranceDelay(index)}
                    showEmails={showEmails}
                    onRefresh={() => void refreshQuota(entry.file, QUOTA_ADAPTERS[entry.type])}
                    onReset={() => resetQuota(entry.file, QUOTA_ADAPTERS[entry.type])}
                  />
                ))}
              </div>
            )}

            {!loading && filteredEntries.length > QUOTA_PAGE_SIZE && (
              <div className={styles.pagination}>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => setPage(Math.max(1, currentPage - 1))}
                  disabled={currentPage <= 1}
                >
                  {t('auth_files.pagination_prev')}
                </Button>
                <div className={styles.pageInfo}>
                  {t('auth_files.pagination_info', {
                    current: currentPage,
                    total: totalPages,
                    count: filteredEntries.length,
                  })}
                </div>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => setPage(Math.min(totalPages, currentPage + 1))}
                  disabled={currentPage >= totalPages}
                >
                  {t('auth_files.pagination_next')}
                </Button>
              </div>
            )}
          </section>
        )}
      </section>
    </div>
  );
}
