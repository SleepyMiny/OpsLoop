import { useEffect, useRef } from 'react';
import { REALITY, REALITY_AS_OF, type RealityItem } from './realityStatus';
import styles from './app.module.css';

/** Honest disclosure of what exists today versus what this screen simulates. */
export function RealityDialog({
  open,
  onClose,
  source,
}: {
  open: boolean;
  onClose: () => void;
  source: 'demo' | 'live' | 'replay';
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    if (open && !dialog.open) dialog.showModal();
    if (!open && dialog.open) dialog.close();
  }, [open]);

  return (
    <dialog ref={ref} className={styles.dialog} onClose={onClose} aria-labelledby="reality-title">
      <div className={styles.dialogHead}>
        <h2 id="reality-title">무엇이 실제인가요?</h2>
        <button type="button" className={styles.toolButton} onClick={onClose}>
          닫기
        </button>
      </div>
      <p className={styles.dialogLead}>
        {source === 'demo'
          ? '지금 화면은 Demo Mode입니다. Runtime·Engineering 동작은 설계 문서(architecture v0.1)의 감지·분류·후보 평가 규칙과 점수 공식을 시뮬레이션 지표에 적용한 결과입니다.'
          : source === 'replay' ? '지금 화면은 과거 실환경 증거의 보존 시점만 재생합니다. 현재 클러스터 상태나 자동 복구 실증이 아닙니다.' : '지금 화면은 BFF가 조회한 현재 데이터입니다. 현재 Incident가 없으면 빈 상태를 표시합니다.'}{' '}
        저장소 기준 {REALITY_AS_OF} 상태는 다음과 같습니다.
      </p>
      <div className={styles.realityGrid}>
        <RealityColumn title="구축됨" state="ok" items={REALITY.built} />
        <RealityColumn title="남은 검증 및 배포" state="caution" items={REALITY.prepared} />
        <RealityColumn title="데이터 출처 구분" state="act" items={REALITY.simulated} />
      </div>
    </dialog>
  );
}

function RealityColumn({ title, state, items }: { title: string; state: 'ok' | 'caution' | 'act'; items: RealityItem[] }) {
  return (
    <section className={styles.realityColumn} data-state={state}>
      <h3>{title}</h3>
      <ul>
        {items.map((item) => (
          <li key={item.title}>
            <strong>{item.title}</strong>
            <span>{item.detail}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}
