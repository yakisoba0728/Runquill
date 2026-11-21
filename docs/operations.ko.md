# Runquill 운영

[Runquill README](../README.ko.md) · [English](operations.md)

이 가이드의 명령은 설치 디렉터리에서 실행합니다. 기존 배포에 사용한 Compose 파일을 같은 순서로 지정하세요. 예제에서 `docker compose`만 보이는 경우에도 패널을 쓴다면 `-f compose.yaml -f compose.panel.yaml`을 붙이고, SSH 워커 또는 자체 오버레이가 있다면 해당 파일도 이어서 지정합니다.

## 재부팅 후 확인

systemd 서버에서 Docker 서비스와 매니저 컨테이너의 자동 시작 상태를 확인합니다. 표준 Compose의 매니저에는 `restart: unless-stopped`가 설정되어 있습니다. 재부팅 전에 매니저가 실행 중이고 활성 작업이 없는지 확인하세요. Cloudflare Tunnel을 쓰는 경우 cloudflared 서비스도 확인합니다.

```sh
cd "$HOME/runquill"
systemctl is-enabled docker.service
docker compose -f compose.yaml -f compose.panel.yaml ps
docker compose -f compose.yaml -f compose.panel.yaml exec -T manager runquill status
systemctl is-enabled cloudflared.service
```

재부팅 후 SSH로 다시 접속합니다. 아래 예제가 사용하는 공개 주소는 내 도메인으로 바꿉니다. 별도 터널이 없다면 cloudflared 점검과 마지막 HTTPS 요청은 사용 중인 패널 접속 방식으로 바꿉니다.

```sh
cd "$HOME/runquill"
uptime -s
systemctl is-active docker.service cloudflared.service
docker compose -f compose.yaml -f compose.panel.yaml ps
curl -fsS http://127.0.0.1:8080/readyz
curl -fsS -o /dev/null -w '%{http_code}\n' https://runner-ci.example.com/
```

