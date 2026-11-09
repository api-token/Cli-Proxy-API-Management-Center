import type { QuotaProviderType } from './providers/types';

/** tab 顺序 = 旧页五分区的纵向顺序，'全部' tab 下卡片也按此分组排列。 */
export const QUOTA_TAB_ORDER: readonly QuotaProviderType[] = [
  'claude',
  'antigravity',
  'codex',
  'xai',
  'kimi',
  'devin',
  'meta',
  'plugin',
];

export type QuotaTabId = 'all' | QuotaProviderType;

/** 页级分页固定 20/页，同时把「刷新全部」的上游并发限制在 20。 */
export const QUOTA_PAGE_SIZE = 20;

/** Accounts-table columns that can be sorted by. */
export const QUOTA_SORT_COLUMNS = ['account', 'plan', 'short', 'long', 'other'] as const;

export type QuotaSortColumn = (typeof QUOTA_SORT_COLUMNS)[number];

/** 排序：默认 = provider 分组序；soonest = 最快恢复优先；其余按表格列排序。 */
export const QUOTA_SORT_MODES = ['default', 'soonest', ...QUOTA_SORT_COLUMNS] as const;

export type QuotaSortMode = (typeof QUOTA_SORT_MODES)[number];
