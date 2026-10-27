# 보안 경계와 인증 계약

## 경계

1. Kubernetes API/Prometheus→Ubuntu 내부: 전용 최소 읽기 권한, 기존 네트워크 경로만 사용.
   localhost BFF를 인터넷에 프록시하지 않는다. 관리자 kubeconfig를 Ubuntu/Cloudflare로 복사하지 않는다.
2. Ubuntu→Cloudflare: 고정 origin의 TLS 검증 HTTPS outbound443만. Collector는 listening port가 없다.
   Kubernetes 요청 credentials와 수집용 HMAC key는 분리하고 브라우저·asset·D1에 넣지 않는다.
3. Worker/D1→브라우저: allowlisted DTO만. Worker의 outbound proxy 기능·사용자 지정 URL·PromQL 없음.
   공개 API에는 Kubernetes token·경로·쓰기 권한·장애 주입 기능이 없다.

HMAC은 신뢰된 Collector의 요청임을 입증할 뿐, 침해된 Collector가 거짓 관측을 서명하는 것을
막지는 못한다. 또한 공개 시각·노드 가용성·부하 자체가 운영 정보임을 소유자가 승인해야 한다.
내부 식별자는 공개 alias로 치환하지만 완전한 익명성이나 인프라 존재 은닉을 보장하지 않는다.

## 수집 HTTP / HMAC

endpoint: `POST https://opsloop.sleepyminy44.workers.dev/api/collect/status`만.
query/다른 host/path/method 거부. Content-Type application/json, UTF-8 raw body 최대16KiB,
압축 body 거부. Content-Length만 믿지 않고 실제 stream 바이트 수를 제한한다.
중복/비정규 header·duplicate JSON key·NaN/Infinity·unknown field를 거절한다.

headers (서명/key 원문을 로그에 기록하지 않음):

- X-OpsLoop-Collector: 서버 등록 ID `home-v1`
- X-OpsLoop-Key-Id: 승인된 key version (비밀값 아님)
- X-OpsLoop-Sequence: canonical decimal, 양의 JS safe integer
- X-OpsLoop-Sent-At: canonical epoch milliseconds
- X-OpsLoop-Signature: 64자리 lowercase hex HMAC-SHA256

canonical bytes: 아래9개 줄을 LF로 연결, 마지막 LF 없음. body 자체를 재정렬해 서명하지 않는다.

```text
OPSLOOP-STATUS-V1
https://opsloop.sleepyminy44.workers.dev
POST
/api/collect/status
<collector-id>
<key-id>
<sequence>
<sent-at-ms>
<SHA256 of exact raw UTF-8 body bytes>
```

서명은 constant-time 검증. sentAt의 과거120초/미래5초 범위만 허용한다.
새 envelope의 collectedAt은 sentAt 기준 과거30초/미래5초 범위다.
metric/lease의 원본 timestamp가 오래된 경우는 값을 새 시각으로 바꾸지 않고 stale로 공개한다.
Worker는 credential 오류를 구체적인 key 존재 여부와 함께 응답하지 않는다.

## replay/order와 쓰기 빈도

- Collector는 root 전용 durable state에 다음 seq를 쓰고 fsync/atomic rename을 마친 뒤 전송한다.
  single-process lock 사용. crash로 빠진 번호는 허용, 재사용/리셋은 금지.
- Worker는 **한 D1 조건 UPDATE**로 `seq > stored_seq`, `collectedAt >= stored_collectedAt`,
  `now >= receivedAt + 15s`를 만족할 때만 counter와 body를 함께 변경한다. affected row=1 확인.
  read→blind write, isolate 메모리 nonce, eventually-consistent KV는 replay fence로 사용하지 않는다.
- `status-storage.sql`의 사전 승인 bootstrap 1행만 대상. 네트워크 요청으로 collector를 생성하지 않는다.
  서명/schema/DB 실패 시 counter도 진전하지 않는다. DB 응답 유실 시 같은 요청 재전송은409가
  될 수 있으며 Collector는 다음 tick에 더 큰 seq와 최신 관측을 전송한다. 무한 retry 금지.
- key rotation은 같은 collector의 counter를 공유한다. old/new key 짧은 overlap 후 폐기.
  counter 파일 유실·D1 복원/롤백으로 seq가 후퇴하면 전송 중단, key/identity 재발급을
  별도 승인한다. 데이터베이스 rollback 중 ingest를 열어 두지 않는다.
- 인증 전에는 body 크기·method/path·header 형식 및 edge rate limit 적용.
  key 검증 실패는 D1 조회 전에 끝낸다. 인증 후 key별 edge limiter와 글로벌 D1 write gate 적용.
  초기 ingest edge limit12/minute/key, 실제 저장 최대4/minute/collector.
  GET `/api/status`는 전시 NAT를 고려해 높은 abuse limit(예:120/minute/IP)과
  전체 usage 관측을 조합한다. 방문객 IP는 저장하지 않고 limiter의 단기 key로만 사용한다.
