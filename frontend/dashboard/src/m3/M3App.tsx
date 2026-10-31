import { useEffect, useMemo, useState } from 'react';
import { App } from '../app/App';
import { toggleFullscreen, useTheme } from '../app/hooks';
import { HttpDataSource } from '../data/live/httpSource';
import { ExhibitionConsole } from './ExhibitionConsole';
import { getJSON, parseCapabilities, type Capabilities, type eventFocus } from './exhibition';
import catalogData from '../data/m3/evidence.json';
import { displayValue, eventIndex, factLabel, factValue, recordedAt, route, safePrURL, type EvidenceCatalog, type EvidenceEvent, type EvidenceRun, type Section } from './model';
import styles from './m3.module.css';

const catalog = catalogData as unknown as EvidenceCatalog;
const sections: Array<[Section, string, string]> = [
  ['overview', 'Overview', '전체 현황'], ['runtime', 'Runtime', '인프라 복구'],
  ['engineering', 'Engineering', '코드 수정'], ['evidence', 'Evidence', '근거 · 재생'],
];

export function M3App() {
  const initial = useMemo(() => route(window.location.search), []);
  const [section, setSection] = useState(initial.section);
  const [runId, setRunId] = useState(initial.section === 'engineering' ? 'demo-b' : initial.run);
  const [frame, setFrame] = useState(0);
  const [playing, setPlaying] = useState(false);
  const [theme, setTheme] = useTheme();
  const [capabilities, setCapabilities] = useState<Capabilities | null>(null);
  const [capabilityError, setCapabilityError] = useState(false);
  const [focus, setFocus] = useState<ReturnType<typeof eventFocus>>(undefined);
  const [history, setHistory] = useState(false);
  const [scope, setScope] = useState(new URLSearchParams(window.location.search).get('scope') === 'b' ? 'b' : 'a');
  const mode = initial.mode === 'live' && capabilities?.mode === 'replay' ? 'replay' : initial.mode;
  useEffect(() => {
    const controller = new AbortController();
    getJSON('/api/exhibition/capabilities', controller.signal).then(parseCapabilities).then(setCapabilities).catch(() => { if (!controller.signal.aborted) setCapabilityError(true); });
    return () => controller.abort();
  }, []);
  const run = catalog.runs.find((item) => item.id === runId) ?? catalog.runs[0];
  // A Replay entry never constructs or starts a live source.
  const source = useMemo(() => mode === 'live' && capabilities?.live ? new HttpDataSource({ baseUrl: capabilities.observationConfigured === false && capabilities.lastObservationAvailable !== true ? undefined : import.meta.env.VITE_OPSLOOP_API_BASE ?? (scope === 'b' ? '/api/b' : '/api/a') }) : null, [mode, capabilities?.live, capabilities?.observationConfigured, capabilities?.lastObservationAvailable, scope]);
  const selected = run?.events[eventIndex(frame, run.events.length)];

  useEffect(() => {
    if (!playing || !run) return;
    if (frame >= run.events.length - 1) { setPlaying(false); return; }
    const timer = window.setTimeout(() => setFrame((index) => index + 1), 5000);
    return () => window.clearTimeout(timer);
  }, [playing, frame, run]);

  function selectRun(id: string) { setRunId(id); setFrame(0); setPlaying(false); }
  function navigate(next: Section) {
    setSection(next);
    if (next === 'runtime') selectRun('demo-a');
    if (next === 'engineering') selectRun('demo-b');
  }

  return <div className={styles.shell}>
    <a href="#m3-content" className={styles.skip}>본문으로 건너뛰기</a>
    <header className={styles.header}>
      <a className={styles.brand} href="?source=live"><span className={styles.logo} aria-hidden="true">↻</span><span><b>OpsLoop</b><small>DEVOPS / SRE CONTROL ROOM</small></span></a>
      <nav className={styles.modes} aria-label="관측 모드">
        <a href={capabilities?.live ? "?source=live" : undefined} aria-disabled={!capabilities?.live} aria-current={mode === 'live' ? 'page' : undefined}>Live <small>{capabilities?.mode === 'replay' ? '이 서버에서 사용 불가' : '현재 실측'}</small></a>
        <a href="?source=replay" aria-current={mode === 'replay' ? 'page' : undefined}>Replay <small>봉인 기록</small></a>
        <a href="?source=exhibition" aria-current={mode === 'exhibition' ? 'page' : undefined}>Exhibition <small>전시 안내</small></a>
      </nav>
      <div className={styles.tools}><button onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')} aria-label="화면 테마 전환">테마</button><button onClick={toggleFullscreen}>전체 화면</button></div>
    </header>
    <div className={styles.sourceBanner} data-live={mode === 'live'} role="note">
      <span className={styles.dot} />
      <strong>{mode === 'live' ? 'LIVE · 실측 / 승인된 실행' : mode === 'replay' ? 'REPLAY · 과거 실증 기록' : 'EXHIBITION · 현재 표시: 기록 Replay'}</strong>
      <span>{mode === 'live' ? 'Kubernetes / Prometheus 관측 · 연결 상태와 관측 시각을 확인하세요.' : 'Demo A · B 원본 ZIP에서 검증한 기록입니다. 현재 클러스터 상태가 아닙니다.'}</span>
      <span className={styles.boundary}>모드 선택은 장애 주입·Enforce를 실행하지 않습니다</span>
    </div>
    {mode === 'live' ? <div id="m3-content">
      {capabilities ? <ExhibitionConsole capabilities={capabilities} onFocus={setFocus} onHistory={setHistory} onTrack={setScope} /> : <section className={styles.content} role="status"><h1>Live 관제 연결</h1><p>{capabilityError ? '서버 기능을 확인하지 못했습니다. 실험 대상 서비스의 장애 판정과는 별개입니다.' : '서버의 관측·실행 기능을 확인하고 있습니다.'}</p><a href="?source=replay">지난 실행 Replay 열기</a></section>}
      {source && !history ? <App source={source} kiosk={false} integrated eventFocus={focus} /> : history ? <p className={styles.content}>완료된 run의 기록을 재생 중입니다. 현재 클러스터 관측과 혼합하지 않습니다.</p> : capabilities ? <p className={styles.content}>관측 API가 설정되지 않았습니다. 실행 원장과 서비스 상태는 별도로 확인합니다.</p> : null}
    </div> : <>
      <nav className={styles.sectionNav} aria-label="관제 화면">
        {sections.map(([key, label, sub]) => <button key={key} aria-current={section === key ? 'page' : undefined} onClick={() => navigate(key)}><strong>{label}</strong><span>{sub}</span></button>)}
      </nav>
      <main id="m3-content" className={styles.content}>
        {section === 'overview' ? <Overview runs={catalog.runs} navigate={navigate} liveAvailable={capabilities?.live ?? false} /> : run ? <>
          <div className={styles.pageHeading}><div><p className={styles.eyebrow}>SEALED EXECUTION / {run.track.toUpperCase()}</p><h1>{section === 'evidence' ? '근거를 따라가는 기록 재생' : run.track === 'runtime' ? '자원을 평가하고, 서비스를 복구합니다' : 'Runtime의 한계에서, 검증된 수정 PR까지'}</h1><p>{run.title}</p></div><span className={styles.historyPill}>과거 실행 · {run.status}</span></div>
          <div className={styles.runSelector} role="group" aria-label="실증 기록 선택">{catalog.runs.map((item) => <button key={item.id} aria-pressed={item.id === run.id} onClick={() => selectRun(item.id)}>{item.id === 'demo-a' ? 'Demo A · Runtime' : 'Demo B · Engineering'}</button>)}<span>동일 이름이어도 UID / run binding이 다르면 별도 기록</span></div>
          {run.track === 'runtime' ? <RuntimePlacement run={run} frame={frame} /> : null}
          <div className={styles.replayGrid}>
            <section className={styles.timeline} aria-label="실행 이벤트 타임라인">
              <div className={styles.panelHeading}><h2>실제 실행 순서</h2><span>{frame + 1} / {run.events.length}</span></div>
              <ol>{run.events.map((event, index) => <li key={event.id} data-reached={index <= frame}><button aria-current={index === frame ? 'step' : undefined} onClick={() => { setFrame(index); setPlaying(false); }}><span className={styles.stepNumber}>{String(index + 1).padStart(2, '0')}</span><span><strong>{event.title}</strong><small>{recordedAt(event.at)}</small></span></button></li>)}</ol>
            </section>
            <div className={styles.detailColumn}>
              {selected ? <EventDetail event={selected} /> : <section className={styles.panel}><h2>재생 이벤트 없음</h2><p>검증된 이벤트가 없어 성공 화면을 구성하지 않습니다.</p></section>}
              <div className={styles.playback} aria-label="기록 재생 조작"><button onClick={() => { setFrame((n) => eventIndex(n - 1, run.events.length)); setPlaying(false); }} disabled={frame === 0}>← 이전</button><button className={styles.primary} onClick={() => { if (frame >= run.events.length - 1) setFrame(0); setPlaying((p) => !p); }} disabled={!run.events.length}>{playing ? '일시 정지' : '기록 재생'}</button><button onClick={() => { setFrame((n) => eventIndex(n + 1, run.events.length)); setPlaying(false); }} disabled={frame >= run.events.length - 1}>다음 →</button><label>재생 위치 <input type="range" min="0" max={Math.max(0, run.events.length - 1)} value={frame} onChange={(e) => { setFrame(Number(e.target.value)); setPlaying(false); }} /></label><small>설명용 5초 / 단계 · 실제 소요 시간은 원본 시각 기준</small></div>
              <Outcome run={run} />
            </div>
          </div>
          {run.candidates?.length ? <CandidateEvidence candidates={run.candidates} /> : null}
          <Binding run={run} />
          <section className={styles.panel}><h2>이 실증의 검증 범위</h2><p>{run.scope}</p><ul className={styles.limitations}>{run.limitations.map((item) => <li key={item}>{item}</li>)}</ul></section>
          {section === 'evidence' ? <ArchiveIndex /> : null}
        </> : <section className={styles.panel}><h1>검증된 기록 없음</h1><p>원본 ZIP 검증 후 Replay 자료를 생성하세요.</p></section>}
      </main>
    </>}
    <footer className={styles.footer}><span>OpsLoop M3 · Hybrid Kubernetes</span><span>실측 → 근거 → 승인된 조치 · PR 생성과 운영 배포는 별도 단계</span></footer>
  </div>;
}

