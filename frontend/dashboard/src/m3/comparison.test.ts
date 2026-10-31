import {createElement} from 'react';
import {renderToStaticMarkup} from 'react-dom/server';
import {describe,expect,it} from 'vitest';
import {InputComparison} from './ExhibitionConsole';
describe('independent same-input comparison', () => {
  const check = (passed: boolean, hash: string) => ({name:'empty-order-regression',passed,httpStatus:passed?200:500,inputSHA256:hash});
  it('shows actual original failure and candidate pass for identical input', () => {
    const html=renderToStaticMarkup(createElement(InputComparison,{data:{accepted:true,baseline:[check(false,'a'.repeat(64))],candidate:[check(true,'a'.repeat(64))]}}));
    expect(html).toContain('동일 입력 SHA-256 확인됨'); expect(html).toContain('FAIL · HTTP 500'); expect(html).toContain('PASS · HTTP 200');
  });
  it('does not turn unmatched input or missing candidate into comparison success', () => {
    for(const candidate of [[check(true,'b'.repeat(64))],[]]) {
      const html=renderToStaticMarkup(createElement(InputComparison,{data:{accepted:true,baseline:[check(false,'a'.repeat(64))],candidate}}));
      expect(html).toContain('동일 입력 연결 미확인'); expect(html).not.toContain('동일 입력 SHA-256 확인됨');
    }
  });
});
