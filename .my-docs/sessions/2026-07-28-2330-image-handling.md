# 세션 요약: kirocc 이미지 처리 수정

- **날짜**: 2026-07-28 23:30
- **프로젝트**: kirocc (Anthropic Messages API → Kiro 백엔드 프록시)
- **브랜치**: main

## 수행 작업

### 1. tool_result 중첩 이미지 유실 버그 수정

이미지를 **파일 경로로** 주면 모델이 전혀 보지 못하던 문제. 원인은 Claude Code가 Read 툴 결과를 `tool_result` 블록 **내부**의 `image` 블록으로 돌려주는데, kirocc의 이미지 추출이 top-level 블록만 순회했고 `extractToolResultContentText`에도 `image` case가 없었음. 결과적으로 텍스트가 빈 문자열이 되어 `"(empty result)"`로 치환되었고, 모델은 이미지 대신 그 문자열을 받아 파일 경로로 내용을 추측하고 있었음.

- `tool_result` 내부 이미지를 메시지 레벨 `userInputMessage.images`로 승격 (Kiro tool result는 text/JSON만 담을 수 있음)
- 원래 자리에는 플레이스홀더를 남김
- 공용 헬퍼 추출: `imageFromBlock`, `nestedImageBlocks`, `toolResultFromBlock`

### 2. 히스토리 이미지 리플레이 추가

Kiro `HistoryUserInputMessage`에 `images` 필드가 없어서, 이미지가 **도착한 턴에만** 보이고 다음 턴부터 사라지던 문제. 붙여넣기·경로 방식 모두 해당. 모델은 이미지가 사라진 걸 인지할 수 없어 이전 답변만 근거로 추측하게 되는 조용한 실패 모드였음.

- `collectHistoryImages`가 히스토리 이미지를 모아 현재 메시지 `images` 앞쪽에 재부착 (오래된 것 → 최신 순)
- 상한: `-max-history-images` / `KIROCC_MAX_HISTORY_IMAGES` (기본 10, 0=비활성, 음수=무제한)
- **붙여넣기+경로 섞어 쓰기 문제도 이걸로 해결됨** — 두 방식은 서로 다른 턴에 도착하므로 리플레이 없이는 동시에 볼 수 없었음
- 설정 경로: flag/env → `Config` → `server.WithMaxHistoryImages` → `messages.WithMaxHistoryImages`
- `count_tokens` 경로에도 동일 옵션 적용 (리플레이 이미지가 실제 토큰을 소비하므로 카운트 일치 필요)

### 3. Kiro 이미지 용량 상한 실측 및 가드

`kiro_imgprobe.py`로 로컬 kirocc에 직접 요청을 보내 측정 (컨텍스트 오염 방지를 위해 Read 미사용).

| 시나리오 | 총 크기 | 결과 |
|---|---|---|
| 1장 4.85 MiB | 4.85 MiB | 200 |
| 1장 5.24 MiB | 5.24 MiB | **502** |
| 4장 × ~3.1 MiB | 12.40 MiB | 200 |
| 40장 (1280×720) | 1.58 MiB | 200 |
| UHD 1장 | 0.35 MiB | 200 |

**결론: 상한은 장당 5 MB (base64 기준), 요청 전체 제한이 아님.** 12.4 MiB 요청이 통과하는데 5.24 MiB 단일 이미지가 거부되므로 판정은 per-image. Anthropic API 문서 수치와 동일.

가드가 필요했던 이유는 리플레이와의 상호작용. 5 MB 초과 이미지가 한 번 들어오면 매 턴 재전송되어 **그 세션의 이후 모든 요청이 502로 죽음**. 에러 메시지는 `upstream API error`뿐이라 원인 추적 불가.

- `maxImageBytes = 5 * 1000 * 1000` 초과 시 전송 전 드롭 + 거부된 크기 로깅
- 플레이스홀더를 `[image omitted: over the 5MB per-image size limit]`로 구분 (첨부되지 않았는데 "attached"라고 하면 모델이 검증할 수 없는 거짓말)
- 같은 메시지의 다른 이미지는 정상 전송

### 4. 실동작 검증

서로 구별 가능한 테스트 PNG를 생성해 4가지 시나리오를 한 번에 확인.

- 같은 턴 2장 병렬 Read → 둘 다 전달
- 이전 턴 이미지 2장 리플레이 유지 (색이 완전히 달라 중복이 아님을 확인)
- 순서: 오래된 것 앞, 새것 뒤
- tool_result 텍스트에 `[image attached to this message]` 반환 (기존 코드였으면 `(empty result)`)

## 변경 파일