function Overview({ runs, navigate, liveAvailable }: { runs: EvidenceRun[]; navigate: (section: Section) => void; liveAvailable: boolean }) {
  return <>
    <div className={styles.pageHeading}><div><p className={styles.eyebrow}>INTEGRATED DEVOPS / SRE PLATFORM</p><h1>두 복구 루프, 하나의 증거 흐름.</h1><p>인프라 장애는 Runtime에서, 코드 결함은 독립 검증을 거친 Engineering에서.</p></div><span className={styles.historyPill}>기록 Replay · 오프라인 사용 가능</span></div>
    <section className={styles.overviewNotice}><div><strong>현재 시스템 상태는 Live에서 확인합니다</strong><p>이 화면은 봉인된 실증 결과입니다. 현재 Node Ready, 서비스 건강도, Policy·Guard 상태를 과거 결과로 대신하지 않습니다.</p></div><a href={liveAvailable ? "?source=live" : undefined} aria-disabled={!liveAvailable}>{liveAvailable ? 'Live 관제 열기 ↗' : 'Live 관측 서버 연결 필요'}</a></section>
    <div className={styles.runCards}>{runs.map((run) => <article className={styles.runCard} key={run.id} data-track={run.track}>
      <div className={styles.cardTop}><span className={styles.eyebrow}>{run.id.toUpperCase()} / {run.track.toUpperCase()}</span><span className={styles.status}>{run.status} · 해당 범위</span></div>
      <h2>{run.track === 'runtime' ? 'Runtime Self-Healing' : 'Engineering Self-Healing'}</h2>
      <p>{run.title}</p><div className={styles.cardResult}><strong>{run.track === 'runtime' ? run.outcome.runtime : run.outcome.engineering}</strong><span>{run.track === 'runtime' ? '과거 실행의 서비스 회복 결과' : '수정 PR 제출 · 실제 수정 이미지 배포 아님'}</span></div>
      <p className={styles.namespace}>Namespace <code>{run.binding.namespace}</code></p>
      <button className={styles.primary} onClick={() => navigate(run.track === 'runtime' ? 'runtime' : 'engineering')}>실제 실행 기록 보기 →</button>
    </article>)}</div>
    <section className={styles.panel}><div className={styles.panelHeading}><h2>하이브리드 환경과 복구 경계</h2><span>구성 설명 · 실시간 상태 표시 아님</span></div><div className={styles.topology}><div><small>CONTROL PLANE</small><strong>archgram의 VM</strong><span>Kubernetes API · 정책 / Incident</span></div><span aria-hidden="true">↔</span><div><small>PHYSICAL WORKER</small><strong>Ubuntu · AMD64</strong><span>Demo A readiness 장애 출발점</span></div><span aria-hidden="true">→</span><div><small>AZURE WORKER</small><strong>ARM64</strong><span>자원 평가 후 실제 재배치 대상</span></div></div><p className={styles.warning}>archgram 전원 차단 시험은 미실증입니다. API 중단 시 원격 strong stop 확인에는 기존 한계가 있습니다.</p></section>
    <section className={styles.panel}><div className={styles.panelHeading}><h2>전시 진행</h2><span>5–8분</span></div><ol className={styles.exhibitionSteps}><li>Live에서 실제 노드와 데이터 시각 확인</li><li>Demo A에서 후보 평가 → 재배치 → 서비스 회복</li><li>Demo B에서 Runtime 소진 → Luna 수정 → 독립 검증 → PR</li><li>Evidence에서 UID·원본 해시와 미실증 범위 확인</li></ol><p>연결이 끊기면 Replay를 명시적으로 선택하세요. 봉인된 기록은 API 연결 없이 사용할 수 있습니다.</p><button onClick={() => navigate('evidence')}>Evidence / Replay 열기 →</button></section>
  </>;
}

