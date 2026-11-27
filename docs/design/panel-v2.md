# 패널 v2 설계: Stratum으로 다시 만들기

상태: 설계 확정(2026-11-28). [열린 질문](#9-열린-질문)은 모두 권하는 답으로 정했습니다. 1~4단계를 구현했습니다([§7.3](#73-1단계-프런트엔드-확인-결과), [§7.4](#74-2단계-읽기-화면-확인-결과), [§7.5](#75-3단계-operator-동작-확인-결과), [§7.6](#76-4단계-관리-화면-확인-결과)). 남은 것은 5단계 전환입니다.

정해진 것: 관리자 패널을 정보 구조까지 처음부터 다시 만들고, UI는 사용자의 라이브러리 **Stratum**(`@yakihyuk0728/stratum-ui` 0.1.1, React 19 컴포넌트와 `stratum.css` 하나)으로 그립니다. React 19 + Stratum을 Vite로 빌드하고, 빌드 결과를 Go embed 디렉터리에 커밋해 `go build`와 릴리스에는 Node가 필요 없게 합니다. CI는 커밋된 번들이 소스와 같은지 확인합니다.

범위 밖: 설정 마법사(`internal/webui/wizard/`)는 이번에 바꾸지 않습니다([§9 Q11](#9-열린-질문)).

이 문서에서 `pw/`는 `internal/webui/panelweb/`, `app/`은 `internal/app/`, `st/`는 Stratum 저장소의 `src/react/`입니다.

## 요약

- **화면 수를 12개에서 10개로 줄입니다.** 대기열(Queue)과 작업 기록(Jobs)을 **작업** 한 화면의 탭으로, 로그(Logs)를 **러너** 상세의 탭으로 합칩니다. API 토큰은 팀 화면에서 떼어 모든 역할이 쓰는 **API 토큰** 화면으로 옮깁니다. 빠지는 기능은 없게 설계했습니다([§1](#1-현재-패널-기능-목록)).
- **목록-상세는 Stratum `SplitView`로 통일합니다.** 작업·러너·호스트를 고르면 오른쪽 칸(폰에서는 전체 화면 시트)에 상세가 열리고, 주소는 `#/jobs/4828`처럼 항목을 가리킵니다. 예전 `#/queue?job=4828` 같은 주소는 새 주소로 바꿔 엽니다([§2.4](#24-주소-체계와-예전-링크)).
- **해시 라우팅을 유지합니다.** 서버 라우트를 늘리지 않고, GitHub Pages 샘플 패널과 이미 배포된 문서·CLI의 링크가 그대로 동작합니다.
- **상태 관리 라이브러리를 쓰지 않습니다.** SSE 스냅샷 저장소 하나를 `useSyncExternalStore`로 읽고, REST는 작은 훅 하나로 부릅니다. 지금 `app.js`의 재연결 상태 기계는 동작을 그대로 옮깁니다([§5](#5-클라이언트-데이터-모델)).
- **소스는 `internal/webui/panel/`, 빌드 결과는 지금과 같은 `internal/webui/panelweb/`** 입니다. `assets.go`, Pages 배포, 릴리스의 데모 tar가 같은 디렉터리를 계속 씁니다. 서버는 정적 파일 라우트만 바뀌고 CSP는 그대로 둡니다([§6](#6-빌드와-임베드)).
- **Stratum에 없는 것**(차트, 미터, 로그 뷰어, 설명 목록, 알림 띠, 복사 필드, 단계 표시, 영어 기본 문구)은 우선 패널 안의 작은 로컬 컴포넌트로 만들고, 범용인 것만 Stratum에 제안합니다([§8](#8-stratum에-없는-것)).

## 1. 현재 패널 기능 목록

모든 화면·동작·설정·API 호출·역할 제한과 v2에서의 위치입니다. "서버 권한"은 `a.require(...)`의 범위입니다(`app/panel_controls.go:58-108`, `app/panel_access.go`). `panelPermission`(`app/panel_controls.go:42`)은 `fleet:read`·`browser`를 모든 역할에, `hosts:write`·`jobs:write`를 operator 이상에, `admin`을 admin에게만 줍니다. "샘플"은 `runquill panel --demo`와 GitHub Pages 샘플 패널에서 보이는지입니다. 샘플 패널은 GET만 fixture로 답하고 나머지 요청은 모두 거부합니다(`pw/api.js:110-136`).

### 1.1 셸, 로그인, 공통 동작

| 현재 기능 | 위치 | v2 위치 |
| --- | --- | --- |
| 화면 목록과 역할·샘플 표시 여부(`routes`) | `pw/app.js:61-74` | [§2.2](#22-화면과-역할별-표시) 표. 라우트 표는 `src/routes.ts` 하나에 둡니다. |
| 볼 수 없는 화면·모르는 화면은 첫 화면으로 | `pw/app.js:176-187` | 같음. 라우터의 기본 동작. |
| "본문으로 건너뛰기" 링크 | `pw/index.html:14`, `pw/app.js:104` | `AppShell`의 `<main>` 앞에 둡니다(Stratum에 없으면 로컬 한 줄). |
| 좁은 화면 메뉴 버튼, Esc로 닫고 버튼으로 포커스 복귀 | `pw/app.js:132-146` | `AppShell` 폰 서랍(Escape·배경·스와이프로 닫힘, 포커스 복귀)이 대신합니다. |
| 화면 전환 시 제목(`data-view-heading`)에 포커스, `document.title = "화면 · Runquill"` | `pw/app.js:216-222` | 라우터가 `PageHeader` 제목에 포커스를 주고 제목을 바꿉니다. |
| 사이드바 개수: 대기 작업, 실행 중 러너, 끊긴 호스트(빨강) | `pw/app.js:415-430` | `Nav.Item`의 `badge`에 `Badge count`. 호스트는 `tone="danger"`. |
| 연결 상태(Live·Polling·Reconnecting·Connection lost·Paused while hidden·Sample data), "N초 전 갱신" | `pw/index.html:44-47`, `pw/app.js:247-253,391-396` | `Nav.Footer`의 연결 표시(로컬 `LiveStatus`), 폰은 `AppShell barActions`. |
| 상단 알림: 연결 끊김과 재시도 카운트다운(라이브 영역 아님), `fleet.docker_error`, `poll_failed` | `pw/app.js:398-413` | 본문 위 알림 띠(로컬 `Notice`). 같은 접근성 규칙(카운트다운은 낭독하지 않음). |
| SSE `GET /api/events`, 실패 시 `GET /snapshot` 폴링(3초→최대 30초), 60초 뒤 스트림 재시도, 4초 넘게 끊기면 "Reconnecting", 10초면 폴링 전환, 숨은 탭은 연결 끊음 | `pw/app.js:255-389`, `docs/panel.md#live-updates` | `src/live.ts`로 동작을 그대로 옮김([§5.2](#52-실시간-스냅샷)). |
| 명령 팔레트: ⌘K/Ctrl+K, 입력 칸 밖의 `/`, 화면 이동, 화면별 명령, 언어·테마, 새로 고침, 로그아웃, 입력했을 때만 러너·대기 작업·호스트 이동, 영어·한국어 둘 다 일치, 50개 제한, Page Up/Down | `pw/app.js:435-635` | Stratum `CommandPalette`. 차이는 [§8](#8-stratum에-없는-것) G7. |
| 언어 전환 링크(`?lang=ko` 또는 `?lang=en`, `runquill-language`에 저장, 새로 고침) | `pw/i18n.js:1108-1118`, `pw/app.js:98-102` | 사용자 메뉴와 팔레트. 같은 쿼리와 저장 키. |
| 테마 System·Light·Dark, `runquill-theme`에 저장, 첫 페인트 전에 적용(`runquill-login-theme`도 읽음) | `pw/theme.js`, `pw/app.js:87-96` | 같은 `theme.js`와 키. 사용자 메뉴 `Menu.RadioGroup`, 로그인 화면은 `SegmentedControl`. |
| 사용자 표시(`@login` + 역할), 로그아웃(`POST /api/logout` 후 새로 고침) | `pw/index.html:57-58`, `pw/app.js:656-665` | `Nav.Footer` 사용자 `Menu`. |
| 확인 대화상자(위험 동작은 빨강) | `pw/ui.js:427` | `ConfirmDialog`(`tone="danger"`). |
| 알림 토스트(`#alerts` assertive, `#toasts` polite) | `pw/ui.js:469`, `pw/index.html:103-106` | `toast()` + `Toaster`. 실패는 `tone: "danger"`. |
| "Source on GitHub" 링크 | `pw/app.js:127-128` | `Nav.Footer`. |
| 화면 모듈을 불러오지 못하면 "이 화면을 불러올 수 없습니다. 페이지를 새로 고치세요." | `pw/app.js:206-214` | 화면마다 오류 경계. 해시가 붙은 청크는 매니저를 업그레이드하면 이름이 바뀌어, 업그레이드 전부터 열려 있던 탭에서 더 자주 일어납니다. 문구에 "매니저가 업데이트되었을 수 있습니다"와 [새로 고침]을 둡니다([§4](#4-화면별-와이어프레임) 상태 표). |
| 로그인 화면: 제목, 안내, GitHub로 계속(`./auth/github`), 패널 주소, 언어·테마 | `pw/index.html:15-38` | [로그인 화면](#41-로그인). |
| `?signin=CODE` 실패 이유 5개(`expired`, `cancelled`, `github`, `not_enrolled`, `unavailable`), 한 번 보여 주고 주소에서 지움 | `pw/app.js:689-704`, `app/panel_access.go:324-358` | 같음([§3.1](#31-로그인)). |
| `GET /api/me`로 세션 확인. 401이면 로그인 화면, 다른 실패는 "세션을 확인할 수 없습니다" | `pw/app.js:706-719` | 같음. |
| 어느 요청이든 401이면 세션 종료 안내와 함께 로그인 화면 | `pw/api.js:8-11,38-42`, `pw/app.js:651` | 같음(`api.ts`의 `onUnauthorized`). |
| 샘플 패널: 시나리오 선택(First job, Webhook missed, Worker offline), 설명, 설치 링크(`README#install-a-public-release`), 시나리오가 가리키는 화면 열기 | `pw/index.html:62-72`, `pw/app.js:667-685`, `pw/demo.js` | 본문 위 샘플 띠(로컬 `Notice` + `Select`). 시나리오 초점은 새 주소로(`#/jobs/4831` 등). |
| 샘플 패널에서는 쓰기 버튼을 숨김(`ctx.can`이 항상 false) | `pw/app.js:155-169` | 같음(`can()`이 샘플에서 false). |

### 1.2 화면별

| 현재 화면(주소) · 서버 권한 · 샘플 | 기능과 API | v2 위치 |
| --- | --- | --- |
| **Overview** `#/overview` · 스냅샷(`fleet:read`) · 샘플 있음 | 용량(실행 중, 대기 = 준비+재시도, 가장 긴 대기, 슬롯 사용 = 사용/한도, 여유·예약·미할당), 호스트 상태(전체·준비·오프라인·사용 불가·유지 보수, K8s면 "Runtime targets"), GitHub 연결(저장소 범위, 공개 저장소 허용, App 연결 증거, 마지막 웹훅, 마지막 폴링/실패), 첫 작업 체크리스트 3단계 + 설정 안내 링크(`README#verify-your-first-job`) + 첫 작업 GitHub 링크, 대기 작업 상위 8개와 "나머지는 대기열에서" (`pw/views/overview.js`) | **개요**. 같은 정보에 "주의 필요" 목록을 더합니다([§4.3](#43-개요)). |
| **Queue** `#/queue`, `#/queue?job=ID` · 스냅샷 + `GET /api/jobs/{id}`(`fleet:read`) · 샘플 있음 | 대기·배정·정리됨 3개 목록, 저장소/작업 ID 검색, 풀 필터, 표(순서, 저장소, 작업, 풀+러너 OS, 상태+재시도 시각, 이유, 대기 시간, 호스트), 상세(상태, 발견 경로, 풀, 호스트, 러너, 라벨, OS, 이미지, 재시도 시각, 이유와 재시도 이유, 단계별 시간 막대, CPU·메모리, 관찰된 사건 타임라인, GitHub 링크), 상세가 선택한 작업을 따라 목록을 옮겨 다님, 사라지면 토스트 (`pw/views/queue.js:1-180`, `pw/views/jobinsight.js`) | ✅ 2단계. **작업** 탭 "대기", "실행 중", "정리됨" + 작업 상세 칸([§4.4](#44-작업)). |
| 〃 재시도·취소 · `jobs:write`(operator) | `POST /api/jobs/{id}/retry`(재시도 예정 작업만), `POST /api/jobs/{id}/cancel`, 둘 다 확인 후, 끝나면 새로 고침 (`pw/fleet.js:25-55`) | ✅ 3단계. 작업 상세 칸 아래(`PaneFooter`)의 "지금 재시도"·"작업 취소"와 칸이 열려 있을 때의 팔레트 명령. ⏳ 대기 탭 행의 `Menu`는 5단계로 넘깁니다([§7.5](#75-3단계-operator-동작-확인-결과), [§7.6](#76-4단계-관리-화면-확인-결과)). |
| 〃 "Missing a job?" 저장소 점검 · `admin` | `POST /api/diagnose {repository}`, 보고서 `<pre>`. admin이 아니면 `runquill diagnose OWNER/REPO` 안내만 (`pw/views/queue.js:190-206`). 샘플에서는 숨김 | ✅ 2·3단계. 안내 문구(`runquill diagnose OWNER/REPO`)는 대기 탭이 비었을 때 모든 역할에 보입니다. admin에게는 **작업** 머리글 동작과 빈 대기 탭의 "저장소 점검" 버튼, 팔레트 명령이 같은 `Dialog`(`POST`, 10초)를 엽니다. 샘플에서는 숨김. |
| 〃 명령: 대기·배정·정리됨 보기, 필터 지우기 | `pw/views/queue.js:281-293` | ✅ 2단계. 탭 전환 명령 4개(대기·실행 중·정리됨·기록), 필터 지우기. |
| **Jobs** `#/jobs`, `#/jobs?job=ID` · `GET /api/jobs?limit&repository&outcome&since&before`, `GET /api/jobs/{id}`(`fleet:read`) · 샘플 있음 | 7일 기록, 저장소·결과(대기/실행/완료/실패)·기간(1시간/24시간/7일) 필터, 25개씩 "이전 작업 더 보기"(`before` 커서), 새로 고침(필터와 열린 작업 유지), 표(저장소 GitHub 링크, 작업 ID+라벨, 결과+이유+재시도 시각, 풀+OS, 호스트, 대기·시작·종료 시각, 대기 시간), 상세(결과, 풀, 호스트, 라벨, OS, 시각, 단계 시간, 리소스), 대기 작업 재시도·취소(operator) (`pw/views/jobs.js`) | ✅ 2·3단계. **작업** 탭 "기록". 같은 상세 칸. 대기 작업의 재시도·취소는 칸 아래(행 버튼 대신). |
| 〃 명령: 필터 지우기, 더 보기 | `pw/views/jobs.js:218-225` | ✅ 2단계. 같음. |
| **Runners** `#/runners`, `#/runners?runner=NAME` · 스냅샷 · 샘플 있음 | 관찰 수와 관찰되지 않은 임대 수, 표(러너, 호스트, 풀, 런타임 상태, 준비 시각, 이미지+OS, GitHub 작업 또는 "추적되지 않은 러너"), 로그 링크(operator, local·ssh만), 사라지면 토스트 (`pw/views/runners.js`) | ✅ 2·3단계. **러너** 목록 + 러너 상세 칸. operator 이상(과 샘플)에게 "로그" 열의 "로그 보기" 링크(local·ssh만). |
| **Logs** `#/logs`, `#/logs?runner=NAME` · `GET /api/runners/{name}/logs?tail&follow=1`(`browser` + operator, 토큰 불가) · 샘플 있음(고정 줄) | 작업 중인 러너 목록, 줄 수(100~2000), 따라가기, 줄바꿈, 일시 정지(대기 줄 수), 중지, 다시 연결, 복사, 다운로드, 지우기, 최신으로, 5000줄·줄당 4000자 제한과 버린 줄 안내, ANSI 제거, 타임스탬프 표시, 15분 따라가기 한도, 러너가 끝나면 스트림 종료, K8s·agent:// 불가 안내, 상태 문구(연결 중, 마지막 N줄, 따라가는 중, 8 MiB 한도에서 멈춤, 15분 한도, 스트림 끊김, 러너 종료), 연결 끊김·429·502·503일 때만 [다시 연결] (`pw/views/logs.js`) | ✅ 3단계. **러너** 상세 칸의 "로그" 탭(`#/runners/NAME/logs`). 별도 메뉴 항목은 없앱니다([§2.1](#21-설계-원칙과-바뀌는-점)). ⏳ 폰에서 도구를 `Menu` 하나로 접는 것은 5단계로 넘깁니다([§7.5](#75-3단계-operator-동작-확인-결과), [§7.6](#76-4단계-관리-화면-확인-결과)). |
| 〃 명령: 러너별 로그 보기, 일시 정지/재개, 다시 연결, 중지, 다운로드 | `pw/views/logs.js:400-413` | ✅ 3단계. 같은 명령(러너별 로그 보기는 러너 화면 어디서나, 나머지는 로그 탭이 열려 있을 때). |
| **Hosts & pools** `#/hosts`, `#/hosts?host=NAME` · 스냅샷 · 샘플 있음 | 호스트 표(이름+오류, 풀, 상태+이미지 상태+마지막 응답+웜 러너, 슬롯 한도·사용·예약·가능), 풀 표(준비 호스트, 사용, 예약, 가능, 한도, 웜 러너, 사용률 막대), 사라지면 토스트 (`pw/views/hosts.js:85-175`) | ✅ 2단계. **호스트** 탭 "호스트"·"풀" + 호스트 상세 칸. |
| 〃 배정 중지/재개 · `hosts:write`(operator) | `POST /api/hosts/cordon {host, cordoned}`, 명령 "{host}에서 배정 중지/재개" | ✅ 3단계. 호스트 상세 칸 아래 동작과 팔레트 명령(확인 없음, v1과 같음). ⏳ 행 `Menu`는 5단계로 넘깁니다([§7.5](#75-3단계-operator-동작-확인-결과), [§7.6](#76-4단계-관리-화면-확인-결과)). |
| 〃 워커 등록 · `admin` | `agent://` 호스트에 `POST /api/hosts/{name}/enrollment`, 일회성 등록 명령과 재시작 명령, 복사, 만료 시각, 숨기기 (`pw/views/hosts.js:34-82`) | ✅ 3단계. 호스트 상세 칸의 "워커 등록"(`Dialog` 안 복사 필드). |
| 〃 서버 리소스 · `GET /api/hosts/resources[?disk=1]`(`fleet:read`) | 용량, 슬롯, 예약 CPU·메모리, 러너 사용량, 디스크(요청 시 측정, 측정 중이면 5초마다 다시 묻기, 10분 보관 안내), agent·K8s "Not available", 30초 갱신 (`pw/views/hosts.js:290-400`) | ✅ 2단계(디스크 측정은 `GET`이라 포함). **호스트** 탭 "리소스" + 호스트 상세 칸 요약. |
| 〃 탄력 한도 · `GET /api/elastic`(`fleet:read`) | 모드(off/observe/apply), 주기, 호스트별 여유·빌려준 CPU·메모리·잡아 둔 슬롯, 최근 결정(시각, 러너, 자원, 변화, 이유, 적용 여부·오류), 꺼져 있으면 설정 안내, 보고서 주기로 갱신 (`pw/views/hosts.js:180-288`) | ✅ 2단계. **호스트** 탭 "탄력 한도". |
| **Usage** `#/usage` · `GET /api/usage?from&to&group&repository`(`fleet:read`) · 샘플 있음 | 범위(어제·오늘, 7·30·90일, 직접 입력 UTC 날짜, 최대 400일), 묶음(일·저장소·풀·호스트), 저장소 필터, 막대 기준 지표, 합계, 일별 막대 차트, 표와 상대 막대, "가장 바쁜 N개만" 안내, 설명 문구 (`pw/views/usage.js:1-160,255-300`) | ✅ 2단계. **사용량** 탭 "집계". 날짜는 `DateRangePicker` 대신 네이티브 날짜 입력 둘입니다([§7.4](#74-2단계-읽기-화면-확인-결과)). |
| 〃 적정 크기 · `GET /api/usage/rightsizing?group&days`(`fleet:read`) | 풀/워크플로 작업별, 1~7일, 표본 수, CPU·메모리 판정(Tight, Oversized, Fits, Too few jobs, No limit), 메모리 종료 수 (`pw/views/usage.js:160-250`) | ✅ 2단계. **사용량** 탭 "적정 크기". |
| 〃 명령: 7·30·90일 보기, 묶음 바꾸기 | `pw/views/usage.js:307-326` | ✅ 2단계. 같음. |
| **Migrate** `#/migrate` · `GET /api/migrate/status`, `POST /api/migrate/preview`, `POST /api/migrate/open`(`admin`) · 샘플 있음(고정 미리보기, 열기 불가) | 1 권한(Contents, Pull requests, Workflows), 2 저장소와 runs-on→풀 매핑(최대 10라벨, 중복·표현식·self-hosted 거부, 설정의 풀 목록 `GET /api/settings`), 3 미리보기(기본 브랜치, 읽은 파일, 바꿀 작업, 파일별 diff 표와 남겨 둔 작업과 이유), 4 PR 열기(확인, 브랜치 `runquill/migrate-runs-on-<hash12>`, 기존 PR 링크, 미리보기가 낡으면 자동 갱신) (`pw/views/migrate.js`) | ✅ 4단계. **워크플로 이전** 화면, 같은 4단계([§4.11](#411-이전)). 매핑의 라벨은 v1처럼 쉼표로 구분한 입력입니다([§7.6](#76-4단계-관리-화면-확인-결과)). |
| **Backups** `#/backups` · `GET /api/backups/status`, `POST /api/backups/verify`, `POST /api/backup`(`admin`) · 샘플 있음(다운로드 불가) | 일정(켜짐, 주기, 보관 수, 디렉터리, 봉인, 원격 사본), 마지막 실행(결과, 시도·성공 시각, 파일, 크기, 봉인, 원격 업로드, 업로드 대기·오류), 켜는 방법(환경 변수 5개), 최신 백업 검증(최대 30분), 큐 백업 다운로드(진행률, 감사 기록), 복원은 `runquill restore` 안내, 새로 고침 (`pw/views/backups.js`) | ✅ 4단계. **백업** 화면. 검증은 화면을 떠나도 계속되고, 돌아오면 진행 중 표시와 결과를 보입니다. |
| 〃 명령: 새로 고침, 검증, 다운로드 | `pw/views/backups.js:203-211` | ✅ 4단계. 같음. |
| **Audit** `#/audit` · `GET /api/audit?limit&action&actor&before`(`admin`) · 샘플 없음 | 동작 종류 10개 필터, 행위자 검색, 50개씩 더 보기, 새로 고침, 표(시각, 동작+원래 코드, 행위자, 대상) (`pw/views/audit.js`) | ✅ 4단계. **감사 기록** 화면. 필터는 주소에(`#/audit?action=job.&actor=octocat`). 팔레트 명령 "새로 고침". |
| **Team & tokens** `#/team` · 샘플 없음 | (admin) `GET /api/team?after`, `POST /api/team {login, role}`, `PUT /api/team/{id} {role, disabled}`: 로그인 또는 숫자 ID로 추가, 역할 바꾸기(고른 뒤 "역할 변경" 버튼, 확인), 사용 중지(세션·토큰 폐기 확인)·다시 사용, 100명씩 이전/다음 (`pw/views/team.js:95-180`) | ✅ 4단계. **팀** 화면(admin). |
| 〃 API 토큰 · `browser`(모든 역할, 세션만) | `GET /api/tokens`, `POST /api/tokens {name, scope, days}`, `DELETE /api/tokens/{id}`: 범위 3개(viewer는 `fleet:read`만 선택 가능), 1~90일, 비밀값 한 번 표시와 복사, 폐기 확인 (`pw/views/team.js:24-90`) | ✅ 4단계. **API 토큰** 화면(모든 역할, 사용자 메뉴). |
| **Settings** `#/settings` · `GET`/`PUT /api/settings`(`admin`, `panel.operations`가 있을 때만 등록) · 샘플 없음 | 전체 슬롯 한도, 폴링 주기(웹훅 상태 안내), 기본 러너 이미지, Docker 워커(주소, 워커별 한도, agent:// 읽기 전용), 워커 풀(이름, CPU, 메모리, 웜 러너, 웜 교체 분, 이미지, 허용 OS, 워커 주소, 저장소 제한, 권한 있는 DinD, 공유 빌드 캐시), 러너 이미지 카탈로그 읽기 전용, `revision` 충돌, 편집기에 없는 풀 필드 보존(`max_per_repository` 등), `privileged`가 없으면 기본값 유지, 풀 이름 중복 거부, 저장 안 한 변경이 있으면 떠나기·다시 불러오기·탭 닫기 전에 확인, 백업 화면 링크 (`pw/views/settings.js`) | ✅ 4단계. **설정** 화면([§4.7](#47-설정), [§3.6](#36-설정과-저장-안-한-변경)). 떠나기는 `SaveBar` 가드(흔들림 + "버리고 이동")입니다. `cache_writes`·`max_per_repository` 같은 편집기에 없는 풀 필드는 그대로 저장됩니다. |

표시: ✅는 구현한 단계, ⏳는 5단계로 넘긴 것과 이유입니다. 4단계 뒤로 표시가 없는 행은 없습니다(1단계의 셸·개요·로그인은 §1.1과 [§7.3](#73-1단계-프런트엔드-확인-결과), 로그인 실패 이유는 [§7.6](#76-4단계-관리-화면-확인-결과)의 e2e로 확인).

확인 수준: 셸, Overview, Queue, Jobs, Runners, Hosts, Settings, Team, Audit는 파일 전체를 읽고 적었습니다. Logs, Usage, Backups, Migrate는 함수·요청·문구 단위로 확인했습니다. 구현 단계의 §1 점검(§7.1 5단계)은 파일 단위로 다시 합니다.

### 1.3 화면이 쓰지 않는 서버 라우트

`POST /mcp`(`app/mcp.go:129`), `GET /metrics`, `/healthz`, `/readyz`, `/webhook`, 워커 전송 라우트는 패널 UI가 부르지 않습니다. v2에서도 그대로입니다.

### 1.4 없애거나 바꾸는 것

| 항목 | 이유 |
| --- | --- |
| **Queue**와 **Jobs**가 따로 있는 것 | 같은 작업을 두 화면이 서로 다른 상세로 보여 줍니다. 한 화면의 탭과 한 상세 칸으로 합칩니다. 기능은 그대로입니다. |
| **Logs** 메뉴 항목 | 로그는 러너 하나에 딸린 정보입니다. 러너 상세의 탭으로 옮깁니다. 예전 주소는 그 탭을 엽니다. |
| **Team & tokens** 한 화면 | viewer·operator에게는 토큰만 보이는 "팀" 화면이 됩니다. 둘로 나눕니다. |
| 화면 상단 `Commands ⌘K` 버튼과 언어·테마 선택 상자 | 데스크톱 상단 막대가 없는 `AppShell` 구조에 맞춰 사이드바 아래 사용자 메뉴와 팔레트로 옮깁니다. 기능은 그대로입니다. |
| 설정에서 저장 안 한 채 다른 화면으로 갈 때 뜨는 확인 대화상자 | `SaveBar`의 `guard`가 대신합니다. 이동을 막고, 저장 바가 흔들리며 "버리고 이동"을 제안합니다. **동작이 바뀝니다**([§3.6](#36-설정과-저장-안-한-변경)). 탭 닫기 경고와 "서버에서 다시 불러오기"의 확인 대화상자는 그대로입니다. |

삭제하는 기능은 없습니다.

## 2. 새 정보 구조

### 2.1 설계 원칙과 바뀌는 점

- **대상 하나에 주소 하나.** 작업 4828, 러너 `runquill-demo-4826`, 호스트 `builder-west`는 어느 화면에서 가든 같은 상세 칸으로 열립니다. 지금은 같은 작업이 Queue와 Jobs에서 다른 상세로 보입니다.
- **목록-상세.** 작업·러너·호스트는 `SplitView`(`st/splitview/SplitView.tsx`)로 목록 옆에 상세를 엽니다. 폰(Stratum 기준 ≤900px)에서는 전체 화면 시트가 되고 뒤로 버튼·오른쪽 스와이프로 닫힙니다.
- **실시간 데이터와 기록 데이터를 화면 안에서 나눔.** 작업 화면의 대기·실행 중·정리됨 탭은 스냅샷에서, 기록 탭은 `GET /api/jobs`에서 옵니다. 탭 이름 옆에 "실시간"/"7일 기록" 표시로 구분합니다.
- **주의가 필요한 것부터.** 개요는 숫자보다 먼저 "주의 필요" 목록을 보여 줍니다. 목록은 스냅샷에 이미 있는 필드만으로 만듭니다(새 서버 필드 없음).
- **역할에 없는 것은 보이지 않게.** 쓰기 버튼은 역할이 없으면 그리지 않습니다(지금과 같음). 서버가 최종 판단을 하고, 403은 토스트로 보여 줍니다.

### 2.2 화면과 역할별 표시

```
AppShell
├─ Nav (sidebar)
│  ├─ Nav.Brand            runquill
│  ├─ Nav.List
│  │  ├─ Nav.Section 운영
│  │  │  ├─ 개요        #/overview        badge: 주의 필요 수 (warning)
│  │  │  ├─ 작업        #/jobs            badge: 대기 작업 수
│  │  │  ├─ 러너        #/runners         badge: 실행 중 러너 수
│  │  │  ├─ 호스트      #/hosts           badge: 오프라인+사용 불가 (danger)
│  │  │  └─ 사용량      #/usage
│  │  └─ Nav.Section 관리 (admin, 샘플에서는 이전·백업만)
│  │     ├─ 설정        #/settings
│  │     ├─ 팀          #/team
│  │     ├─ 감사 기록   #/audit
│  │     ├─ 백업        #/backups
│  │     └─ 워크플로 이전 #/migrate
│  └─ Nav.Footer
│     ├─ 명령 (⌘K)      → CommandPalette
│     ├─ LiveStatus     ● 실시간 · 3초 전 갱신
│     ├─ 사용자 Menu    @login · 역할 → API 토큰, 언어, 테마, 로그아웃
│     └─ GitHub 소스 링크
└─ main
   ├─ Notice 띠 (연결 끊김, docker_error, poll_failed, 샘플 시나리오)
   ├─ PageHeader (제목, 설명, 동작, 탭)
   └─ 화면 본문 (SplitView 또는 블록)
```

| 화면 | 주소 | viewer | operator | admin | 샘플 |
| --- | --- | --- | --- | --- | --- |
| 개요 | `#/overview` | 보기 | 보기 | 보기 | 있음 |
| 작업 | `#/jobs` | 보기 | + 재시도·취소 | + 저장소 점검 | 있음(쓰기 없음) |
| 러너 | `#/runners` | 보기 | + 로그 탭 | + 로그 탭 | 있음(고정 로그) |
| 호스트 | `#/hosts` | 보기 | + 배정 중지·재개 | + 워커 등록 | 있음 |
| 사용량 | `#/usage` | 보기 | 보기 | 보기 | 있음 |
| API 토큰 | `#/tokens` | `fleet:read`만 발급 | 모든 범위 | 모든 범위 | 없음 |
| 설정 | `#/settings` | | | 편집 | 없음 |
| 팀 | `#/team` | | | 편집 | 없음 |
| 감사 기록 | `#/audit` | | | 보기 | 없음 |
| 백업 | `#/backups` | | | 검증·다운로드 | 있음(쓰기 없음) |
| 워크플로 이전 | `#/migrate` | | | 미리보기·PR | 있음(고정 미리보기) |

설정 화면은 서버가 `panel.operations`를 가질 때만 `/api/settings`를 등록합니다(`app/panel_controls.go:74-82`). v2는 지금처럼 화면을 보여 주고 404를 오류 상태로 보여 줍니다. API로 기능 여부를 알리는 필드는 만들지 않습니다.

### 2.3 화면 구성 요약

| 화면 | 구성 |
| --- | --- |
| 개요 | 주의 필요 목록 → 용량 4칸 → 호스트 상태 띠 → GitHub 연결 → 첫 작업 체크리스트(끝나면 접힘) → 오래 기다린 작업 8개 |
| 작업 | 탭 대기·실행 중·정리됨(실시간)·기록(7일) → 필터 → `DataTable` → 작업 상세 칸(요약, 이유, 단계 시간, 리소스, 정보, 타임라인, 동작) |
| 러너 | 요약 줄 → `DataTable` → 러너 상세 칸(탭 정보·로그) |
| 호스트 | 탭 호스트·풀·리소스·탄력 한도 → 표 → 호스트 상세 칸(상태, 슬롯, 이미지, 웜 러너, 리소스, 동작) |
| 사용량 | 탭 집계·적정 크기 → 필터 → 합계 → 차트 → 표 |
| API 토큰 | 만들기 폼 → 새 비밀값(한 번) → 토큰 표 |
| 설정 | 섹션 전체·Docker 워커·워커 풀 → 카탈로그(읽기 전용) → `SaveBar` |
| 팀 | 추가 폼 → 구성원 표 → 이전/다음 |
| 감사 기록 | 필터 → 표 → 더 보기 |
| 백업 | 일정 → 마지막 실행 → 검증 → 켜는 방법 → 큐 백업 다운로드 |
| 워크플로 이전 | 1 권한 → 2 저장소와 매핑 → 3 미리보기 → 4 PR 열기 |

### 2.4 주소 체계와 예전 링크

해시 라우팅을 유지합니다. 경로 라우팅은 서버에 화면마다 `index.html` 대체 라우트를 더해야 하고, GitHub Pages 샘플 패널은 경로 대체를 할 수 없습니다. 쿼리 `?lang=`과 `?signin=`은 해시 앞의 진짜 쿼리로 그대로 둡니다.

```
#/overview
#/jobs                          대기 탭
#/jobs?tab=running|cleaned|history
#/jobs?tab=history&repo=OWNER/REPO&outcome=failed&range=86400
#/jobs?tab=waiting&q=forecast&pool=build
#/jobs/4828                     상세 칸. 탭은 그 작업이 있는 실시간 탭, 없으면 기록
#/runners
#/runners/NAME                  상세 칸 정보 탭
#/runners/NAME/logs             상세 칸 로그 탭 (operator 이상, 아니면 정보 탭)
#/hosts?tab=pools|resources|elastic
#/hosts/NAME                    호스트 이름은 encodeURIComponent (agent://edge-1 → agent%3A%2F%2Fedge-1)
#/usage?from=2026-11-01&to=2026-11-28&group=pool&repo=&metric=jobs
#/usage?tab=rightsizing&group=job&days=7
#/tokens  #/settings  #/team  #/audit?action=job.&actor=octocat  #/backups  #/migrate
```

필터는 주소에 둡니다. 새로 고침과 공유가 같은 화면을 엽니다. 값은 `history.replaceState`로 바꿔 뒤로 가기 기록을 늘리지 않습니다. 상세 칸을 열고 닫는 것만 기록에 남깁니다.

예전 링크는 라우터가 처음 읽을 때 새 주소로 바꿉니다(`replaceState`, 표 하나):

| 예전 | 새 주소 |
| --- | --- |
| `#/queue` | `#/jobs` |
| `#/queue?job=ID` | `#/jobs/ID` |
| `#/jobs?job=ID` | `#/jobs/ID` |
| `#/runners?runner=NAME` | `#/runners/NAME` |
| `#/logs` | `#/runners` |
| `#/logs?runner=NAME` | `#/runners/NAME/logs` |
| `#/hosts?host=NAME` | `#/hosts/NAME` |
| `#/team`(admin이 아님) | `#/tokens` |
| 그 밖의 모르는 화면, 역할 밖의 화면 | 볼 수 있는 첫 화면(지금과 같음) |

`docs/panel.md`와 `docs/panel.ko.md`가 예로 드는 `#/queue`, `#/jobs?job=ID`, `#/logs?runner=NAME`은 모두 이 표로 계속 열립니다. 새 문서는 새 주소를 씁니다. README의 `#install-a-public-release`(샘플 띠 링크)와 `#verify-your-first-job`(첫 작업 체크리스트 링크) 앵커는 README 쪽이 지켜야 하는 약속이고 v2도 같은 앵커를 가리킵니다.

## 3. 주요 사용자 흐름

### 3.1 로그인

1. 브라우저가 `/`를 열면 `GET /api/me`를 부릅니다. 200이면 셸을 그리고 처음 볼 화면으로 갑니다.
2. 401이면 로그인 화면입니다. `?signin=CODE`가 있으면 아래 문구를 `Notice`(위험 톤, `role="alert"`)로 보여 주고 주소에서 지웁니다. 모르는 코드는 무시합니다.

   | 코드 | 문구 |
   | --- | --- |
   | `expired` | 로그인이 만료되었습니다. GitHub로 계속을 다시 누르세요. |
   | `cancelled` | 로그인이 취소되었거나 이미 사용되었습니다. |
   | `github` | GitHub 로그인을 확인할 수 없습니다. |
   | `not_enrolled` | 이 GitHub 계정은 이 패널에 등록되어 있지 않습니다. |
   | `unavailable` | 패널 세션을 시작할 수 없습니다. |

   (영어 원문은 `pw/app.js:691-697`의 문장을 그대로 i18n 키로 씁니다.)
3. 401이 아닌 실패는 "패널이 세션을 확인할 수 없습니다. 나중에 다시 시도하세요."입니다.
4. "GitHub로 계속"은 `./auth/github`로 가는 링크입니다(`form-action 'none'` CSP 때문에 폼이 아닙니다).
5. 사용 중 어떤 요청이든 401을 받으면 스트림을 멈추고, 열린 대화상자를 닫고, 로그인 화면에 "세션이 끝났습니다"를 보여 주고 제목에 포커스를 줍니다.

### 3.2 한눈에 보는 상태

개요를 열면 첫 스냅샷이 올 때까지 각 칸은 `Skeleton`입니다. "주의 필요"는 아래를 우선순위대로 모은 목록이고, 항목마다 해당 상세로 가는 링크가 있습니다. 비어 있으면 "주의할 것이 없습니다"와 마지막 갱신 시각만 보입니다.

| 조건(스냅샷 필드) | 항목 | 링크 |
| --- | --- | --- |
| `fleet.docker_error` | 러너 호스트를 찾지 못했습니다 | `#/hosts` |
| `poll_failed` | GitHub 폴링이 실패했습니다 | `#/overview`의 GitHub 연결 |
| 호스트 `status` OFFLINE/UNAVAILABLE | 호스트 이름, 마지막 응답, `error` | `#/hosts/NAME` |
| 호스트 `image_status` failed/missing | 이미지 준비 실패 | `#/hosts/NAME` |
| 작업 `status` OFFLINE | 끊긴 호스트에 배정된 작업 | `#/jobs/ID` |
| 작업 `status` RETRY | 재시도 대기와 `retry_reason` | `#/jobs/ID` |
| 작업 `diagnostic.code` = `runner_memory_limit` | 메모리 한도로 종료됨 | `#/jobs/ID` |
| `last_webhook`·`last_poll` 둘 다 없음 | App 연결 증거를 기다리는 중 | 첫 작업 체크리스트 |

대기 시간 기준("N분 넘게 기다림")은 서버에 기준값이 없어 넣지 않습니다. 가장 오래 기다린 작업은 용량 칸에 그대로 보입니다([§9 Q5](#9-열린-질문)).

### 3.3 실패하거나 멈춘 작업 조사

1. 개요의 주의 항목, 팔레트에서 작업 번호 입력, 또는 GitHub에서 받은 `#/jobs/4828` 링크로 작업 상세 칸을 엽니다.
2. 칸 머리: `#4828`, 저장소, 상태 `Badge`, "GitHub에서 열기". 그 아래 이유(`diagnostic.message` → `retry_reason` → `pool_error` → 기록의 `failure`, 지금 순서 그대로, `serverText`로 번역)가 `Notice`로 먼저 보입니다.
3. 단계 시간 막대(발견, 스케줄러 대기, JIT 등록, 런타임 시작, GitHub 배정, 실행)와 범례. 리소스(CPU·메모리 최고치와 한도, 메모리 종료)는 `GET /api/jobs/{id}`로 불러오며, 404면 "기록 없음", 다른 실패는 칸 안의 오류입니다.
4. 정보(풀, 호스트, 러너 → `#/runners/NAME`, 라벨, OS, 이미지, 발견 경로)와 타임라인.
5. operator 이상이고 러너가 없는 대기 작업이면 "지금 재시도"(재시도 예정일 때만)와 "작업 취소"(위험 확인). 끝나면 토스트, 스냅샷 새로 고침, 작업이 대기열을 떠나면 칸을 닫고 목록에 포커스.
6. 러너가 있으면 "러너 로그 보기"가 `#/runners/NAME/logs`로 갑니다.
7. 작업이 스냅샷에서 사라지면(정리됨) 칸은 열린 채로 "정리됨" 탭으로 옮기고, 거기서도 사라지면 기록에서 다시 불러옵니다. 지금은 "작업이 더 이상 대기열에 없습니다" 토스트만 보여 줍니다.
8. 작업이 아예 안 보이면(웹훅 누락 의심) admin은 머리글 "저장소 점검"으로 `POST /api/diagnose`를 실행합니다. 다른 역할에게는 대기 탭이 비었을 때 `runquill diagnose OWNER/REPO` 안내를 보여 줍니다.

### 3.4 대기열

작업 화면 "대기" 탭이 기본입니다. 순서(`queue_position`), 저장소, 작업, 풀과 OS, 상태와 재시도 시각, 이유, 대기 시간, 호스트. 대기 시간은 스냅샷마다 늘어나므로 그 칸만 다시 그리고 행 애니메이션은 일으키지 않습니다(`DataTable`의 행 id는 작업 id). 검색(저장소·작업 번호)과 풀 필터는 주소에 남습니다. 필터가 상세 칸의 작업을 가려도 칸은 닫지 않습니다(지금과 같음).

### 3.5 호스트와 워커 풀

호스트 탭에서 행을 고르면 상세 칸이 열립니다: 상태(UP, CORDONED, OFFLINE+마지막 응답, UNAVAILABLE+오류), 이미지 상태, 슬롯 미터(사용·예약·가능/한도), 웜 러너 n/목표, 이 호스트의 서버 리소스 행, 이 호스트에서 도는 러너 목록(각각 `#/runners/NAME`). operator는 "배정 중지"/"배정 재개"(확인 없이 실행하고 토스트, 지금과 같음), admin은 `agent://` 호스트에 `enrollment_available`이면 "워커 등록". 등록은 `Dialog` 안에 등록 명령과 재시작 명령의 복사 필드, 만료 시각을 보여 주고, 닫으면 값을 지웁니다.

풀 탭은 풀별 합계 표와 사용률 미터, "전체 슬롯 한도 적용 전 용량" 설명. 리소스 탭은 서버 리소스 표와 "디스크 사용량 측정"(측정 중에는 버튼 비활성, 5초마다 결과 확인, 측정 시각과 10분 보관 안내). 탄력 한도 탭은 모드, 주기, 호스트별 여유 표, 최근 결정 표, 꺼져 있으면 `RUNQUILL_ELASTIC=observe` 안내. 리소스·탄력 탭은 그 탭이 보일 때만 폴링합니다(지금은 호스트 화면을 열면 둘 다 폴링).

### 3.6 설정과 저장 안 한 변경

1. 화면을 열면 `GET /api/settings`. 불러오는 동안 `Skeleton`.
2. 칸을 바꾸면 `SaveBar`(`docked`)가 "저장하지 않은 변경 N건"으로 올라옵니다. N은 `useFormDirty`(평평한 값만 `Object.is`로 비교)가 세는 전체 섹션 값과, 워커·풀 목록의 추가·삭제·변경 수를 따로 센 값의 합입니다. 목록은 원래 값의 JSON과 지금 값의 JSON을 항목별로 비교합니다.
3. ⌘S/Ctrl+S 또는 저장: 네이티브 검사 → 지금과 같은 본문 조립(원래 풀 객체를 펼친 뒤 덮어쓰기, `privileged`가 원래 없었으면 없는 채로, 허용 OS 이름 검사, 풀 이름 중복 거부, `revision`) → `PUT /api/settings`. 성공하면 응답을 새 기준으로 삼고 "저장했습니다". 실패하면 저장 바가 오류 톤과 서버 문구를 보여 주고 값은 그대로입니다. `revision` 충돌 문구("다른 세션에서 설정이 변경됐습니다")도 그대로 나옵니다.
4. 다른 화면으로 가려 하면 라우터의 떠나기 가드가 막고 `SaveBar`의 `guard`를 올립니다. 바가 흔들리며 "변경 N건을 저장하지 않아 이동하지 않았습니다"와 "버리고 이동"을 보여 줍니다. "버리고 이동"을 누르면 막혔던 주소로 갑니다. 지금의 확인 대화상자를 대신하는 **바뀐 동작**입니다.
5. 탭 닫기·새로 고침은 `SaveBar warnOnUnload`가 브라우저 경고를 띄웁니다(지금의 `beforeunload`와 같음).
6. "서버에서 다시 불러오기"(PageHeader 동작)는 변경이 있으면 `ConfirmDialog`(위험 톤)로 묻습니다. "되돌리기"는 서버를 부르지 않고 마지막으로 불러온 값으로 돌립니다.

### 3.7 팀과 토큰

- 팀(admin): 추가 폼(GitHub 로그인 또는 숫자 ID, 역할). 숫자만 쓰면 `PUT /api/team/{id}`, 아니면 `POST /api/team`(지금 규칙, `@1234`는 숫자 로그인). 표의 역할은 `Select`로 고르고 바뀌었을 때만 "역할 변경" 버튼이 나타나 확인 후 저장합니다. 사용 중지는 "세션과 토큰을 폐기합니다" 위험 확인. 100명씩 이전/다음(`after` 커서, 전체 수를 모르므로 `Pagination` 대신 버튼 둘).
- API 토큰(모든 역할): 이름, 범위(viewer는 `fleet:read`만 고를 수 있고 나머지는 비활성 이유 표시), 유효 일수 1~90. 만들면 비밀값을 읽기 전용 복사 필드로 한 번 보여 주고 포커스·선택합니다. "숨기기"나 화면을 떠나면 지웁니다. 폐기는 위험 확인.

### 3.8 백업

admin만 엽니다. 일정·마지막 실행·검증·켜는 방법은 `GET /api/backups/status` 하나로 그립니다. "최신 백업 검증"은 최대 30분 걸릴 수 있어 버튼이 `loading` 상태로 남고, 화면을 떠나도 요청은 계속되며 돌아오면 결과(서버가 `verification`으로 기억)를 보여 줍니다. "큐 백업 다운로드"는 스트리밍 다운로드 진행률(`Progress`, 전체 크기를 모르면 불확정)과 받은 바이트, 끝나면 `runquill-queue-backup.sqlite3` 저장. 복원은 `runquill restore` 안내만 합니다.

### 3.9 워크플로 이전

1. 권한: 상태를 불러오는 동안 "확인 중", 빠진 권한은 위험 `Badge`와 App 권한 안내, 실패하면 "다시 확인".
2. 저장소와 매핑: 저장소 `Input`, 매핑 행(runs-on 라벨 `TagInput`, 풀 `Select`), 행 추가·삭제. 풀 목록은 스냅샷의 풀 + `GET /api/settings`의 풀(실패해도 스냅샷 풀로 진행). 검증 규칙(1~10 라벨, 100자, 제어 문자·`${{` 금지, self-hosted·Runquill 라벨 거부, 같은 라벨 묶음 중복 거부, 최소 한 행)은 지금 규칙 그대로, 문제 칸에 `Field error`.
3. 미리보기: `POST /api/migrate/preview`(95초 제한). 기본 브랜치, 읽은 파일 수, 바꿀 작업 수, 파일마다 `Accordion` 항목(바뀐 작업 수·남긴 작업 수, diff 표, 남긴 작업과 이유). 파일이 없거나 바뀔 것이 없으면 그 안내. 입력을 바꾸면 미리보기가 낡았다고 표시하고 4단계를 잠급니다.
4. PR 열기: 요약 문장(브랜치 `runquill/migrate-runs-on-<hash 12자>`, 작업·파일 수, "병합하지 않습니다") → `ConfirmDialog` → `POST /api/migrate/open {..., hash}`. 성공하면 PR 링크. 이미 브랜치가 있으면 서버가 준 `pull_request` 링크와 그 코드의 문구, 미리보기가 낡았다는 코드면 자동으로 다시 미리보고 "다시 검토하세요".

샘플 패널은 고정 미리보기를 보여 주고 4단계 버튼을 그리지 않습니다.

### 3.10 사용량

집계 탭: 범위 `SegmentedControl`(어제·오늘, 7일, 30일, 90일, 직접) + 직접이면 `DateRangePicker`(UTC, 최대 400일 검사), 묶음 `Select`, 저장소 검색, 막대 지표 `Select`. 합계 칸, 묶음이 "일"이면 일별 막대 차트(로컬 `BarChart`), 표(상대 막대 칸 포함), `truncated`면 "가장 바쁜 N개만, 합계는 전체" 안내, 정의 설명 문구. 적정 크기 탭: 묶음(풀/워크플로 작업), 기간(1·2·3·5·7일), 표(표본 n/전체, CPU·메모리 판정 `Badge`와 최고치/한도, 메모리 종료 수), 판정 기준 설명.

### 3.11 감사 기록

admin. 동작 종류 `Select`(전체, 로그인, 팀 변경, API 토큰, 호스트 유지 보수, 작업 동작, 설정 변경, 큐 백업, 이전, 러너 로그 열람)와 행위자 검색은 주소에 남고, 동작 종류를 바꾸면 바로, 검색은 입력이 멈춘 뒤 불러옵니다. 표(시각, 번역된 동작과 원래 코드, 행위자, 대상), "이전 기록 더 보기"(`before` = 마지막 id, 50개 미만이면 숨김), 새로 고침.

### 3.12 샘플 패널

`runquill panel --demo`(`app/panel.go:279`)와 GitHub Pages가 같은 빌드 결과를 씁니다. 차이는 `index.html`의 `data-mode`뿐이고(라이브 서버가 `demo`를 `live`로 바꿔 보냄, `app/panel.go:210-221`), 앱은 그 값으로 샘플 모드를 정합니다.

- 본문 위 샘플 띠: 시나리오 `Select`(첫 작업, 웹훅 누락, 워커 오프라인), 시나리오 설명, "Runquill 설치" 링크. 시나리오를 바꾸면 그 시나리오의 초점 화면을 엽니다(`first` → `#/overview`, `missed` → `#/jobs/4831`, `offline` → `#/jobs/4835`).
- `/api/me`를 부르지 않고 로그인 없이 셸을 그립니다. 사용자 메뉴에는 언어·테마만 있습니다.
- 연결 표시는 "샘플 데이터". SSE를 열지 않습니다.
- 모든 GET은 화면별 fixture(`src/demo/<화면>.ts`)가 답하고, 나머지는 "샘플 패널에서는 사용할 수 없습니다"로 거부합니다(지금 `createApi`와 같음).
- 쓰기 버튼은 그리지 않습니다. 로그 탭은 고정 줄을 보여 주고 따라가기를 끕니다.
- fixture와 시나리오 코드는 별도 청크(`assets/demo-*.js`)로만 빌드되고, 라이브 서버는 이 청크를 404로 거부합니다([§6.4](#64-서버-변경)).

## 4. 화면별 와이어프레임

공통 상태 규칙(모든 화면):

| 상태 | 표시 |
| --- | --- |
| 첫 스냅샷 전 | 스냅샷을 쓰는 칸은 `Skeleton`, 표는 `DataTable loading` |
| REST 불러오는 중 | `DataTable loading` 또는 `Spinner`(버튼 안이면 `Button loading`) |
| 비어 있음 | `EmptyState`(제목 + 한 줄 설명 + 필요하면 동작) |
| REST 오류 | 그 블록 안의 `Notice`(위험) + "다시 시도". 401은 로그인 화면, 403은 "권한이 없습니다" |
| 화면 청크를 불러오지 못함 | 화면 오류 경계: "이 화면을 불러올 수 없습니다. 매니저가 업데이트되었을 수 있습니다." + [새로 고침]. 셸과 연결은 그대로 |
| 연결 끊김(오프라인) | 본문 위 `Notice`: "연결이 끊겼습니다. N초 뒤 다시 시도합니다. 마지막 스냅샷을 보여 줍니다." 화면은 마지막 스냅샷으로 남고, `LiveStatus`가 "연결 끊김" |
| 숨은 탭 | 연결을 끊고 `LiveStatus` "숨김 중 일시 정지". 돌아오면 다시 연결 |
| 폰(≤900px) | `AppShell`이 사이드바를 서랍으로, 상단 막대(메뉴 버튼, 제목, `barActions`)를 보여 줌. `SplitView` 상세는 전체 화면 시트. 넓은 표는 앞 열 `sticky`와 가로 스크롤 |

그림의 `[ ]`는 버튼, `( )`는 `Badge`, `▾`는 `Select`/`Menu`, `▮▮▯`는 미터입니다.

### 4.1 로그인

```
┌──────────────────────────────────────────────┐
│                 runquill                     │
│        Runquill에 로그인                      │  h1, 포커스 대상
│  관리자가 등록한 GitHub 계정을 사용하세요.        │
│ ┌──────────────────────────────────────────┐ │
│ │ ⚠ 이 GitHub 계정은 등록되어 있지 않습니다.    │ │  Notice(danger, role=alert), ?signin 있을 때만
│ └──────────────────────────────────────────┘ │
│        [ GitHub로 계속 ]                      │  <a class="btn" href="./auth/github"> (Stratum의 기본 강조 버튼 클래스)
│  패널 주소  runquill.example.com              │  mono
│  English · [시스템|밝게|어둡게]                 │  링크 + SegmentedControl
└──────────────────────────────────────────────┘
```

Stratum: `Button` 스타일의 링크(클래스 `btn`), `SegmentedControl`, 로컬 `Notice`. 폰: 같은 세로 배치, 가로 16px 여백.

### 4.2 셸

```
데스크톱 (>900px)
┌───────────────┬───────────────────────────────────────────────────────┐
│ runquill      │ ⚠ 연결이 끊겼습니다. 12초 뒤 다시 시도…  (Notice)          │
│               │───────────────────────────────────────────────────────│
│ 운영           │ 작업                                [저장소 점검]       │ PageHeader
│ ▸ 개요    (2) │ 실시간 대기열과 7일 기록                                   │
│ ▸ 작업    (5) │ [대기 5][실행 중 3][정리됨 8] │ [기록]                    │ Tabs.List
│ ▸ 러너    (3) │                                                       │
│ ▸ 호스트  (1) │  …화면 본문…                                            │
│ ▸ 사용량      │                                                       │
│ 관리           │                                                       │
│ ▸ 설정        │                                                       │
│ ▸ 팀          │                                                       │
│ ▸ 감사 기록    │                                                       │
│ ▸ 백업        │                                                       │
│ ▸ 워크플로 이전 │                                                       │
│───────────────│                                                       │
│ [명령  ⌘K]    │                                                       │
│ ● 실시간·3초 전 │                                                       │
│ @octo · Admin▾│ → API 토큰 / 언어 / 테마 ◉시스템 ○밝게 ○어둡게 / 로그아웃  │ Menu
│ GitHub 소스 ↗  │                                                       │
└───────────────┴───────────────────────────────────────────────────────┘

폰 (≤900px)
┌──────────────────────────────┐
│ ☰  작업              ⌘  ●    │  AppShell 상단 막대, barActions = 팔레트 IconButton, LiveStatus 점
├──────────────────────────────┤
│ ⚠ 연결이 끊겼습니다…            │
│ 작업                           │
│ [대기][실행 중][정리됨][기록] →  │  가로 스크롤 탭
│ …                              │
└──────────────────────────────┘
```

Stratum: `AppShell`(`sidebar`, `barActions`, `drawerLabel`, `menuLabel`, `renderLink`), `Nav`, `Nav.Brand`, `Nav.List`, `Nav.Section`, `Nav.Item`(`label`, `icon`, `badge`, `href`, `current`), `Nav.Footer`, `Nav.RailToggle`, `Menu`(+`Menu.RadioGroup`, `Menu.RadioItem`, `Menu.Separator`), `Avatar`, `Badge`, `CommandPalette`, `Toaster`, `PageHeader`, `Tabs`. 로컬: `LiveStatus`, `Notice`.

### 4.3 개요

```
개요                                                      PageHeader
┌ 주의 필요 (3) ──────────────────────────────────────────┐
│ ● builder-east  사용 불가 · 이미지를 받는 중 …          → │  링크 목록
│ ● #4828 pattern-kit  재시도 대기 · 러너 이미지 없음…     → │
│ ● GitHub 폴링 실패 · 마지막 성공 3분 전                 → │
└──────────────────────────────────────────────────────┘
┌ 실행 중 ─┐┌ 대기 ─────────┐┌ 가장 긴 대기 ┐┌ 슬롯 ──────────┐
│   2      ││ 2 (준비1·재시도1)││   9초       ││ 3 / 4  ▮▮▮▯     │  StatTile ×4 (로컬)
└──────────┘└───────────────┘└────────────┘│ 여유1·예약0    │
                                           └───────────────┘
┌ 호스트 ─────────────────────────────────────────────────┐
│ 4대  (준비 2) (오프라인 0) (사용 불가 1) (유지 보수 0)        │  Badge dot
└──────────────────────────────────────────────────────┘
┌ GitHub 연결 ───────────────┐┌ 첫 작업 설정 ──── 설정 안내 ↗ ┐
│ 접근 범위   선택한 저장소     ││ ✓ GitHub App 연결            │  Steps (로컬)
│ 공개 저장소 허용, 포크 거부   ││ ✓ 첫 워크플로 실행            │
│ App 연결   (활동 확인됨)     ││ ○ 정리 확인  …              │
│ 마지막 웹훅 8초 전           ││ GitHub에서 첫 작업 보기 ↗     │
│ 마지막 폴링 3초 전           │└─────────────────────────────┘
└───────────────────────────┘
┌ 오래 기다린 작업 ─────────────────────────── 작업 열기 → ┐
│ 순서 저장소           작업   풀      상태     이유     대기 │  DataTable compact, 8행
└──────────────────────────────────────────────────────┘
```

- 첫 작업 세 단계가 모두 끝나면 체크리스트는 `Disclosure`로 접혀 "첫 작업 설정 완료"만 보입니다(지금은 항상 펼침).
- 빈 상태: 주의 필요 없음 → "주의할 것이 없습니다". 대기 없음 → `EmptyState` "대기열이 비어 있습니다".
- 폰: 타일 2×2, 나머지는 한 열.
- Stratum: `PageHeader`, `Badge`, `DataTable`, `Disclosure`, `Skeleton`, `EmptyState`. 로컬: `StatTile`, `Meter`, `KeyValue`, `Steps`.

### 4.4 작업

```
작업                                                   [저장소 점검]   (admin)
[대기 2][실행 중 2][정리됨 1] │ [기록]                        Tabs
[🔍 저장소 또는 작업 번호     ] [풀: 전체 ▾]                     SearchInput, Select
┌──────────────────────────────────────┬──────────────────────────────────┐
│순서 저장소        작업   풀/OS   상태  …│ #4828  garden-lab/pattern-kit  ✕ │ SplitView.PaneHeader
│ 1  forecast       #4827 기본    대기   │ (재시도) GitHub에서 열기 ↗  ‹ ›   │ useSplitViewNavigation
│ 2 ▸pattern-kit    #4828 build  재시도 ◀│──────────────────────────────────│
│                                      │ ⚠ 러너 이미지 "ubuntu2004"가      │ Notice
│                                      │   카탈로그에 없습니다 …           │
│                                      │ 단계 시간                         │
│                                      │ ▇▇▇▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁            │ StageBar (로컬)
│                                      │ 발견 15초 · 대기 — · 시작 —       │
│                                      │ 리소스   표본 없음                 │
│                                      │ 정보  풀 build · OS ubuntu2004 …  │ KeyValue
│                                      │ 타임라인  생성 · 발견 · …          │
│                                      │──────────────────────────────────│
│                                      │        [지금 재시도] [작업 취소]    │ PaneFooter (operator)
└──────────────────────────────────────┴──────────────────────────────────┘

기록 탭
[저장소 OWNER/REPO ][결과: 전체 ▾][기간: 전체 ▾]            [새로 고침]
│저장소 ↗  작업(라벨)  결과(이유)  풀/OS  호스트  대기  시작  종료  대기시간│
                                  25개 표시   [이전 작업 더 보기]
```

- 상태: 첫 스냅샷 전 `loading`. 대기 비어 있음 → `EmptyState` "대기열이 비어 있습니다" + (admin이 아니면) "작업이 안 보이면 `runquill diagnose OWNER/REPO`를 실행하세요". 필터 결과 없음 → "필터에 맞는 작업이 없습니다" + [필터 지우기]. 기록 오류 → "작업 기록을 불러올 수 없습니다" + 다시 시도. 상세 리소스 실패는 칸 안에서만.
- 저장소 점검 `Dialog`: 저장소 `Input`(OWNER/REPO), [점검], 결과 `Notice`(완료/주의) + 보고서 `<pre>`(라이브 영역).
- 폰: 표는 저장소·작업·상태 3열과 가로 스크롤, 행을 누르면 전체 화면 시트, 시트 아래에 동작.
- Stratum: `Tabs`, `SearchInput`, `Select`, `DataTable`(`onRowAction`, `getRowId`, `empty`, `loading`, `density`), `SplitView`(+`List`, `Pane`, `PaneHeader`, `PaneBody`, `PaneFooter`), `useSplitViewNavigation`, `Button`, `ConfirmDialog`, `Dialog`, `Badge`, `EmptyState`. 로컬: `StageBar`, `ResourceMeters`, `KeyValue`, `Notice`.

### 4.5 러너

```
러너                                                    2개 관찰 · 0개 미관찰 임대
┌───────────────────────────────────────┬──────────────────────────────────────┐
│러너              호스트  풀  상태  준비  …│ runquill-demo-4826            ✕      │
│▸runquill-demo-4826 local 기본 (실행) 10:02│ [정보][로그]                          │ Tabs (로그는 operator, local·ssh)
│ runquill-demo-4824 b-west build(실행)…  │──────────────────────────────────────│
│                                       │ 줄 [200 ▾] ☑따라가기 ☐줄바꿈            │ Select, Switch
│                                       │ [일시 정지][중지][복사][다운로드][지우기] │
│                                       │ ┌──────────────────────────────────┐ │
│                                       │ │10:02:11 Listening for Jobs        │ │ LogView (로컬)
│                                       │ │10:02:14 Running job: build        │ │
│                                       │ │…                     [최신으로 ↓] │ │
│                                       │ └──────────────────────────────────┘ │
│                                       │ 1,204줄 · 새 줄 32개 대기 중            │ 라이브 영역 아님
└───────────────────────────────────────┴──────────────────────────────────────┘
```

- 정보 탭: 호스트(→ `#/hosts/NAME`), 풀, 런타임 상태(관찰 없음이면 "임대가 관찰되지 않음"), 준비 시각, 이미지, OS, 작업(→ `#/jobs/ID` 또는 "추적되지 않은 러너"), GitHub 링크.
- 로그 탭: K8s·`agent://`면 탭 대신 "이 러너는 로그를 읽을 수 없습니다" 이유. viewer에게는 탭이 없습니다. 오류는 탭 안 `Notice` + [다시 연결]. 러너가 끝나면 "러너가 종료되어 따라가기를 멈췄습니다".
- 폰: 로그 칸은 전체 화면 시트, 도구는 `Menu` 하나로 접음.
- Stratum: `DataTable`, `SplitView`, `Tabs`, `Select`, `Switch`, `Button`, `Menu`, `Badge`. 로컬: `LogView`, `KeyValue`.

### 4.6 호스트

```
호스트                                               (Kubernetes면 "런타임 대상")
[호스트][풀][리소스][탄력 한도]
┌─────────────────────────────────────────┬──────────────────────────────────┐
│호스트        풀   상태          한도 사용 예약 가능│ builder-east                 ✕   │
│local        기본 (UP) 웜 1/1     2   1   0   1 │ (사용 불가) 이미지를 받는 중       │
│builder-west build(UP) 웜 1/2     2   1   0   1 │ pulling ghcr.io/…/runner-build  │
│▸builder-east build(사용 불가)    2   0   0   0 │ 슬롯 0/2 ▯▯   웜 0/2             │
│agent://edge-1 기본 (UP)          1   0   0   1 │ 리소스 8 CPU · 16 GiB …          │
│                                         │ 러너 없음                          │
│                                         │──────────────────────────────────│
│                                         │ [배정 중지]  [워커 등록](agent만)   │
└─────────────────────────────────────────┴──────────────────────────────────┘

풀 탭:  풀 │ 준비 호스트 │ 사용 │ 예약 │ 가능 │ 한도 │ 웜 러너 │ 사용률 ▮▮▯
리소스 탭: 호스트 │ 용량 │ 슬롯 ▮▯ │ 예약 CPU ▮▯ │ 예약 메모리 ▮▯ │ 러너 사용량 │ 디스크   [디스크 사용량 측정]
탄력 탭: 모드 (관찰만) · 주기 30초 / 호스트별 여유 표 / 최근 결정 표
```

- 상태: 호스트 관찰 없음 → `EmptyState` "아직 호스트 관찰이 없습니다". `docker_error` → 표 위 `Notice`. 리소스·탄력 오류는 그 탭 안. 탄력 꺼짐 → `EmptyState`와 환경 변수 안내.
- 폰: 호스트 표는 이름·상태·가능 3열, 상세는 시트.
- Stratum: `Tabs`, `DataTable`, `SplitView`, `Button`, `Dialog`, `Badge`, `EmptyState`. 로컬: `Meter`, `KeyValue`, `CopyField`, `Notice`.

### 4.7 설정

```
설정                                   [서버에서 다시 불러오기]
매니저를 재시작하지 않고 적용됩니다.
┌ 전체 ─────────────────────────────────────────────┐   FormSection
│ 전체 슬롯 한도   [ 4      ]                          │   NumberInput
│ GitHub 폴링 주기 [ 30s    ]  웹훅이 켜져 있습니다 …     │   Input + description
│ 기본 러너 이미지 [ ghcr.io/…/runquill-runner:v… ]    │
└──────────────────────────────────────────────────┘
┌ Docker 워커 ──────────────────────── [워커 추가] ──┐   (backend = docker일 때만)
│ 워커 주소                 워커별 한도                 │
│ [local                ] [전체 한도 사용] [제거]       │
│ agent://edge-1 (읽기 전용, 배포에서 설정)              │
└──────────────────────────────────────────────────┘
┌ 워커 풀 ──────────────────────────── [풀 추가] ────┐
│ 러너 OS 이름: ubuntu22.04, debian13 (카탈로그, 읽기 전용) │
│ ▾ build                                            │   Accordion item, Fieldset
│   이름 [build] CPU [2] 메모리(MiB)[4096]              │
│   웜 러너 [2] 웜 교체(분) [30]                         │
│   풀 이미지 [기본 이미지 사용]  허용 OS [ubuntu22.04 ×] │   TagInput
│   워커 주소 [builder-west ×][builder-east ×]          │   TagInput
│   ☑ DinD(권한 있음) ☐ 공유 빌드 캐시 ☐ 저장소 제한      │   Switch / Checkbox
│   허용 저장소 [garden-lab/forecast ×]                  │
│                                       [풀 제거]     │
└──────────────────────────────────────────────────┘
큐 백업은 백업 화면에 있습니다.
╔══════════════════════════════════════════════════╗
║ 저장하지 않은 변경 3건          [되돌리기] [저장 ⌘S]   ║   SaveBar docked
╚══════════════════════════════════════════════════╝
```

- 상태: 불러오는 중 `Skeleton`, 404(운영 설정 없음)·오류 → `Notice` + 다시 시도. 저장 오류는 `SaveBar` 오류 톤.
- 허용 OS·워커 주소·저장소를 `TagInput`으로 바꾸는 것은 입력 방식 변경입니다. 저장 형식(배열)은 같습니다.
- 폰: 한 열, 저장 바는 화면 아래 고정.
- Stratum: `PageHeader`, `FormSection`, `Fieldset`, `Field`, `Input`, `NumberInput`, `TagInput`, `Switch`, `Checkbox`, `Accordion`, `Button`, `SaveBar`, `useFormDirty`, `ConfirmDialog`.

### 4.8 팀

```
팀
[GitHub 로그인 또는 ID ][역할: Viewer ▾] [구성원 저장]
│구성원     GitHub ID   역할                 상태     │
│@octocat   583231      [Operator ▾][역할 변경] (활성) [사용 중지]│
                                        [이전] [다음]
```

Stratum: `Input`, `Select`, `Button`, `DataTable`, `Badge`, `ConfirmDialog`. 빈 상태는 없음(자기 자신이 있음). 오류 → `Notice`.

### 4.9 API 토큰

```
API 토큰
스크립트와 연동에 쓰는 범위가 정해진 자격 증명입니다.
[이름: Metrics collector ][범위: 플릿 상태 읽기 ▾][유효 일수 30] [토큰 만들기]
┌ 새 토큰 ─────────────────────────────────────────┐
│ 지금 복사하세요. 값은 한 번만 보입니다.                │
│ [rq_xxxxxxxxxxxxxxxxxxxxxxxx      ][복사] [숨기기]   │ CopyField (로컬)
└────────────────────────────────────────────────┘
│이름          범위                 만료        │
│Metrics       플릿 상태 읽기 fleet:read 12월 28일 [폐기]│
```

빈 상태: `EmptyState` "활성 API 토큰이 없습니다. 스크립트가 접근해야 할 때 만드세요." Stratum: `Input`, `Select`, `NumberInput`, `Button`, `DataTable`, `ConfirmDialog`, `EmptyState`.

### 4.10 감사 기록

```
감사 기록                                              [새로 고침]
[동작: 전체 ▾][행위자: 로그인 또는 GitHub ID ]
│시각        동작                      행위자     대상          │
│10:02:11    작업 취소  job.cancel      @octocat   4828          │
                                       [이전 기록 더 보기]
```

빈 상태: "감사 기록이 아직 없습니다". Stratum: `Select`, `SearchInput`, `DataTable`, `Button`.

### 4.11 이전

```
워크플로 이전
① 권한  ② 저장소와 라벨  ③ 미리보기  ④ PR 열기                Steps (로컬)
┌ 1. 권한 ────────────────────────────────────┐
│ Contents (허용) · Pull requests (허용) · Workflows (없음) │
└────────────────────────────────────────────┘
┌ 2. 저장소와 라벨 ──────────────────────────────┐
│ 저장소 [garden-lab/forecast]                     │
│ runs-on 라벨                  풀                  │
│ [ubuntu-latest ×]            [build ▾]  [제거]   │  TagInput, Select
│ [변환 추가]                       [변경 미리보기]   │
└────────────────────────────────────────────┘
┌ 3. 미리보기 ─────────────────────────────────┐
│ 기본 브랜치 main · 파일 3개 · 작업 4개 변경        │
│ ▾ .github/workflows/ci.yml   2개 변경, 1개 그대로  │  Accordion
│   이전 새  │ 줄                                  │  DiffView (로컬)
│   12      │ -   runs-on: ubuntu-latest          │
│      12   │ +   runs-on: runquill-…-build       │
└────────────────────────────────────────────┘
┌ 4. PR 열기 ──────────────────────────────────┐
│ 새 브랜치 runquill/migrate-runs-on-1a2b3c4d5e6f에 …│
│                                  [PR 열기]       │
└────────────────────────────────────────────┘
```

Stratum: `Input`, `TagInput`, `Select`, `Button`, `Accordion`, `Badge`, `ConfirmDialog`. 로컬: `Steps`, `DiffView`, `KeyValue`. 폰: diff는 가로 스크롤.

### 4.12 백업

```
백업                                                     [새로 고침]
복원은 매니저에서 `runquill restore`로 합니다. 패널은 복원하지 않습니다.
┌ 일정 ──────────────┐┌ 마지막 예약 실행 ─────────────┐
│ 예약 백업 (켜짐)     ││ 결과 (성공) · 시도 3시간 전      │  KeyValue
│ 주기 24시간 · 7개 보관 ││ 파일 runquill-….sqlite3 12 MB │
│ 봉인 (켜짐)          ││ 원격 업로드 (업로드됨)           │
└────────────────────┘└──────────────────────────────┘
┌ 백업 검증 ─────────────────────────── [최신 백업 검증] ┐
└──────────────────────────────────────────────────┘
┌ 큐 백업 ─────────────────────────── [큐 백업 다운로드] ┐
│ ▮▮▮▮▮▯▯▯ 62%  다운로드 중…                            │  Progress
└──────────────────────────────────────────────────┘
```

예약 백업이 꺼져 있으면 검증 블록 대신 켜는 방법(환경 변수 5개와 설명) `EmptyState`. Stratum: `Button`, `Progress`, `Badge`, `EmptyState`. 로컬: `KeyValue`.

### 4.13 사용량

```
사용량
[집계][적정 크기]
[어제·오늘|7일|30일|90일|직접] [2026-11-01 – 2026-11-28] [묶음: 일 ▾] [저장소] [막대: 완료 작업 ▾]
┌ 합계 ── 11월 1일 – 11월 28일, UTC ──────────────────────┐
│ 완료 214 · 실패 3 · 러너 시간 41시간 · 중앙 대기 6초 · p95 41초 │
└───────────────────────────────────────────────────┘
▁▂▅▇▃▂▁▂▆▇▅▃ …                                          BarChart (로컬, 일 묶음일 때만)
│일        완료  실패  러너 시간  중앙 대기  p95 대기  상대 ▮▮▯ │
```

빈 상태: "이 범위에 완료된 작업이 없습니다". Stratum: `Tabs`, `SegmentedControl`, `DateRangePicker`, `Select`, `SearchInput`, `DataTable`, `Badge`. 로컬: `BarChart`, `Meter`.

## 5. 클라이언트 데이터 모델

런타임 의존성은 `react`, `react-dom`, `@yakihyuk0728/stratum-ui` 셋뿐입니다. 라우터, 상태 관리, 데이터 패칭, i18n, 차트 라이브러리는 쓰지 않습니다. 각각 지금 패널에 이미 있는 수십 줄짜리 코드를 옮기면 되고, 번들과 공급망 위험을 늘릴 이유가 없습니다.

### 5.1 소스 구성

```
internal/webui/panel/
  index.html              <body data-mode="demo">, <script src="./theme.js">, <script type="module" src="/src/main.tsx">
  public/theme.js         지금 pw/theme.js 그대로 (첫 페인트 전 테마)
  src/main.tsx            언어 결정 → (ko면) 카탈로그 불러오기 → 세션 확인 → 렌더
  src/routes.ts           화면 표(id, 제목 키, 그룹, 최소 역할, 샘플 표시, lazy import)와 예전 주소 표
  src/router.ts           해시 파싱·이동·떠나기 가드
  src/api.ts              pw/api.js 이식 (ApiError, request, download, stream, createApi)
  src/live.ts             pw/app.js의 source 상태 기계 이식 + 외부 저장소
  src/fleet.ts            pw/fleet.js 이식 (summary, runnerRows, workerState, availableSlots …)
  src/format.ts           pw/ui.js format·time 이식 (Intl, 언어별)
  src/i18n/index.ts       t(), T, serverText, language
  src/i18n/ko.json        영어 문장 → 한국어 (지금 words 표)
  src/i18n/server-patterns.json  serverText가 맞춰 보는 템플릿 목록
  src/types.ts            스냅샷·API 응답 타입 (api/openapi.yaml을 보고 손으로, 쓰는 필드만)
  src/components/         로컬 컴포넌트 (§8)
  src/screens/<id>/       화면별 코드
  src/demo/               샘플 시나리오와 화면별 fixture (별도 청크)
  src/panel.css           강조색 등 토큰 덮어쓰기와 로컬 컴포넌트 CSS
```

### 5.2 실시간 스냅샷

`src/live.ts`는 지금 `pw/app.js:255-389`의 동작을 그대로 옮긴 모듈 하나입니다. React 밖에서 돌고, 화면은 `useSyncExternalStore`로 읽습니다.

```ts
type Connection = "connecting" | "live" | "polling" | "reconnecting" | "lost" | "hidden" | "sample";
interface LiveState { snapshot: Snapshot | null; connection: Connection; retryAt: number; }
export const live: { subscribe(fn: () => void): () => void; get(): LiveState; start(): void; stop(): void; refresh(): Promise<void>; };
export const useLive = <T>(select: (s: LiveState) => T) => useSyncExternalStore(live.subscribe, () => select(live.get()));
```

옮길 때 지켜야 하는 수치와 규칙(모두 `docs/panel.md#live-updates`에 문서화된 약속입니다):

- `EventSource("./api/events")`의 `snapshot` 이벤트가 기본. 열리면 "실시간".
- 오류가 나도 4초 안에 다시 열리면 아무것도 바꾸지 않고(라이브 영역 낭독 방지), 4초가 지나면 "다시 연결 중", 10초가 지나도 안 열리면 폴링으로 내려갑니다. 서버가 10분마다 스트림을 닫는 것은 평소 일입니다.
- 폴링은 `GET /snapshot` 5초 제한, 성공이면 3초마다, 실패하면 3초×2ⁿ, 최대 30초. 실패 중에는 "연결 끊김"과 재시도까지 남은 초.
- 폴링으로 내려간 뒤 60초가 지나면 성공한 폴링 다음에 스트림을 다시 시도합니다.
- `/snapshot`이 401이면 세션 종료. `EventSource`가 없거나 서버가 거부(429 등)하면 폴링.
- 탭이 숨으면 모두 멈추고 "숨김 중 일시 정지", 보이면 다시 시작.
- `refresh()`는 쓰기 동작 뒤 바로 한 번 `/snapshot`을 읽습니다.
- 샘플 모드는 시나리오의 스냅샷을 한 번 넣고 아무 연결도 열지 않습니다.

스냅샷은 서버가 2초마다 바뀐 것만 보내므로 렌더 빈도는 낮습니다. 화면은 필요한 부분만 고르는 선택자(`useLive(s => s.snapshot?.fleet?.hosts)`)를 쓰고, 파생 값(`summary`)은 `useMemo`로 스냅샷이 바뀔 때만 계산합니다. `DataTable` 행 id는 작업 id·러너 이름·호스트 이름이라 같은 행이 다시 들어와도 행 애니메이션이 일어나지 않습니다.

"N초 전 갱신"처럼 시간이 흐르는 문구는 `useNow(1000)` 훅 하나가 1초마다 바뀌는 시각을 주고, 그 문구를 쓰는 작은 컴포넌트만 다시 그립니다(지금 `setInterval(renderChrome, 1000)`).

### 5.3 REST

```ts
function useRequest<T>(path: string | null, deps?: unknown[]): { data?: T; error?: ApiError; loading: boolean; reload(): void };
function usePoll<T>(path: string | null, interval: (data?: T, error?: ApiError) => number): …  // 숨은 탭이면 멈춤
```

- `api.ts`는 `pw/api.js`를 그대로 옮깁니다: 같은 출처 `fetch`, `cache: "no-store"`, 기본 7초 제한(`AbortSignal.timeout` + 호출자 `signal`을 `AbortSignal.any`로 묶음), JSON 본문이면 `Content-Type`, 오류는 본문의 문장(또는 JSON `{error, code, pull_request}`)을 `serverText`로 번역한 `ApiError`, 상태 0은 "매니저에 연결할 수 없습니다"/"제한 시간 안에 답하지 않았습니다", 401은 세션 종료. 브라우저가 `Origin`을 붙이므로 서버의 출처 검사(`app/panel_controls.go:33-37`)는 그대로 통과합니다.
- `useRequest`는 주소가 바뀌거나 화면이 사라지면 이전 요청을 `AbortController`로 취소합니다(지금의 `ticket` 패턴을 대신함).
- 화면별 요청 제한 시간은 지금 값을 유지합니다: 저장소 점검·팀 추가·설정 저장 10초, 이전 미리보기·열기 95초, 백업 검증 30분+10초, 큐 백업 다운로드와 로그 스트림은 제한 없음.
- 폴링 주기도 지금 값입니다: 탄력 한도는 보고서의 `interval_seconds`(최소 5초, 오류면 60초), 서버 리소스는 30초(디스크 측정 중이면 5초, 오류면 60초).
- 캐시는 두지 않습니다. 작업 상세의 `GET /api/jobs/{id}`만 "작업 id + 상태"를 키로 한 번 불러오고 상태가 바뀌면 다시 부릅니다(지금 `loadInsight`).

### 5.4 라우터와 떠나기 가드

`src/router.ts`(목표 100줄 안쪽):

- `useRoute()`: `location.hash`를 `{ screen, id, sub, params }`로 파싱(`#/jobs/4828?tab=history` → `jobs`, `4828`, `undefined`, `tab=history`). `hashchange`를 `useSyncExternalStore`로 구독.
- `navigate(path, { replace })`: 같은 주소로 다시 가면 화면에 "다시 초점"을 알립니다(지금 `go()`가 같은 해시에서도 라우터를 다시 돌리는 동작).
- 첫 파싱에서 예전 주소 표([§2.4](#24-주소-체계와-예전-링크))를 적용하고 `replaceState`.
- `useLeaveGuard(blocked: boolean, onBlocked: (retry: () => void) => void)`: 가드가 있으면 `hashchange`를 되돌리고(`replaceState`로 원래 주소) `onBlocked`에 원래 가려던 이동을 넘깁니다. 설정 화면은 여기서 `SaveBar guard`를 올리고, "버리고 이동"이 `retry()`를 부릅니다. 지금 `canLeave()` 계약과 같습니다.
- 화면 전환 후 `PageHeader` 제목에 포커스(`preventScroll`), 맨 위로 스크롤, `document.title` 갱신. 같은 화면 안의 쿼리 변경은 포커스를 옮기지 않습니다.
- `AppShell renderLink`는 일반 `<a href="#/…">`를 그대로 씁니다. 해시 링크라 별도 링크 컴포넌트가 필요 없습니다.

### 5.5 세션과 역할

`main.tsx`가 `GET /api/me`의 `{ login, role }`을 한 번 읽어 컨텍스트 하나(`SessionContext`)에 둡니다. `can(role)`은 지금처럼 viewer < operator < admin 순서이고 샘플 모드에서는 항상 false입니다. 역할은 세션 동안 바뀌지 않는다고 봅니다(지금과 같음). 역할이 줄어든 뒤의 403은 토스트로 알립니다.

### 5.6 i18n

- **언어 결정은 지금과 같습니다**: `?lang=ko|en`이 있으면 `runquill-language`에 저장, 없으면 저장값, 없으면 `navigator.language`가 `ko`로 시작하면 한국어. 바꾸면 `?lang=`을 붙여 새로 고침합니다(앱 전체 다시 그리기와 Stratum 문구를 단순하게 유지).
- **카탈로그를 JSON으로 옮깁니다.** `pw/i18n.js`의 `words`(영어 문장 → 한국어)를 `src/i18n/ko.json`으로, `serverPatterns`를 `src/i18n/server-patterns.json`으로 한 번 변환합니다. 키는 지금처럼 영어 원문입니다. 서버가 보내는 영어 문장도 같은 표로 번역하므로(`serverText`), Go 테스트가 이 JSON을 읽어 서버 문장마다 번역이 있는지 검사합니다([§7.2](#72-테스트)).
- 영어 사용자는 한국어 카탈로그를 받지 않도록, 한국어일 때만 `import("./i18n/ko.json")`으로 불러온 뒤 첫 렌더를 합니다.
- `t(key, values)`는 문자열을 돌려줍니다. 문장 안에 `<code>`나 링크가 들어가는 경우(지금 `fill()`)는 `<T k="Run {command} …" values={{ command: <code>runquill diagnose</code> }} />`.
- 숫자·날짜·상대 시간·바이트·기간은 `Intl`로, 언어는 `document.documentElement.lang`. `DataTable`의 정렬 collation도 `<html lang>`을 따릅니다(`st/table/DataTable.tsx:37`).
- **Stratum 기본 문구는 한국어입니다.** 영어 패널에서는 쓰는 곳마다 문구 prop을 넘겨야 합니다: `Toaster label/closeLabel`, `AppShell drawerLabel/menuLabel`, `Nav aria-label`, `CommandPalette label/placeholder/emptyLabel/statusLabel/hints/recentLabel`, `SplitView labels`, `SaveBar messages`, `SearchInput clearLabel/placeholder`, `Progress label`, `Spinner label`, `Pagination previousLabel/nextLabel`, `Popover closeLabel`, `ConfirmDialog cancelLabel`, `SelectionBar` 관련 문구, `Textarea limitMessage`. 이 목록을 `src/i18n/stratum.ts` 한 곳에서 만들어 넘깁니다([§8](#8-stratum에-없는-것) G8).

### 5.7 테마와 스타일

- `public/theme.js`는 지금 파일 그대로입니다. `runquill-theme`(없으면 `runquill-login-theme`)을 읽어 `<html data-theme>`를 `system|light|dark`로 둡니다. Stratum은 `data-theme="light"|"dark"`만 고정으로 읽고(`tokens.css:110-111`) 나머지는 OS를 따르므로 `system` 값도 그대로 동작합니다.
- 사용자 메뉴·팔레트의 테마 선택은 같은 키에 저장합니다.
- 스타일은 `stratum.css` 하나 + `src/panel.css`. 레이아웃은 Stratum 유틸리티 클래스로, 유틸리티로 안 되는 것만 `panel.css`에 둡니다. 강조색을 바꾸려면 `:root { --color-accent: light-dark(…, …) }` 한 줄([§9 Q9](#9-열린-질문)).
- 상태 색은 지금 `ok/waiting/failed/offline` 톤을 `Badge tone` `success/warning/danger/neutral`로 옮깁니다.
- 글꼴: Stratum의 `--font-sans`는 Pretendard를 먼저 찾고 시스템 글꼴로 내려갑니다. Pretendard CDN은 CSP(`default-src 'self'`)가 막고 오프라인 설치에서도 못 쓰므로 불러오지 않습니다([§9 Q8](#9-열린-질문)).

### 5.8 접근성과 키보드

지금 패널이 하는 것을 모두 유지하고, Stratum 컴포넌트가 이미 하는 것은 그쪽에 맡깁니다.

| 지금 | v2 |
| --- | --- |
| 건너뛰기 링크 | `AppShell` 앞 링크(로컬, `sr-only` + 포커스 시 보임) |
| 화면 전환 시 제목 포커스 | 라우터 |
| 표의 가로 스크롤 영역은 스크롤될 때만 탭 정지(`scrollStop`) | `DataTable`의 동작을 따르고, 로컬 `DiffView`·`LogView`에만 같은 규칙 |
| 딥 링크 대상 행 강조와 스크롤(`linkTarget`) | 상세 칸 열기 + `useSplitViewNavigation`의 `aria-current` |
| 상세 칸 Esc로 닫기, 닫으면 행으로 포커스 복귀 | `SplitView`가 함 |
| 확인 대화상자, 팔레트, 메뉴의 포커스 가두기·복귀 | `ConfirmDialog`, `CommandPalette`, `Menu` |
| 상태 문구 라이브 영역(`statusLine`, 연결 상태) | 로컬 `Notice`와 `LiveStatus`가 같은 `role`을 씀. 카운트다운·로그 줄 수는 라이브 영역이 아님 |
| 동작 줄이기 | Stratum이 지킴. 로컬 컴포넌트도 `reducedMotion()`을 씀 |

## 6. 빌드와 임베드

이 절은 전환이 끝난 뒤의 모습입니다. 개발 중(§7.1의 1~5단계)에는 출력 디렉터리와 embed가 다릅니다([§7.1](#71-단계) "겹치는 동안의 값").

### 6.1 위치

| 경로 | 내용 | Git |
| --- | --- | --- |
| `internal/webui/panel/` | 소스, `package.json`, `package-lock.json`, `vite.config.ts`, `tsconfig.json`, `.node-version`, 테스트 | 커밋 |
| `internal/webui/panel/node_modules/` | 설치물 | `.gitignore` |
| `internal/webui/panelweb/` | 빌드 결과(생성물). `assets.go`의 `//go:embed panelweb`가 그대로 읽음 | 커밋, `.gitattributes`에 `linguist-generated=true -diff` |

결과 디렉터리 이름을 지금과 같게 두면 `internal/webui/assets.go`, `.github/workflows/demo-pages.yml`의 복사, `.github/workflows/release.yml:91`의 `runquill-panel-demo.tar.gz`가 경로를 바꾸지 않아도 됩니다(내용 검사는 바뀝니다, [§6.7](#67-바뀌는-다른-파일)).

TypeScript를 씁니다. Stratum이 타입을 제공하고, 빌드는 Vite가 타입을 지우기만 하므로 런타임 비용이 없습니다. 타입 검사는 `tsc --noEmit`로 CI에서만 합니다.

### 6.2 Vite 설정의 모양

```ts
// internal/webui/panel/vite.config.ts
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

const target = process.env.RUNQUILL_PANEL_DEV_TARGET; // 예: http://127.0.0.1:8081 (로컬 매니저 패널)

// 개발 서버에서 매니저에 붙을 때만 index.html을 라이브 모드로 (서버가 하는 치환과 같음)
const liveMode = (): Plugin => ({
  name: "runquill-live-mode",
  apply: "serve",
  transformIndexHtml: (html) => (target ? html.replace('data-mode="demo"', 'data-mode="live"') : html),
});

export default defineConfig({
  base: "./",                       // GitHub Pages 하위 경로와 매니저 루트 둘 다
  plugins: [react(), liveMode()],
  build: {
    outDir: "../panelweb",
    emptyOutDir: true,
    target: "es2023",
    modulePreload: { polyfill: false }, // 인라인·추가 코드 없이
    sourcemap: false,
    // Vite 8에서는 rolldownOptions(이전 이름 rollupOptions).
    rolldownOptions: {
      output: {
        entryFileNames: "assets/panel-[hash].js",
        // src/demo/ 아래 모듈은 모두 "demo" 그룹 하나로. 설치된 Rolldown 1.3.4는 advancedChunks를 deprecated로 표시하고 codeSplitting을 권함
        codeSplitting: { groups: [{ name: "demo", test: /[\\/]src[\\/]demo[\\/]/ }] },
        chunkFileNames: (chunk) => (chunk.name === "demo" ? "assets/demo-[hash].js" : "assets/chunk-[hash].js"),
        assetFileNames: "assets/panel-[hash][extname]",
      },
    },
  },
  server: target && {
    proxy: Object.fromEntries(["/api", "/snapshot", "/auth"].map((path) => [path, {
      target, changeOrigin: true, headers: { origin: target }, // 매니저의 Host·Origin 검사를 통과하게
    }])),
  },
});
```

파일 이름을 `panel-`, `chunk-`, `demo-`로 시작하게 고정하는 이유:

- `//go:embed panelweb`처럼 디렉터리로 embed하면 `_`나 `.`로 시작하는 파일이 빠집니다. 번들러가 만드는 청크 이름(`_commonjsHelpers` 등)을 그대로 쓰면 바이너리에서 조용히 사라질 수 있습니다. `all:` 접두사 대신 이름을 고정하고, Go 테스트가 `index.html`이 참조하는 모든 파일이 200으로 나오는지 확인합니다([§7.2](#72-테스트)).
- 서버가 샘플 청크를 이름만으로 거를 수 있습니다.

샘플 코드(`src/demo/`)는 `if (mode === "demo") await import("./demo/index.ts")`로만 불러옵니다. 청크 이름은 파일 이름(`index`, `jobs`)에서 오므로 이름 접두사로 거르지 않고, **모듈 경로**(`src/demo/` 아래)로 `demo` 그룹에 묶습니다. 그래야 fixture가 `chunk-*`로 섞여 라이브 서버에서 나가는 일이 없습니다. 이 규칙은 Go 테스트로 지킵니다([§7.2](#72-테스트)).

### 6.3 Node 고정

- `internal/webui/panel/.node-version`에 정확한 버전(이 문서 작성 시 로컬 `node --version`은 v26.13.1, Stratum의 개발 의존성은 Vite ^8, `@types/node` ^26)을 적고, `package.json`의 `engines.node`를 같은 메이저로 제한합니다.
- CI는 `actions/setup-node`의 `node-version-file`로 같은 파일을 읽습니다(커밋 SHA로 고정, 다른 액션과 같은 방식).
- 의존성은 `package-lock.json`으로 고정하고 `npm ci`만 씁니다. Stratum은 정확한 버전(`0.1.1`)으로 고정합니다.
- 어느 Node 버전이 LTS인지는 구현할 때 Node 릴리스 일정을 보고 정합니다.

### 6.4 서버 변경

`app/panel.go:205-277`의 정적 파일 부분만 바뀝니다.

```go
var panelAssetName = regexp.MustCompile(`^(?:panel|chunk|demo)-[A-Za-z0-9_-]+\.(?:js|css)$`)

mux.HandleFunc("GET /{$}", …)                 // 그대로: index.html, live면 data-mode 치환
mux.HandleFunc("GET /theme.js", serveFile("theme.js"))
mux.HandleFunc("GET /assets/{file}", func(w, r) {
    name := r.PathValue("file")
    if !panelAssetName.MatchString(name) { 400 }
    if panel != nil && strings.HasPrefix(name, "demo-") { 404 }  // 라이브 패널은 샘플 데이터를 보내지 않음
    http.ServeFileFS(w, r, assets, "assets/"+name)
})
```

- 지금의 개별 라우트(`/panel.css`, `/app.js`, `/api.js`, `/ui.js`, `/fleet.js`, `/i18n.js`, `/views/{file}`, `/demo/{file}`, `/demo.js`)는 없어집니다. `api/openapi.yaml:1330-1451`의 해당 경로도 `/theme.js`와 `/assets/{file}`로 바꿉니다.
- **CSP는 그대로 둡니다**: `default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'`(`app/panel.go:274`). v2가 지켜야 하는 것:
  - 인라인 `<script>` 없음. 빌드된 `index.html`은 `<script src>`만(모듈 프리로드 폴리필 끔). 테마 스크립트는 지금처럼 파일.
  - `style=""` 속성과 `<style>` 요소 없음. React의 `style` prop은 CSSOM(`element.style`)으로 적용되어 CSP에 막히지 않습니다. Stratum 소스에는 `<style>` 주입, `dangerouslySetInnerHTML`, `setAttribute("style")`이 없습니다(`grep`으로 확인). Vite 개발 서버는 CSS를 `<style>`로 넣지만 개발 서버는 CSP를 보내지 않습니다.
  - 외부 출처 없음: 글꼴 CDN 불가, 이미지는 `'self'`와 `data:`만(Stratum `shell.css:132`의 `mask: url("data:image/svg+xml…")`은 허용 범위).
  - `form-action 'none'`: 모든 폼은 `onSubmit`에서 `preventDefault`. 로그인은 링크.
  - 다운로드(`saveFile`의 `blob:` 링크)는 지금과 같이 동작합니다.
  - **1단계에서 확인함**([§7.3](#73-1단계-프런트엔드-확인-결과)): 빌드된 `index.html`에는 인라인 스크립트와 `style`이 없고, 실제 CSP 문자열 아래 Chromium에서 위반이 0건이었습니다.
- `Cache-Control: no-store`는 그대로 둡니다. 해시가 붙은 `assets/`는 `immutable`로 캐시할 수 있지만, 지금 모든 응답이 `no-store`라고 문서화되어 있고(`api/openapi.yaml:9`) 번들이 작습니다([§9 Q12](#9-열린-질문)).
- `index.html`의 `data-mode="demo"` 치환은 그대로입니다. Vite가 `<body>` 속성을 보존하는지 Go 테스트가 확인합니다.

### 6.5 CI 최신성 검사

`.github/workflows/ci.yml`에 `panel` 작업을 더합니다.

```yaml
panel:
  runs-on: ubuntu-24.04
  defaults: { run: { working-directory: internal/webui/panel } }
  steps:
    - uses: actions/checkout@<sha>
    - uses: actions/setup-node@<sha>
      with: { node-version-file: internal/webui/panel/.node-version, cache: npm, cache-dependency-path: internal/webui/panel/package-lock.json }
    - run: npm ci
    - run: npm run typecheck
    - run: npm test
    - run: npm run build
    - name: The committed panel matches its source
      run: |
        git diff --exit-code -- ../panelweb
        test -z "$(git status --porcelain -- ../panelweb)"  # 새로 생기거나 지워진 파일
    - run: npm run size       # 예산 검사, §6.8
    - uses: actions/setup-go@<sha>
      with: { go-version-file: go.mod }
    - run: npx playwright install --with-deps chromium && npm run e2e   # §7.2
```

실패하면 "`internal/webui/panel`에서 `npm run build`를 실행해 결과를 커밋하세요"를 출력합니다.

**이 검사는 macOS와 Linux에서 바이트까지 같은 빌드가 나와야 성립합니다.** 1단계에서 먼저 확인합니다(같은 lockfile과 Node 버전으로 두 OS에서 빌드해 `diff -r`). 다르면 대안은 둘입니다.

1. 빌드가 `panelweb/.source-hash`에 소스·lockfile·`.node-version`의 SHA-256을 적고, CI는 해시만 비교합니다. 커밋된 번들이 해당 소스로 만들어졌다는 것은 보장하지만 번들 내용 자체는 검증하지 않습니다.
2. 번들은 CI(Linux)에서만 만들고, PR에 올리는 것도 CI가 만든 결과만 받습니다. 사람이 로컬에서 커밋하기 번거롭습니다.

1을 권합니다.

**1단계 결과: 바이트까지 같습니다**([§7.3](#73-1단계-프런트엔드-확인-결과)). 같은 lockfile과 Node 26.13.1로 macOS arm64, Linux arm64, Linux amd64에서 빌드한 결과가 `diff -r`로 같았습니다. 대안이 필요 없으므로 CI는 위의 `git diff --exit-code`를 그대로 씁니다.

### 6.6 개발 방식

```bash
cd internal/webui/panel
npm ci
npm run dev                         # 샘플 모드 (fixture), http://127.0.0.1:5173
RUNQUILL_PANEL_DEV_TARGET=http://127.0.0.1:8081 npm run dev   # 로컬 매니저에 붙음
npm test && npm run typecheck
npm run build                       # ../panelweb 갱신, 커밋할 것
go run . panel --demo               # (저장소 루트) 빌드 결과를 Go 서버로 확인
```

로컬 매니저에 붙는 방법:

- 매니저의 `RUNQUILL_PANEL_URL`을 `http://127.0.0.1:8081`처럼 루프백 HTTP로 두면 세션 쿠키 이름이 `rq_session`(Secure 없음)입니다(`api/openapi.yaml:36`). 쿠키는 포트를 가리지 않으므로 매니저 주소에서 GitHub 로그인을 한 번 하면 `127.0.0.1:5173`의 개발 서버에도 쿠키가 갑니다.
- 프록시가 `Host`(`changeOrigin`)와 `Origin`을 매니저 주소로 바꿔 매니저의 Host 검사(`app/panel.go:257-270`)와 쓰기 요청의 Origin 검사를 통과합니다. GitHub 콜백은 개발 서버가 아니라 매니저로 갑니다.
- SSE도 같은 프록시로 지나갑니다.

### 6.7 바뀌는 다른 파일

| 파일 | 바뀌는 것 |
| --- | --- |
| `app/panel.go` | §6.4 |
| `api/openapi.yaml` | 정적 파일 경로(`/panel.css`~`/demo/{file}`)를 `/theme.js`, `/assets/{file}`로 |
| `.github/workflows/ci.yml` | `panel` 작업(§6.5) |
| `.github/workflows/demo-pages.yml` | `test -f _site/app.js && test -f _site/views/overview.js && test -f _site/demo/jobs.js`를 `test -f _site/index.html && test -f _site/theme.js && ls _site/assets/demo-*.js`로. `grep -Fq 'data-mode="demo"'`는 그대로 |
| `.github/workflows/release.yml` | 변경 없음(같은 디렉터리를 tar). 데모 tar에 `assets/`가 들어감 |
| `AGENTS.md` | "The panel is embedded static HTML, CSS and JavaScript and has no frontend build step"을 "패널 소스는 `internal/webui/panel/`, 빌드 결과는 커밋된 `internal/webui/panelweb/`. Go 빌드에는 Node가 필요 없고, 패널을 고칠 때만 `npm run build`"로. 작업 표의 패널 행에 소스 경로 추가 |
| `docs/development.md`, 저장소 구조 | 위와 같은 내용, Node 버전과 명령 |
| `docs/panel.md`, `docs/panel.ko.md` | 탐색 표, 화면 설명, 주소, 팔레트, 로그 위치(§2) |
| `.gitignore`, `.gitattributes` | `node_modules/`, 생성물 표시 |
| `.dockerignore` | 지금 `internal/**`를 통째로 넣으므로 `internal/webui/panel/`(소스와 `node_modules`)을 빼는 줄 추가. 이미지 빌드는 커밋된 `panelweb/`만 필요 |
| `.github/scripts/*`, `Dockerfile` | 패널 파일 경로를 참조하지 않음(`grep`으로 확인). 변경 없음 |
| `internal/app/*_test.go` | §7.2 |

`internal/webui/panel/go.mod`를 모듈 경계로 두어 루트의 `go vet ./...`와 `go test ./...`가 `node_modules/`를 탐색하지 않게 합니다. `gofmt`는 모듈 경계를 따르지 않으므로 CI에서는 Git이 추적하는 Go 파일만 검사합니다.

### 6.8 번들 크기 예산

측정값(이 문서 작성 시):

| 대상 | 원본 | gzip -9 |
| --- | --- | --- |
| 지금 패널 전체(`pw/` 아래 JS·CSS·HTML, 모든 화면과 한국어 포함) | 436 KB(`du -sk`) | 약 100 KB(파일을 이어 붙여 압축) |
| Stratum `dist/stratum.css` | 152 KB | 26.7 KB |

React·ReactDOM과 Stratum JS의 압축 크기는 구현하면서 빌드 결과로 잽니다. 아래는 목표입니다.

| 목표 | gzip |
| --- | --- |
| 첫 화면(`index.html` + 진입 청크 + 공유 청크 + CSS) | ≤ 150 KB |
| 화면 청크 하나 | ≤ 30 KB |
| 한국어 카탈로그 청크 | ≤ 30 KB |
| CSS 전체 | ≤ 35 KB |
| `panelweb/` 원본 합계(바이너리에 들어가는 양) | ≤ 1.2 MB |

`scripts/size.mjs`(Node 기본 `zlib`만 사용)가 빌드 결과를 재고 넘으면 실패합니다. 화면은 `React.lazy`로 나눠 첫 화면에는 셸과 개요만 들어갑니다.

1단계 빌드의 측정값(gzip -9, `npm run size`):

| 대상 | 측정 | 예산 |
| --- | --- | --- |
| 첫 화면(`index.html` + 진입 청크 + CSS). 진입 청크는 정적 import가 없습니다 | 125.1 KB | ≤ 150 KB |
| 〃 + 개요 화면 청크(첫 화면에서 바로 불러옴) | 132.2 KB | ≤ 150 KB |
| 진입 청크 `panel-*.js`(React, ReactDOM, Stratum, 셸) | 원본 314 KB, gzip 97.6 KB | |
| CSS(`stratum.css` + `panel.css`) | 27.1 KB | ≤ 35 KB |
| 개요 화면 청크 | 7.1 KB | ≤ 30 KB |
| 한국어 카탈로그 청크(`ko.json`, 1,042개 키) | 25.3 KB | ≤ 30 KB |
| 서버 문장 템플릿 청크 | 1.0 KB | |
| 샘플 청크 `demo-*.js` | 1.8 KB | |
| `panelv2/` 원본 합계 | 576 KB | ≤ 1.2 MB |

한국어 카탈로그는 v1 문장 전체를 한 청크로 받으므로 화면이 늘어도 크게 늘지 않지만, 예산까지 4.7 KB만 남았습니다. 넘으면 서버 문장(`serverText`용)과 화면 문장을 두 청크로 나눕니다.

2단계 빌드의 측정값(작업·러너·호스트·사용량 추가):

| 대상 | 측정 | 예산 |
| --- | --- | --- |
| 첫 화면(`index.html` + 진입 청크 + CSS) | 128.8 KB | ≤ 150 KB |
| CSS | 27.8 KB | ≤ 35 KB |
| 작업 화면 청크 | 5.6 KB | ≤ 30 KB |
| 러너 화면 청크 | 1.4 KB | ≤ 30 KB |
| 호스트 화면 청크 | 4.5 KB | ≤ 30 KB |
| 사용량 화면 청크 | 4.2 KB | ≤ 30 KB |
| 화면들이 함께 쓰는 Stratum 청크(`SplitView`, `Tabs` 등)와 작은 공유 청크 | 5.6 KB + 3.9 KB + 3.6 KB + 그 밖 1.4 KB 이하 | 각 ≤ 30 KB |
| 한국어 카탈로그 청크(1,056개 키) | 25.5 KB | ≤ 30 KB |
| 샘플 청크 `demo-*.js`(시나리오 + 화면 fixture) | 4.4 KB | ≤ 30 KB |
| `panelv2/` 원본 합계 | 661 KB | ≤ 1.2 MB |

한국어 카탈로그는 2단계에서 0.2 KB만 늘었습니다(새 키 14개, 나머지는 v1 문장 재사용). 남은 4.5 KB는 3·4단계 새 문장에 충분할 것으로 봅니다.

3단계 빌드의 측정값(재시도·취소, 저장소 점검, 배정 중지·재개, 워커 등록, 러너 로그 탭 추가):

| 대상 | 측정 | 예산 |
| --- | --- | --- |
| 첫 화면(`index.html` + 진입 청크 + 진입 청크가 정적으로 부르는 공유 청크 2개 + CSS) | 132.8 KB | ≤ 150 KB |
| CSS | 28.1 KB | ≤ 35 KB |
| 작업 화면 청크(저장소 점검 대화상자 포함) | 6.8 KB | ≤ 30 KB |
| 러너 화면 청크(로그 뷰어 포함) | 5.4 KB | ≤ 30 KB |
| 호스트 화면 청크(복사 필드 포함) | 5.5 KB | ≤ 30 KB |
| 한국어 카탈로그 청크(1,059개 키) | 25.5 KB(파일만 gzip -9 하면 24.5 KB) | ≤ 30 KB |
| 샘플 청크 `demo-*.js`(로그 fixture 추가) | 4.8 KB | ≤ 30 KB |
| `panelv2/` 원본 합계 | 683 KB | ≤ 1.2 MB |

한국어 카탈로그는 3단계에서 새 키가 3개뿐이라(나머지는 v1 문장) 거의 그대로입니다. 남은 4.5 KB로 4단계를 감당할 수 있는지는 4단계에서 다시 잽니다. 넘으면 1단계에 적은 대로 서버 문장과 화면 문장을 두 청크로 나눕니다.

4단계 빌드의 측정값(설정, 팀, API 토큰, 감사 기록, 백업, 워크플로 이전 추가):

| 대상 | 측정 | 예산 |
| --- | --- | --- |
| 첫 화면(`index.html` + 진입 청크 + 정적으로 부르는 공유 청크 + CSS) | 133.8 KB | ≤ 150 KB |
| CSS | 28.7 KB | ≤ 35 KB |
| 설정 화면 청크 | 7.0 KB | ≤ 30 KB |
| 팀 · API 토큰 · 감사 기록 화면 청크 | 2.0 KB · 1.8 KB · 1.6 KB | 각 ≤ 30 KB |
| 백업 화면 청크 | 3.1 KB | ≤ 30 KB |
| 워크플로 이전 화면 청크(`DiffView` 포함) | 5.5 KB | ≤ 30 KB |
| 한국어 카탈로그 청크(1,067개 키) | 25.7 KB(파일만 gzip -9 하면 24.6 KB) | ≤ 30 KB |
| 샘플 청크 `demo-*.js`(백업 상태, 이전 미리보기 추가) | 5.9 KB | ≤ 30 KB |
| `panelv2/` 원본 합계 | 751 KB | ≤ 1.2 MB |

관리 화면 문장은 v1 카탈로그에 이미 있어 새 키가 10개뿐이었고, 카탈로그는 0.2 KB 늘었습니다. 30 KB를 넘지 않아 서버 문장과 화면 문장을 두 청크로 나누는 대안은 쓰지 않았습니다.

## 7. 이전 계획과 테스트

### 7.1 단계

한 번에 바꿉니다. 화면 단위로 옛 패널과 섞으면 두 라우터, 두 셸, 두 스타일이 한 페이지에 공존해야 해서 비용이 큽니다. 대신 개발 중에는 v2를 같은 바이너리에 **선택해서 켜는 패널**로 넣어 실제 매니저에서 써 볼 수 있게 합니다([§9 Q2](#9-열린-질문)).

| 단계 | 내용 | 끝나는 기준 |
| --- | --- | --- |
| 0 | 이 문서의 열린 질문 결정 | 결정 기록 |
| 1 기초 | `internal/webui/panel/` 골격, 빌드 결정성 확인(§6.5), embed·CSP·크기 확인, 셸·로그인·`live.ts`·라우터(예전 주소 포함)·i18n JSON 변환·팔레트·토스트·확인, 개요. 결과는 `internal/webui/panelv2/`에 두고 서버는 `RUNQUILL_PANEL_V2=1`일 때 그 디렉터리를 서빙 | 샘플 모드에서 개요, 라이브 모드에서 로그인·연결 상태, CI `panel` 작업 통과 |
| 2 읽기 화면 | 작업(4탭, 상세), 러너(정보), 호스트(4탭, 상세), 사용량(2탭) | 해당 화면의 §1 행이 모두 체크 |
| 3 operator 동작 | 재시도·취소, 배정 중지·재개, 로그 탭, 워커 등록, 저장소 점검(admin, 2단계에서 미룸) | 〃 |
| 4 관리 화면 | 설정(`SaveBar`, 가드), 팀, API 토큰, 감사, 백업, 이전 | 〃 |
| 5 전환 | §1 전체 점검, 문서(영어·한국어) 갱신, `openapi.yaml`, Pages·CI 검사 수정, v2 결과를 `panelweb/`으로 옮기고 기본값으로 | 릴리스 노트에 화면 변경과 예전 주소 동작 |
| 6 정리 | 다음 마이너 릴리스에서 v1 소스와 `RUNQUILL_PANEL_V2` 제거 | |

**겹치는 동안의 값**(1~5단계, Q2를 권하는 대로 정했을 때):

| 항목 | 겹치는 동안 | 전환 후(§6) |
| --- | --- | --- |
| Vite `outDir` | `../panelv2` | `../panelweb` |
| embed | `assets.go`에 `//go:embed panelv2`와 `PanelV2()`를 더함 | `panelweb`만, `PanelV2()` 제거 |
| 서빙 | `RUNQUILL_PANEL_V2=1`이면 라이브·`panel --demo` 모두 v2, 아니면 v1 | v2 |
| CI 최신성 검사 경로 | `internal/webui/panelv2` | `internal/webui/panelweb` |
| GitHub Pages, 릴리스 데모 tar | v1 | v2 |
| 서버 정적 라우트 | v1 라우트 유지, v2일 때만 `/theme.js`·`/assets/{file}` | §6.4 |

`emptyOutDir`가 v1 디렉터리를 지우지 않도록 1단계 설정은 처음부터 `panelv2`를 가리킵니다.

겹치는 동안(1~5단계) Go의 번역 테스트는 v1 `i18n.js`와 v2 JSON을 둘 다 검사합니다. 서버에 새 문장을 더하는 변경은 두 곳에 번역을 넣어야 합니다.

### 7.2 테스트

**Go 테스트(유지하되 고칠 것)**

| 테스트 | 지금 하는 일 | v2 |
| --- | --- | --- |
| `TestPanelRoutesAndFirstJobEvidence` (`app/panel_test.go:29`) | 라이브·샘플 서버의 라우트와 응답 코드, `/views/queue.js` 200, 라이브의 `/demo/jobs.js` 404, `..%2f` 400 (`:90-103`) | 같은 표를 `/theme.js`, `/assets/panel-*.js` 200, 라이브의 `/assets/demo-*.js` 404, 샘플은 200, 잘못된 이름 400으로. 파일 이름은 embed FS에서 읽어 채움 |
| (새) 임베드된 파일 참조 | | 임베드된 `index.html`의 모든 `src`/`href`(상대 경로)와 진입 청크가 import하는 청크가 서버에서 200. `data-mode="demo"`가 있고 라이브에서 `live`로 바뀜. 인라인 `<script>`(src 없음)·`style=` 속성 없음 |
| (새) 샘플 데이터 분리 | | `assets/demo-*.js`가 하나 이상 있고, fixture에만 있는 표식(샘플 러너 이름 접두사 `runquill-demo-`)이 `demo-*` 밖의 임베드 파일에 없음. 라이브 서버가 `demo-*`를 404로 답함 |
| `panelTranslations` (`app/panel_test.go:120`) | 임베드된 `i18n.js`를 정규식으로 읽어 키와 `serverPatterns` | `internal/webui/panel/src/i18n/ko.json`과 `server-patterns.json`을 `os.ReadFile`로(테스트는 패키지 디렉터리에서 돌므로 `../webui/panel/src/i18n/…`). 번역 카탈로그는 번들 안에 녹아 있어 embed에서 읽을 수 없습니다. JSON 디코더는 중복 키를 조용히 덮으므로 `json.Decoder.Token()`으로 중복을 따로 검사 |
| `TestPanelTranslationsHaveNoDuplicateKeys`, `TestPanelServerMessagesHaveTranslations`, `image_pull_test.go:514`, `runner_images_test.go:644` | `panelTranslations`를 씀 | 헬퍼만 바뀌고 그대로 |
| `TestPanelSamplePanelIsReadOnly` (`app/panel_test.go:159`) | `views/*.js`, `demo/*.js` 등에서 fixture가 GET만 답하는지, 화면 코드가 `fetch`·`EventSource`·`XMLHttpRequest`·`sendBeacon`·`WebSocket`을 직접 쓰지 않는지 | 같은 검사를 소스 `internal/webui/panel/src/**/*.{ts,tsx}`에, `src/api.ts`와 `src/live.ts`만 예외로. 번들은 축약되어 검사할 수 없음 |
| 나머지 패널 서버 테스트(`panel_access_test.go`, `panel_jobs_test.go`, `panel_settings_test.go`, `panel_backups_test.go`, `panel_migrate_test.go`, `panel_resources_test.go`, `panel_live_oauth_test.go`) | API 동작 | 그대로 |

**프런트엔드 단위 테스트**(Vitest + Testing Library + jsdom, Stratum과 같은 도구). 1단계에는 `api`, `live`, `router`, `i18n`(소스에 적힌 영어 키, 곧 `t("…")`, `fill("…")`, `title: "…"`, 삼항 양쪽의 문자열이 `ko.json`에 있는지 포함. 변수로 넘기는 키는 잡지 못함), 주의 항목·`summary`, `confirm()` 테스트가 있습니다.

- `api.ts`: 일반 문장·JSON 오류 해석, 401 콜백, 시간 초과·네트워크 문구, 샘플 모드 거부.
- `live.ts`: 가짜 타이머와 가짜 `EventSource`로 4초/10초/60초 규칙, 폴링 백오프, 숨은 탭, 401.
- `router.ts`: 예전 주소 표 전체, 역할 밖 화면, 떠나기 가드와 `retry`.
- `i18n`: `t` 자리 채우기, `serverText`의 첫 줄 번역과 템플릿 일치.
- 설정 본문 조립: 원래 풀 필드 보존, `privileged` 기본값, OS 이름 검사, 중복 풀 거부, 목록 변경 수 세기.
- 로그 정리: ANSI·OSC 제거, 제어 문자, 4000자 자르기, 5000줄 상한.
- `fleet.ts`의 `summary`, 개요의 주의 항목 계산, 단계 시간 계산.

**브라우저 테스트**(Playwright)

- 대상: `go run . panel --demo --port 0`으로 띄운 실제 Go 서버의 샘플 패널. 진짜 CSP와 헤더 아래에서 돌고, `securitypolicyviolation` 이벤트가 하나라도 나면 실패.
- 시나리오 3개 × 모든 샘플 화면 렌더, 예전 주소 리디렉션, 팔레트(⌘K, `/`, 영어·한국어 검색), 테마 저장, `?lang=ko`, 폰 크기(390×844)에서 서랍과 시트, `@axe-core/playwright` 위반 0.
- 라이브 모드는 같은 서버의 HTML을 `page.route`로 `data-mode="live"`로 바꾸고 `/api/me`, `/api/events`(`text/event-stream` 본문 하나), 쓰기 API를 가로채서: 로그인 실패 이유 5개, 세션 만료, 연결 끊김 표시, 재시도·취소 확인, 설정 저장 안 한 변경 가드, 토큰 한 번 표시.
- CI는 Chromium만. WebKit·Firefox는 수동 실행(Stratum과 같은 방식).

### 7.3 1단계 프런트엔드 확인 결과

2026-11-28, `internal/webui/panel/`의 첫 빌드 기준입니다. 빌드 방법과 결과 구조는 [`internal/webui/panel/README.md`](../../internal/webui/panel/README.md)에 있습니다.

**CSP.** `app/panel.go:274`의 CSP 문자열을 그대로 보내는 임시 서버로 Chromium에서 확인했습니다. `securitypolicyviolation`은 0건이었습니다. 확인한 상황은 다음과 같습니다.

- 샘플 패널: 하위 경로, 영어·한국어, 밝게·어둡게, 폰 폭(390×844) 서랍.
- 라이브 패널: 로그인 화면과 `?signin=` 이유, 세션 확인 실패, SSE 실시간, 연결 끊김 카운트다운, 실패한 로그아웃 토스트, 401 뒤 로그인 화면.
- 그 밖에 `Menu`, `Select`, `CommandPalette`, `Disclosure`를 열어 보았습니다.

Stratum 0.1.1의 `dist`를 직접 읽은 결과도 같습니다.

- `<style>` 주입과 `setAttribute("style")`이 없습니다.
- 스타일은 React `style` prop(`SplitView`, `Progress`, `DataTable`, `Slider`)과 CSSOM(`.style.setProperty`, `.style.translate`, `popover/place.js`의 `style.cssText`)으로만 바꿉니다. CSP는 CSSOM을 막지 않습니다.
- `Tooltip`의 `innerHTML`은 `style` 속성이 없는 고정 마크업입니다. 1단계 화면은 `Tooltip`을 쓰지 않으므로 브라우저에서는 확인하지 않았습니다.
- `stratum.css`의 외부 `url()`은 `data:` SVG 하나뿐입니다.

빌드된 `index.html`에는 `<script src>`와 `<link>`만 있습니다. 모듈 프리로드 폴리필은 껐습니다.

**다른 엔진과 개발 서버.** 같은 확인 스크립트를 Firefox와 WebKit(Playwright 캐시의 엔진)에서도 돌렸습니다.

- WebKit은 모두 통과했습니다.
- Firefox도 모두 통과했습니다. 다만 일부러 끊은 연결 시험에서 `EventSource` 연결 실패가 콘솔 오류로 찍힙니다. Chromium이 같은 상황에서 503을 찍는 것과 같은 성격입니다.
- e2e를 만들 때 주의할 점: WebKit에서는 Playwright의 `page.screenshot()`이 `<style>`을 넣어 CSP 위반이 생깁니다. CSP 검사와 스크린샷을 한 페이지에서 같이 하지 않습니다.

그 밖에 확인한 것:

- 건너뛰기 링크: 첫 Tab 위치에서 화면에 보이고, Enter를 누르면 제목으로 포커스가 갑니다.
- 영어 페이지의 한글: 팔레트, 사용자 메뉴, 폰 서랍, 토스트, 로그인 화면 상태에서 한글이 없습니다(언어 전환 링크 "한국어"만 예외).
- 개발 서버(`npm run dev`, 개발용 React): 경고와 오류가 없습니다.

[§9 Q7](#9-열린-질문)의 정확한 브라우저 하한은 아직 정하지 않았습니다.

**결정성.** 같은 lockfile과 Node 26.13.1로 빌드해 `diff -r`로 비교했고, 결과가 모두 같았습니다.

- 같은 macOS arm64에서 두 번 빌드한 결과.
- `rm -rf node_modules && npm ci` 뒤에 다시 빌드한 결과.
- Docker `node:26.13.1-bookworm-slim` 컨테이너의 Linux arm64와 Linux amd64(에뮬레이션) 빌드 결과.

lockfile에는 Linux용 `@rolldown/binding-linux-*`와 `lightningcss-linux-*`가 들어 있습니다. [§6.5](#65-ci-최신성-검사)의 대안은 필요 없습니다.

**샘플 청크 분리.** `src/demo/` 아래 코드는 `demo-*.js` 하나에만 들어갑니다. 샘플 표식 `runquill-demo-`도 이 파일에만 있습니다. 진입 청크는 다른 청크를 정적으로 import하지 않고 모두 `import()`로 부르므로, 라이브 서버가 `demo-*`를 404로 답해도 라이브 패널은 영향을 받지 않습니다. 브라우저 확인에서 라이브 모드는 `demo-*`를 요청하지 않았습니다.

**설계와 달라진 점.**

- `rolldownOptions.output.codeSplitting`을 씁니다. 설치된 Rolldown 1.3.4의 타입에서 `advancedChunks`는 deprecated입니다.
- 내비게이션 아이콘이 아직 없어 사이드바 레일(`⌘\`)을 `rail={false}`로 막았습니다.
- `api.ts`에는 `request`만 옮겼습니다. `download`, `stream`, 샘플 응답용 `createApi`는 쓰는 화면과 함께 2~4단계에서 옮깁니다.
- 떠나기 가드(`useLeaveGuard`)는 설정 화면(4단계)과 함께 만듭니다.
- `ko.json`은 v1 `words`를 그대로 옮기고 v2 문장을 더했습니다. `"Jobs"`만 "작업 기록"에서 "작업"으로 바꿨습니다(대기열과 기록을 합친 화면).

**Go·CI 단계에 넘기는 것**(이번 커밋에서 하지 않음).

1. `internal/webui/assets.go`에 `//go:embed panelv2`와 `PanelV2()`를 더합니다. `panelv2/`에는 `_`나 `.`로 시작하는 파일이 없어 `all:`이 필요 없습니다.
2. `RUNQUILL_PANEL_V2=1`이면 라이브 패널과 `panel --demo` 모두 v2를 서빙합니다. 경로는 `GET /`(라이브면 `data-mode` 치환), `GET /theme.js`, `GET /assets/{file}`입니다. `{file}`은 `^(?:panel|chunk|demo)-[A-Za-z0-9_-]+\.(?:js|css)$`가 아니면 400이고, 라이브면 `demo-*`는 404입니다. CSP와 `no-store`는 v1과 같습니다. 아니면 v1 라우트를 그대로 둡니다.
3. [§7.2](#72-테스트)의 Go 테스트를 `panelv2` 기준으로 더합니다.
   - 임베드된 `index.html`이 참조하는 파일이 200인지 확인합니다. 진입 청크는 다른 청크를 ``import(`./chunk-….js`)``처럼 백틱 문자열로 부르고, 화면 청크는 진입 청크를 `from"./panel-….js"`로 되부릅니다. 따옴표 종류와 상관없이 `\./(panel|chunk|demo)-[A-Za-z0-9_-]+\.(js|css)`로 찾으면 됩니다.
   - `demo-*` 분리와 404를 확인합니다.
   - 번역 테스트가 v1 `i18n.js`와 `internal/webui/panel/src/i18n/ko.json`, `server-patterns.json`을 둘 다 검사하게 합니다.
   - 읽기 전용 검사(`TestPanelSamplePanelIsReadOnly`의 v2판)는 `src/**/*.test.ts(x)`를 빼야 합니다. 테스트 파일은 `fetch`와 `EventSource`를 직접 바꿔 끼웁니다. 예외는 `src/api.ts`와 `src/live.ts`입니다.
4. `.github/workflows/ci.yml`에 `panel` 작업을 더합니다([§6.5](#65-ci-최신성-검사), 경로는 `internal/webui/panelv2`). 순서는 `npm ci`, `npm run typecheck`, `npm test`, `npm run build`, `git diff --exit-code -- ../panelv2`와 `git status --porcelain -- ../panelv2`가 비었는지 확인, `npm run size`입니다.
   - `-diff` 속성 때문에 `git diff`는 내용을 보여 주지 않습니다. 종료 코드는 그대로이므로 실패 안내 문구를 함께 출력합니다.
   - 빌드는 Vite 경고 `<script src="./theme.js"> … can't be bundled without type="module"`를 출력합니다. 의도한 동작입니다.
5. `.dockerignore`에 `internal/webui/panel/`을 더합니다. 이미지에는 커밋된 `panelv2/`만 필요합니다.
6. Playwright e2e([§7.2](#72-테스트))를 Go 서버 대상으로 만들 때 1단계 확인 항목을 옮기면 됩니다.
   - 샘플: 개요 렌더, `/` 팔레트 → 호스트 → 제목 포커스, `#/queue?job=4828` → `#/jobs/4828`, 시나리오 전환, 테마 저장, 영어 페이지에 한글 없음, 폰 서랍.
   - 라이브: `?signin=not_enrolled`, `/api/me` 500, SSE, `/snapshot` 401, 연결 끊김.

### 7.4 2단계 읽기 화면 확인 결과

2026-11-28. 작업(4탭, 상세), 러너(목록, 상세 정보), 호스트(4탭, 상세), 사용량(2탭)을 만들었습니다. 쓰기 동작은 없고, 상세 칸 아래 동작 자리는 3단계가 채웁니다.

**확인한 것.** `npm run typecheck`, `npm test`(54개), `npm run build`, `npm run size`, `go vet ./...`, `go test ./...`. 브라우저는 `RUNQUILL_PANEL_V2=1 go run . panel --demo`(실제 CSP)를 Chromium(Playwright)으로 열었습니다.

- 새 화면과 상세 칸, 아래 주소를 모두 열었습니다. `securitypolicyviolation`과 콘솔 오류·경고가 0건이었습니다.
  - `#/jobs?tab=…`, `#/jobs/ID`(대기·실행 중·정리됨·기록에만 있음·없는 작업), 기록 필터
  - `#/runners/NAME`, `#/runners/NAME/logs`, 사라진 러너
  - `#/hosts?tab=…`, `#/hosts/agent%3A%2F%2Fedge-1`
  - `#/usage`(프리셋, 직접 날짜, 잘못된 범위), `#/usage?tab=rightsizing&group=job&days=3`
- 예전 주소: `#/queue`, `#/queue?job=4828`, `#/jobs?job=4823`, `#/runners?runner=…`, `#/logs`, `#/logs?runner=…`, `#/hosts?host=…`.
- 영어 페이지 본문과 `aria-label`·`title`·`placeholder`에 한글이 없습니다. 한국어 페이지에서 표 머리글·탭·버튼·설명에 영어 문장이 남지 않았습니다.
- 밝게·어둡게, 폰 폭(390×844). 폰에서는 상세가 전체 화면 시트이고 "목록으로"로 닫힙니다. 페이지 가로 넘침이 없고, 넓은 표는 앞 열을 고정한 채 가로로 스크롤합니다.
- 키보드 동작:
  - 표에서 ↑/↓와 Enter로 상세를 엽니다. 포커스가 칸으로 가고, J/K로 이전·다음 항목, Esc로 닫으면 포커스가 행으로 돌아옵니다.
  - 열고 닫기만 기록에 남아 뒤로·앞으로가 칸을 다시 열고 닫습니다. 필터와 탭은 `replaceState`입니다.
- 샘플 시나리오 전환(`#/jobs/4831`, `#/jobs/4835`)과 화면별 팔레트 명령을 확인했습니다.
- 클릭과 입력으로도 확인했습니다: 칸이 열린 채 탭 바꾸기, 작업 검색·풀·기록 필터, 디스크 사용량 측정, 워커 오프라인 시나리오의 호스트(`last_seen`)와 "임대가 관찰되지 않음" 러너. 샘플 모드에서는 `/api/` 요청이 하나도 나가지 않았습니다.

**확인하지 않은 것.** 라이브 모드의 REST는 브라우저에서 돌려 보지 않았습니다. 오류 알림, 403, 불러오는 중 표시, "이전 작업 더 보기" 페이지, 리소스·탄력 한도 폴링 주기가 여기에 해당합니다. `useRequest`의 불러오기·다시 불러오기·주소 변경·오류·401만 단위 테스트(`request.test.ts`)로 확인했습니다. §7.2의 `page.route` e2e가 생기면 같이 다룹니다.

**설계와 달라진 점.**

- **날짜 입력.** 사용량의 날짜는 `DateRangePicker` 대신 Stratum `Input type="date"` 두 개입니다. Stratum 0.1.1의 `DateRangePicker`는 달력의 `prevLabel`/`nextLabel`("이전 달"/"다음 달")을 넘기지 않아, 영어 페이지에 한글이 남습니다. Stratum에 제안할 것(G8과 같은 문제): `DatePicker`·`DateRangePicker`가 달력 문구 prop을 넘기게. 네이티브 입력은 브라우저 언어로 날짜를 보여 줍니다.
- **범위 주소.** 프리셋은 `range=2|7|90`(없으면 30일, 오늘 기준 UTC)이고 `from`/`to`가 있으면 직접 지정입니다. 프리셋 범위는 날짜를 주소에 넣지 않으므로, 공유한 링크는 여는 날을 기준으로 다시 계산됩니다.
- **표 행 높이.** Stratum `DataTable`은 행 높이가 하나로 고정입니다(`--data-table-row`). 보조 줄(재시도 시각, 이미지 상태, 판정 근거)이 있는 표는 `rows-2`·`rows-3` 클래스로 행을 높였습니다. Stratum에 제안할 것: 표별 행 높이 prop이나 여러 줄 셀.
- **`SplitView` 높이.** `.splitview`는 `height: 100%`라 넓은 화면에서는 목록 칸에 높이(`max(28rem, 100dvh − 12rem)`)를 주고 목록이 안에서 스크롤합니다. 폰에서는 높이를 풀어 페이지가 스크롤합니다.
- **탭과 상세 칸.** 화면마다 `SplitView` 하나가 모든 탭을 감싸므로, 탭을 바꾸거나 작업이 다른 목록으로 옮겨 가도 칸이 다시 열리지 않습니다. `Tabs.Tab`마다 `aria-controls`가 있어 `Tabs.Panel`은 모두 그리고, 내용은 보이는 탭에만 넣습니다. 리소스·탄력 한도의 폴링은 그 탭이 보일 때만 돕니다(§3.5).
- **작업 탭 고르기.** `#/jobs/ID`에 `tab`이 없으면 그 작업이 있는 실시간 목록 탭을 보여 주고, 작업이 대기 → 실행 중 → 정리됨으로 옮겨 가면 탭도 따라갑니다. 스냅샷에 없으면 기록 탭입니다. 실시간 탭에서 행을 열면 `tab`을 빼고, 기록 탭에서 열면 `tab=history`를 남깁니다.
- **작업 상세.** 스냅샷의 작업이면 스냅샷 필드를 보이고, `GET /api/jobs/{id}`는 리소스와 실행 시간을 더합니다. 작업 상태가 바뀌면 다시 부릅니다. 스냅샷에서 사라지면 같은 응답이 칸을 이어받습니다. 404는 "기록 없음"입니다. 기록 탭의 "새로 고침"은 목록과 열린 칸을 함께 다시 부릅니다.
- **라우터 포커스.** 1단계는 `hashchange`마다 제목에 포커스를 줬습니다. 이제는 화면이 바뀔 때와 같은 주소를 다시 열 때만 줍니다. 상세 칸을 여닫을 때는 `SplitView`의 포커스 처리를 따릅니다.
- **샘플 패널 API.** `main.tsx`가 샘플 청크의 `answer`를 `api.ts`의 `answerSamples`에 등록합니다. 그 뒤 `request()`는 GET만 fixture로 답하고 나머지는 "Not available in the sample panel."로 거부합니다. fixture가 `null`이면 404입니다. 샘플 스냅샷에 v1처럼 `recent`와 러너의 `runner_os`/`runner_image`를 넣었습니다.
- **화면별 팔레트 명령.** `src/commands.ts`의 `useScreenCommands`로 화면이 명령을 등록하고, 셸이 화면 이름 그룹으로 맨 앞에 둡니다.
- **문구.** v1 문장을 그대로 썼습니다. 새 키는 14개입니다(`SplitView` 문구, "History", "Show running jobs" 등). "실행 중" 탭은 v1의 "Assigned"(배정·끊김·정리 중 작업 포함)와 같은 목록입니다.

**3단계에 넘기는 것.**

- 작업 상세 칸 아래(`JobPane`)에 `SplitView.PaneFooter`를 더해 "지금 재시도"·"작업 취소"를 넣습니다. 끝나면 `live.refresh()`를 부르고 칸을 닫습니다(§3.3 5). 기록 탭 행의 대기 작업도 같습니다.
- "저장소 점검" 대화상자(admin, `POST /api/diagnose`)도 이때 넣습니다. 지금은 모든 역할이 대기 탭 빈 상태의 안내를 봅니다.
- 러너 칸에 `Tabs`(정보·로그)를 더합니다. `#/runners/NAME/logs`(`route.sub`)는 지금 정보만 엽니다. 러너 표의 "로그 보기" 열도 이때 더합니다(local·ssh만).
- 호스트 칸 아래에 배정 중지/재개(operator)와 워커 등록(admin, `agent://`, `enrollment_available`)을 넣습니다. 팔레트 명령은 `useScreenCommands`로 등록합니다.
- `request.ts`의 `useRequest`가 401을 무시하고 `AbortError`를 버리며, 샘플에서는 폴링하지 않습니다. 쓰기 요청은 `request()`를 직접 부르고 403은 토스트로 보입니다(§2.1).
- `stratum.ts`의 `split` 문구에는 저장하지 않은 변경 문구(`keepEditing`, `discardClose` 등)가 없습니다. 칸에서 편집하는 화면이 생기면 더합니다.

### 7.5 3단계 operator 동작 확인 결과

2026-11-28. operator·admin 쓰기 동작과 러너 로그 탭을 넣었습니다. 역할 제한과 확인 문구, API 호출은 v1과 같습니다.

**만든 것.**

- **작업 재시도·취소**(operator). 작업 상세 칸 아래 `PaneFooter`에 둡니다. 대상은 v1 `jobActionButtons`와 같습니다.
  - 실시간 목록의 작업은 `pendingJob && !runner`일 때, 기록의 작업은 `outcome === "waiting"`일 때입니다.
  - "지금 재시도"는 `retry_at`이 있을 때만 보입니다. "작업 취소"는 위험 확인(`ConfirmDialog tone="danger"`)을 거칩니다.
  - 끝나면 성공 토스트를 띄우고, 칸을 닫고, `live.refresh()`를 부르고, 기록을 다시 불러옵니다.
  - 칸이 열려 있으면 팔레트에도 "작업 #ID 지금 재시도"·"작업 #ID 취소"가 나옵니다.
- **저장소 점검**(admin). 작업 머리글, 빈 대기 탭, 팔레트에서 같은 `Dialog`가 열립니다. `POST /api/diagnose`를 10초 제한으로 부르고, 결과 `Notice`와 보고서 `<pre>`를 보입니다.
- **배정 중지·재개**(operator). 호스트 상세 칸 아래와 팔레트("{host}에서 배정 중지/재개", 모든 호스트)에 둡니다. v1처럼 확인 없이 `POST /api/hosts/cordon`을 부르고, 성공 토스트 뒤 `live.refresh()`를 부릅니다.
- **워커 등록**(admin, `agent://`, `enrollment_available`). 상세 칸 아래 버튼이 `POST /api/hosts/{name}/enrollment`를 부르고, 결과를 `Dialog` 안 로컬 `CopyField` 두 개(등록 명령, 시작 명령)와 만료 시각으로 보입니다. 닫으면 상태에서 지웁니다.
- **러너 로그 탭**. `#/runners/NAME/logs`이고, operator 이상과 샘플 패널에서 보입니다(v1과 같음). viewer는 탭이 없고 같은 주소에서 정보 탭을 봅니다.
  - K8s·`agent://`·작업 없는 러너는 탭 안에 이유를 보입니다.
  - 러너 표에 "로그" 열("로그 보기" 링크, local·ssh만)을 더했습니다. 러너 화면 팔레트에 러너별 "로그 보기"가 있습니다.
- **로컬 `LogView`**(G3, `screens/runners/RunnerLogs.tsx`). v1 `logs.js` 동작을 그대로 옮겼습니다.
  - 줄 수 100~2000, 따라가기, 줄바꿈, 일시 정지(대기 줄 수), 중지, 다시 연결(연결 끊김·429·502·503일 때만), 복사, 다운로드, 지우기, 최신으로.
  - 5000줄과 줄당 4000자 상한, 버린 줄 안내, 15분·8 MiB 한도 문구, 러너가 끝나면 스트림을 닫습니다.
  - 줄 정리와 상한은 순수 함수(`logLines.ts`)와 단위 테스트로 뺐습니다.
  - 줄은 `textContent`로만 쓰고, 프레임마다 한 번 React 밖에서 붙입니다. 가상 스크롤은 쓰지 않습니다(v1도 쓰지 않음).
  - 상태 줄만 라이브 영역입니다. 줄 수는 낭독하지 않습니다. 출력 영역은 `role="region"` + 이름이 있고 키보드로 스크롤합니다.
  - 다운로드는 v1처럼 보이는 줄을 브라우저에서 `Blob`으로 저장합니다.
- **`api.ts`의 `stream`**. v1과 같고 제한 시간이 없습니다. 401이면 세션을 끝냅니다. 샘플 패널에서는 fixture 문자열을 끝난 `Response` 하나로 돌려주므로, 로그 탭이 같은 코드로 고정 줄을 보입니다. `download`(백업의 진행률 POST)는 쓰는 화면이 없어 4단계로 미룹니다.
- **공통 쓰기 처리**(`src/actions.ts`, `src/session.ts`).
  - `can(role)`은 v1 `ctx.can`과 같고 샘플에서는 항상 false입니다. 그래서 샘플 패널에는 쓰기 버튼과 쓰기 명령이 없습니다.
  - `post()`는 실패를 위험 토스트로 보입니다. 403도 서버 문장 그대로 보이고, 401은 보이지 않습니다(세션 종료가 맡음). `ApiError`가 아니면 "Request failed."입니다.
  - `useBusy()`는 진행 중 버튼에 Stratum `loading`을 줍니다. `disabled`가 아니라서 포커스가 버튼에 남습니다. 진행 중에 클릭이나 팔레트로 같은 동작을 또 시작하면 무시합니다.
- **팔레트 명령 병합.** `useScreenCommands`가 목록 하나만 받던 것을 여러 목록(화면과 열린 칸)을 합치게 바꿨습니다. 칸이 닫히면 칸의 명령도 빠집니다.

**확인한 것.**

- `npm run typecheck`, `npm test`(61개), `npm run build`(두 번 빌드해 `diff -r` 같음), `npm run size`, `go vet ./...`, `go test ./...`. `TestPanelV2SamplePanelIsReadOnly`도 통과했습니다.
- **Playwright e2e**(`npm run e2e`, `e2e/panel.e2e.ts`, 13개). 대상은 `go run . panel --demo`로 띄운 실제 Go 서버입니다. Chromium, Firefox, WebKit에서 모두 통과했습니다.
  - 매 테스트에서 CSP 위반과 콘솔 오류가 0건인지 확인합니다. 라이브 모드에서 `demo-*`를 요청하지 않는지도 확인합니다.
  - 샘플: 로그 탭의 고정 줄(이스케이프 제거), 따라가기 꺼짐, 쓰기 버튼 없음, `/api/` 요청 0건.
  - 라이브: 재시도·취소·배정 중지 왕복(확인 → `POST` → 토스트 → `/snapshot` 다시 읽기), 403 토스트와 버튼 포커스 유지, 팔레트의 작업 명령과 확인 뒤 포커스, 워커 등록 대화상자(포커스 진입·복귀), 저장소 점검 보고서와 팔레트로 연 뒤의 포커스, viewer에게 쓰기·로그 없음, REST 오류 알림, 401 뒤 로그인 화면, 기록 불러오는 중(`aria-busy`), "이전 작업 더 보기"(`before` 커서), 스냅샷이 없거나 `fleet`이 없을 때 작업·러너·호스트 화면.
  - 로그: 한 번에 끝나는 응답과 다시 연결, 그리고 시간에 따라 줄을 흘려보내는 따라가기 스트림. 일시 정지(대기 줄 수, 줄이 늘지 않음), 재개, 위로 스크롤한 뒤 "최신으로", 중지, 다시 연결을 확인합니다. 정보 탭으로 바꾸거나 칸을 닫으면 서버 쪽 연결이 닫히는지도 확인합니다. 이 테스트는 같은 출처의 작은 Node 서버가 Go 서버를 프록시하고 로그 경로만 직접 씁니다. Chromium이 다른 포트의 loopback으로 돌리는 `route.continue`를 Local Network Access로 막기 때문입니다.
- **라이브 모드는 가로채기로 확인했습니다.** 실제 매니저의 브라우저 세션은 GitHub OAuth 콜백으로만 생기고, 매니저를 띄우려면 GitHub App 설정도 필요합니다. 그래서 §7.2대로 Go 데모 서버의 `/`를 `route.fetch()`로 받아 `data-mode="live"`로 바꿉니다. 이렇게 하면 실제 CSP 헤더가 남습니다. `/api/*`와 `/snapshot`은 테스트가 답합니다(`e2e/live.ts`).
  - `/api/events`와 `/snapshot`은 바뀌는 스냅샷 하나를 같이 씁니다. 쓰기 처리기가 이 스냅샷을 바꿉니다.
  - 쓰기 응답은 Go 처리기와 같은 상태 코드와 본문입니다(204, 403 문장).
- **눈으로 본 것.** Go 데모 서버를 Chromium으로 열어 영어·한국어 × 밝게·어둡게로 작업 칸, 확인 대화상자, 로그 탭, 워커 등록, 저장소 점검을 봤습니다. 폰 폭(390×844)에서는 로그 시트를 봤습니다.
  - 영어 페이지에 한글이 없고, 페이지 가로 넘침이 없습니다.

**설계와 달라진 점·알게 된 것.**

- **진입 청크의 정적 import.** 이번 변경 뒤로 Rolldown이 진입 청크와 화면 청크가 함께 쓰는 모듈(셸 코드와 Stratum 내부)을 공유 청크 2개로 끌어냈습니다. 진입 청크는 이제 이 둘을 정적으로 import합니다. 러너·호스트 화면 변경을 되돌리면 사라지는 것으로 원인 범위를 확인했지만, 정확한 규칙은 찾지 않았습니다.
  - 첫 화면은 3.9 KB 늘어 132.8 KB입니다. `demo-*`는 여전히 정적 import에 없습니다.
  - Go 쪽 "임베드된 파일 참조" 검사는 따옴표 종류와 상관없이 `./(panel|chunk|demo)-…`를 찾으므로 영향이 없습니다.
  - 1단계 기록의 "진입 청크는 정적 import가 없습니다"는 더 이상 맞지 않습니다.
- **팔레트에서 연 확인의 포커스.** 팔레트 명령이 연 `ConfirmDialog`를 닫으면, 원래 포커스를 돌려받을 팔레트 입력란이 이미 없거나 닫히는 중입니다. Firefox에서는 닫히는 `<dialog>` 안에 남아 있습니다. 그래서 포커스가 `<body>`로 떨어졌습니다.
  - `confirm()`에 `returnFocus`를 더했습니다. 닫은 뒤 포커스가 `<body>`나 닫힌 `<dialog>` 안에 있으면 그 요소(칸 아래 같은 버튼)로 옮깁니다.
  - 배정 중지 팔레트 명령은 대화상자가 없어 포커스를 옮기지 않습니다.
- **WebKit은 클릭한 버튼에 포커스를 주지 않습니다**(macOS Safari 동작). 포커스를 확인하는 e2e는 버튼에 포커스를 준 뒤 Enter로 누릅니다.
- **스냅샷이 없을 때 호스트 화면의 무한 렌더.** 첫 구현은 `fleet?.hosts ?? []`로 렌더마다 새 배열을 만들었습니다. 이 배열을 키로 한 팔레트 명령이 매번 다시 등록되어 셸이 다시 그렸고, React 오류 #185(업데이트 깊이 초과)가 났습니다. 첫 스냅샷 전, 연결이 끊긴 채 연 경우, 호스트가 없는 경우에 해당합니다. 모듈 수준의 빈 배열로 고쳤고 e2e로 막았습니다. `useScreenCommands`에 넘기는 목록은 안정된 값으로 memo해야 합니다.
- **팔레트로 연 대화상자의 포커스(저장소 점검).** `confirm()`과 같은 문제입니다. 공용 `refocus(fallback)`(`dialogs.tsx`)가 닫은 뒤 포커스가 `<body>`나 닫힌 `<dialog>` 안에 있으면 대체 요소로 옮깁니다. 팔레트에서 열기 전에 버튼에 포커스를 주는 방법은, 닫히는 팔레트와 엇갈려 열린 대화상자 밖으로 포커스가 빠져서 쓰지 않았습니다.
- **대화상자 안의 결과는 대화상자 안에.** 저장소 점검 오류와 결과, 복사 결과는 대화상자 안 상태 줄에 보입니다. 대화상자 밖 동작의 실패만 토스트입니다.
- **행 `Menu`는 미룹니다.** §1이 적은 대기 탭 행과 호스트 행의 `Menu`는 만들지 않았습니다. 같은 동작이 칸 아래와 팔레트에 있습니다. `DataTable`은 `onRowAction`이 행 전체 클릭·Enter를 받아서, 셀 안 메뉴 버튼과의 키보드·포인터 충돌을 따로 확인해야 합니다. Stratum에 행 동작 열을 제안하거나 4·5단계에서 다시 봅니다.
- **폰 로그 도구.** §4.5의 "폰에서는 도구를 `Menu` 하나로 접음"은 미룹니다. 지금은 도구 줄이 접혀 두 줄이 되고, 시트 폭(390px)에서 넘치지 않습니다.
- **새 문장.** "Report", "Retry job #{id} now", "Cancel job #{id}" 3개입니다. 나머지는 v1 문장입니다.
- **의존성.** `@playwright/test` 1.65.1을 dev 의존성으로 정확히 고정했습니다. 이 버전의 `browsers.json`이 Playwright 캐시의 엔진(chromium 1255, firefox 1572, webkit 2388)과 같습니다. 런타임 의존성은 그대로입니다.

**확인하지 않은 것.**

- 눈으로 본 확인(스크린샷)은 Chromium만 했습니다. Firefox와 WebKit은 e2e 스위트만 돌렸습니다.
- 로그 출력 영역의 키보드 확인은 프로그램으로 포커스를 준 것까지입니다. 화살표·Page Up/Down 스크롤은 손으로 확인하지 않았습니다.
- 진입 청크가 공유 청크 2개를 정적으로 부르게 된 Rolldown의 정확한 규칙은 찾지 않았습니다.
- 15분 따라가기 한도와 8 MiB 한도 문구, 러너가 끝나 스트림이 닫히는 경우는 실제 서버로 확인하지 않았습니다. 문구 분기는 v1 코드를 그대로 옮겼습니다.
- 스크린 리더로 낭독을 듣지는 않았습니다. 역할과 이름만 e2e로 확인했습니다.

**4단계에 넘기는 것.**

- `api.ts`에 `download`(v1 `api.js`, 진행률 콜백, 제한 없음)를 옮깁니다. 백업의 큐 백업 다운로드가 처음 씁니다. 샘플에서는 거부합니다.
- 쓰기 화면은 `post()`·`useBusy()`·`can()`과 `confirm()`을 그대로 씁니다. 폼 오류처럼 대화상자 안에서 일어나는 실패는 그 자리에 보입니다(`Diagnose.tsx`가 예).
- 관리 화면의 팔레트 명령은 `useScreenCommands`로 등록합니다. 이제 여러 목록을 합칩니다.
- e2e는 `e2e/live.ts`의 `startLive(page, role, snapshot)`로 라이브 모드를 띄웁니다. 4단계에서는 §7.2에 남은 항목을 더합니다.
  - 로그인 실패 이유 5개, 연결 끊김 표시, 설정의 저장 안 한 변경 가드, 토큰 한 번 표시.
  - 설정·팀·토큰·감사·백업·이전의 왕복.
- CI에는 아직 e2e가 없습니다. 5단계(또는 4단계 끝)에 `panel` 작업에 `npx playwright install --with-deps chromium`과 `npm run e2e`를 더합니다. 서버는 `go run`으로 뜨므로 그 작업에 Go가 필요합니다.
- 한국어 카탈로그는 25.5 KB / 30 KB입니다. 4단계 화면 문장은 대부분 v1에 있지만, 넘으면 서버 문장과 화면 문장을 나눕니다.
- `stratum.ts`의 `split` 문구에 저장하지 않은 변경 문구가 아직 없습니다(설정 화면에서 더함).
- 행 `Menu`(작업·호스트)와 폰 로그 도구 `Menu`는 위의 이유로 미뤘습니다.

### 7.6 4단계 관리 화면 확인 결과

2026-11-28. 설정, 팀, API 토큰, 감사 기록, 백업, 워크플로 이전을 만들었습니다. API 호출, 검증, 역할 제한과 확인 문구는 v1과 같습니다. §1에서 열려 있는 것은 아래 "5단계에 넘기는 것"에 적은 항목뿐입니다.

**만든 것.**

- **설정**(`screens/settings/`). `GET`/`PUT /api/settings`(10초)입니다. 전체·Docker 워커·워커 풀 세 `FormSection`과 `SaveBar`(`docked`, ⌘S/Ctrl+S, 탭 닫기 경고)로 이뤄집니다.
  - 변경 수는 `useFormDirty`(전체 설정 세 값)와 워커·풀 목록의 항목별 JSON 비교(추가·삭제·변경 각 1건)의 합입니다.
  - 본문 조립은 순수 함수 `settingsBody`(`draft.ts`)로 뺐고 단위 테스트가 있습니다. 원래 풀 객체를 먼저 펼친 뒤 덮어씁니다. 그래서 `cache_writes`·`max_per_repository`처럼 편집기에 없는 필드가 그대로 저장됩니다. 그 밖의 v1 규칙도 같습니다:
    - `privileged`가 원래 없었으면 없는 채로 둡니다.
    - `warm`·`warm_max_idle_minutes`·`images`는 비면 보내지 않습니다.
    - 저장소를 제한하지 않으면 `repositories: null`입니다.
    - `host_limits`에는 값이 있는 워커만 넣습니다.
    - 러너 OS 이름(형식, 64개)을 검사하고 풀 이름 중복을 거부합니다.
  - 저장 실패(위 검사와 서버 문장, `revision` 충돌 포함)는 `SaveBar`의 오류 톤에 문장으로 나오고, 고친 값은 그대로 남습니다. 네이티브 검사(필수, 풀 이름 패턴, 숫자 범위)는 요청 전에 막고 첫 칸에 포커스를 줍니다. 접힌 풀 안의 잘못된 칸은 그 풀을 엽니다.
  - `agent://` 워커는 읽기 전용이고 제거할 수 없습니다. 워커·풀 제거는 이름이 있을 때 위험 확인을 거치고, 포커스는 다음 항목이나 추가 버튼으로 갑니다.
  - "저장된 설정 불러오기"(머리글과 팔레트)는 변경이 있으면 위험 확인을 거칩니다. `backend`가 `docker`가 아니면 v1처럼 워커와 풀 섹션을 숨깁니다(값은 그대로 저장).
- **떠나기 가드**(`router.ts`의 `useLeaveGuard`). 설정 화면에 저장하지 않은 변경이 있으면, 다른 화면으로 가는 이동이 막힙니다. 링크, 팔레트, 뒤로 가기 모두 해당합니다. 주소는 `replaceState`로 되돌리고, `SaveBar`가 흔들리며 "변경 N건을 저장하지 않아 이동하지 않았습니다"와 "버리고 이동"을 보입니다. "버리고 이동"은 값을 되돌리고 막혔던 주소로 갑니다. 같은 화면 안의 쿼리 변경은 막지 않습니다.
- **팀**(`screens/team/`, admin). 추가(숫자만이면 `PUT /api/team/{id}`, 아니면 `POST /api/team` 10초, `@1234`는 숫자 로그인), 역할 `Select`와 바뀌었을 때만 나타나는 "역할 변경"(확인), 사용 중지(위험 확인)·다시 사용, `after` 커서 100명씩 이전/다음(빈 다음 페이지면 머무름, v1과 같음). 추가 폼의 오류는 폼 아래, 행 동작의 실패는 토스트입니다.
- **API 토큰**(`screens/tokens/`, 모든 역할, 사용자 메뉴). viewer는 `fleet:read`만 고를 수 있고 나머지는 비활성이며 이유가 보입니다. 만든 비밀값은 `CopyField`로 한 번 보이고 포커스·선택됩니다. "토큰 숨기기", 그 토큰 폐기, 화면 떠나기가 값을 지웁니다(상태에만 있음). 폐기는 위험 확인입니다. 만료일은 서버가 주는 Unix 초를 그대로 읽습니다(`format.ts`의 `toDate`가 v1처럼 숫자를 받게 바꿈).
- **감사 기록**(`screens/audit/`, admin). 동작 종류와 행위자가 주소에 남습니다. 동작은 바꾸면 바로, 행위자는 입력이 450ms 멈춘 뒤(Enter는 바로) 다시 부릅니다. "이전 기록 더 보기"는 `before`, 50개 미만이면 숨깁니다. 영어 페이지에서는 동작 코드에 영어 문구가 없어 코드만 한 번 보입니다(v1은 같은 코드를 두 번 보였음).
- **백업**(`screens/backups/`, admin, 샘플 있음). 일정·마지막 실행·검증·켜는 방법은 `GET /api/backups/status` 하나로 그립니다. 검증(`POST`, 30분+10초)은 모듈 수준 약속으로 두어 화면을 떠나도 계속됩니다. 돌아오면 진행 중 표시를 보이고, 끝나면 결과와 새 상태를 보입니다. 큐 백업 다운로드는 `api.ts`의 `download`(v1과 같이 제한 없음, 401이면 세션 종료, 샘플에서 거부)와 Stratum `Progress`(크기를 모르면 불확정)를 씁니다. 받은 Blob은 `actions.ts`의 `saveFile`로 저장합니다(러너 로그 다운로드와 공용).
- **워크플로 이전**(`screens/migrate/`, admin, 샘플 있음). 1 권한(빠지면 안내, 실패하면 "다시 확인"), 2 저장소와 매핑(풀 목록은 스냅샷 + 라이브에서 `GET /api/settings`), 3 미리보기, 4 PR 열기입니다.
  - 검증 규칙과 diff 해석은 순수 함수(`mapping.ts`)이고 단위 테스트가 있습니다. 문제가 있으면 그 칸에 `aria-invalid`와 포커스를 주고, 상태 줄에 이유를 보입니다.
  - 미리보기는 파일마다 `Accordion` 항목입니다. 로컬 `DiffView`(G11, 이전·새 줄 번호, 낭독용 "삭제"/"추가", 가로 스크롤 영역)와 남긴 작업 표가 들어갑니다.
  - 입력을 바꾸면 미리보기를 버립니다. PR 열기는 확인 뒤 `POST /api/migrate/open`입니다. `stale` 코드면 한 번 자동으로 다시 미리 보고, `branch_exists`면 기존 PR 링크나 브랜치 삭제 안내를 보입니다.
  - 샘플은 고정 미리보기(`GET /api/migrate/preview` fixture)를 보이고 4단계를 그리지 않습니다.
- **셸.** 라우트 표의 모든 화면에 컴포넌트가 생겨 자리표시자(`Placeholder`)와 그 문장 2개를 지웠습니다. 공용 `StatusLine`(설명 줄, `role="status"`)과 쓰기 도우미 `send(method, …)`(`post`는 그 위의 한 줄)를 더했습니다.

**확인한 것.**

- `npm run typecheck`, `npm test`(81개), `npm run build`(두 번 빌드해 `diff -r` 같음), `npm run size`, `go vet ./...`, `go test ./...`. `TestPanelV2SamplePanelIsReadOnly`와 Go 번역 테스트도 통과했습니다.
- **Playwright e2e**(`npx playwright test`, 29개 × Chromium·Firefox·WebKit = 87개 통과). 새 파일 `e2e/admin.e2e.ts`(16개)는 모두 `startLive(page, role, snapshot)`로 라이브 모드를 띄웁니다.
  - 로그인 실패 이유 5개(한 번 보이고 주소에서 지움, 다시 불러오면 없음)와 모르는 코드.
  - 저장 안 한 변경 가드: 탐색 링크·팔레트·뒤로 가기가 모두 막히고 값이 남습니다. 머문 채 다시 고치면 경고가 사라지고, "버리고 이동"이 막혔던 화면을 엽니다.
  - 설정 저장 왕복: 편집기에 없는 필드 보존과 `privileged` 부재를 확인합니다. 서버 400 문장이 저장 바에 나오고 값이 남습니다. ⌘S 저장 뒤 가드가 풀립니다. 네이티브 검사 실패는 요청을 보내지 않습니다.
  - 토큰 한 번 표시(viewer 범위 제한, 포커스, 숨기기 뒤 페이지에 값 없음, 화면을 떠났다 오면 없음).
  - 팀 역할 변경(확인 → `PUT {role, disabled}` → 토스트).
  - 감사 필터의 주소와 이전 페이지.
  - 백업 다운로드: 세 엔진 모두 실제 CSP 아래에서 `download` 이벤트와 파일 이름 `runquill-queue-backup.sqlite3`을 확인했습니다.
  - 이전 미리보기(검증 포커스, 요청 본문, diff 줄 번호, 권한이 빠지면 열기 비활성, 입력 변경 시 미리보기 제거).
  - 샘플 패널의 백업·이전 fixture, 쓰기 버튼 없음, `/api/` 요청 0건, 샘플에 없는 관리 화면은 개요로 이동.
  - 매 테스트에서 CSP 위반과 콘솔 오류가 0건입니다.
- **눈으로 본 것.** Go 데모 서버(실제 CSP)에서 라이브 모드를 가로채 Chromium으로 봤습니다.
  - 여섯 화면을 영어·밝게와 한국어·어둡게로 확인했습니다. 저장 바 가드, 확인 대화상자, diff 표도 봤습니다.
  - 폰 폭(390px)에서 페이지 가로 넘침이 없습니다. diff는 표 안에서 가로로 스크롤합니다.
  - 영어 페이지 본문과 `aria-label`·`title`·`placeholder`에 한글이 없습니다(스크립트로 확인).
  - 키보드: 설정에서 Tab이 전체 → 워커(입력, 제거) → 워커 추가 → 풀(접기 버튼, 칸, 풀 제거) → 풀 추가 → 저장 바 순서로 갑니다. 가드 상태에서는 "버리고 이동"에 닿습니다.

**설계와 달라진 점·알게 된 것.**

- **`TagInput` 대신 텍스트 입력.** 허용 OS(쉼표), 워커 주소·허용 저장소(한 줄에 하나, `Textarea`), 이전 매핑의 라벨(쉼표)은 v1과 같은 텍스트 입력입니다. Stratum 0.1.1 `TagInput`은 Enter나 쉼표를 누르지 않은 입력을 blur 때 칩으로 만들지 않습니다. 그래서 그대로 저장하면 마지막에 친 값이 조용히 빠집니다. 기본 문구도 한국어입니다. **Stratum에 제안할 것:** blur(또는 폼 제출) 때 남은 입력을 칩으로 추가하는 옵션. 저장 형식(배열)은 같습니다.
- **`NumberInput` 대신 `Input type="number"`.** `NumberInput`의 증감 버튼 이름과 범위 문구 기본값이 한국어라 모든 칸에 넘겨야 합니다. 네이티브 숫자 입력과 브라우저의 검사 문구로 충분했습니다.
- **라우터의 주소 저장소.** `useHash`가 `location.hash`가 아니라 "받아들인 주소"를 돌려줍니다. `location.hash = …`는 주소를 바로 바꾸지만 `hashchange`(와 가드)는 다음 작업에서 옵니다. 그 사이에 셸이 다시 그리면(팔레트가 닫히며 그럼) 가드가 돌기 전에 설정 화면이 사라집니다. 막힌 이동은 기록 항목 하나를 남깁니다(v1과 같음). 그래서 뒤로 가기로 막힌 뒤에는 같은 주소 항목이 둘이 됩니다.
- **`SaveBar`는 폼과 같이 붙입니다.** `SaveBar`는 마운트할 때 감싼 폼의 `submit`에 한 번 붙습니다. 그래서 폼과 바를 설정을 불러온 뒤 함께 그리고, 폼은 `preventDefault`도 합니다. CSP의 `form-action 'none'` 때문에 네이티브 제출은 어차피 막힙니다.
- **`useFormDirty.commit(next)`는 기준값만 바꿉니다.** 불러오기와 저장 뒤에는 `commit`과 `setValues`를 같이 부릅니다. 한 응답에서 만든 draft 하나를 기준과 현재 값에 함께 써서, 불러온 직후 변경 수가 0입니다(단위 테스트).
- **팔레트 명령 목록은 안정된 값이어야 합니다**(§7.5). 감사 기록의 "새로 고침"처럼 `reload` 함수에 기대는 명령은 ref로 감쌌습니다.
- **단계 표시.** §4.11의 `Steps` 머리띠 대신 "1. 권한"…"4. PR 열기" 블록 제목을 씁니다(v1과 같은 문장). 상태는 블록 안 `Badge`와 상태 줄이 보입니다.
- **새 문장 10개.** 저장 바 문구 8개("{n} unsaved changes", "Discard and leave" 등), "Read-only: set in the deployment.", "Viewers can create read-only tokens."입니다. 나머지는 v1 문장입니다.
- **서버 변경 없음.** Go 코드는 바꾸지 않았습니다.

**확인하지 않은 것.**

- 실제 매니저(GitHub 로그인 세션)로는 돌리지 않았습니다. 모든 라이브 확인은 §7.5와 같은 가로채기입니다. `PUT /api/settings`의 워커 제거 확인(연결 확인, 사용 중 거부)과 `revision` 충돌 문구는 서버 테스트와 문장 전달만 확인했습니다.
- 30분짜리 백업 검증과 큰 큐 백업의 진행률(Content-Length가 있을 때의 퍼센트)은 작은 응답으로만 확인했습니다.
- `stale`·`branch_exists` 분기는 v1 코드를 옮겼고 e2e로 돌리지 않았습니다.
- 눈으로 본 확인은 Chromium만 했습니다. 스크린 리더로 듣지는 않았습니다.

**5단계에 넘기는 것.**

v2를 기본값으로 만들기 위해 5단계가 할 일입니다(§6, §7.1 "겹치는 동안의 값" 표).

1. **출력 옮기기.** `vite.config.ts`의 `outDir`를 `../panelweb`으로 바꿉니다. v1 소스(`panelweb/*.js`, `views/`, `demo/`, `i18n.js`, CSS)는 `emptyOutDir` 전에 지우거나, 6단계까지 둘 경우 다른 곳으로 옮깁니다. `panelv2/`는 지웁니다. `.gitattributes`의 생성 표시와 README의 경로도 바꿉니다.
2. **서빙 전환.** `internal/webui/assets.go`에서 `//go:embed panelv2`와 `PanelV2()`를 지우고 `panelweb`만 남깁니다. `internal/app/panel.go`에서 `RUNQUILL_PANEL_V2` 분기를 없애고 §6.4의 정적 라우트(`/`, `/theme.js`, `/assets/{file}`, 라이브에서 `demo-*` 404)만 남깁니다. v1의 `/views/*.js`·`/demo/*.js` 라우트도 지웁니다. `TestPanelRoutesAndFirstJobEvidence`와 임베드 참조 검사도 같이 바꿉니다.
3. **번역 테스트.** Go의 `panelTranslations`에서 v1 `i18n.js` 읽기를 지우고 v2 JSON만 검사합니다. 서버 문장의 번역은 한 곳(`ko.json`, `server-patterns.json`)에만 넣습니다.
4. **CI.** 최신성 검사 경로를 `internal/webui/panelweb`으로 바꿉니다(§6.5). `panel` 작업에 `npx playwright install --with-deps chromium`과 `npm run e2e`를 더합니다(Go가 필요함). 서버 라우트를 쓰는 다른 검사도 확인합니다.
5. **Pages와 데모 tar.** GitHub Pages 배포와 릴리스의 데모 tar가 새 `panelweb/`(`index.html`, `theme.js`, `assets/`)를 올리는지 확인합니다. 해시 라우팅이라 서버 대체 라우트는 필요 없습니다.
6. **문서.** `docs/panel.md`·`docs/panel.ko.md`를 새 화면 구성과 주소로 고칩니다: 작업 탭, 러너 로그 탭, API 토큰 화면, 설정 가드의 바뀐 동작, 예전 주소 표(§2.4), 지원 브라우저(§9 Q7). `docs/development.md`의 패널 빌드, README의 패널 설명(설치·첫 작업 앵커는 유지), `internal/webui/panel/README.md`의 출력 경로도 고칩니다. 릴리스 노트에는 화면 변경과 예전 주소 동작을 적습니다.
7. **`api/openapi.yaml`.** 패널이 쓰는 응답 필드와 맞는지 확인합니다. 특히 `expires_at`·`occurred_at`이 Unix 초이고, `/api/migrate/preview`의 `files[].jobs`가 `null`일 수 있습니다.
8. **§1 전체 점검**(파일 단위)과 남은 ⏳ 항목:
   - 대기 탭 행과 호스트 행의 `Menu`. `DataTable`의 `onRowAction`과 셀 안 버튼의 충돌을 확인한 뒤 넣거나, Stratum 행 동작 열을 기다린다고 기록합니다.
   - 폰 로그 도구 `Menu`.
9. **Stratum 제안 정리**(§8과 §7.4~7.6): `TagInput`의 blur 추가, `NumberInput`·`DatePicker` 문구 prop, `DataTable` 행 높이와 현재 행, 행 동작 열.

## 8. Stratum에 없는 것

Stratum의 컴포넌트 표(README "컴포넌트")와 `st/` 소스를 기준으로, 패널에 필요한데 없는 것입니다. 원칙: **Runquill에만 의미가 있는 것은 패널 안 로컬 컴포넌트**(`internal/webui/panel/src/components/`), **어느 앱에나 쓸 일반 부품은 Stratum에 제안**(Stratum 저장소에서 따로, CONVENTIONS의 범용성 규칙을 따라)하되 패널은 Stratum 릴리스를 기다리지 않고 로컬로 먼저 만듭니다. Stratum에 들어가면 로컬 것을 지웁니다.

| # | 필요한 것 | 지금 패널 | 제안 | 이유 |
| --- | --- | --- | --- | --- |
| G1 | 미터(용량·사용률·예약 대비 사용) | `<progress class="meter">` | **Stratum에 `Meter`**(네이티브 `<meter>` 스타일, low/high/optimum 톤). 그때까지 로컬 | 용량은 진행이 아니라 `progressbar` 역할이 맞지 않습니다. `Progress`(`st/progress/Progress.tsx`)는 진행률 전용. 범용 부품 |
| G2 | 일별 막대 차트(사용량), 단계 시간 누적 막대(작업 상세) | 손으로 그린 SVG(`pw/views/usage.js:73-100`, `pw/views/jobinsight.js:37-80`) | **로컬** `BarChart`, `StageBar`(SVG, 장식용 `aria-hidden` + 숫자는 범례·표에) | 각각 한 곳에서만 쓰고 수십 줄입니다. 차트 라이브러리는 번들 예산을 넘습니다. 두 번째 앱이 필요로 하면 Stratum `Sparkline`/`BarChart`를 검토 |
| G3 | 로그 뷰어(스트리밍, 따라가기, 일시 정지 버퍼, 줄 상한, ANSI 제거, 줄바꿈 토글, 최신으로) | `pw/views/logs.js` | **로컬** `LogView` | 줄 정리 규칙이 서버 동작(타임스탬프, 상한 표시 `[Runquill: log output limit reached]`)과 묶여 있습니다. Stratum에는 가상 스크롤도 없습니다(README "알려진 한계"). 5000줄 상한과 프레임당 한 번 붙이기로 충분 |
| G4 | 실시간 연결 표시(점 + 문구 + 갱신 시각, 상태가 바뀔 때만 낭독) | `#connection-state` | **로컬** `LiveStatus`(`Badge dot` + 문구 + `useNow`) | 상태 이름과 낭독 규칙이 패널 고유. `Badge dot`으로 모양은 충분 |
| G5 | 키-값 목록(작업 정보, 백업 일정, GitHub 연결) | `createKv`(`pw/ui.js:144-180`) | **Stratum에 `DescriptionList`**. 그때까지 로컬 `KeyValue`(`<dl>` + 유틸리티) | 관리 UI 어디에나 있는 범용 부품 |
| G6 | 페이지·블록 안 알림 띠(연결 끊김, `docker_error`, 이유, 로그인 실패) | `.notice`, `.reason` | **Stratum에 `Callout`**(tone, 아이콘, 제목, 동작, `role` 선택). 그때까지 로컬 `Notice` | `Toast`는 사라지고, `EmptyState`는 빈 화면용. 머무는 알림이 없습니다 |
| G7 | 명령 팔레트 차이 | `pw/app.js:435-635` | 앱에서 메울 것: `/` 키는 앱이 `open`을 제어해 열기, 영어·한국어 동시 일치는 `CommandItem.keywords`에 영어 원문. **Stratum에 제안**: 입력했을 때만 보이는 항목(`searchOnly`, 러너·작업·호스트 이동용), 결과 수 상한(`limit`, 지금 50), Page Up/Down | `CommandPalette`는 ⌘K/Ctrl+K만 처리하고(`st/commandpalette/CommandPalette.tsx:104-113`), 빈 검색에서 모든 항목을 보여 주며, 상한이 없습니다. 스냅샷 항목이 수백 개면 빈 팔레트가 길어집니다. 그 전까지는 스냅샷 항목을 마지막 그룹에 두고 개수를 줄여 넘깁니다 |
| G8 | 영어 기본 문구 | `tr()` | **Stratum에 문구 공급자**(`<StratumMessages value={…}>` 같은 컨텍스트 하나). 그때까지 §5.6의 prop 목록을 `src/i18n/stratum.ts`에서 넘김 | Stratum의 문구 기본값은 한국어이고(CONVENTIONS "문구 기본값은 한국어. 바꿀 수 있게 prop으로 연다"), 전역으로 바꾸는 방법이 없습니다. 영어가 기본인 패널은 모든 사용처에 prop을 넘겨야 합니다 |
| G9 | 복사 필드(토큰 비밀값, 워커 등록 명령) | `copyButton`(`pw/ui.js:331`) | **로컬** `CopyField`(`Input readOnly` + `IconButton` + `toast`). 두 번째 앱에서 필요하면 Stratum `CopyButton` | 작고 패널에서 두 곳만 씀 |
| G10 | 단계 표시(이전 4단계, 첫 작업 체크리스트) | `<ol class="checklist">`, 블록 제목 번호 | **로컬** `Steps`(`<ol>` + `Badge`) | 두 곳, 상호작용 없음 |
| G11 | diff 표(이전 미리보기) | `diffTable`(`pw/views/migrate.js:248-290`) | **로컬** `DiffView` | 서버 diff 형식에 묶임 |
| G12 | 숫자 타일(개요 용량) | `createKv` | **로컬** `StatTile`(유틸리티 `rounded-lg border p-4` + 큰 숫자 + 보조 줄). Stratum에 `Card`가 생기면 그 위로 | 한 화면 |
| G13 | `DataTable`의 외부 제어 "현재 행"(딥 링크로 연 행 강조와 스크롤) | `linkTarget`, `highlighted`(`pw/ui.js:215,348`) | **Stratum에 `currentRowId`/`onCurrentRowChange` 제안.** 그때까지 `useSplitViewNavigation`과 행 `cell`의 표시로 대신 | `DataTable`의 현재 행은 내부 상태뿐입니다(`st/table/DataTable.tsx:109`). 상세 칸과 목록의 현재 행을 맞추려면 외부 제어가 필요합니다 |
| G14 | 서버 페이지(커서 "더 보기", 이전/다음) | 버튼 | 표 밖의 `Button`으로 충분. Stratum 변경 불필요 | `DataTable`은 클라이언트 정렬만 하고(README "서버 정렬 없음") 페이지는 표 밖 일 |
| G15 | 건너뛰기 링크 | `#skip` | **로컬** 한 줄(`sr-only` + 포커스 시 표시). Stratum `AppShell`에 제안 | `st/shell/`에 건너뛰기 링크가 없습니다(`grep -i skip`) |
| G16 | 흐르는 상대 시간("3초 전") | `format.relative` + 1초 타이머 | **로컬** `useNow` + `format.ts` | 패널 고유 문구 |

Stratum이 이미 덮는 것(새로 만들지 않음): 앱 틀과 폰 서랍(`AppShell`, `Nav`), 목록-상세와 미저장 확인(`SplitView`), 표(`DataTable`), 저장 바와 변경 수(`SaveBar`, `useFormDirty`), 확인(`ConfirmDialog`), 대화상자(`Dialog`), 메뉴(`Menu`), 탭(`Tabs`, `SegmentedControl`), 폼 부품(`Field`, `Input`, `NumberInput`, `SearchInput`, `Select`, `Checkbox`, `Switch`, `TagInput`, `Fieldset`, `FormSection`), 날짜 범위(`DateRangePicker`), 토스트(`toast`, `Toaster`), 팔레트(`CommandPalette`), 상태 표시(`Badge`), 로딩(`Skeleton`, `Spinner`, `Progress`), 빈 상태(`EmptyState`), 접기(`Accordion`, `Disclosure`), 아바타(`Avatar`).

## 9. 열린 질문

각 질문에 권하는 답을 적었습니다. 2026-11-28에 모두 권하는 답으로 정했습니다(마지막 열).

| # | 질문 | 권하는 답 | 결정 |
| --- | --- | --- | --- |
| Q1 | 주소는 해시(`#/jobs/4828`)인가 경로(`/jobs/4828`)인가? | **해시.** 서버 라우트가 늘지 않고, GitHub Pages 샘플 패널이 그대로 동작하며, 예전 링크를 클라이언트에서 바꿀 수 있습니다. 경로 주소의 이점(주소가 조금 깔끔함)은 운영 도구에서 작습니다. | 결정: 추천안 |
| Q2 | 개발 중 v2를 어떻게 실제 매니저에서 써 보나? | **같은 바이너리에 v2를 `internal/webui/panelv2/`로 넣고 `RUNQUILL_PANEL_V2=1`일 때만 서빙**, 전환 릴리스에서 기본값으로, 다음 릴리스에서 v1 제거. 대안(긴 브랜치에서만 개발)은 main의 활발한 서버 변경과 충돌하고 실제 데이터로 써 볼 수 없습니다. 비용은 겹치는 동안 바이너리가 수백 KB 커지고 번역을 두 곳에 넣는 것입니다. | 결정: 추천안 |
| Q3 | Queue와 Jobs를 한 화면으로 합치나? | **합칩니다**(§2). 탭으로 실시간과 기록을 구분합니다. | 결정: 추천안 |
| Q4 | Logs를 러너 상세의 탭으로 옮기나? | **옮깁니다.** 예전 주소는 그 탭을 엽니다. 넓게 보려면 상세 칸 너비를 끌어 넓히거나(폰은 전체 화면) 다운로드합니다. | 결정: 추천안 |
| Q5 | 개요의 "주의 필요"에 "N분 넘게 기다린 작업"을 넣나? | **처음에는 넣지 않습니다.** 기준값이 서버에 없고 저장소마다 다릅니다. 필요하면 서버가 진단 코드로 판단해 보내는 쪽이 CLI·MCP와 일관됩니다. | 결정: 추천안 |
| Q6 | TypeScript를 쓰나? | **씁니다.** Stratum이 타입을 주고, 스냅샷 필드 오타를 빌드 때 잡습니다. 런타임 비용은 없습니다. OpenAPI에서 타입을 생성하는 도구는 쓰지 않고, 쓰는 필드만 손으로 적습니다. | 결정: 추천안 |
| Q7 | 지원 브라우저 하한은? | **2024년 이후 에버그린 브라우저(Chrome·Edge·Firefox·Safari 최신 두 버전)로 문서화.** Stratum CSS가 `light-dark()`, `@layer`, `@starting-style`, popover를 쓰고, 지금 패널도 이미 `AbortSignal.any`와 `<dialog>`를 씁니다. 정확한 버전은 1단계에서 Stratum이 쓰는 기능 목록으로 정해 `docs/panel.md`에 적습니다. | 결정: 추천안 |
| Q8 | Pretendard 글꼴을 넣나? | **넣지 않습니다.** CDN은 CSP가 막고 오프라인 설치에서도 안 됩니다. 글꼴 파일을 번들하면 크기 예산을 크게 넘습니다. Stratum의 `--font-sans`가 시스템 한글 글꼴로 내려갑니다. | 결정: 추천안 |
| Q9 | 강조색(브랜드)은? | **Stratum 기본 강조색으로 시작**하고, 원하는 색이 있으면 `--color-accent` 한 줄로 바꿉니다. 색을 정해 주세요. | 결정: 추천안 |
| Q10 | Stratum에 없는 부품(§8)을 기다리나? | **기다리지 않습니다.** 로컬로 먼저 만들고, G1·G5·G6·G7·G8·G13·G15는 Stratum에 따로 제안합니다. Stratum 버전은 정확히 고정하고 올릴 때 로컬 대체물을 지웁니다. | 결정: 추천안 |
| Q11 | 설정 마법사도 바꾸나? | **이번에는 아닙니다.** 마법사는 `internal/webui/wizard/`의 CSS·JS 문자열을 Go가 페이지에 넣는 구조이고(`internal/webui/assets.go:21-25`) 설치 중에만 잠깐 씁니다. v2 전환 뒤에 `stratum.css`만 써서(React 없이) 모양을 맞추는 작은 후속 작업으로 권합니다. | 결정: 추천안 |
| Q12 | 해시가 붙은 `assets/`를 오래 캐시하나? | **처음에는 `no-store` 유지.** 문서화된 약속이고 번들이 작습니다. 원격 사용자가 느리다고 하면 `assets/`에만 `immutable`을 거는 작은 변경으로 충분합니다. | 결정: 추천안 |
| Q13 | 커밋된 번들이 PR diff를 어지럽히는 것은? | **`.gitattributes`에 `linguist-generated=true -diff`.** 리뷰는 소스만 보고, CI 최신성 검사가 번들을 지킵니다. | 결정: 추천안 |
| Q14 | 빌드가 OS마다 바이트가 다르면? | **1단계에서 확인하고, 다르면 소스 해시 방식(§6.5 대안 1).** | 결정: 추천안 |
| Q15 | 샘플 패널(GitHub Pages)을 계속 운영하나? | **계속.** 같은 빌드 결과를 그대로 올리고 Playwright가 같은 대상을 검사합니다. | 결정: 추천안 |
