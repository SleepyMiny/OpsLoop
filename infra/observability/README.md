# Observability: 현재 설치 기록

사용자 작성 `kube-prometheus-stack.values.yaml`은 기존 `monitoring/opsloop-mon` 설치에 대응하는 설정이다. 이번 작업에서는 파일을 변경하거나 Helm install/upgrade를 실행하지 않았다. 고정 chart는 **kube-prometheus-stack 91.8.1**이다. [실제 검증 기록](../../docs/experiments/p2-observability-verification.md)에 release·이미지·target·저장소 및 남은 gate를 기록했다.

Prometheus/Grafana/Operator/kube-state-metrics/Alertmanager는 `opsloop-control`에 고정되고 exporter는 각 Node에서 실행된다. Prometheus 10Gi·Grafana 2Gi PVC는 local-path를 사용한다. control 디스크에 묶이는 저장소이며 Delete reclaim 정책이므로 재설치·PVC 삭제가 복구 절차가 되지 않는다.

현재 scrape/evaluation은 30초다. ADR의 초기 10초 scrape 및 30초 sample age 정책과 차이가 있어 P3 전에 실제 지연·freshness 검증과 정책 결정을 완료해야 한다. 현재 Alertmanager 실행도 최소 스택 대비 추가 구성으로 기록한다.

```bash
python3 scripts/verify-platform.py
```

검증기는 Kubernetes API GET, Service proxy GET, Helm metadata 및 메모리 안에서 allowlist한 values 비교만 수행한다. raw Helm values·Grafana admin password·Secret·kubeconfig 내용은 출력하지 않는다. 실행 요구사항·exit 의미·offline 테스트는 [scripts README](../../scripts/README.md)에 있다. credentials를 values나 Git에 넣지 않는다. Helm 차이·target down은 진단 결과로 보고하며 자동 수정·재시작하지 않는다.