- Cloudflare Rate Limiting binding은 PoP별·eventually consistent이다. 정확한 글로벌 quota나
  비용 보호 장치로 표현하지 않는다. D1 CAS는 **승인된 write 빈도**를 제한하며,
  공격자의 GET/무효 요청이 Worker 일일 quota를 소모하는 것까지 막지 못한다.

운영 구현의 header parser·HMAC(WebCrypto/Go)·body streaming·status code·D1 동작은
아직 미구현/미검증이다. Python 모델의 고정 `test-key`는 공개된 비밀이 아닌 테스트 문자열이며
실제 secret 생성·저장·등록은 수행하지 않았다.

## Kubernetes 최소 Identity

새 SA 제안: 별도 `opsloop-observer` namespace의 `public-status-collector`.
아래 exact name은 서버 private 설정에서 확정하며 public schema에는 포함하지 않는다.

| 범위 | resource | verb / name 제한 |
|---|---|---|
| ClusterRole | core/nodes | **get만**, 승인된3노드 resourceNames |
| kube-node-lease Role | coordination.k8s.io/leases | **get만**, 승인된3개 Node Lease resourceNames |
| opsloop-system Role | apps/deployments | **get만**, Controller Deployment resourceName |
| opsloop-demo Role | apps/deployments | **get만**, Demo API Deployment resourceName |
| opsloop-demo Role | opsloop.io/recoverypolicies | **get만**, 운영 Policy resourceName |
| opsloop-demo Role | opsloop.io/recoveryincidents | **list만**, 해당 namespace. 내부에서 Policy/Workload UID 매칭 후 count만 공개 |

이 권한으로 기존 BFF의 nodes LIST / cluster-wide Pods LIST는 사용할 수 없다.
따라서 좁은 `PublicStatusReader`를 별도로 구현한다. 초기 Home은 전체13개 Pod의
세부 목록·cluster requested resources를 제공하지 않고 Deployment replica 요약만 표시한다.
이 선택으로 cluster-wide Pod env/volume까지 읽을 권한을 피한다.
Node OS/arch는 GET 결과, freshness는 Lease, Demo/Controller Ready는 Deployment.status와
observedGeneration으로 판단한다. Pod 단위 상세가 필요하면 별도 권한/공개 범위를 재승인한다.

Secrets, ConfigMaps, Pod exec/logs, nodes/proxy, services/proxy, nonResourceURLs,
watch, create/patch/update/delete, bind/escalate/impersonate, serviceaccounts/token 권한 없음.
RBAC뿐 아니라 클라이언트 GetOnly transport를 재사용해 우발적 POST도 차단한다.
Incident LIST에는 내부 diagnostic text가 포함될 수 있다. token 침해 시 읽기 위험은 남으므로
namespace 제한·호스트 계정 격리·출력 projection을 함께 적용한다.

## 외부 Collector의 credential 갱신

SA token을 필요로 하지만 public Collector 자체에 TokenRequest create 권한을 주지 않는다.
초기 전시안은 소유자가 신뢰된 관리 환경에서 **짧은 수명 TokenRequest**를 발급하고
관리자 kubeconfig 없이 CA+API URL+전용 token만 Ubuntu credential 파일로 전달하는 방식이다.
희망24시간 만료는 실제 API server가 반환한 expiration으로 검증해야 하며 보장하지 않는다.
만료 전 별도 운영 절차로 교체, 원자적 file replacement 및 Collector reload를 검증한다.
장기 Secret token 생성이나 자동 인증 우회는 선택하지 않는다.
상시 무인 운용이 필요하면 별도 trusted in-cluster issuer/회전 경로를 추가 설계·승인해야 한다.

프로세스 전용 Unix 계정, credential 파일600/부모700, systemd LoadCredential 등 argv/env 노출을
피하는 전달 방식, NoNewPrivileges/ProtectSystem/재시작 backoff를 이후 설치에서 검증한다.
Prometheus 주소는 기존 private 네트워크의 고정 query endpoint다. 실제 인증/TLS 정책은 미검증.
Kubernetes SA token을 Prometheus 임의 호스트에 보내지 않는다. 관리자 API/변경 endpoint는 호출 금지.

## Cloudflare 403/1010

기존 probe는 `/`와 `/api/status` 모두403, root 오류문은1010이었다.
공식 문서상 browser signature 차단과 부합한다. 그러나 어떤 account policy가 원인인지,
일반 관람객 브라우저도 거부되는지, Worker가 실행됐는지는 확인하지 못했다.
사이트 전체 장애/인증 오류/미배포 API로 단정하지 않으며 헤더·client 위장으로 우회하지 않았다.
향후 소유자 Cloudflare 보안 이벤트 조회 또는 정상 브라우저 결과로 확인한다.
보안 완화가 필요하더라도 별도 승인 대상이며 이번에는 설정을 변경하지 않는다.
