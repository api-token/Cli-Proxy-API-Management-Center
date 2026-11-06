import type { AuthFileItem } from '@/types';
import { normalizeRecentRequestAuthIndex } from '@/utils/recentRequests';
import { isDevinFile, isPluginQuotaFile } from './validators';

const QUOTA_IDENTITY_SEPARATOR = '\0';

/**
 * Devin and backend quota providers can expose multiple credential identities
 * from one physical file, distinguished by auth_index.
 */
export function getQuotaCacheKey(file: AuthFileItem): string {
  if (!isDevinFile(file) && !isPluginQuotaFile(file)) return file.name;
  const authIndex = normalizeRecentRequestAuthIndex(file.authIndex);
  return `${file.name}${QUOTA_IDENTITY_SEPARATOR}${authIndex ?? ''}`;
}

/** Disambiguate same-name cards without ever falling back to account (a secret). */
export function getQuotaDisplayName(file: AuthFileItem): string {
  if (!isDevinFile(file) && !isPluginQuotaFile(file)) return file.name;
  const identity = file.email?.trim() || normalizeRecentRequestAuthIndex(file.authIndex);
  return identity ? `${file.name} · ${identity}` : file.name;
}

const EMAIL_MASK = '•••';
// The TLD may not be `json`, so `user@example.com.json` masks only the address.
const EMAIL_PATTERN = /[a-z0-9._%+-]+@(?:[a-z0-9-]+\.)+(?!json\b)[a-z]{2,}/gi;
const escapeRegExp = (value: string) => value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

/**
 * Replace email addresses in a label. CLIProxyAPI builds auth file names from
 * the account email (`claude-<hash>-<email>.json`), so hiding the email field
 * alone is not enough. The known email is masked exactly; the pattern catches
 * the rest.
 */
export function maskEmails(text: string, knownEmail?: string | null): string {
  const email = knownEmail?.trim();
  const masked = email ? text.replace(new RegExp(escapeRegExp(email), 'gi'), EMAIL_MASK) : text;
  return masked.replace(EMAIL_PATTERN, EMAIL_MASK);
}

/** Card title: the display name, with emails masked unless the user shows them. */
export function getQuotaTitle(file: AuthFileItem, showEmails: boolean): string {
  const name = getQuotaDisplayName(file);
  return showEmails ? name : maskEmails(name, file.email);
}

/** Resolve a cache identity back to the physical filename used by file mutations. */
export function getQuotaCacheFileName(key: string): string {
  const separatorIndex = key.indexOf(QUOTA_IDENTITY_SEPARATOR);
  return separatorIndex === -1 ? key : key.slice(0, separatorIndex);
}