| 타입 | 파일 | 설명 |
|------|------|------|
| Modified | `internal/reqconv/images.go` | 중첩 이미지 추출, 크기 상한, 플레이스홀더 상수, `collectHistoryImages` |
| Modified | `internal/reqconv/content_scan.go` | 현재 메시지 경로에서 중첩 이미지 승격, `toolResultFromBlock` 추출 |
| Modified | `internal/reqconv/message_normalizer.go` | `extractToolResultContentText`가 image 블록 처리 + 플레이스홀더 인자 |
| Modified | `internal/reqconv/tool_results.go` | 히스토리 경로 플레이스홀더, 중복 로직 제거 |
| Modified | `internal/reqconv/build_payload.go` | `MaxHistoryImages` 옵션, 히스토리 이미지 병합 |
| Modified | `internal/reqconv/history.go` | 경고 문구 정정 (Warn → Debug) |
| Added | `internal/reqconv/history_images_test.go` | 리플레이·상한·크기 제한 테스트 10개 |
| Modified | `internal/reqconv/tool_results_test.go` | 중첩 이미지 케이스 추가 |
| Modified | `internal/app/messages/service.go` | `WithMaxHistoryImages` 옵션 |
| Modified | `internal/app/messages/handler.go` | BuildOptions에 옵션 전달 (일반 + tool search) |
| Modified | `internal/app/messages/request.go` | count_tokens 경로에 옵션 전달 |
| Modified | `internal/server/server.go` | `WithMaxHistoryImages` ServerOption |
| Modified | `internal/config/config.go` | `MaxHistoryImages` 필드 + env override |
| Modified | `cmd/kirocc/main.go` | `-max-history-images` 플래그 |
| Modified | `README.md` | `#### Images` 절 추가 (동작 원리, 트레이드오프, 실측 수치) |
| Modified | `CLAUDE.md` | 문서 리라이트 (이번 작업 전부터 있던 변경) |

## Git 커밋

| 해시 | 메시지 |
|------|--------|
| 7a35eab | fix(reqconv): carry images that Kiro's shape would drop |
| b62c274 | docs: rewrite CLAUDE.md around non-obvious package behaviour |
| 292a9b1 | fix(reqconv): drop images the backend would reject on size |

`origin/main`에 푸시 완료. 검증: `go build`, `go vet`, `gofmt`, `go test -race ./...` 모두 통과.

## 조사만 하고 미구현

### 웹 검색 대안 (MCP)

WebSearch는 Anthropic API의 server-side tool(`web_search_20250305`)이라 Anthropic 서버가 직접 실행 → Kiro 경유로는 원리적으로 불가. 두 가지 방향을 비교:

1. **MCP 검색 서버** (선택) — 코드 변경 0, 클라이언트사이드 툴로 내림. 단점: 사용자마다 `.mcp.json` 설정 + WebSearch 비활성화 + 서브에이전트 `tools: WebSearch` 수정 필요, 프롬프트/스킬에 박힌 "WebSearch 사용" 지시와 불일치
2. kirocc가 server-side tool 에뮬레이트 — 프록시 안에서 완결되지만 `web_search_tool_result` 스키마를 **양방향**(다음 턴 요청에 실려 오는 것까지) 구현 필요

MCP 후보 조사용 Gemini 프롬프트 작성 완료 (Brave/Tavily/Exa/Perplexity/SearXNG/DuckDuckGo/Google CSE/Firecrawl 비교).

### 토큰 갱신 직후 500 에러

세션 리줌·맥북 잠자기 후 초반에만 502가 반복되다 풀리는 현상. `~/Library/Logs/kirocc/kirocc.log` 분석 결과 **유휴 자체가 아니라 토큰 갱신 직후가 트리거**:

```
13:16:40  credentials expired, refreshing
13:16:42  token refreshed
13:16:42  --> POST /v1/messages
13:16:52  kiro: upstream error, retrying  st=500 att=1   ← 실패
```

갱신 없는 유휴(11분/12분/22분)에서는 에러 없음. 잠자기·리줌이 트리거로 보였던 건 둘 다 액세스 토큰 만료(~1시간)를 넘길 시간이 지났기 때문. 단, 11:44의 갱신은 200이었으므로 100%는 아님.

Kiro가 돌려주는 건 `InternalServerException`("please try again")으로 재시도 가능한 일시적 실패. 새 자격증명 전파 전에 요청이 도착하는 것으로 추정(미검증 — 인증 실패라면 401/403이 맞는데 500이 옴).

## 남은 작업

