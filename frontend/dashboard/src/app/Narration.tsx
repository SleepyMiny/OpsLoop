import type { Narration as NarrationData } from '../domain/types';
import type { ScenarioInfo } from '../data/source';
import styles from './app.module.css';

/** Plain-language caption for visitors. Live data has none; then the region explains why. */
export function Narration({ narration, scenario }: { narration: NarrationData | undefined; scenario: ScenarioInfo | undefined }) {
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
          <h2 className={styles.narrationTitle}>실시간 상태</h2>
          <p className={styles.narrationBody}>Live 데이터에는 해설이 없습니다. 아래 단계와 Incident 기록을 확인하세요.</p>
        </>
      )}
    </section>
  );
}
