import { useEffect, useRef } from 'react';
import { REALITY, REALITY_AS_OF, type RealityItem } from './realityStatus';
import styles from './app.module.css';

/** Honest disclosure of what exists today versus what this screen simulates. */
export function RealityDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
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
        이 화면의 Runtime·Engineering 동작은 설계 문서(architecture v0.1)의 규칙을 그대로 따르는 시뮬레이션입니다.
        저장소 기준 {REALITY_AS_OF} 상태는 다음과 같습니다.
      </p>
      <div className={styles.realityGrid}>
        <RealityColumn title="구축됨" state="ok" items={REALITY.built} />
        <RealityColumn title="준비됨, 아직 만들지 않음" state="caution" items={REALITY.prepared} />
        <RealityColumn title="이 화면에서 시뮬레이션" state="act" items={REALITY.simulated} />
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
