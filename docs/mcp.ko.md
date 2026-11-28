# 코딩 에이전트용 MCP

[Runquill README](../README.ko.md) · [English](mcp.md)

Runquill은 에이전트가 운영 현황을 살펴볼 수 있도록 읽기 전용 [Model Context Protocol](https://modelcontextprotocol.io/) 서버를 제공합니다. 이 서버로는 아무것도 바꿀 수 없습니다.

## 도구

| 도구 | 인자 | 반환값 |
| --- | --- | --- |
| `fleet_status` | 없음 | 최신 스냅샷의 대기 작업 수, 실행 중인 작업, 호스트 합계, 현재 문제입니다. `runquill fleet status --json`도 같은 결과를 출력합니다. |
| `list_jobs` | `repository`, `outcome`, `since`(`90m`, `24h`, `7d` 등, 최대 `7d`), `before`, `limit`(1–100, 기본 20) | 최신 작업부터 보여 주는 작업 기록입니다. 기록된 단계별 소요 시간과 현재 진단이 있으면 함께 보여 줍니다. |
| `get_job` | `id`(필수) | 작업 하나의 결과, 실패 또는 재시도 이유, 소요 시간, 러너, 실시간 상태, [기록된 자원](elastic-limits.ko.md#작업-자원)과 메모리 부족 종료 여부입니다. |
| `list_hosts` | 없음 | 호스트별 상태, 슬롯, 호스트당 최대 100개의 러너입니다. |
| `usage_summary` | `window`(`24h` 또는 `7d`), `group`(`day`, `repository`, `pool`, `host`) | 끝난 작업 수, 실패, 러너 사용 시간, 대기 시간입니다. `group`을 주면 같은 통계를 그룹별로 나눈 `series`도 반환합니다. 작업이 많은 그룹부터 최대 500개이며, `window` 기간을 포함하는 UTC 날짜 전체를 `from`과 `to`로 지정한 `GET /api/usage`처럼 읽습니다. |
| `elastic_decisions` | `limit`(1–200, 기본 20) | 최근 [탄력적 한도](elastic-limits.ko.md) 결정입니다. |
| `job_rightsizing` | `group`(`pool` 또는 `job`), `days`(1–7, 기본 7) | 최근 끝난 작업의 한도 대비 CPU와 메모리 최고값, 메모리 부족 종료 수, 자원별 판정입니다. `GET /api/usage/rightsizing`과 같은 내용입니다. |
| `pool_rightsizing` | `days`(1–7, 기본 7) | 최근 끝난 작업의 풀별 CPU 최고값(코어), 메모리 최고값(MiB), 메모리 부족 종료 수, 자원별 판정입니다. `GET /api/usage/rightsizing?group=pool`과 같은 내용입니다. |

결과는 JSON 텍스트와 구조화된 콘텐츠로 함께 반환합니다. 저장소, 작업, 러너 이름은 GitHub 사용자가 정한 값이므로, 서버 지침은 에이전트에게 이를 데이터로만 다루라고 안내합니다.

## 에이전트로 작업 점검

1. `fleet_status`를 호출해 대기 작업 수, 온라인 호스트, 남은 슬롯을 확인합니다.
2. 저장소와 짧은 시간 범위를 지정해 `list_jobs`를 호출합니다. GitHub 작업 ID를 알고 있다면 `get_job`에 바로 전달합니다.
3. 작업의 결과, 마지막 재시도 또는 실패 이유, 호스트, 소요 시간을 확인합니다. 작업에 풀이 필요하거나 워커가 오프라인이면 `list_hosts`를 사용합니다.
4. Runquill에 보이지 않는 작업은 [서버 쪽 점검](operations.ko.md#증상별-점검)으로 App 접근 범위를 확인하고 GitHub가 웹훅을 보냈는지 확인합니다.

에이전트의 점검에는 `fleet:read`를 사용합니다. 운영자가 승인한 상태 변경에는 알맞은 쓰기 범위의 토큰으로 [패널](panel.ko.md)이나 [원격 CLI](remote-cli.ko.md)를 사용합니다.

## 접근

패널 사용자 메뉴의 **API 토큰**에서 `fleet:read` API 토큰을 만듭니다. 엔드포인트는 API 토큰만 받습니다. 브라우저 세션 쿠키는 항상 HTTP 401로 거부하므로, 웹 페이지가 내 패널 세션으로 엔드포인트를 호출할 수 없습니다. `Origin` 헤더가 있는 요청은 패널 주소에서 와야 합니다. 매니저 하나는 MCP 요청을 동시에 최대 4개까지 처리하며, 요청마다 5초 제한이 있습니다.

엔드포인트는 MCP 리비전 `2026-07-28`을 상태 없이 처리하고, `initialize`로 시작하는 클라이언트에는 리비전 `2025-11-25`를 사용합니다. 그보다 오래된 리비전만 지원하는 클라이언트는 지원하지 않는 버전 오류로 거부합니다.

## HTTP

Streamable HTTP를 지원하는 클라이언트에 `https://panel.example.com/mcp`를 지정하고 토큰을 bearer 헤더로 보냅니다. 정확한 파일과 키는 클라이언트마다 다릅니다. 일반적인 항목은 다음과 같습니다.

```json
{
  "mcpServers": {
    "runquill": {
      "type": "http",
      "url": "https://panel.example.com/mcp",
      "headers": { "Authorization": "Bearer rqk_..." }
    }
  }
}
```

토큰을 공유 파일에 직접 적지 말고 클라이언트의 비밀값이나 환경 변수 기능을 사용하세요.

## 표준 입출력

`runquill mcp`는 각 메시지를 패널의 `/mcp` 엔드포인트로 전달하는 로컬 stdio 서버를 실행합니다. [원격 CLI](remote-cli.ko.md)처럼 주소는 `--url`이나 `RUNQUILL_URL`에서, 토큰은 `--token-file`이나 `RUNQUILL_TOKEN`에서 읽습니다. 표준 출력에는 JSON-RPC 메시지만 나오고, 진단 메시지는 표준 오류로 나갑니다.

```json
{
  "mcpServers": {
    "runquill": {
      "command": "/usr/local/bin/runquill",
      "args": ["mcp", "--url", "https://panel.example.com", "--token-file", "/home/me/.config/runquill/token"]
    }
  }
}
```

MCP 서버와 stdio 프록시는 이 저장소의 프로토콜 테스트로 확인합니다. 실제 MCP 클라이언트로는 아직 실행하지 않았으므로, 의존하기 전에 클라이언트에서 도구 목록을 확인하세요.
