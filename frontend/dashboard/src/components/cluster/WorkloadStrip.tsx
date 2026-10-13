import type { Arch, WorkloadStatus } from '../../domain/types';
import { shortHash } from '../shared/format';
import { EnumCode, Lamp } from '../shared/primitives';
import type { QuarantineView } from './clusterModel';
import styles from './cluster.module.css';

/** Deployment spec as OpsLoop changes it: replicas, nodeSelector, and the one multi-arch image index. */
export function WorkloadStrip({
  workload,
  servingArch,
  quarantine,
}: {
  workload: WorkloadStatus;
  servingArch: Arch | null;
  quarantine: QuarantineView | null;
}) {
  return (
    <div className={styles.workload} role="group" aria-label="Deployment">
      <span className={styles.field}>
        <span className={styles.fieldLabel}>Deployment</span>
        <EnumCode>
          {workload.namespace}/{workload.name}
        </EnumCode>
      </span>

      {quarantine ? (
        <span className={styles.quarantine} data-tone={quarantine.tone}>
          <Lamp state={quarantine.tone} label="격리" />
          <strong>{quarantine.label}</strong>
          <EnumCode>{quarantine.state}</EnumCode>
        </span>
      ) : (
        <span className={styles.field}>
          <span className={styles.fieldLabel}>replicas</span>
          <span className={styles.fieldNumber}>{workload.replicas}</span>
        </span>
      )}

      <span className={styles.field}>
        <span className={styles.fieldLabel}>nodeSelector</span>
        {workload.nodeSelectorHostname ? (
          <EnumCode>{workload.nodeSelectorHostname}</EnumCode>
        ) : (
          <span className={styles.fieldNote}>지정 없음</span>
        )}
      </span>

      <span
        className={styles.image}
        title={`이미지 index ${workload.imageIndexDigest}`}
        role="group"
        aria-label="이미지 index 하나에 두 플랫폼 manifest"
      >
        <span className={styles.imageIndex}>
          <span className={styles.fieldLabel}>이미지 index</span>
          <EnumCode>{shortHash(workload.imageIndexDigest, 8)}</EnumCode>
        </span>
        {workload.platforms.map((p) => {
          const arch = p.platform.slice('linux/'.length) as Arch;
          const serving = arch === servingArch;
          return (
            <span
              key={p.platform}
              className={styles.platform}
              data-arch={arch}
              data-serving={serving || undefined}
              title={`${p.platform} manifest ${p.digest}`}
            >
              <svg viewBox="0 0 12 12" aria-hidden="true" className={styles.platformGlyph}>
                {arch === 'amd64' ? (
                  <rect x="1.5" y="1.5" width="9" height="9" rx="1" />
                ) : (
                  <polygon points="6,0.8 10.6,3.4 10.6,8.6 6,11.2 1.4,8.6 1.4,3.4" />
                )}
              </svg>
              <span className={styles.platformName}>{p.platform}</span>
              {serving ? (
                <span className={styles.platformServing}>
                  <Lamp state="ok" label="이 플랫폼이 지금 응답 중" />
                  <span className={styles.platformServingText}>응답 중</span>
                </span>
              ) : null}
            </span>
          );
        })}
      </span>
    </div>
  );
}
