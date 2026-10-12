# Read-only environment verification

저장소 root에서 Python 3으로 실행한다. 클러스터 검증은 PyYAML, 기존 OpsLoop kubectl·관리자 kubeconfig·Helm 접근이 필요하고 Azure 조사는 로그인된 Azure CLI가 필요하다. credentials·kubeconfig·Secret 본문은 출력하거나 복사하지 않는다. 아래 명령은 자원을 생성·수정·삭제하지 않는다.

```bash
python3 scripts/verify-platform.py
python3 scripts/azure-arm64-survey.py
```

`verify-platform.py`는 현재 로컬 두 노드 baseline을 검사한다. Node Ready/IP/architecture/version, System·Monitoring Pod, 배치, Helm release와 allowlist values 비교, PVC/PV/StorageClass, Prometheus Targets를 검사한다. exit 0은 선택한 baseline 통과, 1은 drift·누락·비정상, 2는 수집·구성 오류다. Azure의 세 번째 Node가 추가되면 예상 topology를 별도로 개정한다. Pod를 생성하는 통신·MTU·장애 실험을 대신하지 않는다.

`azure-arm64-survey.py`는 `--region`으로 조회 범위를 지정하고 구독별 filtered nonsecret 응답을 `.opsloop/azure-survey/`에 캐시한다. 지역 인수를 생략하면 과거 Korea Central → Japan East → Japan West 범위를 사용하므로 현재 학생 구독에서는 아래 허용 지역을 명시한다. 재사용 응답에는 최초 관측 시각을 유지한다. 배포 검토 직전에는 캐시를 우회한다.

```bash
python3 scripts/azure-arm64-survey.py --all-arm64 --region eastasia --region indonesiacentral --region malaysiawest --region norwayeast --region polandcentral --output .opsloop/azure-survey/allowed-regions.json
python3 scripts/azure-arm64-survey.py --region eastasia --refresh
```

`--all-arm64`는 고정 7개 SKU 대신 모든 ARM64 series를 조회하고 2 vCPU/4 GiB/Gen2 조건으로 추린다. 새 series의 family quota를 놓치지 않도록 모든 quota family를 함께 읽는다. 옵션 생략 시 기존 SKU allowlist를 유지한다. 한 지역당 SKU·usage·image 조회를 재사용하며 정책 자체를 평가하거나 등록하지 않는다. 기본 Terraform은 zone을 지정하지 않는 regional 배포다. zone 선택을 바꾸려면 Terraform·SKU 계약도 함께 검토한다. quota가 비거나 provider/SKU/image 응답이 실패하면 성공으로 대체하지 않는다. 읽기 전용 조건이 통과해도 capacity·RBAC/policy·크레딧·가격/예산·backend·사용자 승인이 남아 `deployment_gate=false`, exit 2를 유지한다. 이는 조사 실패와 배포 차단 사유를 JSON으로 확인하라는 의미이며 실행 허가가 아니다. 가격과 실제 정책 검토는 [허용 지역 조사](../docs/experiments/p1-c-azure-allowed-regions.md)를 별도로 확인한다.

`--disk-gib` 생략은 Terraform의 `os_disk_size_gb=null`과 같은 이미지 크기 상속이다. JSON의 기존 `disk_gib` 필드는 이때 `null`, 추가 `disk_mode`는 `inherit`다. `checks.disk_size=pass`는 override 제약이 없다는 뜻이며 실제 용량·과금 계층 확인을 뜻하지 않는다. 이미지 조회 실패는 계속 unknown이다. 기존 `--disk-gib 32` 등 양의 정수 옵션은 동일하게 지원하며 `disk_mode=override`, 알려진 이미지 최소 크기 이상이면 pass, 작으면 blocked, 최소 크기 미확인이면 unknown이다. 기존 상태 문자열과 exit 2는 유지한다. 이전에 옵션 생략을 32 GiB로 해석한 외부 CLI 사용자는 `--disk-gib 32`를 명시하고, JSON 소비자는 상속 시 null을 처리해야 한다. 문자열 `null` 옵션은 받지 않는다.

인터넷·local socket 접근이 sandbox에 의해 차단되면 같은 읽기 전용 명령에 필요한 실행 권한을 요청한다. 인증 변경·provider 등록·quota 요청으로 우회하지 않는다. 로그에 raw CLI stderr·Helm values·state를 붙이지 않는다.

로컬 fixture 검증은 실제 cloud/cluster에 연결하지 않는다.

```bash
python3 -m unittest discover -s tests -p 'test_*.py' -v
```

실제 조회 결과와 미검증 범위는 [P1-B](../docs/experiments/p1-b-local-platform.md), [Observability](../docs/experiments/p2-observability-verification.md), [Azure 배포 준비](../docs/runbooks/azure-arm64-deployment.md)를 따른다.
