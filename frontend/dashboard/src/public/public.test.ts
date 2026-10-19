import {describe,expect,it,vi} from 'vitest';
import {createElement} from 'react';
import {renderToStaticMarkup} from 'react-dom/server';
import {createPublicDataSource,UnsupportedPublicMode} from './createPublicDataSource';
import {parseReplay} from '../data/replay/replayCore';
import original from '../data/replay/evidence.json';
import publicEvidence from './evidence.public.json';
import boundary from './evidence-boundary.json';
import {Header} from '../app/Header';
import {SourceNotice} from '../app/SourceNotice';

describe('public source is fail-closed and never polls an API', () => {
  it.each(['','?source=demo&play=0','?source=replay'])('supports only explicit static sources: %s', (search) => {
    const fetcher=vi.fn();vi.stubGlobal('fetch',fetcher);vi.stubGlobal('window',{setInterval:()=>1,clearInterval:()=>{}});
    try {
      const source=createPublicDataSource(search);source.start();source.getSnapshot();source.stop();
      expect(source.kind).toBe(search.includes('replay')?'replay':'demo');expect(fetcher).not.toHaveBeenCalled();
    } finally {vi.unstubAllGlobals();}
  });
  it.each(['?source=live','?source=unknown','?source=','?source=demo&source=live','?source=LIVE','?source=demo&scenario=unknown'])('rejects without switching source: %s', (search) => {
    const fetcher=vi.fn();vi.stubGlobal('fetch',fetcher);vi.stubGlobal('window',{setInterval:()=>1,clearInterval:()=>{}});
    try {expect(()=>createPublicDataSource(search)).toThrow(UnsupportedPublicMode);expect(fetcher).not.toHaveBeenCalled();}
    finally {vi.unstubAllGlobals();}
  });
  it('cannot be configured with query API/base override', () => {
    const s=createPublicDataSource('?source=demo&play=0&api=https://invalid.test&VITE_OPSLOOP_SOURCE=live');
    expect(s.kind).toBe('demo');expect('baseUrl' in s).toBe(false);
  });
});

describe('public data provenance and disclosure', () => {
  it('keeps every preserved decision, score, attempt, timing and classification modulo identity aliases', () => {
    const rename=(v:unknown):unknown => {
      if(Array.isArray(v))return v.map(rename);
      if(v && typeof v==='object')return Object.fromEntries(Object.entries(v).map(([k,a])=>[k,rename(a)]));
      if(typeof v==='string'){let text=v;for(const [a,b] of Object.entries(boundary.aliases))text=text.replaceAll(a,b);return text;}
      return v;
    };
    expect(parseReplay(publicEvidence)).toEqual(rename(parseReplay(original)));
    for(const e of parseReplay(publicEvidence))for(const f of e.frames){
      expect(f.snapshot.policy.spec.mode).toBe('ObserveOnly');
      expect(f.snapshot.activeIncident).toBeNull();
      for(const i of f.snapshot.recentIncidents)expect(i.status.runtime.attempts).toHaveLength(0);
    }
  });
  it('public navigation and first-screen source disclosure exclude Live', () => {
    const source=createPublicDataSource('?play=0');
    const header=renderToStaticMarkup(createElement(Header,{source,status:source.getStatus(),playback:source.demo!.getPlayback(),theme:'dark',onCycleTheme:()=>{},onOpenReality:()=>{},publicView:true}));
    expect(header).not.toContain('href="?source=live"');
    const notice=renderToStaticMarkup(createElement(SourceNotice,{kind:source.kind,snapshot:source.getSnapshot(),status:source.getStatus(),publicView:true}));
    expect(notice).toContain('공개 전시');expect(notice).toContain('자동 복구 장면은 실측 결과가 아닙니다');
    const local=renderToStaticMarkup(createElement(Header,{source,status:source.getStatus(),playback:null,theme:'dark',onCycleTheme:()=>{},onOpenReality:()=>{}}));
    expect(local).toContain('href="?source=live"');
  });
});
