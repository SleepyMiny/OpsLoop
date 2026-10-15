/**
 * What is real versus simulated, shown to visitors on request. Sourced from
 * README.md and docs/implementation-plan.md as of the date below; update this
 * file (not the components) when the repository status changes.
 */
export const REALITY_AS_OF = '2026-10-13';

export interface RealityItem {
  title: string;
  detail: string;
}

export const REALITY: { built: RealityItem[]; prepared: RealityItem[]; simulated: RealityItem[] } = {
  built: [
    {
      title: '로컬 AMD64 2-Node k3s',
      detail: '노트북 VM(opsloop-control, Control Plane)과 물리 서버(sarmubuntuserver) Ready, k3s v1.35.9+k3s1, Tailscale 연결.',
    },
    {
      title: '관측 스택',
      detail: 'Prometheus·Grafana·node exporter·kube-state-metrics. 10월 12일 조회에서 Targets 17개 UP.',
    },
    {
      title: 'Azure Remote Backend',
      detail: 'Terraform 원격 State 초기화·연결 검증 완료.',
    },
  ],
  prepared: [
    {
      title: 'Azure ARM64 워커',
      detail: 'East Asia / Standard_B2pls_v2(2 vCPU, 4 GiB) Saved Plan 준비. Apply는 아직 승인·실행되지 않았습니다.',
    },
  ],
  simulated: [
    {
      title: 'Demo API와 Multi-Arch 이미지',
      detail: '구현 전입니다. 화면의 Pod·HTTP 응답·이미지 digest는 시뮬레이션입니다.',
    },
    {
      title: 'OpsLoop Controller와 CRD',
      detail: 'RecoveryPolicy·RecoveryIncident, 감지·분류·후보 평가·배치는 구현 전입니다. 화면의 판단은 설계 문서의 규칙과 점수 공식을 시뮬레이션 지표에 적용한 결과입니다.',
    },
    {
      title: 'Engineering Runner와 PR',
      detail: '구현 전입니다. 실제 Codex 실행이나 GitHub PR 생성은 일어나지 않습니다.',
    },
    {
      title: '시간과 수치',
      detail: '모든 시간·사용률·점수는 시뮬레이션 값이며 실측 결과가 아닙니다.',
    },
  ],
};
