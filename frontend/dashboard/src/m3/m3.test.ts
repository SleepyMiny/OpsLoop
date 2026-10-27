import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { M3App } from './M3App';
import { eventIndex, route, safePrURL } from './model';
import evidence from '../data/m3/evidence.json';

afterEach(() => vi.unstubAllGlobals());
const render = (search: string) => {
  vi.stubGlobal('window', { location: { search } });
  return renderToStaticMarkup(createElement(M3App));
};

describe('M3 sealed evidence exhibition', () => {
  it('defaults to historical Exhibition, including the previous demo URL', () => {
    expect(route('').mode).toBe('exhibition');
    expect(route('?source=demo').mode).toBe('exhibition');
    const html = render('');
    expect(html).toContain('EXHIBITION · 현재 표시: 기록 Replay');
    expect(html).toContain('현재 클러스터 상태가 아닙니다');
    expect(html).toContain('PR_CREATED');
  });

  it('renders both real run bindings without network access', () => {
    const fetch = vi.fn(() => { throw new Error('Replay cannot use network'); });
    vi.stubGlobal('fetch', fetch);
    for (const run of evidence.runs) {
      const html = render(`?source=replay&view=evidence&run=${run.id}`);
      expect(html).toContain(run.binding.incidentUID);
      expect(html).toContain(run.binding.runID);
      expect(html).toContain(run.binding.epoch);
      expect(html).toContain('SHA-256');
      expect(html).not.toContain('시뮬레이션 데이터');
    }
    expect(fetch).not.toHaveBeenCalled();
  });

  it('keeps PR creation, remote CI and service recovery distinct', () => {
    const html = render('?source=replay&view=engineering');
    expect(html).toContain('PR_CREATED');
    expect(html).toContain('NOT_DEPLOYED');
    expect(html).toContain('NOT_RUN');
    expect(html).toContain('복구되지 않음');
    expect(html).toContain('href="https://github.com/SleepyMiny/OpsLoop/pull/6"');
    expect(html).toContain('원본에 시각 없음');
  });

  it('rejects unsafe PR links and clamps replay boundaries', () => {
    expect(safePrURL('javascript:alert(1)')).toBeUndefined();
    expect(safePrURL('https://github.com/other/repo/pull/6')).toBeUndefined();
    expect(eventIndex(-1, 8)).toBe(0);
    expect(eventIndex(12, 8)).toBe(7);
    expect(eventIndex(NaN, 8)).toBe(0);
  });
});