- [ ] 웹 검색 MCP 서버 선정 → `.mcp.json` 설정, WebSearch 비활성화, 서브에이전트 정의의 `tools: WebSearch` 정리
- [ ] 토큰 갱신 직후 500 원인 검증 (갱신 후 짧은 지연을 넣어 재현율 변화 측정) 또는 `internal/kiroclient` 재시도 횟수 3 → 5~6 상향으로 완화
- [ ] 리플레이 실사용 관찰: 이미지 많은 세션에서 페이로드 증가 및 컨텍스트 윈도 소비 확인 (필요시 `KIROCC_MAX_HISTORY_IMAGES` 하향)
- [ ] `/tmp/kirocc_*.png`, `/tmp/kiro_imgprobe.py` 정리 여부 결정 (상한 재측정 시 프로브 스크립트 재사용 가능)

---

## 후속 세션: 턴 기반 리플레이 창으로 교체 + 실사용 검증 (2026-08-03 14:14)

이전 세션의 "장수 기준 상한 10"이 실사용에서 컨텍스트 오염을 일으켜, 다른 PC에서 `fix/history-image-turns` 브랜치로 수정한 것을 머지·검증한 세션. 코드 작성은 다른 PC에서 진행됨.

### 수행 작업

#### 1. 브랜치 머지 (`fix/history-image-turns` → main)

`git pull --rebase`로 `5f50a07`(웹 검색) 수신 후, `origin/fix/history-image-turns`를 fast-forward 머지.

- `e6f3827` fix(service): launchd 경쟁 조건 — `launchctl bootout`이 실제 teardown 완료 전에 리턴해서 곧바로 오는 bootstrap이 `Load failed: 5: Input/output error`로 실패. install이 `/health` 체크에서 죽고, 재실행하면 그 사이 teardown이 끝나서 성공하던 문제
- `3933862` feat(reqconv): 리플레이를 턴 단위로 범위 지정 + 출처 명시

빌드·`go test -race ./...` 전체 통과.

#### 2. 왜 장수 기준이 틀렸는지 (핵심 설계 판단)

이전 세션에서 놓친 두 가지가 이번 커밋에서 분리됨.

**출처 소실**: 리플레이는 이미지를 살리지만 **어디서 왔는지를 지운다**. 10턴 전 스크린샷이 방금 붙여넣은 것과 **정확히 같은 자리**(현재 메시지 `images`)에 도착하고, `kiroproto.Image`는 bytes와 format만 담아서 모델이 구별할 방법이 없음. 그래서 무관한 이미지를 현재 질문의 일부로 읽음 — "컨텍스트 오염"의 실체가 이것. `historyImageNote`가 메시지 텍스트로 "앞의 N장은 이전 턴에서 온 것"을 명시해 해결. 출처가 명시되면 상한은 **비용 문제만** 남음.

**단위 오류**: 이미지 개수는 틀린 단위. 상한 2면 "이것들 비교해봐"로 5장 준 세트가 다음 턴에 3장 잘려나가고, 남은 2장이 더 관련 있는 것도 아님. 턴으로 세면 한 턴에 붙은 건 함께 만료 — 5장 세트가 1장과 같은 기간 유지됨.

**프롬프트 캐시 불가**: 리플레이는 매 턴 바뀌는 현재 메시지에 붙으므로 캐시가 절대 안 걸림. 모든 이미지가 매 턴 새로 과금됨. 그래서 창을 넉넉히가 아니라 짧게(2턴) 잡는 것이 맞음.

#### 3. 구현 내용 (`3933862`)

- `DefaultMaxHistoryImages = 10` → `DefaultHistoryImageTurns = 2` (현재 턴 + 이전 사용자 턴 2개)
- 이름 변경: `MaxHistoryImages` → `HistoryImageTurns`, 플래그 `-history-image-turns`, 환경변수 `KIROCC_HISTORY_IMAGE_TURNS` (숫자 의미가 달라졌으므로)
- `historyImageNote(n)` / `appendHistoryImageNote` — 출처 안내를 현재 메시지 텍스트에 부착. tool_result만 있는 턴은 content가 비어 있어 노트가 전부가 됨 (kiro-cli의 빈 문자열 형태를 맞추는 것보다 출처가 더 중요하다는 판단)
- `isUserTurnStart` — **tool_result는 턴으로 세지 않음**. tool_result도 user role이라 그냥 세면 Read 몇 번에 창이 소진됨. 그 continuation 중에 붙은 것은 그것을 시작한 턴에 속함
- `historyImageWindowStart` — 뒤에서부터 사용자 턴을 세어 창 시작 인덱스 반환
- 창 밖으로 나가면 `[image provided earlier in this conversation]` 플레이스홀더가 히스토리에 남아, 모델이 이미지가 있었다는 걸 알고 다시 요청할 수 있음 (추측하지 않음)
- 테스트: 턴 전체 유지, 창 밖 턴 드롭, tool_result 비카운트 등으로 재작성

