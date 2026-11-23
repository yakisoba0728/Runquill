# 원격 CLI

[Runquill README](../README.ko.md) · [English](remote-cli.md)

`runquill` 실행 파일은 API 토큰과 패널 API를 이용해 다른 컴퓨터에서 매니저를 조회하고 조작할 수 있습니다. 토큰은 패널의 **팀과 토큰**에서 만듭니다. 클라이언트에는 그 밖에 설정할 것이 없습니다. 릴리스에는 Linux 실행 파일만 포함되므로, Windows나 macOS에서는 소스 체크아웃에서 `go build -o runquill .`(Windows에서는 `-o runquill.exe`)로 클라이언트를 빌드합니다.

## 연결

| 설정 | 동작 |
| --- | --- |
| `RUNQUILL_URL` 또는 `--url` | 패널 주소입니다. `RUNQUILL_PANEL_URL`과 똑같이 씁니다. HTTPS이거나 루프백 주소의 HTTP여야 하며, 패널은 다른 호스트 이름을 거부합니다. |
| `RUNQUILL_TOKEN` | `rqk_` 뒤에 43자가 붙는 토큰입니다. |
| `--token-file FILE` | 대신 파일에서 토큰을 읽습니다. 다른 사용자가 읽을 수 있는 파일이면 CLI가 경고합니다. |

`--token` 인자는 같은 컴퓨터의 다른 사용자가 프로세스 목록에서 볼 수 있으므로 거부합니다. 클라이언트는 다른 출처로의 리다이렉트를 따르지 않으며, 15초가 지나면 요청을 포기합니다.

```sh
export RUNQUILL_URL=https://panel.example.com
runquill jobs list --token-file ~/.config/runquill/token --outcome failed --since 24h
```

## 명령

| 명령 | 토큰 범위 | 출력 |
| --- | --- | --- |
| `fleet status [--check]` | `fleet:read` | 최신 스냅샷의 대기 작업 수, 실행 중인 작업, 호스트 합계, 현재 문제입니다. MCP `fleet_status` 도구와 같은 요약입니다. |
| `jobs list [--repo OWNER/NAME] [--outcome waiting\|running\|finished\|failed] [--since 24h\|7d] [--limit 1-100]` | `fleet:read` | 7일간의 작업 기록을 최신 작업부터 보여 줍니다. |
| `jobs show JOB_ID` | `fleet:read` | 작업 기록의 작업 하나입니다. 결과, 실패 또는 재시도 이유, 진단, 시각, 대기 시간, 단계별 소요 시간, 한도 대비 측정한 CPU와 메모리, 러너의 메모리 부족 종료 여부, GitHub URL을 보여 줍니다. |
| `runners list` | `fleet:read` | 최신 스냅샷의 러너입니다. |
| `hosts list` | `fleet:read` | 호스트별 상태, 정비 상태, 슬롯입니다. |
| `pools list` | `fleet:read` | 호스트를 풀별로 요약합니다. |
| `usage [--group day\|repository\|pool\|host] [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--repo OWNER/NAME]` | `fleet:read` | 그룹별 끝난 작업 수, 실패, 러너 사용 시간, 평균 대기 시간과 `(total)` 행입니다. 범위는 UTC 날짜 단위이며 기본값은 최근 30일, 최대 400일입니다. |
| `usage --rightsizing [--group pool\|job] [--days 1-7]` | `fleet:read` | 풀별로, 또는 저장소와 러너 라벨별로 [자원이 기록된](elastic-limits.ko.md#작업-자원) 작업 수, 메모리 부족 종료 수, 한도 대비 CPU와 메모리 최고값과 `tight`나 `oversized` 같은 판정입니다. 기본값은 최근 7일 동안 끝난 작업입니다. |
| `elastic` | `fleet:read` | [탄력적 한도](elastic-limits.ko.md)의 모드, 호스트별 여유분, 러너 한도, 최근 크기 조정 결정 20개입니다. |
| `hosts cordon HOST`, `hosts uncordon HOST` | `hosts:write` | `hosts list`에 나온 호스트의 새 작업 배정을 멈추거나 재개합니다. |
| `jobs cancel JOB_ID`, `jobs retry JOB_ID` | `jobs:write` | 러너 없이 대기 중인 작업을 실패 처리하거나 재시도 대기 시간을 없앱니다. |
| `audit tail [--limit 1-200]` | 없음 | 항상 거부합니다. 감사 기록에는 관리자 세션이 필요하고, 어떤 토큰 범위로도 얻을 수 없습니다. 패널을 사용하세요. |

`hosts:write`와 `jobs:write` 토큰은 소유자가 Operator나 Admin이어야 합니다. 토큰에는 범위가 하나뿐이므로 운영 현황을 조회하고 변경하려면 토큰 두 개가 필요합니다.

`runquill hosts --help`처럼 `runquill GROUP --help`를 실행하면 모든 명령을 표준 출력에 보여 줍니다. 명령 뒤에 `--help`를 붙이면 그 명령의 플래그도 표준 출력에 보여 주고 상태 코드 0으로 종료합니다. 어느 명령에든 `--json`을 붙이면 기계가 읽을 수 있는 형식으로 출력합니다. `jobs list`는 패널의 `GET /api/jobs` 페이지를, `jobs show`는 `GET /api/jobs/{id}` 행을 출력합니다. `runners list`와 `hosts list`는 스냅샷의 `runner_inventory`와 `fleet.hosts` 배열을 출력합니다. `usage`, `usage --rightsizing`, `elastic`은 `GET /api/usage`, `GET /api/usage/rightsizing`, `GET /api/elastic`을 출력하며, 여기에는 대기 시간의 p50과 p95, 보관 중인 모든 결정도 들어 있습니다. `fleet status`는 MCP `fleet_status` 결과를, `pools list`는 자체 요약을 출력합니다. 변경 명령은 `{"job_id":…,"action":…}` 또는 `{"host":…,"cordoned":…}`를 출력합니다.

CLI는 성공하면 상태 코드 0, 패널이 요청을 거부하거나 처리하지 못하면 1, 인자가 잘못되었거나 URL이나 토큰이 없으면 2로 종료합니다. `fleet status --check`는 요약에 문제가 있으면 그 내용을 출력한 뒤 상태 코드 1로 종료하므로, 예약 점검에서 이를 알림 조건으로 쓸 수 있습니다. 주소가 `404 page not found`만 응답하면 CLI는 그 주소에서 패널 API를 제공하지 않는다고 덧붙입니다. `runquill panel --demo`에는 API가 없고, 이전 버전의 매니저에는 새 엔드포인트가 없습니다.

원격 CLI는 로컬 테스트 서버를 대상으로 테스트했습니다. 스크립트에 쓰기 전에 내 패널에서 먼저 실행해 보세요.
