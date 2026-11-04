import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18n from '../src/i18n/index';
import { QuotaPoolChart } from '../src/features/quota/components/QuotaPoolChart';
import { buildPool, poolSpan } from '../src/features/quota/poolModel';

const HOUR = 60 * 60 * 1000;
const NOW = new Date(2026, 10, 4, 12).getTime();

const render = (samples: { hoursAgo: number; remaining: number }[][], viewMode = 'remaining') => {
  const span = poolSpan(NOW, 0);
  const members = samples.map((series, index) => ({
    key: `cred-${index}`,
    label: `cred-${index}`,
    periodHours: 168,
    samples: series.map((sample) => ({
      observedAtMs: NOW - sample.hoursAgo * HOUR,
      remainingPercent: sample.remaining,
      resetAtMs: NOW + 30 * HOUR,
    })),
  }));
  const summary = buildPool(members, {
    now: NOW,
    spanStartMs: span.startMs,
    spanEndMs: span.endMs,
    burnWindowHours: 3,
    futureUsage: 'same',
  });
  return renderToStaticMarkup(
    createElement(QuotaPoolChart, {
      provider: 'claude',
      accountCount: members.length,
      summary,
      viewMode: viewMode as 'remaining' | 'drain',
      span,
      now: NOW,
      burnWindowHours: 3,
      resolvedTheme: 'dark',
    })
  );
};

describe('QuotaPoolChart rendering', () => {
  // Restore the language afterwards: other suites assume the default.
  const original = i18n.language;
  beforeAll(async () => {
    await i18n.changeLanguage('en');
  });
  afterAll(async () => {
    await i18n.changeLanguage(original);
  });

  test('shows pooled totals, burn, empties and the now marker', () => {
    const markup = render([
      [
        { hoursAgo: 3, remaining: 30 },
        { hoursAgo: 0, remaining: 24 },
      ],
      [{ hoursAgo: 0, remaining: 100 }],
    ]);
    expect(markup).toContain('124%');
    expect(markup).toContain('of 200%');
    expect(markup).toContain('2 accounts');
    expect(markup).toContain('Burn, last 3h');
    expect(markup).toContain('2.0%/h');
    // cred-0 alone burns, so it runs dry first: 24 points at 2%/h.
    expect(markup).toContain('cred-0 in 12h 0m');
    expect(markup).toContain('+136%');
    expect(markup).toContain('>Now<');
    expect(markup).toMatch(/role="img" aria-label="Claude pool: 124% of 200% remaining/);
  });

  test('without history the burn and empties are unknown, not invented', () => {
    const markup = render([[{ hoursAgo: 0, remaining: 50 }]]);
    expect(markup).toContain('50%');
    expect((markup.match(/<dd>—<\/dd>/g) ?? []).length).toBe(3);
  });

  test('drain view labels the axis as a rate and hides reset labels', () => {
    const markup = render(
      [
        [
          { hoursAgo: 3, remaining: 30 },
          { hoursAgo: 0, remaining: 24 },
        ],
      ],
      'drain'
    );
    expect(markup).toContain('%/h</span>');
    expect(markup).not.toMatch(/top:[^"]*">\+\d/);
  });
});
