# 공개 정적 전시 profile

`npm run build:public`은 Demo/과거 Evidence Replay 전용이며 `dist-public/`을 생성한다.
기존 `npm run build`/`npm run dev`는 로컬 Demo/Live/Replay 계약을 유지한다.

## 격리 계약

- public HTML/main/source factory는 별도 진입점이다. `?source=live`, 잘못된 source,
  빈 source, 중복 source는 안내 화면에서 거절한다. 다른 출처로 몰래 전환하지 않는다.
- 공개 mode navigation은 Demo/Replay뿐. SourceNotice에 시뮬레이션/과거 실증/현재 Live의
  차이를 첫 화면부터 표시한다. ObserveOnly·attempts0·운영자 수동 복원·Enforce 미실증을 유지한다.
- HTTP adapter와 parser, Replay core와 local fixture wrapper만 분리했다. 기존
  `parseSnapshot` export/default local Replay API와 HttpDataSource 동작은 유지한다.
- public graph에는 Live adapter/local factory/local Replay fixture가 들어가면 빌드가 실패한다.
  Go BFF/Worker runtime/Functions/DB/bindings는 없다.
- `build-public.mjs`는 OS 필수 환경만 하위 빌드에 전달한다. Vite `envDir:false`로
  `.env*`를 읽지 않는다. VITE_*에는 어떤 profile에서도 비밀정보를 넣지 않는다.
- local publicDir을 사용하지 않고 `public-static`의 검토된 `_headers`, `404.html`만 허용한다.
  source maps와 symlink·알 수 없는 output extension/path는 허용하지 않는다.
- CSP `connect-src 'none'`는 공개 빌드의 API/외부 연결을 추가로 차단한다.
  폰트는 `assetsInlineLimit:0`으로 별도 파일, `font-src 'self'` 유지.
  동적 UI style attribute 때문에 `style-src 'unsafe-inline'`이 필요하며 script에는 적용하지 않는다.

## 공개 데이터

`src/public/evidence.public.json`은 PR4의 이미 정제된 `data/replay/evidence.json`에서
파생됐다. 원본 실험 로그·private state 디렉터리를 읽거나 업로드하지 않는다.

`evidence-boundary.json`의 승인 입력 SHA256과 `verify-public-evidence.mjs`의 정확한 파생
비교로 미검토 입력을 차단한다. source 변경 시 해시만 자동 갱신해서는 안 된다.
노드 이름은 public alias로 일관되게 바꾸고 중복 verification 문구만 제거한다.
수치·분류·Decision·선택 관계·timestamp·provenance hash는 유지한다. 공개 Demo의 가상
노드 이름과 표시용 endpoint도 public 전용 빌드 transform으로 비식별화한다.
원래 Demo/Live/Replay 데이터 파일은 변경하지 않는다.

보안 검증은 입력 출처/해시·정확한 파생 비교·import graph·환경 격리·산출물 허용 목록·
브라우저 network trace·내용 검토를 결합한다. 문자열 패턴 검사는 보조 수단이며
모든 비밀정보 부재를 증명하는 완전한 탐지기라고 주장하지 않는다.

## 로컬 검증

```bash
npm run test:public-build
npm test
npm run build:public
npm run build
```

기존 Node24.22.0 및 lockfile 의존성으로 검증했다. 공개 브라우저 검증은 production
`dist-public`을127.0.0.1에만 제공하고 API/외부 요청을 차단한다. 실제 클러스터/서버는
종료하지 않는다. public manifest는 출력 폴더 밖 `public-artifact-manifest.json`에 기록한다.
이는 로컬 검증 metadata이며 사이트에 업로드하지 않는다.

## Workers Static Assets — 아직 배포하지 않음

기존 Pages 조사는 보존한다. Cloudflare 공식 신규 프로젝트 권고에 따라 이번 배포 대상만
Workers Static Assets로 조정했다. [공식 권고](https://developers.cloudflare.com/workers/best-practices/workers-best-practices/#use-workers-static-assets-for-new-projects).

`wrangler.public.json`은 name/date/assets.directory만 갖는 순수 정적 설정이다.
`assets.directory=./dist-public`, `not_found_handling=none`; query 기반 root 화면을 사용한다.
`main`, Worker code, ASSETS binding, API proxy, account ID/token, route/도메인 설정이 없다.
Cloudflare 프로젝트를 생성하거나 Wrangler 설치·로그인·배포 명령을 실행하지 않았다.
설정은 공식 문서와 정적 guard로 확인했으며 실제 Edge/계정 검증은 게시 승인 후 수행한다.
[설정 계약](https://developers.cloudflare.com/workers/static-assets/binding/).

다음 별도 승인 범위: 수정본 리뷰/필요시 PR → 프로젝트명·정제 Replay 공개 범위·정확한
artifact hash 승인 → Cloudflare 계정에서 순수 정적 프로젝트/업로드/URL 활성화.
GitHub 연결, 자동 main deploy, Live/BFF 공개, 서버·DNS 변경은 이 승인에 포함하지 않는다.