#### 4. 실사용 검증

`make service-install` 후 Claude Code 재시작. 바이너리 확인(`-history-image-turns` 플래그 존재, 14:10 빌드).

- 재시작 이후 로그에서 예전 경고 `images in history messages are not supported and will be dropped` 완전히 사라짐 (이번 브랜치에서 제거된 문구)
- **검증 과정에서 알게 된 것**: assistant의 tool 호출로는 창을 넘길 수 없음. `isUserTurnStart`가 tool_result를 의도적으로 제외하므로 설계대로임. 사용자가 직접 메시지를 보내야 창이 밀림
- 이미지 없는 사용자 메시지 3회 후 이미지 8장이 **전부 만료됨** → 턴 창 동작 확인
- 이 결과로 "Claude Code가 히스토리 이미지를 매 턴 재전송하는 것 아닌가"라는 의심은 기각됨 (재전송이라면 창을 넘겨도 계속 왔을 것). kirocc 리플레이가 맞고 정상 만료됨

### 변경 파일

머지로 받은 것 (다른 PC에서 작성):

| 타입 | 파일 | 설명 |
|------|------|------|
| Modified | `internal/reqconv/images.go` | `DefaultHistoryImageTurns`, `historyImageNote`, `isUserTurnStart`, `historyImageWindowStart` |
| Modified | `internal/reqconv/build_payload.go` | `HistoryImageTurns` 옵션, 노트 부착 |
| Modified | `internal/reqconv/history_images_test.go` | 턴 기반 테스트로 재작성 |
| Modified | `internal/reqconv/history.go` | 예전 경고 문구 제거 |
| Modified | `internal/config/config.go`, `cmd/kirocc/main.go` | 플래그·환경변수 이름 변경 |
| Modified | `internal/server/server.go`, `internal/app/messages/{service,handler,request}.go` | 옵션 이름 변경 전파 |
| Modified | `scripts/service.sh` | launchd label 해제 대기 |
| Modified | `README.md` | Images 절을 턴 기반 설명으로 갱신 (출처 소실·캐시 불가 근거 포함) |

### Git 커밋

| 해시 | 메시지 |
|------|--------|
| 5f50a07 | feat: resolve Claude Code's WebSearch through Kiro's hosted web_search (#1) |
| e6f3827 | fix(service): wait for launchd to release the label before loading |
| 3933862 | feat(reqconv): scope history image replay to turns and label it |

이번 세션에서 새로 만든 커밋은 없음 (머지·검증만). 로컬 main이 `origin/main`보다 2커밋 앞선 상태로 **푸시 안 됨**.

### 웹 검색 문제 해결됨 (`5f50a07`, 다른 PC 작업)

이전 세션의 "남은 작업" 1번이 해결됨. 예상과 달리 **MCP 서버 방식이 아니라 프록시 에뮬레이트(2번 방안)** 로 갔고, 핵심은 **AWS가 Kiro 구독에 MCP 엔드포인트를 이미 호스팅하고 있고 거기에 `web_search`가 있다**는 발견. 검색 API 키 발급이나 SearXNG 셀프호스트가 불필요.

이전 세션에서 "Kiro에는 대응물이 없다"고 판단한 것은 **틀렸음** — runtime API에는 없지만 MCP 엔드포인트에 있었음.

- `internal/kiromcp` — InvokeMCP 클라이언트 (런타임 클라이언트와 자격증명·403 갱신 경로 공유)
- `internal/reqconv/server_tools.go` — Anthropic `web_search_20250305` 선언을 Kiro 함수 툴로 변환
- `internal/app/messages/websearch.go` — 실행. 라운드 루프는 tool search 오케스트레이터 재사용
- `-web-search` 기본 true. MCP 클라이언트가 nil이면 Anthropic 선언만 제거해 요청 실패 방지
- `scripts/mcpprobe/main.go` — MCP 엔드포인트 탐색 도구

### 남은 작업

- [ ] `git push origin main` (로컬 2커밋 앞선 상태)
- [ ] 토큰 갱신 직후 500 원인 검증 — 갱신 후 짧은 지연을 넣어 재현율 변화 측정, 또는 `internal/kiroclient` 재시도 3 → 5~6 상향. 이전 세션 로그 분석에서 트리거는 확인됨(유휴가 아니라 토큰 갱신 직후)
- [ ] `historyImageNote`가 실제 페이로드에 붙는지 미확인 — 이번 세션에서 노트를 관측하지 못했으나 창 만료는 정상 동작했음. `-debug` 캡처로 확인 필요 (동작 자체는 검증됐으므로 우선도 낮음)
- [ ] `/tmp/kirocc_*.png`, `/tmp/kiro_imgprobe.py` 정리 여부