function EventDetail({ event }: { event: EvidenceEvent }) {
  return <section className={styles.eventDetail} aria-label="선택한 이벤트" aria-live="polite"><div className={styles.panelHeading}><span className={styles.eyebrow}>RECORDED EVENT</span><span className={styles.status}>{event.status}</span></div><h2>{event.title}</h2><time dateTime={event.at ?? undefined}>{recordedAt(event.at)}</time><p className={styles.eventSummary}>{event.summary}</p><dl className={styles.facts}>{Object.entries(event.facts).map(([key, value]) => <div key={key}><dt>{factLabel(key)}</dt><dd>{factValue(key, value)}</dd></div>)}</dl><details className={styles.provenance}><summary>근거 파일 · SHA-256 ({event.sources.length})</summary>{event.sources.map((source) => <div key={`${source.archive}:${source.path}`}><strong>{source.archive}</strong><a href={source.href?.startsWith('m3-evidence/') && !source.href.includes('..') ? source.href : undefined} target="_blank" rel="noopener noreferrer">{source.path} ↗</a><code>원본 SHA-256 {source.sha256}</code>{source.downloadSha256 ? <code>다운로드 SHA-256 {source.downloadSha256}</code> : null}{source.transformation && source.transformation !== 'none' ? <span>민감 연결 정보 제거 / 선별 사본 · 원본은 봉인 ZIP에서 확인</span> : null}</div>)}</details></section>;
}

