export type Section = 'overview' | 'runtime' | 'engineering' | 'evidence';
export type ViewMode = 'live' | 'replay' | 'exhibition';

export interface EvidenceSource { archive: string; path: string; sha256: string; href?: string; downloadSha256?: string; transformation?: string }
export interface EvidenceEvent {
  id: string; title: string; at: string | null; status: string; summary: string;
  facts: Record<string, string | number | boolean | null>;
  sources: EvidenceSource[];
}
export interface EvidenceRun {
  id: string; title: string; track: 'runtime' | 'engineering'; scope: string; status: string;
  binding: { incidentUID: string | null; runID: string | null; epoch: string | null; namespace: string; policyUID: string | null; deploymentUID: string | null; podUIDs: string[] };
  outcome: { runtime: string; engineering: string | null; serviceRecovered: boolean; patchedImageDeployed: boolean; remoteCI: string | null; prURL: string | null; prNumber: number | null; commitSHA: string | null };
  events: EvidenceEvent[];
  candidates?: Array<Record<string, unknown>>;
  limitations: string[];
}
export interface EvidenceCatalog {
  schema: string; mode: 'historical';
  archives: Array<{id: string; file: string; sha256: string; crc: string | null; manifestChecks: unknown}>;
  runs: EvidenceRun[];
}

export function route(search: string): { mode: ViewMode; section: Section; run: string } {
  const params = new URLSearchParams(search);
  const source = params.get('source');
  const section = params.get('view');
  return {
    mode: source === 'replay' ? 'replay' : source === 'exhibition' ? 'exhibition' : 'live',
    section: section === 'runtime' || section === 'engineering' || section === 'evidence' ? section : 'overview',
    run: params.get('run') === 'demo-b' ? 'demo-b' : 'demo-a',
  };
}

export function eventIndex(value: number, length: number): number {
  return Math.max(0, Math.min(Math.max(0, length - 1), Number.isFinite(value) ? Math.floor(value) : 0));
}

export function displayValue(value: unknown): string {
  if (value === null || value === undefined || value === '') return '미확인';
  if (typeof value === 'boolean') return value ? '예' : '아니요';
  return typeof value === 'object' ? JSON.stringify(value) : String(value);
}

export function recordedAt(value: string | null): string {
  if (!value || !Number.isFinite(Date.parse(value))) return '원본에 시각 없음';
  return new Intl.DateTimeFormat('ko-KR', { timeZone: 'Asia/Seoul', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false }).format(new Date(value)) + ' KST';
}

const factLabels: Record<string, string> = {
  podUID: 'Pod UID', oldPodUID: '기존 Pod UID', newPodUID: '새 Pod UID', readyPodUID: 'Ready Endpoint Pod UID', node: '실제 Node',
  incidentUID: 'Incident UID', actionID: 'Controller action ID', reason: '판정 사유', selectedNode: '선택된 Node', candidateCount: '평가 후보 수',
  deploymentUID: 'Deployment UID', beforeTemplateHash: '변경 전 Template SHA-256', afterTemplateHash: '변경 후 Template SHA-256',
  outsideTemplateUnchanged: 'Template 외 spec 보존', architecture: 'CPU 아키텍처', lastObservedAt: '마지막 관측 시각', stableSamples: '연속 정상 HTTP 응답',
  stableElapsedMS: '서비스 안정화 시간', policyEnabled: 'Policy 활성화', generation: 'Policy generation', closedThroughGeneration: '실행 fence 종료 generation',
  observerCleanup: 'Observer 정리', errorCode: '실제 오류 코드', httpStatus: 'HTTP 상태', attempts: '실제 Runtime 조치 횟수',
  finishExitCode: 'Finish 종료 코드', bundleSHA256: 'Evidence Bundle SHA-256', runID: 'Runtime run ID', epoch: 'Runtime epoch',
  model: '실제 실행 모델', reasoningEffort: '추론 수준', auth: '인증 방식', requests: '실제 모델 호출 수', engineeringJobID: 'Engineering 작업 ID',
  isolation: 'Runner 격리', providerCredentialsInSandbox: 'Sandbox 내 모델 자격증명', changedFiles: '수정 파일', patchSHA256: 'Patch SHA-256',
  baselineDefectReproduced: '수정 전 결함 재현', checksPassed: '독립 검증 통과', checksTotal: '전체 검증 수', candidateSourceSHA256: '후보 소스 SHA-256',
  nativeARM64: '실제 ARM64 실행', prNumber: 'PR 번호', prURL: 'PR URL', commitSHA: '수정 commit SHA', draft: 'Draft 상태', merged: 'Merge 완료',
  deployed: '수정 이미지 배포', remoteCI: '원격 CI', prObservedAt: 'PR 상태 관측 시각', outageCorroborated: '장애 교차 확인',
};
export const factLabel = (key: string) => factLabels[key] ?? key;
export const factValue = (key: string, value: unknown) => key === 'stableElapsedMS' && typeof value === 'number' ? `${(value / 1000).toFixed(2)}초 (${value} ms)` : displayValue(value);

export function safePrURL(value: string | null): string | undefined {
  return value && /^https:\/\/github\.com\/SleepyMiny\/OpsLoop\/pull\/[1-9]\d*$/.test(value) ? value : undefined;
}
