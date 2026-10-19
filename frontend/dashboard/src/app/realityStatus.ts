/** Verified implementation and retained live evidence as of 2026-10-20.
 * This inventory is not a live health assertion; Live snapshots carry current state. */
export const REALITY_AS_OF = '2026-10-20';
export interface RealityItem { title: string; detail: string; }
export const REALITY: { built: RealityItem[]; prepared: RealityItem[]; simulated: RealityItem[] } = {
  built: [
    {title: 'Hybrid 3-node k3s', detail: 'Control VM·물리 Ubuntu AMD64·Azure ARM64 Worker, Tailscale 연결. 현재 Ready 여부는 Live에서 확인합니다.'},
    {title: 'Controller·Demo API·관측', detail: 'Fence v2 ObserveOnly Controller, Digest 고정 멀티 아키텍처 이미지, 내부 Service·Prometheus 실제 scrape 검증 완료.'},
    {title: '두 장애 실환경 검증', detail: 'IMAGE_PULL 분류·MANUAL_REQUIRED·Circuit, NODE_NOT_READY 후보 평가·OBSERVED·운영자 복원 PASS. attempts=0이며 자동 복구 실증이 아닙니다.'},
    {title: 'Dashboard 데이터 연결', detail: '읽기 전용 BFF·HttpDataSource·검증된 증거 Replay 구현. 클러스터 운영 배포는 별도 승인 대상입니다.'},
    {title: '게시 파이프라인', detail: 'verify/image/dashboard CI, main 보호·수동 승인 Gate·Azure OIDC·ACR 게시 검증 완료. 게시와 배포는 분리합니다.'},
  ],
  prepared: [
    {title: 'Enforce 자동 복구', detail: '코드·안전성 테스트와 실환경 자동 복구는 별개입니다. Enforce는 차단 상태이며 Node–Deployment 객체 간 원자성도 보장하지 않습니다.'},
    {title: 'Live 관측 범위', detail: '현재 BFF는 HTTP 복구 probe와 Registry platform index를 수집하지 않습니다. 결측값을 보완하지 않으며 후보 미리보기는 fail-closed입니다.'},
    {title: 'Engineering 자동화', detail: 'Runner·자동 수정·PR 생성은 미구현입니다. Dashboard가 이를 실행하지 않습니다.'},
  ],
  simulated: [
    {title: 'Demo Mode', detail: '가상 시간·지표·장애·복구·Engineering 장면 전체가 시뮬레이션입니다. 실제 자동 복구로 해석하면 안 됩니다.'},
    {title: 'Evidence Replay', detail: '보존된 실측 시점만 표시합니다. 과거 전체 Node snapshot·중간 상태·HTTP probe를 만들어 넣지 않습니다.'},
  ],
};
