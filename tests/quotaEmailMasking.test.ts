import { describe, expect, test } from 'bun:test';
import type { AuthFileItem } from '@/types';
import { getQuotaTitle, maskEmails } from '@/utils/quota/identity';

const claudeFile: AuthFileItem = {
  name: 'claude-8e9d5330-person@example.test.json',
  provider: 'claude',
  email: 'person@example.test',
};

describe('quota email masking', () => {
  test('hides the email that CLIProxyAPI embeds in auth file names', () => {
    expect(getQuotaTitle(claudeFile, false)).toBe('claude-8e9d5330-•••.json');
    expect(getQuotaTitle(claudeFile, true)).toBe(claudeFile.name);
  });

  test('masks the known email regardless of case', () => {
    expect(maskEmails('codex-Person@Example.test-plus.json', 'person@example.test')).toBe(
      'codex-•••-plus.json'
    );
  });

  test('masks addresses the email field does not list', () => {
    // Without a known email the prefix may be masked too; over-masking is the safe side.
    expect(maskEmails('gemini-a.b@ex-ample.co.uk-project.json')).toBe('•••-project.json');
    expect(maskEmails('shared.json · demo@example.test')).toBe('shared.json · •••');
    expect(maskEmails('antigravity.json')).toBe('antigravity.json');
  });
});
