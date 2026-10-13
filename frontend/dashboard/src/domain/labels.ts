import type {
  ActionType,
  EngineeringPhase,
  FailureClass,
  NodeSite,
  RejectReason,
  RuntimePhase,
  VerificationStage,
} from './types';

/**
 * Visitor-facing Korean wording for contract enums. Raw enum values stay visible
 * in the UI next to these labels so engineers can map them back to the CRD.
 */

export const REJECT_REASON_LABEL: Record<RejectReason, { short: string; detail: string }> = {
  NODE_NOT_READY: { short: '노드 응답 없음', detail: 'Kubernetes가 Ready가 아니라고 보고했습니다.' },
  UNSCHEDULABLE: { short: '배치 금지', detail: '노드가 cordon되어 새 Pod를 받지 않습니다.' },
  ARCH_MISMATCH: { short: '아키텍처 불일치', detail: '이미지 index에 이 노드의 플랫폼 manifest가 없습니다.' },
  UNTOLERATED_TAINT: { short: '허용되지 않은 taint', detail: 'workload가 노드의 taint를 허용하지 않습니다.' },
  MEMORY_PRESSURE: { short: '메모리 압박', detail: '메모리 사용률이 정책 상한을 넘었거나 MemoryPressure 상태입니다.' },
  CPU_PRESSURE: { short: 'CPU 과부하', detail: 'CPU 사용률이 정책 상한(85%)을 넘었습니다.' },
  DISK_PRESSURE: { short: '디스크 압박', detail: 'DiskPressure 상태입니다.' },
  PID_PRESSURE: { short: 'PID 압박', detail: 'PIDPressure 상태입니다.' },
  INSUFFICIENT_REQUESTED_CPU: { short: 'CPU 요청 여유 부족', detail: '새 Pod와 예약 여유를 더하면 할당 가능 CPU를 넘습니다.' },
  INSUFFICIENT_REQUESTED_MEMORY: { short: '메모리 요청 여유 부족', detail: '새 Pod와 예약 여유를 더하면 할당 가능 메모리를 넘습니다.' },
  INSUFFICIENT_AVAILABLE_MEMORY: { short: '가용 메모리 부족', detail: '최소 여유 512 MiB와 시작 예산 256 MiB를 함께 만족하지 못합니다.' },
  METRICS_STALE: { short: '관측값 오래됨', detail: '표본이 30초보다 오래되어 판단 근거로 쓰지 않습니다.' },
  METRICS_MISSING: { short: '관측값 없음', detail: '값이 없거나 NaN입니다. 0%로 간주하지 않습니다.' },
  COOLDOWN: { short: 'cooldown 중', detail: '최근 이 노드에서 조치가 실패해 120초 동안 다시 고르지 않습니다.' },
  UNSUPPORTED_CONSTRAINT: { short: '지원하지 않는 제약', detail: '대략 계산해 통과시키지 않고 명시적으로 거부합니다.' },
};

export function rejectReasonLabel(reason: RejectReason): string {
  return REJECT_REASON_LABEL[reason].short;
}

export const RUNTIME_PHASE_LABEL: Record<RuntimePhase, string> = {
  MONITORING: '감시 중',
  DETECTED: '장애 감지',
  CLASSIFIED: '규칙 분류',
  COLLECTING_CANDIDATES: '근거 수집',
  FILTERING: '후보 거르기',
  WAITING: '대기',
  SCORING: '점수 계산',
  TARGET_SELECTED: '대상 선택',
  EXECUTING: '배치 변경',
  VERIFYING: 'HTTP 검증',
  RECOVERED: '복구 완료',
  COOLDOWN: 'cooldown',
  RUNTIME_RECOVERY_EXHAUSTED: 'Runtime 예산 소진',
  EVIDENCE_FINALIZING: '증거 확정',
  ENGINEERING_HANDOFF: 'Engineering 인계',
  FAILED: '운영 실패',
  ABORTED: '중단됨',
  SUPERSEDED: '외부 변경으로 중단',
  UNSUPPORTED_WORKLOAD: '지원하지 않는 workload',
  MANUAL_REQUIRED: '수동 판단 필요',
};

export const ENGINEERING_PHASE_LABEL: Record<EngineeringPhase, string> = {
  ENGINEERING_QUEUED: '대기열',
  VALIDATING_INPUT: '입력 검증',
  ANALYZING: '원인 분석',
  PATCHING: '수정 작성',
  BUILDING: 'Build',
  TESTING: 'Test',
  VERIFYING: '재현 검증',
  INDEPENDENT_REVIEW: '독립 리뷰',
  PR_READY: 'PR 준비',
  PR_CREATED: 'PR 생성',
  FEEDBACK: 'Feedback',
  ENGINEERING_FAILED: 'Engineering 실패',
  INVALID_BUNDLE: '잘못된 Bundle',
  BLOCKED_EVIDENCE: '증거 부족',
  BLOCKED_ENVIRONMENT: '환경 차단',
  BLOCKED_NATIVE_VALIDATION: 'native 검증 불가',
  STALE_BASE: 'base 변경됨',
  TIMED_OUT: '시간 초과',
  CANCELLED: '취소됨',
  PUBLISH_RETRY_PENDING: '게시 재시도 대기',
};

export const FAILURE_CLASS_LABEL: Record<FailureClass, string> = {
  INFRASTRUCTURE_FAILURE: '인프라 장애',
  RESOURCE_FAILURE: '자원 장애',
  CONFIGURATION_FAILURE: '설정 장애',
  APPLICATION_FAILURE: 'Application 장애',
  UNKNOWN: '판단 불가',
};

export const STAGE_LABEL: Record<VerificationStage, string> = {
  build: 'Build',
  unit: 'Unit test',
  integration: 'Integration',
  'regression-fixture': 'Regression fixture',
  'multi-arch-smoke': 'Multi-Arch smoke',
  'independent-review': '독립 리뷰',
};

export const SITE_LABEL: Record<NodeSite, string> = {
  'laptop-vm': '노트북 위 Ubuntu VM',
  'physical-server': 'Ubuntu 물리 서버',
  'azure-vm': 'Azure Linux VM',
};

export function actionTypeLabel(action: ActionType | null): string {
  if (action === 'restart') return '같은 노드에서 다시 시작합니다(restart).';
  if (action === 'relocation') return '다른 노드로 옮깁니다(relocation).';
  return '';
}

/** The ordered happy path of the Runtime state machine, for the pipeline strip. */
export const RUNTIME_MAIN_PATH: RuntimePhase[] = [
  'DETECTED',
  'CLASSIFIED',
  'COLLECTING_CANDIDATES',
  'FILTERING',
  'SCORING',
  'TARGET_SELECTED',
  'EXECUTING',
  'VERIFYING',
  'RECOVERED',
];

export const ENGINEERING_MAIN_PATH: EngineeringPhase[] = [
  'ENGINEERING_QUEUED',
  'VALIDATING_INPUT',
  'ANALYZING',
  'PATCHING',
  'BUILDING',
  'TESTING',
  'VERIFYING',
  'INDEPENDENT_REVIEW',
  'PR_READY',
  'PR_CREATED',
];
