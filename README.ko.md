# Runquill

[English](README.md) · 한국어

개인 GitHub 계정의 비공개 저장소에서 요청한 Actions 작업을 내 서버에서 실행합니다. GitHub App을 **All repositories**로 한 번 설치하면 이후에 만드는 비공개 저장소도 같은 설치로 처리됩니다. 서버에서 관리할 저장소 목록은 필요 없습니다.

## 서버 설치

cgroup v2를 사용하는 amd64 또는 arm64 Linux 서버에 일반 모드(rootful)의 Docker Engine과 Docker Compose가 필요합니다. 폴링만 사용하면 공개 HTTPS 주소 없이 운영할 수 있습니다.

### 공개 릴리스 설치

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh
```

설치 스크립트는 다운로드 체크섬을 확인하고, 브라우저 설정을 거쳐 같은 버전의 이미지를 받은 뒤 `./runquill`에서 Compose를 시작합니다. `--dir PATH`로 경로를 바꾸고, `--version v0.1.2`으로 버전을 지정하거나, `--download-only`로 바이너리와 Compose 파일만 받을 수 있습니다.

### GitHub App 연결

`compose.yaml`이 있는 디렉터리에서 `setup`을 실행합니다. 소스에서 빌드할 때는 Go 1.26.11 이상에서 해당 Go 버전의 최신 패치를 사용합니다.

```sh
CGO_ENABLED=0 go build -o runquill .
./runquill setup
```

공개된 릴리스를 사용하는 경우 같은 버전 태그의 `compose.yaml`과 릴리스에 첨부된 Linux 실행 파일, `SHA256SUMS`를 한 디렉터리에 받습니다. 아래는 amd64 예시입니다. arm64 서버에서는 `runquill-linux-arm64`를 사용합니다.

```sh
sha256sum --check --ignore-missing SHA256SUMS
chmod +x ./runquill-linux-amd64
./runquill-linux-amd64 setup
```

1. `setup`이 출력한 링크를 브라우저에서 엽니다. 다른 컴퓨터에서 브라우저를 열려면 터미널에 표시된 SSH 포트 포워딩 안내를 따릅니다.
2. GitHub에서 비공개 App 생성을 승인합니다. `setup`이 `.env`와 `github-app.pem`을 권한 `0600`으로 저장하고 설치 링크를 출력합니다.
3. 링크를 열고 개인 계정에 **All repositories**로 설치합니다. 이후 만드는 비공개 저장소도 자동으로 감지합니다.

두 파일을 저장한 뒤 중단했다면 `setup`을 다시 실행해 App 설치를 이어갈 수 있습니다. 기본 연결 방식은 GitHub API 폴링입니다.

[GitHub App 매니페스트](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest) · [GitHub App 설치](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app)

### 매니저 시작

소스에서 설치할 때는 러너 이미지를 빌드한 뒤 매니저를 시작합니다.

```sh
docker build --pull -t runquill-runner:local runner
docker compose up -d --build
```

릴리스 실행 파일을 사용하면 `setup`이 같은 태그의 매니저와 러너 이미지 이름을 `.env`에 기록합니다. `setup`이 출력한 러너 이미지 다운로드 명령을 실행한 뒤 매니저를 시작합니다.

```sh
docker compose pull manager
docker compose up -d --no-build
```

### 첫 작업 확인

아래 명령으로 Docker, 러너 이미지 및 GitHub App 권한을 점검합니다. 특정 저장소가 설치 범위에 들어 있는지 확인하려면 `--repo OWNER/REPO`를 붙입니다.

```sh
docker compose exec manager runquill doctor
```

앱 설치 후 새 비공개 저장소를 만들고, `.github/workflows/`에 [확인용 워크플로](examples/runquill-smoke.yml)를 복사해 실행합니다. GitHub Actions에서 Docker 빌드와 PostgreSQL 연결을 확인합니다. 작업이 끝나면 매니저 로그에 `runner removed`가 나오고, 저장소의 **Settings → Actions → Runners**에서 러너가 사라지는지 확인합니다.

### 기존 GitHub App 또는 웹훅 사용

앱을 수동으로 만들 때는 **Only on this account**를 선택하고 **Actions → Read-only**와 **Administration → Read and write** 권한을 부여합니다. 발급한 PEM 키를 보관하고 앱을 개인 계정에 **All repositories**로 설치합니다. [환경 변수 예제](.env.example)를 `.env`로 복사해 GitHub 로그인, App ID, PEM 키 경로를 입력하고 두 파일의 권한을 `0600`으로 설정합니다. 폴링으로 연결할 때는 앱의 웹훅을 끄고 `RUNQUILL_WEBHOOK_SECRET`을 비워둡니다. [저장소 러너 JIT API](https://docs.github.com/en/rest/actions/self-hosted-runners#create-configuration-for-a-just-in-time-runner-for-a-repository)

수동으로 작성한 `.env`에 릴리스 이미지를 사용하려면 예제에 있는 매니저와 러너 이미지 이름을 같은 버전 태그로 맞춥니다. 러너 이미지를 먼저 내려받은 뒤 Compose를 시작합니다.

웹훅을 켜려면 GitHub App에서 **Workflow job** 이벤트를 구독하고 공개 HTTPS 주소를 등록합니다. `openssl rand -hex 32`로 생성한 시크릿을 GitHub와 `RUNQUILL_WEBHOOK_SECRET`에 같은 값으로 설정합니다. HTTPS 프록시에서 `/webhook`을 매니저의 8080 포트로 전달합니다. 같은 서버에서 Caddy를 실행한다면 아래 설정을 사용합니다.

```caddyfile
runquill.example.com {
    reverse_proxy /webhook 127.0.0.1:8080
}
```

### HTTPS 웹훅으로 연결

새 App을 만들 때 `sh install.sh --webhook-url https://runquill.example.com/webhook`을 실행하거나 `runquill setup`에 같은 옵션을 전달합니다. App이 `workflow_job`을 구독하고, `setup`은 웹훅 시크릿과 놓친 작업 복구에 쓰는 `RUNQUILL_POLL_INTERVAL=30s`를 저장합니다. HTTPS 프록시에서 `/webhook`을 위 예시처럼 로컬 매니저로 전달합니다. 웹훅만 쓰려면 `.env`에서 폴링 간격을 `0`으로 설정합니다.

