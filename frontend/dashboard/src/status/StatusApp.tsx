import {useEffect,useMemo,useState,useSyncExternalStore} from 'react';
import {App} from '../app/App';
import {createPublicDataSource} from '../public/createPublicDataSource';
import {DEMO_SCENARIOS} from '../data/demo/scenarios';
import {StatusClient} from './client';
import {statusState,type Sample} from './contract';
import {DemoSession} from './session';
import {readHistory,clearHistory,type DemoRecord} from './history';
import './status.css';

export function statusMode(search:string){const p=new URLSearchParams(search),m=p.getAll('source');if(m.length>1)throw Error('지원하지 않는 공개 모드입니다');const mode=m[0]??'home';if(!['home','demo','replay','history'].includes(mode))throw Error('지원하지 않는 공개 모드입니다. Live는 비공개 환경에서만 사용할 수 있습니다.');return mode;}
function Navigation(){return <nav className="status-nav" aria-label="전시 탐색"><a className="status-brand" href="?source=home">OpsLoop<span>HYBRID OBSERVATORY</span></a><div><a href="?source=home">관제 Home</a><a href="?source=demo">장애 시뮬레이션</a><a href="?source=history">Demo History</a><a href="?source=replay">Evidence Replay</a></div></nav>;}
export function StatusApp(){let mode;try{mode=statusMode(location.search);}catch(e){return <><Navigation/><main className="status-main"><h1>지원하지 않는 모드</h1><p>{(e as Error).message}</p><p>다른 데이터로 대체하지 않았습니다.</p></main></>;}
  return <><Navigation/>{mode==='home'?<Home/>:mode==='demo'?<Demo/>:mode==='history'?<History/>:<Evidence/>}</>;
}
function Evidence(){const source=useMemo(()=>createPublicDataSource('?source=replay'),[]);return <div className="status-existing"><App source={source} kiosk={false} publicView/></div>;}
export function Home(){
  const [client]=useState(()=>new StatusClient());const view=useSyncExternalStore(client.subscribe,client.getSnapshot,client.getSnapshot);
  useEffect(()=>{const visibility=()=>{if(document.hidden)client.stop();else client.start();};visibility();document.addEventListener('visibilitychange',visibility);return()=>{document.removeEventListener('visibilitychange',visibility);client.stop();};},[client]);
  const s=view.envelope?.snapshot,state=view.error?{dataState:'unavailable',health:'unknown'}:s?statusState(s,view.now):{dataState:'unavailable',health:'unknown'};
  const time=(t:number|null|undefined)=>t?new Date(t).toLocaleString('ko-KR'):'관측 없음';
  const metric=(x:Sample)=>x.value===null?'—':`${Math.round(x.value*100)}%`;
  const sampleTime=(x:Sample)=>time(x.observedAt)+(x.observedAt!==null&&(view.now-x.observedAt>90000||view.now-x.observedAt< -5000)?' · 표본 지연/시각 오류':'');
  return <main className="status-main"><header className="status-hero"><p className="status-eyebrow">PUBLIC READ-ONLY OBSERVATIONS</p><h1>Hybrid Kubernetes<br/><span>현재 관측 Home</span></h1><p>실제 관측값을 약 30초마다 갱신합니다. 시뮬레이션이나 과거 실험으로 대체하지 않습니다.</p><div className="status-badges"><strong data-state={state.dataState}>{view.loading?'연결 중':`관측 ${state.dataState}`}</strong><strong>health: {state.health}</strong><span>Enforce 실증 없음 · 관측 전용</span></div></header>
    <section aria-label="관측 연결 상태" className="status-notice" role="status">{view.error||(!s?'관측 데이터가 없습니다. 실제 연결이 준비될 때까지 정상 노드를 표시하지 않습니다.':`수집 완료 ${time(s.collectedAt)} · 서버 수신 ${time(view.envelope?.receivedAt)}`)}{s&&<p>CPU·메모리는 원본 측정 시각, Exporter는 PromQL 평가 시각입니다. 지연·결측을 정상으로 치환하지 않습니다.</p>}</section>
    {s&&<><section className="status-node-grid" aria-label="실제 노드 관측">{s.nodes.map(n=><article className="status-card" key={n.id}><div className="status-card-head"><h2>{n.id}</h2><span>{n.site}</span></div><p className="status-node-state">{n.readiness??'Unknown / 관측 없음'}{state.dataState!=='current'&&<small>마지막 관측값 · 현재 정상 판정 아님</small>}</p><p>{n.os??'—'} / {n.arch??'—'}</p><dl><dt>CPU</dt><dd>{metric(n.cpu)}<small>{sampleTime(n.cpu)}</small></dd><dt>Memory</dt><dd>{metric(n.memory)}<small>{sampleTime(n.memory)}</small></dd><dt>Exporter</dt><dd>{n.exporter.value===null?'관측 없음':n.exporter.value===1?'UP':'DOWN'}<small>평가 {sampleTime(n.exporter)}</small></dd></dl><p className="status-small">Node Lease {time(n.readyObservedAt)}</p></article>)}</section>
    <section className="status-detail-grid"><article className="status-card"><h2>운영 워크로드 · 관측 범위</h2>{s.workloads.map(w=><p key={w.id}><strong>{w.id}</strong> Ready {w.ready??'—'} / {w.desired??'—'}<small>{time(w.observedAt)}</small></p>)}</article><article className="status-card"><h2>RecoveryPolicy 요약</h2><p>{s.policy.mode??'관측 없음'} · enabled {String(s.policy.enabled??'unknown')}</p><p>Monitoring {String(s.policy.monitoring??'unknown')}</p><p>현재 활성 Incident {s.policy.activeIncidentCount??'알 수 없음'} · 보관 객체 {s.policy.incidentCount??'알 수 없음'}</p><p className="status-small">내부 식별정보와 정책 상세는 공개하지 않습니다.</p></article><article className="status-card"><h2>수집 출처</h2>{Object.entries(s.sources).map(([name,x])=><p key={name}><strong>{name}</strong> {x.state}<small>{x.errorCode??'조회 성공'} · {time(x.observedAt)}</small></p>)}</article></section></>}
    <footer className="status-footer">Home = 현재 실측 · Demo = 브라우저 시뮬레이션 · Replay = 과거 ObserveOnly 실증 / 운영자 복원</footer></main>;
}
function Demo(){const [source,setSource]=useState<DemoSession|null>(null),[error,setError]=useState('');
  useEffect(()=>{if(!source)return;const leave=()=>{void source.finalize('interrupted').catch(()=>{});};window.addEventListener('pagehide',leave);return()=>window.removeEventListener('pagehide',leave);},[source]);
  const finish=async()=>{try{await source?.finalize('stopped');source?.stop();location.href='?source=history';}catch{setError('기록을 저장하지 못했습니다. 재시도하거나 저장 없이 Home으로 돌아갈 수 있습니다.');}};
  if(source)return <><div className="status-session-bar"><strong>시뮬레이션 · 실제 장애 주입/자동 복구 실증 아님</strong><button onClick={()=>void finish()}>시연 종료 · History 저장</button>{error&&<span role="alert">{error}</span>}</div><div className="status-existing"><App source={source} kiosk={false} publicView/></div></>;
  return <main className="status-main"><h1>선택형 장애 시뮬레이션</h1><p>시작을 누르기 전에는 Demo 엔진을 실행하지 않습니다. Kubernetes에 요청하거나 장애를 주입하지 않습니다.</p><div className="status-node-grid">{DEMO_SCENARIOS.map(s=><article key={s.id} className="status-card"><h2>{s.title}</h2><p>{s.summary}</p><button onClick={()=>setSource(new DemoSession(s.id,()=>setError('브라우저 기록 저장에 실패했습니다')))}>시뮬레이션 시작</button></article>)}</div>{error&&<p role="alert">{error}</p>}</main>;
}
function History(){const [records,setRecords]=useState<DemoRecord[]|null>(null),[error,setError]=useState('');useEffect(()=>{let active=true;readHistory().then(r=>{if(active)setRecords(r);}).catch(()=>{if(active)setError('브라우저 기록을 읽지 못했습니다. 이력 없음으로 처리하지 않습니다.');});return()=>{active=false;};},[]);
  return <main className="status-main"><h1>Demo History</h1><p>이 브라우저에 저장된 시뮬레이션 이력입니다. 실제 Incident 또는 Evidence Replay가 아닙니다. 최대 50개 / 30일.</p><button onClick={()=>{void clearHistory().then(()=>setRecords([])).catch(()=>setError('기록 삭제 실패'));}}>이 브라우저 기록 지우기</button>{error&&<p role="alert">{error}</p>}{!records&&!error&&<p>기록 읽는 중</p>}{records?.length===0&&<p>저장된 시뮬레이션이 없습니다.</p>}<div className="status-detail-grid">{records?.map(r=><article className="status-card" key={r.id}><h2>{r.title}</h2><p>simulation · {r.completion} {r.seekUsed?'· 타임라인 탐색 포함':''}</p><p>{new Date(r.startedAt).toLocaleString('ko-KR')} → {new Date(r.endedAt).toLocaleString('ko-KR')}</p><p className="status-small">{r.completion==='unfinished'?'종료 미확인: 다른 탭에서 진행 중이거나 저장되지 않은 중단입니다.':r.completion==='interrupted'?'중단 시각은 마지막 저장 시각이며 정확한 종료 시각이 아닙니다.':'브라우저 시연 종료 기록'} · 실제 자동복구 증거 아님</p></article>)}</div></main>;
}
