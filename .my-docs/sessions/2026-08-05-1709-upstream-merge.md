# 세션 요약: upstream v0.8.0 머지

- **날짜**: 2026-08-05 17:09
- **프로젝트**: kirocc (Anthropic Messages API → Kiro 백엔드 프록시)
- **브랜치**: main (작업은 `merge/upstream-v0.8.0`에서 진행 후 fast-forward)

## 수행 작업

### 1. upstream v0.8.0 머지 (커밋 `701f2d5`)

`d-kuro/kirocc`의 v0.6.0~v0.8.0 16커밋을 fork에 통합. 68파일 +4444/-478.
20개 파일이 충돌했고, 기본 방침은 합집합 — 양쪽이 같은 테이블·구조체·생성자에
각자 추가한 것이 대부분이었다.

upstream에서 들어온 것: GPT 5.6 모델 지원(#97), Kiro API 키 인증(#88),
SSE keep-alive(#86), 비객체 tool schema 래핑(#93), usage 토큰 폴백(#84),
의존성 업데이트(otel 1.45.0, sqlite 1.56.0, grpc 1.83.0, x/sync 0.22.0).

fork 기능은 전부 유지: 웹검색 개선, 이미지 처리·재생, 리전 오버라이드,
모델 발견, launchd 서비스.

**단순 합집합으로 안 된 3개 지점**

- **리전 통합** — 양쪽이 `-kiro-api-region` / `KIRO_API_REGION`이라는 같은
  이름을 다른 필드에 붙였다(우리: `-region` 별칭, upstream: API 키 전용 리전).
  둘 다 업스트림 API 리전이므로 `KiroAPIRegion`을 삭제하고 `Region`으로 통합.
  `main.go`가 `cfg.Region`을 `WithAPIKey`와 `WithRegionOverride` 양쪽에 전달.
  IDC의 API 리전 / OIDC 리전 분리는 병합 후에도 유지됨을 4개 버전 대조로 확인
  (fork 첫 버그 `afb5d1e`가 고친 그 축).
- **스키마 가드** — 우리 `ensureObjectSchema`를 삭제하고 upstream
  `EnsureObjectRoot`로 수렴(비객체 type 래핑까지 커버). 다만 upstream이
  `type: "object"`에서 조기 반환하며 `properties`를 보장하지 않아, 우리 쪽
  보장을 `schema_sanitize.go`에 이식. 입력 8형태 전부 유효 스키마 산출 확인.
- **redacted blob 재생** — upstream의 tool search는 라운드당 1개 전제였으나
  `2a8f730`이 팬아웃 루프로 바꿔 1:1 대응이 깨졌다. 첫 assistant 턴만
  blob을 싣도록 처리(스트리밍·비스트리밍 양쪽).

**충돌 없이 깨진 테스트 2건** — `synthetic_echo_test.go`가
`OnVisibleOutput func()` → `func() error` 변경에, `catalog_test.go`가
`ListModels() []string` → `[]ModelInfo` 변경에 걸렸다. 후자는 병합으로
빌트인에 `gpt-5.6-terra`가 생겨 "non-claude 제외" 단정이 무의미해져,
양쪽 테이블에 없는 ID로 교체.

### 2. redacted blob 주석 수정 (커밋 `cd38140`)

주석은 "blob이 라운드 단위 산출물이라 첫 턴만 싣는다"고 설명했지만 실제
이유가 달랐다. `extractReplayableRedactedThinking`이 blob을 함께 있는
`server_tool_use`에 귀속시키고 그 경우를 건너뛴다 — 프록시가 대신 실행한
라운드의 추론은 백엔드가 이미 소비했으므로 재생하면 거부된다. 따라서 첫 턴만
싣는 것은 추론 연결이 아니라 히스토리 페이로드 절감이 목적. 잘못된 근거를
남기면 `appendWebSearchMessages`에도 blob을 넣어야 한다고 오판하게 된다
(넣으면 안 된다). 동작 변경 없음.

### 3. 실제 API 셀프 테스트 (17항목, 커밋 없음)

브랜치 바이너리로 서비스를 띄워 실제 Kiro API에 요청. 전부 통과.

가장 불확실했던 경로가 실측으로 성공: **GPT 5.6 + tool search 다중 라운드**
— `server_tool_use` 3회, 발견한 도구 2개 호출, redacted blob 1992바이트가
text/tool_use 뒤에 도착. 그 blob을 히스토리로 되돌린 다음 턴도 정상 응답.

- 웹검색: "Go 1.26.5, 2026-07-07" 정확(스니펫만으로는 "Go 1.25" 오답이던 케이스),
  결과 10건 + `encrypted_content` 8260바이트
- 이미지: base64 붙여넣기, `tool_result` 내부(Read 경로), 2턴 뒤 히스토리 재생
  모두 색상 정확히 식별. 7.48 MiB 오버사이즈는 502 대신 정상 응답 + 로그 기록
- Opus 7변형 전부 통과, 응답 model ID `[1m]` 부착 규칙 의도대로
- upstream 신규: GPT 별칭 3개 `display_name` 노출, keep-alive 5회 발사,
  비객체 schema 3종 백엔드 통과
- 합성 플레이스홀더 에코 2건 감지 → `synthetic_empty_echo`로 재시도 → 정상 응답
- 재시작 이후 WARN 7건 전부 의도적 유발분, 예상 밖 에러 없음

### 4. upstream 머지 가이드 작성 (커밋 `b721ac2`)

`docs/MERGING-UPSTREAM.md` 신규. 반복되는 충돌 부류를 표로 8곳,
union만으로 안 되는 5가지 경우를 실례와 함께, diff만 보고 재도출하면 틀리는
결정 3개(위 §1)를 근거와 함께 기록. fork 고유 파일 목록도 포함 — 충돌은
안 나지만 upstream 리팩터가 seam을 건드리면 여기서 컴파일 에러가 난다.
`CLAUDE.md` 마지막에 한 줄 추가해 가리킴(지침 섹션 22줄로 제약 유지).
문서가 인용한 파일 7개·심볼 8개·커밋 3개 실재 확인.

## 변경 파일

| 타입 | 파일 | 설명 |
|------|------|------|
| Added | `docs/MERGING-UPSTREAM.md` | upstream 머지 절차·충돌 부류·결정 기록 |
| Modified | `CLAUDE.md` | 머지 가이드 참조 한 줄 |
| Modified | `internal/config/config.go` | `Config` 합집합, `KiroAPIRegion` 삭제 |
| Modified | `cmd/kirocc/main.go` | 플래그 합집합, `-kiro-api-region`을 `Region` 별칭으로 |
| Modified | `internal/auth/refresh.go` | `WithAPIKey`(upstream) + `WithRegionOverride`(ours) 공존 |
| Modified | `internal/models/models.go` | `findMappingSkipReasoning`으로 ID 정규화 + GPT 제외 통합 |
| Modified | `internal/models/effort.go` | `effortCapability` 구조체 + catalog 폴백 통합 |
| Modified | `internal/reqconv/schema_sanitize.go` | `EnsureObjectRoot`에 `properties` 보장 이식 |
| Modified | `internal/reqconv/tool_convert.go` | `ensureObjectSchema` 삭제 |
| Modified | `internal/reqconv/build_payload.go` | `buildCurrentMessage`에 `historyImages` 인자 추가 |
| Modified | `internal/respconv/accumulator.go` | `IsEmptyVisibleEndTurn`에 `RedactedContents` 조건 병합 |
| Modified | `internal/app/messages/toolsearch.go` | `session` 리팩터 수용 + redacted 처리 + 주석 수정 |
| Modified | `internal/app/messages/service.go` | `Service` 옵션 합집합 |
| Modified | `internal/server/server.go` | `Server` 옵션 합집합 |
| Modified | `internal/models/catalog_test.go` | 무의미해진 non-claude 단정을 유효 ID로 교체 |
| Modified | `internal/respconv/synthetic_echo_test.go` | `OnVisibleOutput` 시그니처 변경 대응 |
| Modified | `README.md` | API 키 섹션 수용 + 플래그·env 표 합집합 |
| Modified | `go.mod` | 의존성 상향, `x/net`은 direct 유지(`webfetch`가 사용) |
| Added | `internal/respconv/reasoning.go` 외 9개 | upstream 신규 파일 |

## Git 커밋

| 해시 | 메시지 |
|------|--------|
| 701f2d5 | Merge remote-tracking branch 'upstream/main' into merge/upstream-v0.8.0 |
| cd38140 | docs(toolsearch): redacted blob 재생 제한의 실제 이유로 주석 수정 |
| b721ac2 | docs: upstream 머지 주의사항 기록 |

origin/main에 푸시 완료 (`2a8f730..b721ac2`, 19커밋). fast-forward로 선형 이력.

## 남은 작업

- [ ] `make service-install` — 실행 중 바이너리는 브랜치 빌드(내용 동일), main 기준 재설치
- [ ] `git branch -d merge/upstream-v0.8.0` — fast-forward라 별도 이력 없음
- [ ] GPT 5.6 + web_search 조합 확인 — 이번엔 tool search 경로만 실측했다
- [ ] social 인증에서 `-region`이 토큰 갱신 엔드포인트까지 옮기는 문제 — 병합과
      무관한 기존 결함이고 현재 IDC + 오버라이드 미사용이라 무영향. `-region`을
      쓰는 social 계정이 생기면 원본 리전 보존 필요

---

## 후속 세션: idle timeout 진단 + 사용자 가이드 갱신 (2026-08-07 14:17)

레포 코드 변경은 없다. 로그 조사와 Confluence 최종 사용자 문서 갱신만 수행.

### 수행 작업

#### 1. `body read idle timeout` 에러 원인 규명 (코드 변경 없음)

작업 중 Claude Code에 "Churned for 8m 9s" 와 함께 stream error가 떠서 로그를 추적했다.

```
22:55:16 INFO  --> POST /v1/messages   model=claude-opus-5 thinking=xhigh stream=true
22:58:52 ERROR stream error  err=reading prelude: kiroclient: body read idle timeout
```

업스트림이 3분 36초간 한 바이트도 보내지 않아 `internal/kiroclient/client.go:68`의
`defaultBodyReadIdleTimeout = 180 * time.Second` 가드가 발동했다. `reading prelude`
이므로 Event Stream 첫 헤더조차 못 받은 상태 — 중간에 끊긴 게 아니라 시작되지 않았다.

**"어떻게 풀렸나"에 대한 답**: kirocc의 재시도 로직이 아니다. 같은 trace ID에는
`<--` 응답 로그가 없고, 22:58:48에 **다른 trace**(`38af67b4`)가 시작되어 15초 만에
200을 받았다. Claude Code가 별도로 재요청한 것이다. kirocc의 retry-once 루프는
403/429/5xx/빈 응답만 대상으로 하고, idle timeout은 상태 코드가 없어 분류되지 않는다.

같은 컨텍스트 크기(215866 토큰)로 재요청이 성공했으므로 요청 과대 문제가 아니라
백엔드 일시적 hang이다. 로그 전체에서 2건(7/28, 8/5)뿐이라 빈도는 낮고, 이번
머지와 무관하다.

**미결**: 180초를 플래그로 노출해 짧게 잡을지(`WithBodyReadIdleTimeout`은 이미 존재,
config·플래그 연결만 필요), `reading prelude` 단계 실패를 재시도 대상에 넣을지,
그냥 둘지 결정하지 않았다. 후자는 스트림 도중 끊긴 경우와 구별이 필요하다
(`GateWriter` promote 여부로 가능).

#### 2. Confluence 사용자 가이드 갱신 (v7 → v9)

`kirocc 설치 및 사용 가이드` (pageId=1139676357, NWAIPLT). 최종 사용자 문서이므로
의존성 업데이트·내부 리팩터는 제외하고 체감 변경만 반영.

- **0. 퀵 가이드 (macOS) 신규** — `brew install go` → 클론 →
  `make service-install SHELL_ENV=1` → 새 터미널에서 `claude` 실행,
  `/model`과 `hello`로 확인. `SHELL_ENV=1`이 환경변수 3개
  (`CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY` 포함)를 넣는 것은
  `scripts/service.sh:130-132`에서 확인
- 기준 버전 `v0.5.0` → `v0.8.0`
- 수정 사항 표에 3건 추가 — 모델 목록 자동 갱신, 빈 응답 자동 재시도, 웹검색 품질 개선
- **1.2절 신규** — 업스트림에서 온 체감 변경 5건 (GPT 5.6, Opus 5, API 키 인증,
  스키마 호환성, 연결 유지)
- **2.1절 신규** — Kiro CLI 없이 `KIRO_API_KEY`로 쓰는 방법
- **정정** — 7번 항목의 `-max-history-images`가 실제로는 `-history-image-turns`
  (기본 2턴). 기존 문서가 틀린 상태였다
- 옵션 표에 `-web-search`, `-model-discovery` 추가. 5번 절에 모델 발견 환경변수 설명.
  트러블슈팅에 모델 미표시·장시간 멈춤 2건 추가
- **백슬래시 표시 문제 4곳 정리** — `tool\_result`, health 응답 JSON, README 인용구,
  `make run ARGS`. markdown 포맷으로 저장할 때 MCP가 이스케이프를 남기는 문제로,
  storage 포맷으로 넣어 해결

### 변경 파일

| 타입 | 파일 | 설명 |
|------|------|------|
| Added | `.my-docs/sessions/2026-08-05-1709-upstream-merge.md` | 이전 세션 요약 (아직 미커밋) |
| (외부) | Confluence pageId=1139676357 | v7 → v9, 퀵 가이드 추가 및 v0.8.0 반영 |

레포 소스 코드 변경 없음.

### Git 커밋

커밋 없음 (레포 코드 변경 없음). 세션 요약 파일만 untracked 상태.

### 남은 작업

- [ ] 세션 요약 파일 커밋 (`.my-docs/sessions/` 2개 파일)
- [ ] idle timeout 대응 방향 결정 — 타임아웃 플래그 노출 / `reading prelude` 재시도 /
      현행 유지 중 선택
- [ ] 이전 세션의 남은 작업 4건은 그대로 유효