### Tailscale Funnel로 웹훅 연결

공유기 뒤의 서버에도 Funnel로 GitHub가 접근할 수 있는 공개 HTTPS 주소를 만들 수 있습니다. Compose 매니저를 `127.0.0.1:8080`으로 내보내는 서버에서 실행합니다. Tailnet에서 MagicDNS와 HTTPS를 활성화하고, 정책에서 해당 서버의 Funnel 사용을 허용합니다. 예제는 기본 HTTPS 포트인 443을 사용합니다. [Tailscale Funnel 요구사항](https://tailscale.com/docs/features/tailscale-funnel)

```sh
tailscale funnel status
tailscale funnel --bg --https=443 http://127.0.0.1:8080
```

Funnel이 출력한 HTTPS 주소에 `/webhook`을 붙여 위에서 받은 설치 스크립트나 `./runquill setup`에 전달합니다. 기존 App을 사용한다면 GitHub에서 웹훅 URL을 바꾸고 해당 App의 시크릿을 사용합니다.

```sh
sh install.sh --webhook-url https://runquill.example-tailnet.ts.net/webhook
```

Compose가 뜬 뒤 tailnet 외부에서 `https://runquill.example-tailnet.ts.net/healthz`가 HTTP 200을 반환하는지 확인합니다. 스모크 워크플로를 실행하고 App의 **Advanced → Recent deliveries**에서 대기 이벤트에 HTTP 202가 기록되는지 확인합니다. 전송이 타임아웃되면 해당 화면에서 **Redeliver**를 사용할 수 있습니다. 폴링이 꺼진 기존 설치에서 놓친 작업을 자동으로 복구하려면 `.env`에 `RUNQUILL_POLL_INTERVAL=30s`를 설정하고 기존 Compose 명령으로 반영합니다. [GitHub App 웹훅 재전송](https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/redelivering-webhooks#redelivering-github-app-webhooks)

다음 명령으로 이 Funnel 주소를 닫습니다. Funnel은 매니저의 HTTP 엔드포인트를 인터넷에 공개합니다. 브라우저 설정 포트와 Docker 소켓은 로컬이나 사설망에서 사용합니다.

```sh
tailscale funnel --https=443 off
tailscale funnel status
```

작업 서버가 같은 tailnet에 있다면, 매니저 컨테이너가 서버 이름을 확인하고 SSH에 연결할 수 있도록 설정한 뒤 `ssh://` 주소에 작업 서버의 MagicDNS 이름을 쓰면 됩니다. [Tailscale Funnel CLI](https://tailscale.com/docs/reference/tailscale-cli/funnel)

### Docker 작업 서버 추가

매니저 한 대에서 로컬 Docker와 SSH로 연결한 여러 Docker 서버를 사용합니다. 각 서버는 Linux의 rootful Docker, cgroup v2, `RUNQUILL_RUNNER_IMAGE`에 해당하는 이미지, Docker 소켓에 접근할 수 있는 SSH 계정이 필요합니다.

```dotenv
RUNQUILL_DOCKER_HOSTS=local,ssh://builder@worker.example.com
RUNQUILL_MAX_RUNNERS=3
RUNQUILL_DOCKER_HOST_LIMITS=local=2,ssh://builder@worker.example.com=1
RUNQUILL_SSH_DIR=/path/to/runquill-ssh
```

지정한 디렉터리에 전용 SSH 키, 서버의 키를 확인한 `known_hosts`, SSH `config`를 넣습니다. `config`에는 `IdentityFile /root/.ssh/id_ed25519`처럼 컨테이너 안의 키 경로와 `BatchMode yes`, `StrictHostKeyChecking yes`를 설정합니다.

```sh
docker compose -f compose.yaml -f compose.ssh.yaml up -d
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill doctor
```

매니저는 배정 가능한 서버 중 사용 중인 슬롯이 가장 적은 곳에 새 러너를 배치합니다. `RUNQUILL_MAX_RUNNERS`는 전체 한도이고, `RUNQUILL_DOCKER_HOST_LIMITS`는 서버별 한도입니다. 서버별 설정에서는 `RUNQUILL_DOCKER_HOSTS`에 적은 서버 이름을 그대로 씁니다.

서버 연결이 끊기면 해당 서버의 러너 기록과 마지막으로 확인한 미추적 러너 슬롯을 전체 한도에 반영합니다. 정상 서버는 남는 슬롯으로 작업을 계속 받습니다. 연결한 적이 없는 서버의 슬롯은 서버별 한도만큼 예약하며, 서버별 한도가 없으면 전체 한도만큼 예약합니다.

정비 전 `cordon`을 실행하면 해당 서버의 새 작업 배정이 멈추고 기존 작업은 계속 실행됩니다. `status`에서 서버 연결 상태, 정비 상태, 슬롯 사용량을 확인할 수 있습니다. 서버를 목록에서 빼기 전에는 해당 서버의 슬롯이 모두 비었는지 확인하세요. 새 작업을 다시 받으려면 `uncordon`을 실행합니다. 정비 상태는 매니저를 재시작해도 유지됩니다.

```sh
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill host cordon ssh://builder@worker.example.com
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill status
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill host uncordon ssh://builder@worker.example.com
```

## 워크플로에서 러너 선택

[스모크 워크플로](examples/runquill-smoke.yml)는 기본값으로 내 서버를 사용합니다. **Run workflow**에서 `github_hosted`를 선택하면 `ubuntu-latest`를 사용합니다. [병렬 작업 예제](examples/runquill-parallel.yml)에는 같은 옵션을 쓰는 단일 작업 1개와 매트릭스 작업 2개가 있습니다. `.github/workflows/`에 복사해 수동 실행하면 각 작업 로그에서 배정된 러너 이름을 볼 수 있습니다.

라벨에는 워크플로 실행 ID, 실행 시도 번호, 해당 실행에서 작업마다 고유한 접미사를 넣습니다. 작업 ID는 식에 직접 적습니다. 예를 들어 `check` 작업은 다음 식을 사용합니다.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-check', github.run_id, github.run_attempt) }}
```

매트릭스 작업은 작업 ID 뒤에 `strategy.job-index`를 붙입니다. 인덱스는 0부터 시작하므로 예제의 `test` 작업은 `test-0`, `test-1`로 구분됩니다. 워크플로를 다시 실행하면 `run_attempt`가 바뀝니다.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-test-{2}', github.run_id, github.run_attempt, strategy.job-index) }}
```

이 식을 `runs-on`의 단일 라벨로 사용합니다. 접미사는 영문자나 숫자로 시작하고 이후에는 하이픈과 밑줄도 쓸 수 있습니다. 라벨 전체는 100자 이내여야 합니다. Runquill은 작업마다 요청한 라벨로 일회용 러너를 등록하고, GitHub는 라벨이 일치하는 러너에 작업을 배정합니다. [GitHub Actions 컨텍스트 문서](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts)

### 다음 작업에서 Docker 빌드 레이어 재사용

루트에 `Dockerfile`이 있는 저장소의 `.github/workflows/`에 [빌드 캐시 예제](examples/runquill-build-cache.yml)를 복사합니다. 기본 브랜치의 커밋을 빌드하고, 같은 작업의 이후 단계에서 결과 이미지를 사용할 수 있습니다. 새 러너는 GitHub Actions 캐시에서 이전 빌드의 레이어를 복원합니다. 이미지마다 `scope`를 하나씩 정해 캐시 입력과 출력 양쪽에 같은 값을 쓰세요. 캐시 저장에 실패해도 빌드 결과를 사용할 수 있습니다. [Docker GitHub Actions 캐시](https://docs.docker.com/build/cache/backends/gha/)

예제는 두 액션을 커밋 SHA로 고정하고 읽기 권한만 사용합니다. 캐시는 기본 브랜치에서 갱신합니다. 빌드 시크릿은 BuildKit 시크릿 마운트로 전달하면 캐시 레이어에 남지 않습니다. [Docker 빌드 시크릿](https://docs.docker.com/build/building/secrets/)

## 대기열과 작업 환경

- `RUNQUILL_MAX_RUNNERS`가 전체 저장소의 동시 실행 수를 정합니다. 정원이 차면 SQLite에 대기하고, 빈자리가 나면 작업을 발견한 순서대로 러너를 생성합니다. 생성에 실패한 작업은 기본 15초 뒤 재시도합니다. 재시도 시간 동안 다음 작업이 빈자리를 사용할 수 있습니다.
- GitHub는 러너를 24시간 넘게 기다린 작업을 실패 처리합니다. [GitHub 러너 배정 규칙](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#routing-precedence-for-self-hosted-runners)
- 러너는 저장소 범위의 일회용 JIT 러너입니다. 작업마다 새 컨테이너와 내부 Docker 데몬이 생기므로 `docker build`, `services:`, `container:`를 사용할 수 있습니다. 작업이 끝나면 컨테이너와 내부 이미지, 볼륨, 작업 디렉터리가 삭제됩니다.
- 내부 Docker는 `--privileged`를 사용합니다. 이 서버에서 실행을 승인한 저장소와 워크플로에 Runquill을 사용하세요. 신뢰하기 어려운 PR의 코드는 GitHub 호스팅 러너로 지정하세요.
- 서버당 매니저 하나를 실행합니다. 매니저를 재시작해도 Compose의 `queue` 볼륨에 있는 대기열과 현재 러너 기록을 이어받습니다.
- 매니저를 시작할 때와 매번 검색을 마치고 `RUNQUILL_POLL_INTERVAL`(기본 `30s`)이 지난 뒤 GitHub API에서 대기 작업을 찾습니다. 웹훅과 폴링으로 같은 GitHub 작업을 받으면 큐 기록 하나를 공유합니다. 이 값을 `0`으로 설정하면 웹훅만 사용합니다.

```sh
docker compose exec manager runquill status
```

실행 중인 러너 수와 서버별 상태·슬롯 사용량, GitHub 작업 링크를 한 번에 확인할 수 있습니다. `WAITING`은 바로 실행할 수 있는 작업의 대기 순서를 표시합니다. `RETRY`에는 다음 시도까지 남은 시간과 기록된 재시도 이유가 표시됩니다. 바이너리를 직접 실행하는 경우 매니저와 같은 큐 및 Docker 환경 설정으로 `runquill status`를 실행합니다.

스크립트나 모니터링 도구에서는 `docker compose exec -T manager runquill status --json`을 사용합니다. 스키마 버전 1에 `slot_limit`, `runners`, `hosts`, `jobs`가 들어 있습니다. 사용 중인 슬롯은 응답한 서버에서 관측한 값이고, 예약 슬롯에는 오프라인 서버와 소유 서버를 확인할 수 없는 러너 기록이 포함됩니다. Docker 호스트 구성이 충돌하면 작업 목록과 `docker_error`가 출력되고, `runners`는 `null`이며 명령은 실패 코드로 종료합니다.

매니저 로그는 `docker compose logs -f manager`로 확인합니다. 새 GitHub 러너 버전을 적용하려면 `docker build --pull -t runquill-runner:local runner`를 다시 실행합니다. 이미 실행 중인 작업은 사용하던 이미지로 마무리됩니다.

태그별 GHCR 이미지 설치와 업그레이드 절차는 [영문 설치 가이드](README.md#start-the-manager)에 있습니다. 이전 버전에서 업그레이드할 때는 Compose의 `queue` 볼륨을 유지하면 대기열과 실행 중인 러너 기록을 이어받습니다.

로컬에서 대기열과 웹훅 테스트를 실행하려면 Go 1.26.11 이상에서 `go test -race ./...`를 사용합니다.

Copyright © 2026 Runquill contributors. [Apache License 2.0](LICENSE)