`/readyz`에서 `ready: true`를 확인합니다. [첫 작업을 실행](workflows.ko.md#첫-작업-확인)하고 매니저를 수동으로 시작하지 않은 상태에서 작업 접수, 실행, 러너 정리까지 확인합니다.

## 웹훅만으로 작업 수신 확인

1. 공개 HTTPS 웹훅이 이미 활성화된 테스트 환경을 사용합니다. 먼저 기존 폴링 간격을 기록합니다.
2. 패널 관리자 **설정**에서 폴링 간격을 `0`으로 저장합니다. 패널 저장값이 없는 배포에서는 `.env`의 `RUNQUILL_POLL_INTERVAL`을 `0`으로 바꾸고 기존 Compose 파일로 매니저를 다시 띄웁니다. 패널에 저장한 값은 `.env` 기본값보다 우선합니다.
3. 연결한 비공개 저장소에서 새 [첫 작업](workflows.ko.md#첫-작업-확인)을 실행합니다. GitHub App의 **Advanced → Recent deliveries**에서 같은 작업 ID의 `queued` 이벤트가 HTTP `202`를 받았는지 확인합니다. 매니저 로그에서 `source=webhook` 및 `added=true`와 함께 `runner started`, `runner removed`가 기록됐는지 봅니다. GitHub에서 작업 성공과 러너 등록 해제를 확인합니다.
4. 테스트 전 간격으로 복원합니다. 웹훅 누락을 폴링으로 보완할 경우 기본값은 `30s`입니다.

## 증상별 점검

| 증상 | 먼저 확인할 항목 |
| --- | --- |
| GitHub에서 작업이 대기 중인데 Runquill에서 보이지 않음 | `runquill doctor --repo OWNER/REPO`와 `runquill diagnose OWNER/REPO`로 App 접근 범위, 저장소 소유자, 작업 라벨, 최근 폴링 상태를 확인합니다. |
| 배정은 됐지만 러너가 시작되지 않음 | 패널의 대기열과 호스트 화면에서 남은 슬롯, 풀 및 OS 설정과 마지막 재시도 사유를 확인합니다. |
| 웹훅 응답이 `401` | 실제 GitHub 전달 요청이면 App과 매니저의 웹훅 시크릿이 같은지 확인합니다. 서명 없는 직접 테스트의 `401`은 정상입니다. |
| 터널에서 `502` 또는 서비스 연결 오류 | `systemctl status cloudflared`, `curl -fsS http://127.0.0.1:8080/readyz`, Compose 컨테이너 상태와 패널의 8081 포트 라우팅을 확인합니다. |
| 로그인 후 콜백 오류 | 실제 브라우저 주소, `RUNQUILL_PANEL_URL`, App의 `/auth/github/callback` 등록 주소에서 스킴과 호스트, 포트가 일치하는지 확인합니다. |

## 샘플 패널

[GitHub Pages 데모](https://yakisoba0728.github.io/Runquill/)가 게시되면 첫 작업 정리, 웹훅 누락 후 복구, 워커 오프라인을 샘플 데이터로 체험할 수 있습니다. 로컬에서는 `"$HOME/runquill/runquill" panel --demo`를 실행하고 [127.0.0.1:8081](http://127.0.0.1:8081/)을 엽니다. 소스 체크아웃에서는 `go run . panel --demo`를 사용합니다.

데모를 게시하려면 **Settings → Pages → Source**에서 **GitHub Actions**를 선택하고 `main`에서 [Publish sample panel](../.github/workflows/demo-pages.yml) 워크플로를 실행합니다. 워크플로는 데모의 HTML, CSS, 스크립트와 라이선스를 게시합니다. 배포 주소는 실행 결과에 표시됩니다. [GitHub Pages 설정](https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site)

## 웹 패널과 팀 권한

안내형 패널 설정을 지원하는 릴리스에서 `sh install.sh --dir "$HOME/runquill" --panel`을 실행하면 설치 프로그램이 패널 오버레이를 포함해 Compose를 시작합니다. `setup`으로 구성한 기존 설치에 추가하려면 설치 디렉터리에서 `./runquill setup --panel`을 실행하고 화면에 표시된 Compose 명령을 따릅니다. 브라우저에서 HTTPS 주소로 접속하려면 `--panel-url https://panel.example.com`을 추가합니다. 기본 주소는 `http://127.0.0.1:8081`입니다.

패널을 켜고 새로 설치하면 GitHub App 하나로 러너 등록과 로그인까지 진행합니다. 설정 과정에서 패널 콜백을 등록하고 앱 자격 증명을 로컬에 저장합니다. 개인 계정은 설치 소유자를 첫 관리자로 자동 등록합니다. 조직 설치에서는 첫 관리자의 숫자 GitHub 계정 ID를 입력하세요. 해당 계정으로 `gh api user --jq .id`를 실행하면 확인할 수 있습니다. 매니저가 시작되면 등록한 GitHub 계정으로 로그인합니다.

기존 러너 전용 설치에 패널을 추가할 때는 별도의 [GitHub OAuth App](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/creating-an-oauth-app)을 연결할 수 있습니다. 수동으로 설정하려면 패널 주소를 OAuth App의 홈페이지로 등록하고 콜백 주소에는 `/auth/github/callback`을 붙입니다. `.env`에 다음 값을 입력합니다.

```dotenv
RUNQUILL_PANEL_URL=http://127.0.0.1:8081
RUNQUILL_PANEL_GITHUB_CLIENT_ID=your-oauth-client-id
RUNQUILL_PANEL_GITHUB_CLIENT_SECRET=your-oauth-client-secret
RUNQUILL_PANEL_BOOTSTRAP_ID=12345678
```

매니저 설정에 위 값을 추가하고 패널 오버레이로 빌드합니다. 패널은 로컬 8081 포트에 열리고 기존 8080 포트는 웹훅 서비스에 사용됩니다.

```sh
RUNQUILL_MANAGER_IMAGE=runquill-manager:local docker compose -f compose.yaml -f compose.panel.yaml up -d --build
```

릴리스로 설치한 경우 이미지 이름을 재정의하지 않고 같은 Compose 파일에 `--no-build`를 사용합니다. 멀티 서버 설정에서는 `-f compose.ssh.yaml`도 전달합니다. 바이너리를 직접 실행하는 경우 `RUNQUILL_PANEL_ADDR=127.0.0.1:8081`을 설정합니다. 원격 루프백 접속에는 SSH 터널을 이용하세요. 팀이 공유할 때는 `RUNQUILL_PANEL_URL`에 HTTPS 주소를 지정하고, HTTPS 역프록시에서 원래 Host 헤더를 유지해 패널의 8081 포트로 전달합니다. 공개 웹훅에는 별도 주소를 씁니다.

| 역할 | 패널 권한 |
| --- | --- |
| Viewer | 개요, 대기열, 작업 기록, 러너, 호스트와 풀, 사용량을 봅니다. `fleet:read` API 토큰을 만들 수 있습니다. |
| Operator | Viewer 권한에 더해 호스트의 새 작업 배정을 멈추거나 재개하고, 대기 중인 작업을 취소하거나 즉시 재시도하고, 러너 로그를 봅니다. `hosts:write`와 `jobs:write` 토큰도 만들 수 있습니다. |
| Admin | Operator 권한에 더해 팀 사용자 등록·비활성화, 모든 토큰 조회와 폐기, 설정 변경, 워크플로 이전, 백업 상태 확인, 대기열 백업 다운로드, 저장소 점검, 감사 기록 조회를 합니다. |

팀 사용자는 등록된 GitHub 계정으로 로그인합니다. 관리자는 GitHub 로그인이나 숫자 계정 ID로 팀원을 추가합니다. 로그인을 입력하면 패널이 GitHub 공개 사용자 API로 계정 ID를 확인하며, 개인 사용자 계정만 등록할 수 있습니다. 토큰은 1~90일 뒤 만료되고 권한 범위는 하나이며 패널에서 폐기할 수 있습니다. 토큰 원문은 발급 시 한 번만 표시됩니다. `Authorization: Bearer <token>` 헤더로 보냅니다. 토큰은 자기 범위의 경로만 호출할 수 있으므로 조회와 변경을 함께 하려면 토큰 두 개가 필요합니다. 러너 로그, 워크플로 이전, 백업 등 관리자 경로에는 어떤 토큰도 쓸 수 없습니다. 모든 경로는 [API 명세](../api/openapi.yaml)에 있습니다.

| 범위 | 경로 |
| --- | --- |
| `fleet:read` | `GET /snapshot`, `GET /api/events`, `GET /api/jobs`, `GET /api/jobs/{id}`, `GET /api/elastic`, `GET /api/usage`, `POST /mcp` |
| `hosts:write` | `POST /api/hosts/cordon`에 `{"host":"local","cordoned":true}`를 보냅니다. 배정을 멈춰도 실행 중인 러너는 계속 처리됩니다. |
| `jobs:write` | `POST /api/jobs/{id}/cancel`, `POST /api/jobs/{id}/retry` |

**작업 기록**은 최신 작업부터 대기, 실행 중, 종료, 실패 결과와 함께 보여줍니다. 저장소, 결과, 최근 1시간·24시간·7일로 필터링할 수 있습니다. 끝난 작업은 7일 동안 보관하고 작업 로그는 GitHub에 남습니다. `GET /api/jobs`도 같은 목록을 반환하며 `repository`, `outcome`, `since`와 `until`(Unix 초), `limit`(최대 100), 그리고 응답의 `next` 값을 `before`로 받습니다.

Operator와 Admin은 러너 없이 대기 중인 작업을 패널이나 `jobs:write` 토큰으로 관리할 수 있습니다. **지금 재시도**는 재시도 대기 시간을 없애 다음 스케줄링에서 작업을 시도하게 합니다. **작업 취소**는 작업을 `cancelled by @LOGIN` 사유로 실패 처리하고 러너를 시작하지 않습니다. GitHub의 실행은 GitHub에서 취소하거나 다시 실행할 때까지 대기 상태로 남습니다.

감사 기록에는 로그인, 팀, 토큰, 호스트, 작업, 설정, 백업 변경과 러너 로그 조회, 워크플로 이전이 실행한 사람의 로그인과 함께 남습니다. 작업 종류나 실행한 사람의 로그인 또는 ID로 필터링하고, 이전 기록을 페이지 단위로 불러올 수 있습니다. 설정 변경은 바뀐 항목마다 이전 값과 새 값을 기록하며, 이름이 비밀값으로 보이는 항목은 값을 남기지 않습니다.

패널은 Server-Sent Events로 실시간 갱신하고, 스트림을 쓸 수 없으면 폴링으로 전환합니다. Cmd+K 또는 Ctrl+K로 명령 팔레트를 열고, 밝은 테마·어두운 테마·시스템 설정 중에서 고를 수 있습니다. 화면별 기능과 사용 권한은 [패널 가이드](panel.md)에서 확인하세요.

패널에서 작업을 선택하면 러너 라벨, 대기 또는 시작 원인, 최근 재시도 이유를 확인할 수 있습니다. 설정된 GitHub App 접근 범위는 **GitHub 연결**에 표시됩니다. 패널에는 발견한 작업만 표시되므로, 화면에 작업이 없으면 `runquill diagnose OWNER/REPO`로 GitHub App의 접근 범위, 작업의 GitHub 라벨, 이벤트 전달을 점검하세요. 같은 원인은 `runquill status --json`의 작업별 `diagnostic` 객체에 `code`와 `message`로도 표시됩니다.

## 사용량 통계

**사용량** 화면과 `GET /api/usage`는 끝난 작업의 수, 실패, 러너 사용 시간, 대기 시간을 UTC 날짜, 저장소, 풀, 호스트별로 보여줍니다. 작업은 끝난 UTC 날짜에 한 번만 집계합니다. 실패는 Runquill이 포기했거나 패널에서 취소한 작업이며 GitHub 작업 결과와는 다릅니다. 일별 합계는 대기열 DB에 400일 동안 보관하지만 대기열 백업에는 포함되지 않습니다. [자세히](panel.md#usage-statistics)

## 러너 로그

Operator와 Admin은 **로그** 화면에서 작업을 맡은 러너의 컨테이너 출력을 보거나 이어서 받을 수 있습니다. GitHub 단계 로그가 아니라 러너 프로세스의 로그입니다. `local`과 `ssh://` 호스트의 러너만 지원하며 API 토큰으로는 볼 수 없습니다. 실제 운영 환경에서는 아직 검증하지 않았습니다. [자세히](panel.md#runner-logs)

## 원격 CLI

다른 컴퓨터에서 패널 API로 `runquill fleet status`, `jobs list`, `jobs show`, `runners list`, `hosts list`, `pools list`, `usage`, `elastic`, `hosts cordon`, `hosts uncordon`, `jobs cancel`, `jobs retry`를 실행할 수 있습니다. `RUNQUILL_URL`에 패널 주소를 지정하고 토큰은 `RUNQUILL_TOKEN`이나 `--token-file`로 전달합니다. 스크립트에서는 `--json`을 붙입니다. 명령마다 맞는 토큰 범위가 필요하며, 감사 기록은 어떤 토큰으로도 읽을 수 없으므로 `audit tail`은 항상 웹 패널을 안내합니다. [원격 CLI 가이드](remote-cli.md)

## 코딩 에이전트용 MCP

`POST /mcp`는 운영 현황, 작업, 호스트, 사용량, 탄력 한도 결정을 읽기 전용 [Model Context Protocol](https://modelcontextprotocol.io/) 도구로 제공합니다. HTTP로 `fleet:read` 토큰을 보내거나, `runquill mcp`를 패널로 전달하는 로컬 stdio 서버로 실행합니다. 브라우저 세션은 항상 거부합니다. 실제 MCP 클라이언트로는 아직 검증하지 않았습니다. [MCP 가이드](mcp.md)

## 상태 확인과 업그레이드

대기 작업 수, 러너 슬롯 사용량, 시작 및 정리 횟수를 모니터링하려면 별도 [Prometheus 메트릭 리스너](metrics.md)를 활성화하세요.

```sh
docker compose exec manager runquill status
```

실행 중인 러너 수와 서버별 상태·슬롯 사용량, GitHub 작업 링크를 한 번에 확인할 수 있습니다. `WAITING`은 바로 실행할 수 있는 작업의 발견 순서를 표시하며, [저장소 간 슬롯 공유](workers.ko.md#저장소-간-슬롯-공유) 때문에 실제 시작 순서는 다를 수 있습니다. `RETRY`에는 다음 시도까지 남은 시간과 기록된 재시도 이유가 표시됩니다. 바이너리를 직접 실행하는 경우 매니저와 같은 큐 및 러너 백엔드 설정으로 `runquill status`를 실행합니다.

운영 패널에서 대기 순서, 대기 시간, 재시도 이유를 확인하고 저장소, 작업 ID, 풀로 작업을 필터링할 수 있습니다. 워커와 러너 표에는 호스트 상태, 사용 중·예약 슬롯, 정비 상태, 실행 시간이 표시됩니다. 풀 용량은 전체 한도를 적용하기 전의 워커 슬롯을 보여주고, 개요에는 실제로 사용할 수 있는 슬롯 수가 표시됩니다.

관리자는 **설정**에서 전체 및 워커별 슬롯 한도, GitHub 폴링 주기, 러너 이미지, Docker 워커와 풀 정책을 바꿀 수 있습니다. 변경 내용은 대기열 DB와 감사 기록에 저장되며, 매니저를 재시작하지 않고 새 작업 할당에 적용됩니다. 초기값은 배포 설정에서 가져옵니다. SSH 워커는 배포 시 등록한 SSH 인증을 사용하고, 아웃바운드 에이전트에는 배포 시 등록한 인증 파일이 필요합니다. GitHub App, 패널 주소 및 Tailscale은 설치 마법사에서 설정합니다.

스크립트나 모니터링 도구에서는 `docker compose exec -T manager runquill status --json`을 사용합니다. 스키마 버전 1에 `slot_limit`, `runners`, `hosts`, `jobs`가 들어 있습니다. 사용 중인 슬롯은 응답한 서버에서 관측한 값이고, 예약 슬롯에는 오프라인 서버와 소유 서버를 확인할 수 없는 러너 기록이 포함됩니다. Docker 호스트 구성이 충돌하면 작업 목록과 `docker_error`가 출력되고, `runners`는 `null`이며 명령은 실패 코드로 종료합니다.

`runquill doctor --json`은 점검 결과를 `{"checks":[{"name":"configuration","ok":false,"error":"set RUNQUILL_OWNER"}],"passed":false}` 형식의 JSON 문서 하나로 출력합니다. 통과한 항목에는 `error`가 없습니다. 하나라도 실패하면 사람이 읽는 출력과 같이 종료 코드 1로 끝납니다. 사람이 읽는 출력은 실패가 있으면 마지막 줄에 이 문서의 [증상별 점검](#증상별-점검) 링크를 안내합니다.

GitHub에서 작업이 대기 중이라면 `docker compose exec manager runquill diagnose OWNER/REPO`로 원인을 확인할 수 있습니다. 저장소 범위의 읽기 전용 토큰으로 App의 접근 권한을 확인하고, 마지막 저장소 폴링 결과, 잘못된 라벨, 재시도 이유, 워커 슬롯 부족을 함께 보여줍니다.

`docker compose exec manager runquill gc --dry-run`은 대기열 리스와 맞지 않는 Runquill 러너 컨테이너를 찾아 Docker 호스트와 상태를 보여줍니다. 큐에 관련 작업 정보가 있으면 GitHub 작업 상태도 조회하고, 큐나 호스트가 응답하지 않으면 부분 조회임을 표시합니다. 이 명령은 컨테이너와 큐 데이터를 읽기만 합니다.

정리하려면 `docker compose exec manager runquill gc --remove-orphans`를 실행합니다. 대기열 리스가 없고 작업을 실행하고 있을 수 없는 러너 컨테이너를 삭제하며, 남겨둔 러너는 이유와 함께 출력합니다. `--grace`(기본 `10m`, 최소 `1m`)보다 최근에 만든 러너는 건드리지 않으므로 시작 중인 러너는 안전합니다. 리스가 다른 호스트에 기록된 러너, 큐에 작업 기록이 없는데 실행 중인 러너, GitHub에서 작업이 끝나지 않았거나 러너가 사용 중으로 보이는 경우, GitHub에 연결할 수 없는 경우에는 러너를 남깁니다. 큐에 작업이 있는 러너는 GitHub 러너 등록을 먼저 삭제한 뒤 Docker ID로 컨테이너를 삭제합니다. `local`과 `ssh://` 호스트에서만 동작하며, `agent://` 워커와 Kubernetes 백엔드는 건너뛰었다고 표시합니다. 호스트 조회나 삭제에 실패하면 상태 코드 1로 종료합니다.

`docker compose exec manager runquill gc --unused --dry-run`은 Runquill이 받은 러너 이미지와 캐시 볼륨 중 `RUNQUILL_CLEANUP_AFTER`(기본 `30d`) 동안 어떤 러너도 쓰지 않은 항목을 보여줍니다. `--dry-run` 없이 실행하면 삭제하며, 매니저도 `RUNQUILL_CLEANUP_INTERVAL`마다 같은 기준으로 정리합니다. `--older-than`으로 다른 보존 기간을 지정할 수 있습니다. 직접 받거나 빌드한 이미지는 삭제하지 않습니다. [정리](runner-images.md#cleanup-of-unused-images-and-cache-volumes)

- `RUNQUILL_MAX_RUNNERS`(기본 2)가 전체 저장소의 동시 실행 수를 정합니다. 정원이 차면 SQLite에 대기합니다. 생성에 실패한 작업은 15초 뒤 재시도하며, 그동안 다음 작업이 빈자리를 사용할 수 있습니다. GitHub는 24시간 넘게 대기한 self-hosted 작업을 실패 처리합니다. [GitHub 러너 배정 규칙](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#routing-precedence-for-self-hosted-runners)
- Runquill은 24시간 안에 시작하지 못한 작업을 사유와 마지막 재시도 오류와 함께 실패로 기록합니다. 패널에서 취소한 작업도 실패로 기록됩니다. 실패한 작업은 대기열로 돌아오지 않습니다. GitHub의 실행은 GitHub에서 취소하거나 다시 실행할 때까지 대기 상태로 남고, 다시 실행하면 새 작업이 생깁니다. 사유는 패널의 작업 기록에 표시되며, 기한이 지난 작업은 매니저 로그에도 `job failed`로 남습니다.
- 러너는 저장소 범위의 일회용 JIT 러너입니다. 기본 풀에서는 작업마다 새 컨테이너와 내부 Docker 데몬이 생기므로 `docker build`, `services:`, `container:`를 사용할 수 있습니다. 작업이 끝나면 컨테이너와 내부 이미지, 볼륨, 작업 디렉터리가 삭제됩니다. 풀에서 `cache`를 켰다면 저장소별 캐시 볼륨만 남고, 정리 보존 기간 동안 쓰이지 않으면 삭제됩니다.
- 매니저 로그는 `docker compose logs -f manager`로 확인합니다. 작업 단계 출력은 평소처럼 GitHub 워크플로에 남습니다. 소스 설치에서 새 GitHub 러너 버전을 적용하려면 `docker build --pull -t runquill-runner:local runner`를 다시 실행합니다. 이미 실행 중인 작업은 사용하던 이미지로 마무리됩니다. `RUNQUILL_LOG_FORMAT`을 `text` 또는 `json`으로 설정하면 구조화된 로그를 남기고, `RUNQUILL_LOG_LEVEL`은 `debug`, `info`, `warn`, `error` 중 하나로 설정합니다. 형식을 지정하지 않으면 기존 형태의 로그를 출력합니다. 잘못된 값을 넣으면 명령이 상태 코드 2로 종료합니다.
- 8080 포트의 `GET /healthz`는 대기열 DB가 응답하면 HTTP 200을 반환합니다. `GET /readyz`는 여기에 더해 최근 2분 안에 스케줄링이 성공했고 GitHub App 인증 실패가 없어야 준비 상태로 봅니다. 그렇지 않으면 HTTP 503과 함께 `scheduler`가 `starting`, `failing`, `stalled` 중 하나인 JSON을 반환하며, `database`와 `github_auth`에 나머지 점검 결과가 표시됩니다. 오류 상세는 매니저 로그에만 남습니다.
- 서버당 매니저 하나를 실행합니다. 원래 Python 버전에서 올라오는 경우를 포함해 업그레이드할 때는 Compose의 `queue` 볼륨을 유지합니다. 매니저를 재시작해도 대기열과 현재 러너 기록을 이어받습니다.
- 매니저를 시작할 때와 매번 검색을 마치고 `RUNQUILL_POLL_INTERVAL`(기본 `30s`)이 지난 뒤 GitHub API에서 대기 작업을 찾습니다. 폴링은 접근 가능한 저장소의 대기 중·실행 중 워크플로를 최근 활동 순으로 확인하며, 요청됨·대기·보류 상태는 저장소 목록이 유지되는 동안 성공한 네 번의 조회 안에 순환 확인합니다. 웹훅과 폴링으로 같은 GitHub 작업을 받으면 큐 기록 하나를 공유합니다. 이 값을 `0`으로 설정하면 웹훅만 사용합니다.
- 작업별 타이밍에는 첫 발견 경로, 러너 등록 성공, GitHub가 제공한 작업 시작 시각이 기록됩니다. 발견 지연은 GitHub가 작업 생성 시각을 제공한 경우에만 계산합니다. 대기 시간은 발견부터 첫 등록까지, 아직 대기 중이면 지금까지 기다린 시간입니다. 값이 없으면 `status --json`에서 생략합니다.

## 업그레이드

`--upgrade`를 지원하는 릴리스에서는 새 설치 스크립트를 받아 기존 설치 디렉터리를 지정합니다. `--dir`의 기본값은 `./runquill`입니다.

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --upgrade --dir "$HOME/runquill"
```

설치 스크립트는 체크섬을 검증하고 바뀐 릴리스 파일을 교체합니다. 이전 파일은 `.previous/`에 보관합니다. 릴리스 파일을 되돌리려면 설치 디렉터리에서 `cp -p .previous/* .`를 실행합니다. 매니저까지 되돌릴 때는 이전 이미지 태그도 복구하고, 데이터베이스 호환 여부는 해당 릴리스 노트를 확인합니다. 업그레이드 전에 [대기열을 백업](#대기열-백업과-복구)하세요. `--upgrade`는 릴리스 파일만 다루며 `--version`, `--dir`, `--yes`, `--no-install`과 함께 쓸 수 있습니다.

설치 스크립트는 아무것도 재시작하지 않습니다. `.env`에 릴리스 이미지가 지정되어 있으면 새로 설정할 `RUNQUILL_MANAGER_IMAGE`와 `RUNQUILL_RUNNER_IMAGE` 값과 함께, 평소 쓰는 Compose 파일로 실행할 이미지 다운로드, `docker compose up -d --no-build`, `doctor` 명령을 출력합니다. 새 러너 이미지는 매니저가 `local`과 `ssh://` 호스트에 받습니다. 아웃바운드 워커에서는 새 이미지를 받아 두고 `runquill worker` 실행 파일도 업그레이드합니다. K3s 설치에서는 대신 `kubectl apply -f runquill-k3s.yaml`을 안내합니다.

`--upgrade`가 없는 릴리스나 수동 설치에서는 `.env`의 두 이미지 태그를 새 릴리스로 바꾸고 [매니저 시작](install.ko.md#매니저-시작)의 다운로드 명령을 다시 실행합니다.

패널을 사용하는 Compose 설치에서는 태그를 수정한 뒤 아래 순서로 적용합니다. 실행 중인 작업이 끝난 뒤 시작하고 `queue` 볼륨은 유지합니다. 대기 작업을 이어받는지 확인한 뒤 [첫 작업](workflows.ko.md#첫-작업-확인)을 실행합니다.

```sh
cd "$HOME/runquill"
docker compose -f compose.yaml -f compose.panel.yaml pull manager
docker compose -f compose.yaml -f compose.panel.yaml up -d --no-build
docker compose -f compose.yaml -f compose.panel.yaml exec -T manager runquill doctor
docker compose -f compose.yaml -f compose.panel.yaml exec -T manager runquill status
```

## 매니저 설정 추가

Compose는 `compose.yaml`에 적힌 변수만 매니저에 전달합니다. `RUNQUILL_MAX_PER_REPOSITORY`, `RUNQUILL_SCHEDULER_WORKERS`, `RUNQUILL_LOG_FORMAT`, `RUNQUILL_LOG_LEVEL`, [알림](#알림-보내기) 변수, `RUNQUILL_ELASTIC*`, `RUNQUILL_BACKUP_*`, `RUNQUILL_METRICS_*`, `RUNQUILL_IMAGE_PULL*`, `RUNQUILL_RUNNER_IMAGES`, `RUNQUILL_CLEANUP_*` 같은 선택 설정은 값을 `.env`에 두고 `compose.yaml` 옆에 `compose.settings.yaml` 같은 오버레이를 저장합니다. `--upgrade`는 `compose.yaml`을 교체하지만 이 파일은 그대로 둡니다.

```yaml
services:
  manager:
    environment:
      RUNQUILL_MAX_PER_REPOSITORY: ${RUNQUILL_MAX_PER_REPOSITORY:-0}
      RUNQUILL_SCHEDULER_WORKERS: ${RUNQUILL_SCHEDULER_WORKERS:-4}
      RUNQUILL_LOG_FORMAT: ${RUNQUILL_LOG_FORMAT:-}
      RUNQUILL_LOG_LEVEL: ${RUNQUILL_LOG_LEVEL:-}
      RUNQUILL_NOTIFY_URL: ${RUNQUILL_NOTIFY_URL:-}
      RUNQUILL_NOTIFY_FORMAT: ${RUNQUILL_NOTIFY_FORMAT:-}
      RUNQUILL_NOTIFY_COOLDOWN: ${RUNQUILL_NOTIFY_COOLDOWN:-}
      RUNQUILL_NOTIFY_STALL_AFTER: ${RUNQUILL_NOTIFY_STALL_AFTER:-}
      RUNQUILL_IMAGE_PULL: ${RUNQUILL_IMAGE_PULL:-}
      RUNQUILL_IMAGE_PULL_INTERVAL: ${RUNQUILL_IMAGE_PULL_INTERVAL:-}
      RUNQUILL_RUNNER_IMAGES: ${RUNQUILL_RUNNER_IMAGES:-}
      RUNQUILL_CLEANUP_AFTER: ${RUNQUILL_CLEANUP_AFTER:-}
      RUNQUILL_CLEANUP_IMAGES_AFTER: ${RUNQUILL_CLEANUP_IMAGES_AFTER:-}
      RUNQUILL_CLEANUP_CACHE_AFTER: ${RUNQUILL_CLEANUP_CACHE_AFTER:-}
      RUNQUILL_CLEANUP_INTERVAL: ${RUNQUILL_CLEANUP_INTERVAL:-}
```

`docker compose -f compose.yaml -f compose.settings.yaml up -d`로 적용합니다. 다른 오버레이도 함께 전달하고, 릴리스 이미지를 쓴다면 `--no-build`를 추가합니다. 예약 백업에는 백업 디렉터리 마운트와 암호 파일도 필요하므로 [백업 가이드](backups.md#compose)의 오버레이를 사용합니다. 탄력적 한도 변수는 [탄력적 한도 가이드](elastic-limits.md#compose)에 있습니다. 비공개 레지스트리에서 받으려면 [비공개 레지스트리](runner-images.md#private-registries)처럼 Docker 로그인 정보를 매니저에 마운트하고 `DOCKER_CONFIG`를 설정합니다. 매니저 이미지에는 Docker CLI만 있으므로 Compose에서 `RUNQUILL_CONTAINER_ENGINE=podman`은 쓸 수 없습니다. k3s에서는 매니저가 `runquill-config` Secret의 모든 변수를 읽으므로 `.env`에 추가하고 Secret을 다시 만든 뒤 Deployment를 재시작합니다.

## 알림 보내기

`RUNQUILL_NOTIFY_URL`에 Slack 같은 서비스의 수신 웹훅 주소를 설정하면 운영 알림을 받습니다. 많은 서비스가 주소에 토큰을 넣으므로 이 주소는 비밀값으로 다룹니다. Runquill은 주소를 로그에 남기지 않습니다. 주소는 HTTPS여야 하며 HTTP는 루프백 호스트에만 허용합니다. 다른 호스트로의 리다이렉트는 따르지 않습니다.

| 이벤트 | 보내는 시점 |
| --- | --- |
| `queue_stalled`, `queue_recovered` | 러너 없이 대기 중인 가장 오래된 작업이 `RUNQUILL_NOTIFY_STALL_AFTER`(기본 `10m`)보다 오래 기다릴 때, 그리고 대기 시간이 기준 아래로 내려갈 때 |
| `host_offline`, `host_recovered` | Docker 호스트, 아웃바운드 워커, Kubernetes 백엔드의 상태를 조회하지 못할 때, 그리고 다시 조회될 때 |
| `job_failed` | 24시간 안에 시작하지 못한 작업을 Runquill이 실패로 기록할 때. 패널에서 취소한 작업은 알리지 않습니다. |

저장소별 한도를 설정한 경우 이미 실행 중인 러너가 있는 저장소의 작업은 대기 시간 계산에서 빠지므로, 한도 때문에 기다리는 작업만으로는 `queue_stalled`가 발생하지 않습니다. 문제가 계속되면 `RUNQUILL_NOTIFY_COOLDOWN`(기본 `30m`)마다 다시 알립니다. 복구 알림은 해당 문제를 알린 경우에만 한 번 보냅니다.

`RUNQUILL_NOTIFY_FORMAT=slack`(기본값)은 `text`와 함께 `source`, `kind`, `host`, `repository`, `job`(러너 라벨), `job_id`, `reason`, `waiting_seconds`, `timestamp`를 보냅니다. `{"text": ...}`만 받는 수신 서비스에는 `text`를 사용합니다. 이벤트마다 최대 세 번, 요청당 10초 제한으로 보내며 네트워크 오류와 HTTP 429, 5xx 응답에서 재시도합니다. 메모리에 최대 64개를 대기시키고, 가득 차면 가장 오래된 이벤트를 버립니다. 매니저가 종료될 때는 남은 알림 전송을 최대 10초 기다립니다. [메트릭](metrics.md)을 켜면 `runquill_notifications_*` 카운터로 전송 결과를 확인할 수 있습니다.

## 대기열 백업과 복구

매니저를 실행한 채 SQLite 대기열을 백업할 수 있습니다. 볼륨을 잃어도 복구할 수 있도록 백업을 볼륨 밖으로 복사합니다.

```sh
umask 077
docker compose exec -T manager runquill backup --output /data/queue-backup.sqlite3
docker compose cp manager:/data/queue-backup.sqlite3 ./queue-backup.sqlite3
```

관리자는 패널에서도 같은 백업을 내려받을 수 있습니다. 다운로드 진행률이 표시되고, 큰 대기열은 최대 30분까지 걸릴 수 있으며, 백업 파일이 준비된 뒤에는 다른 패널 작업을 막지 않습니다.

백업은 버전 정보와 함께 소유자만 읽고 쓸 수 있는 새 파일에 저장합니다. 대기 작업, 러너 리스, 워커 정비 상태, 저장소 조회 기록, 패널에서 저장한 운영 설정, 등록된 워커의 식별 정보와 토큰 해시가 포함됩니다. 저장소와 작업 정보가 들어 있으므로 비공개로 보관합니다. GitHub App PEM, 배포 설정, OAuth 비밀값은 별도로 백업합니다. 패널 사용자, 세션, 토큰, 감사 기록은 제외됩니다. 복구 후 패널을 사용하려면 `RUNQUILL_PANEL_BOOTSTRAP_ID`에 첫 관리자의 숫자 GitHub 계정 ID를 설정하고 팀 등록과 토큰 발급을 다시 진행합니다.

기존 매니저를 중지하고 원본 큐를 보관합니다. 백업할 때 사용한 `RUNQUILL_OWNER`와 `RUNQUILL_APP_ID`로 새 큐를 준비합니다.

```sh
docker compose stop manager
docker compose run --rm --no-deps -e RUNQUILL_DB=/data/recovered.sqlite3 \
    -v "$PWD/queue-backup.sqlite3:/run/queue-backup.sqlite3:ro" \
    manager restore --from /run/queue-backup.sqlite3
```

새 큐는 활성화할 때까지 매니저 시작을 차단합니다. 워커와 GitHub에서 백업 시점 이후 생성된 러너를 확인합니다. 해당 환경에서 준비한 큐로 `gc --dry-run`을 실행해 추적되지 않은 러너를 점검할 수 있습니다. 남은 러너를 정리하고 기존 매니저가 중지되어 있는지 확인한 뒤 `.env`에 `RUNQUILL_DB=/data/recovered.sqlite3`를 설정하고 다음 명령을 실행합니다.

```sh
docker compose run --rm --no-deps manager restore --activate
docker compose up -d
```

SSH 워커나 패널을 사용한다면 기존 Compose 파일 조합을 그대로 사용합니다. k3s에서는 기존 매니저를 중지한 상태로 PVC에 복구 파일을 준비하고 활성화한 뒤 Deployment의 `RUNQUILL_DB`에 새 경로를 설정합니다. 원본 큐 파일은 되돌릴 수 있도록 남아 있고, 한 운영 환경에는 매니저 하나만 실행합니다.

## 백업 예약, 암호화, 업로드

`RUNQUILL_BACKUP_INTERVAL`과 `RUNQUILL_BACKUP_DIR`을 설정하면 백업을 자동으로 만들고, `RUNQUILL_BACKUP_PASSPHRASE` 또는 `RUNQUILL_BACKUP_PASSPHRASE_FILE`을 설정하면 암호화합니다. 관리자는 패널의 **최신 백업 검증**에서 가장 최근 로컬 백업을 확인할 수 있습니다. `runquill backup --seal`로 같은 형식의 암호화 백업을 직접 만들 수 있고, `runquill restore --from`에 암호를 주면 복구합니다. `RUNQUILL_BACKUP_S3_*` 변수를 설정하면 매니저가 백업을 S3 호환 버킷에도 복사하며 `runquill restore --from-s3`로 복구합니다. `RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true`가 없으면 암호화하지 않은 백업은 올리지 않습니다. [백업 가이드](backups.md)

## 제거

먼저 호스트를 `cordon`하고 실행 중인 작업이 끝나기를 기다립니다. 다시 설치할 수도 있다면 [대기열을 백업](#대기열-백업과-복구)합니다. 설치 디렉터리에서 평소 쓰는 Compose 파일로 매니저를 내립니다. `-v`는 대기 작업, 리스, 패널에서 저장한 설정이 담긴 `queue` 볼륨을 삭제하고, `--rmi all`은 매니저 이미지를 삭제합니다.

```sh
docker compose -f compose.yaml -f compose.panel.yaml down -v --rmi all
```

러너 컨테이너, 캐시 볼륨, 러너 이미지는 Compose가 관리하지 않습니다. SSH 워커와 아웃바운드 워커를 포함한 각 Docker 호스트에서 남은 항목을 삭제합니다. 이미지는 `RUNQUILL_RUNNER_IMAGE`와 풀의 `image`에 지정한 이름, 작업이 고른 러너 이미지 목록의 이미지를 확인합니다.

```sh
docker ps -a --filter label=io.runquill.runner=1
docker rm -f $(docker ps -aq --filter label=io.runquill.runner=1)
docker volume rm $(docker volume ls -q -f label=io.runquill.cache=1)
docker image rm ghcr.io/yakisoba0728/runquill-runner:YOUR_RELEASE_TAG
```

그다음 `.env`, GitHub App 키, 토큰 파일이 들어 있는 설치 디렉터리를 삭제합니다. 직접 만든 Tailscale Funnel이나 Serve 리스너를 끄고, 아웃바운드 워커의 `runquill worker`도 중지합니다. Cloudflare Tunnel을 썼다면 이 배포에 해당하는 DNS 레코드와 라우팅 규칙만 제거합니다. 다른 서비스와 터널을 공유한다면 그 터널은 유지합니다.

설치 스크립트가 호스트에 적용한 변경은 그대로 남습니다. 되돌리려면 `sudo gpasswd -d "$USER" docker`로 사용자를 `docker` 그룹에서 뺍니다. Docker는 배포판별 [Docker 제거 절차](https://docs.docker.com/engine/install/ubuntu/#uninstall-docker-engine)에 따라 삭제하고, `/etc/apt/keyrings/docker.asc`와 `/etc/apt/sources.list.d/docker.sources` 또는 `/etc/yum.repos.d/docker-ce.repo`도 지웁니다.

k3s에서는 `kubectl delete -f runquill-k3s.yaml`로 Runquill 네임스페이스 두 개와 그 안의 매니저, 러너 Pod, Secret, 대기열 PVC를 삭제합니다.

마지막으로 **Settings → Developer settings → GitHub Apps → *해당 App* → Advanced → Delete GitHub App**에서 GitHub App을 삭제합니다. 조직 App은 조직 설정에서 같은 메뉴를 사용합니다. App을 삭제하면 모든 설치에서 제거됩니다. 별도의 패널 OAuth App도 같은 방식으로 삭제하고, 저장소의 **Settings → Actions → Runners**에 남은 `runquill-` 러너가 있으면 삭제합니다. [GitHub App 삭제](https://docs.github.com/en/apps/maintaining-github-apps/deleting-a-github-app)

## 기능 검증 현황

Runquill 문서에서 설명하는 기능은 Go 테스트로 확인하며, CI의 중첩 Docker 테스트와 K3s 수명 주기 점검은 실제 컨테이너를 실행합니다. 다음 기능은 구현되어 있지만 가짜 서버나 기록된 출력으로만 테스트했고, 실제 환경에서는 아직 실행하지 않았습니다.

- Podman과 rootless 컨테이너 엔진
- 실제 GitHub 저장소를 대상으로 한 워크플로 이전
- 실제 MCP 클라이언트에서 사용하는 MCP 엔드포인트와 `runquill mcp`
- K3s, SSH, 아웃바운드 워커에서의 스케줄러 병렬 처리(`RUNQUILL_SCHEDULER_WORKERS`)
- 부하가 걸린 실제 Docker 호스트의 탄력적 한도(`local` 호스트에만 적용)
- 실제 운영 환경의 러너 로그 스트리밍
- 실제 `actions/setup-*` 작업에서의 도구 캐시
- `ssh://` 호스트에서 작업이 요청한 러너 이미지 받기
- 비공개 레지스트리에서 러너 이미지 받기
- 아웃바운드 `agent://` 워커에서의 러너 OS 선택(`os__NAME__`)

러너 이미지 자동 받기와 실제 Docker에서의 정리, 다섯 가지 OS 변형의 러너 실행, 저장소별 도구 캐시 디렉터리 유지는 개발자 컴퓨터의 실제 Docker로 확인했지만 운영 환경에서는 아직 확인하지 않았습니다.

암호화된 예약 백업의 업로드와 복구는 인증을 켠 로컬 SeaweedFS에서 확인했습니다. [S3 검증 테스트](backups.md#upload-to-s3-compatible-storage)

이 기능에서 문제를 발견하면 이슈로 알려주세요.