function RuntimePlacement({ run, frame }: { run: EvidenceRun; frame: number }) {
  const fault = run.events.find((event) => event.id === 'fault');
  const placement = run.events.find((event) => event.id === 'placement');
  const placementIndex = run.events.findIndex((event) => event.id === 'placement');
  const endpointIndex = run.events.findIndex((event) => event.id === 'endpoint');
  const placed = placementIndex >= 0 && frame >= placementIndex;
  const endpoint = endpointIndex >= 0 && frame >= endpointIndex;
  return <section className={styles.placement} aria-label="기록 내 실제 Pod 배치 변화"><div><small>기존 Pod / readiness 장애 관측</small><strong>{displayValue(fault?.facts.node)}</strong><code>{displayValue(fault?.facts.podUID)}</code></div><span className={styles.placementArrow} data-confirmed={placed} aria-hidden="true">→</span><div data-confirmed={placed}><small>{placed ? '새 Pod / Scheduler 배치 확인' : '재배치 전 · 새 Pod 배치 아직 미도달'}</small><strong>{placed ? displayValue(placement?.facts.node) : '선택·배치 기록을 따라가세요'}</strong><code>{placed ? displayValue(placement?.facts.newPodUID) : '기록 재생 단계에 따라 표시'}</code></div><p>{endpoint ? '새 Pod UID로 Ready Endpoint 전환 확인' : 'Endpoint 전환 단계 이전'}<small>과거 실행 단계 · 현재 클러스터 아님</small></p></section>;
}

