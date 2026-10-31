import type { DataSourceKind, Narration as NarrationData } from '../domain/types';
import type { ScenarioInfo } from '../data/source';
import styles from './app.module.css';

/** Plain-language caption for visitors. Live data has none; then the region explains why. */
export function Narration({ narration, scenario, source = 'demo' }: { source?: DataSourceKind; narration: NarrationData | undefined; scenario: ScenarioInfo | undefined }) {
  return (
    <section className={styles.narration} aria-live="polite" aria-atomic="true">
      {scenario ? <p className={styles.narrationScenario}>{scenario.summary}</p> : null}
      {narration ? (
        <>
          <h2 key={narration.title} className={styles.narrationTitle}>
            {narration.title}
          </h2>
          <p className={styles.narrationBody}>{narration.body}</p>
        </>
      ) : (
        <>
          <h2 className={styles.narrationTitle}>{source === 'replay' ? '과거 ObserveOnly 실증' : '실시간 상태'}</h2>
          <p className={styles.narrationBody}>{source === 'replay' ? '장애 분류·후보 선택의 보존 기록입니다. 실제 복구 조치는 수행하지 않았습니다.' : '표시된 관측 시각의 클러스터 기록입니다. 아래 단계와 Incident 기록을 확인하세요.'}</p>
        </>
      )}
    </section>
  );
}