function Outcome({ run }: { run: EvidenceRun }) {
  const pr = safePrURL(run.outcome.prURL);
  return <section className={styles.panel}><div className={styles.panelHeading}><h2>이 기록의 최종 결과</h2><span>현재 재생 단계와 별도</span></div><dl className={styles.outcomes}><div><dt>Runtime</dt><dd>{run.outcome.runtime}</dd></div>{run.outcome.engineering ? <div><dt>Engineering</dt><dd>{run.outcome.engineering}</dd></div> : null}<div><dt>기록 내 서비스 복구</dt><dd>{run.outcome.serviceRecovered ? '확인됨' : '복구되지 않음'}</dd></div>{run.track === 'engineering' ? <><div><dt>수정 이미지 배포</dt><dd>{run.outcome.patchedImageDeployed ? '배포 기록 있음' : 'NOT_DEPLOYED'}</dd></div><div><dt>원격 CI (봉인 시점)</dt><dd>{run.outcome.remoteCI ?? '미확인'}</dd></div></> : null}</dl>{pr ? <a className={styles.prLink} href={pr} target="_blank" rel="noopener noreferrer">GitHub Draft PR #{run.outcome.prNumber} ↗<small>봉인 시점 Draft · 현재 상태는 GitHub에서 확인</small></a> : null}{run.outcome.commitSHA ? <p className={styles.commit}>수정 commit <code>{run.outcome.commitSHA}</code></p> : null}</section>;
}

function Binding({ run }: { run: EvidenceRun }) {
  const labels: Record<string, string> = { incidentUID: 'Incident UID', runID: 'Runtime run ID', epoch: 'Runtime epoch', namespace: 'Namespace', policyUID: 'Policy UID', deploymentUID: 'Deployment UID', podUIDs: 'Pod UID' };
  return <section className={styles.panel}><div className={styles.panelHeading}><h2>실행과 리소스의 연결</h2><span>이름이 아닌 UID + run binding</span></div><dl className={styles.binding}>{Object.entries(run.binding).map(([key, value]) => <div key={key}><dt>{labels[key] ?? key}</dt><dd><code>{Array.isArray(value) ? value.join(' → ') || '미확인' : displayValue(value)}</code></dd></div>)}</dl></section>;
}

function CandidateEvidence({ candidates }: { candidates: Array<Record<string, unknown>> }) {
  const keys = Array.from(new Set(candidates.flatMap((candidate) => Object.keys(candidate))));
  const cpu = (value: unknown) => typeof value === 'number' ? `${(value / 10).toFixed(1)}%` : '미확인';
  const bytes = (value: unknown) => typeof value === 'number' ? `${(value / 1024 ** 3).toFixed(2)} GiB` : '미확인';
  return <section className={styles.panel}><div className={styles.panelHeading}><h2>Controller의 실제 후보 평가</h2><span>기록 전체 요약 · 선택은 원본 Controller 판정</span></div><div className={styles.candidateCards}>{candidates.map((candidate, index) => <article key={index} data-selected={candidate.selected === true}><div className={styles.panelHeading}><h3>{displayValue(candidate.node)}</h3><span className={styles.status}>{candidate.selected === true ? '선택됨' : '비선택'}</span></div><p>{displayValue(candidate.platform)} · 적격 {displayValue(candidate.eligible)} · 점수 {displayValue(candidate.scoreMilli)} milli-points</p><dl className={styles.facts}><div><dt>CPU 사용률</dt><dd>{cpu(candidate.cpuUtilizationPermille)}</dd></div><div><dt>가용 메모리 / 전체</dt><dd>{bytes(candidate.memAvailableBytes)} / {bytes(candidate.memTotalBytes)}</dd></div><div><dt>CPU 요청 / 할당 가능</dt><dd>{displayValue(candidate.requestedCPUMilli)}m / {displayValue(candidate.allocatableCPUMilli)}m</dd></div><div><dt>메모리 요청</dt><dd>{bytes(candidate.requestedMemoryBytes)}</dd></div></dl></article>)}</div><details className={styles.provenance}><summary>평가 원본 수치 · 표본 시각 · Node UID</summary><div className={styles.tableScroll}><table><thead><tr>{keys.map((key) => <th key={key} scope="col">{key}</th>)}</tr></thead><tbody>{candidates.map((candidate, index) => <tr key={index}>{keys.map((key) => <td key={key}>{displayValue(candidate[key])}</td>)}</tr>)}</tbody></table></div></details></section>;
}

function ArchiveIndex() {
  return <section className={styles.panel}><div className={styles.panelHeading}><h2>봉인 원본 ZIP</h2><span>내보내기 시 원본 대조 · 원본 보존</span></div>{catalog.archives.map((archive) => <div className={styles.archive} key={archive.id}><strong>{archive.file}</strong><code>SHA-256 {archive.sha256}</code><span>CRC: {archive.crc === null ? 'PASS' : archive.crc}</span><details><summary>Manifest 검증 상세</summary><pre>{JSON.stringify(archive.manifestChecks, null, 2)}</pre></details></div>)}</section>;
}
