# Runquill

[English](README.md) · 한국어

개인 GitHub 계정의 비공개 저장소에서 요청한 Actions 작업을 내 서버에서 실행합니다. 기본 설치 범위인 **All repositories**를 고르면 이후에 만드는 비공개 저장소도 자동으로 감지합니다. 원한다면 [선택한 저장소만 연결](#선택한-저장소만-연결)할 수 있습니다.

[빠른 시작](#빠른-시작) · [첫 작업](#첫-작업-확인) · [샘플 패널](#샘플-패널) · [팀 패널](#웹-패널과-팀-권한) · [릴리스 다운로드](https://github.com/yakisoba0728/Runquill/releases)

## 빠른 시작

1. [Docker](#공개-릴리스-설치) 또는 [k3s](deploy/k3s/README.md)에 설치합니다. 비공개 GitHub App 생성을 승인하고 개인 계정에 **All repositories**로 설치합니다.
2. 비공개 저장소를 내려받은 디렉터리에서 설치한 실행 파일로 `"$HOME/runquill/runquill" workflow init`을 실행합니다. 생성한 워크플로를 기본 브랜치에 커밋한 뒤 GitHub에서 **Actions → Runquill first job → Run workflow**를 선택합니다. [첫 작업 실행 방법](#첫-작업-확인)
3. 작업 로그에서 `runquill-`로 시작하는 러너 이름을 확인합니다. 작업이 끝나면 매니저의 `runner removed` 로그 또는 [웹 패널](#웹-패널과-팀-권한)에서 정리 결과를 확인합니다.

| 실행 환경 | 러너와 작업 지원 범위 |
| --- | --- |
| [Docker](#공개-릴리스-설치) | 작업마다 일회용 컨테이너를 생성합니다. `docker build`, 서비스 컨테이너, 작업 컨테이너를 지원합니다. [SSH 워커](#docker-작업-서버-추가)를 추가하면 여러 Docker 호스트가 대기열을 공유합니다. |
| [k3s](deploy/k3s/README.md) `plain` | 작업마다 권한을 제한한 Pod를 생성합니다. 셸 등 러너 Pod에서 직접 실행할 수 있는 단계를 지원합니다. |
| [k3s](deploy/k3s/README.md) `dind` | 작업마다 권한을 부여한 Docker-in-Docker Pod를 생성합니다. Docker 빌드, 서비스 컨테이너, 작업 컨테이너가 필요할 때 선택합니다. |

매니저 한 대와 영속 SQLite 대기열이 저장소 전체의 동시 실행 수를 제한합니다. 작업마다 새 파일시스템을 사용하는 저장소 범위 러너를 받습니다. 워크플로에서 GitHub 호스팅 러너도 선택할 수 있습니다.

**신뢰 범위** · Docker-in-Docker에는 `--privileged`가 필요합니다. 전용 서버에서 신뢰하는 코드만 실행하고, 신뢰하기 어려운 PR 코드는 GitHub 호스팅 러너로 보냅니다. GitHub App 키와 매니저의 Docker 소켓은 각각 설치 범위의 저장소와 호스트 Docker를 관리할 수 있으므로 접근을 제한합니다.

## 서버 설치

cgroup v2를 사용하는 amd64 또는 arm64 Linux 서버에 일반 모드(rootful)의 Docker Engine, Docker Compose, `curl`, `sha256sum`이 필요합니다. 기본 폴링 방식은 외부 GitHub API에 접근할 수 있으면 공유기 뒤에서도 동작합니다. 웹훅을 쓰는 경우 공개 HTTPS 주소를 설정합니다.

### 공개 릴리스 설치

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --dir "$HOME/runquill"
```

1. 설치 프로그램이 출력한 브라우저 링크를 열어 비공개 GitHub App을 만듭니다. 개인 계정에 **All repositories**로 설치합니다.
2. 설치 프로그램이 Compose를 시작하고 `Preflight passed.`를 출력할 때까지 기다립니다.
3. 내 비공개 저장소에서 [첫 작업을 실행합니다](#첫-작업-확인).

설치 스크립트는 다운로드 체크섬을 확인하고 같은 버전의 매니저와 러너 이미지를 받습니다. 위 명령은 `$HOME/runquill`에 설치합니다. `--version v0.2.0`로 버전을 지정하거나 `--download-only`로 바이너리와 배포 파일을 먼저 받을 수 있습니다.

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

### 선택한 저장소만 연결

새 설치에서 일부 비공개 저장소만 연결하려면 아래 옵션을 사용합니다.

```sh
sh install.sh --dir "$HOME/runquill" --repository-scope selected
# 설치 디렉터리에 실행 파일이 있는 경우
./runquill setup --repository-scope selected
```

GitHub에서 **Only select repositories**를 고르고 사용할 비공개 저장소를 선택합니다. 저장소 목록은 GitHub가 관리하고, `setup`은 `.env`에 `RUNQUILL_REPOSITORY_SCOPE=selected`를 기록합니다. 나중에 GitHub App 설치 설정에서 저장소를 추가하면 매니저를 재시작하지 않아도 다음 폴링에서 해당 작업을 발견할 수 있습니다. 웹훅 전용 설정에서는 GitHub가 보내는 이벤트를 통해 작업을 감지합니다. 첫 작업을 실행하기 전 매니저 환경에서 `runquill doctor --repo OWNER/REPO`로 접근을 확인하세요. [GitHub App 저장소 접근 변경](https://docs.github.com/en/apps/using-github-apps/reviewing-and-modifying-installed-github-apps)

기존 앱의 설치 범위를 바꾸려면 GitHub 설정을 변경한 뒤 매니저 설정의 `RUNQUILL_REPOSITORY_SCOPE`도 `all` 또는 `selected`로 맞춥니다. Compose 매니저를 다시 만들거나 k3s 설정 Secret을 갱신하고 Deployment를 재시작하세요. 이 값을 생략하면 기본값은 `all`입니다.

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

비공개 Git 저장소를 내려받은 디렉터리의 루트에서 [첫 작업용 워크플로](examples/runquill-first-job.yml)를 만듭니다. 빠른 시작과 설치 위치가 다르다면 경로를 맞춰 실행합니다.

```sh
"$HOME/runquill/runquill" workflow init
```

이 명령은 `.github/workflows/runquill-first-job.yml`을 생성하고 같은 경로의 기존 파일을 보호합니다. `workflow init`이 추가되기 전 버전을 쓰거나 실행 파일이 다른 서버에 있다면 링크의 YAML을 내 저장소의 `.github/workflows/`에 직접 복사합니다.

워크플로를 저장소의 기본 브랜치에 커밋하고 푸시하거나 기존 리뷰 절차로 병합합니다. GitHub에서 **Actions → Runquill first job → Run workflow**를 누릅니다. **Show the runner** 단계에 `runquill-`로 시작하는 이름이 출력됩니다. 이 작업은 쉘만 사용하므로 애플리케이션 의존성 없이 러너 배정을 먼저 확인할 수 있습니다. [GitHub 수동 실행 요구사항](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow)

서버의 Runquill 설치 디렉터리에서는 App이 저장소에 접근할 수 있는지 확인하고 매니저 로그를 볼 수 있습니다.

```sh
docker compose exec -T manager runquill doctor --repo OWNER/REPO
docker compose logs -f manager
```

작업이 끝나면 매니저 로그에 `runner removed`가 출력되고, 저장소의 **Settings → Actions → Runners**에서 해당 러너가 사라집니다. Docker 빌드와 서비스 컨테이너까지 확인하려면 [Docker·PostgreSQL 검증 워크플로](examples/runquill-smoke.yml)를 실행합니다.

### 기존 GitHub App 또는 웹훅 사용

앱을 수동으로 만들 때는 **Only on this account**를 선택하고 **Actions → Read-only**와 **Administration → Read and write** 권한을 부여합니다. 발급한 PEM 키를 보관하고 앱을 개인 계정에 설치합니다. [환경 변수 예제](.env.example)를 `.env`로 복사해 GitHub 로그인, App ID, PEM 키 경로를 입력하고 두 파일의 권한을 `0600`으로 설정합니다. **Only select repositories**로 설치했다면 `RUNQUILL_REPOSITORY_SCOPE=selected`를 설정합니다. 폴링으로 연결할 때는 앱의 웹훅을 끄고 `RUNQUILL_WEBHOOK_SECRET`을 비워둡니다. [저장소 러너 JIT API](https://docs.github.com/en/rest/actions/self-hosted-runners#create-configuration-for-a-just-in-time-runner-for-a-repository)

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

### 워커 풀로 작업 분리

`.env`에 `RUNQUILL_WORKER_POOLS` JSON을 설정하면 작업을 워커 풀로 보낼 수 있습니다. `hosts`에는 `RUNQUILL_DOCKER_HOSTS`에 적은 이름을 그대로 사용합니다. 호스트는 풀 하나에 속하고, 목록에 없는 호스트는 `default` 풀에 속합니다.

```dotenv
RUNQUILL_WORKER_POOLS='{"build":{"hosts":["ssh://builder@worker.example.com"],"repositories":["you/project"],"cpus":4,"memory_mib":8192}}'
```

`repositories`에 입력한 저장소만 해당 풀을 사용합니다. 필드를 생략하면 조건을 충족하는 모든 저장소가 접근할 수 있고, 빈 목록을 설정하면 어느 저장소도 접근할 수 없습니다. `cpus`와 `memory_mib`는 새 러너 컨테이너에 적용되는 Docker 자원 한도입니다. 전체 동시 실행 한도와 서버별 슬롯 한도도 적용됩니다.

작업 라벨의 접미사에 풀 이름을 지정합니다. 풀 접두사가 없는 작업은 `default`를 사용합니다. 매트릭스 빌드라면 작업별 라벨을 구분하는 `strategy.job-index`도 포함합니다.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-pool__build__compile-{2}', github.run_id, github.run_attempt, strategy.job-index) }}
```

한 풀의 슬롯이 다 차도 다른 풀에서 작업을 시작할 수 있습니다. 서로 다른 신뢰 수준의 작업을 운영할 때는 전용 워커를 배치하고 해당 풀에 접근할 저장소를 제한하세요.

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

### 샘플 패널

[GitHub Pages 데모](https://yakisoba0728.github.io/Runquill/)가 게시되면 첫 작업 정리, 웹훅 누락 후 복구, 워커 오프라인을 샘플 데이터로 체험할 수 있습니다. 로컬에서는 `"$HOME/runquill/runquill" panel --demo`를 실행하고 [127.0.0.1:8081](http://127.0.0.1:8081/)을 엽니다. 소스 체크아웃에서는 `go run . panel --demo`를 사용합니다.

데모를 게시하려면 **Settings → Pages → Source**에서 **GitHub Actions**를 선택하고 `main`에서 [Publish sample panel](.github/workflows/demo-pages.yml) 워크플로를 실행합니다. 워크플로는 데모의 HTML, CSS, 스크립트와 라이선스를 게시합니다. 배포 주소는 실행 결과에 표시됩니다. [GitHub Pages 설정](https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site)

### 웹 패널과 팀 권한

안내형 패널 설정을 지원하는 릴리스에서 `sh install.sh --dir "$HOME/runquill" --panel`을 실행하면 설치 프로그램이 패널 오버레이를 포함해 Compose를 시작합니다. `setup`으로 구성한 기존 설치에 추가하려면 설치 디렉터리에서 `./runquill setup --panel`을 실행하고 화면에 표시된 Compose 명령을 따릅니다. 브라우저에서 HTTPS 주소로 접속하려면 `--panel-url https://panel.example.com`을 추가합니다. 기본 주소는 `http://127.0.0.1:8081`입니다.

설정에서 표시한 홈페이지와 콜백 주소로 [GitHub OAuth App](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/creating-an-oauth-app)을 만듭니다. 로컬 설정 화면에 앱의 Client ID와 Client Secret을 입력하고 첫 관리자의 숫자 GitHub 계정 ID를 확인합니다. 계정 ID를 조회할 수 있으면 러너 App 설치 소유자 ID가 기본값으로 표시됩니다. 계정 ID는 해당 계정에서 `gh api user --jq .id`로 확인할 수 있습니다. 러너용 GitHub App은 러너 등록에 계속 사용합니다. 매니저 시작 후 등록한 GitHub 계정으로 로그인해 OAuth 콜백을 확인합니다.

v0.2.0이나 수동 설치에서는 패널 주소를 OAuth App의 홈페이지 주소로 등록하고, 콜백 주소에는 `/auth/github/callback`을 붙입니다. `.env`에 다음 값을 입력합니다.

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

Viewer는 상태를 보고 읽기 토큰을 만들 수 있습니다. Operator는 워커에 새 작업을 배정하거나 잠시 멈출 수 있습니다. Admin은 팀 사용자와 권한, 토큰, 감사 기록을 관리합니다. 토큰은 1~90일 뒤 만료되며 원문은 발급 시 한 번 표시됩니다. 워커 정비를 활성화해도 실행 중인 작업은 계속 처리됩니다.


### 슬롯과 상태 확인

- `RUNQUILL_MAX_RUNNERS`가 전체 저장소의 동시 실행 수를 정합니다. 정원이 차면 SQLite에 대기하고, 빈자리가 나면 작업을 발견한 순서대로 러너를 생성합니다. 생성에 실패한 작업은 기본 15초 뒤 재시도합니다. 재시도 시간 동안 다음 작업이 빈자리를 사용할 수 있습니다.
- GitHub는 러너를 24시간 넘게 기다린 작업을 실패 처리합니다. [GitHub 러너 배정 규칙](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#routing-precedence-for-self-hosted-runners)
- 러너는 저장소 범위의 일회용 JIT 러너입니다. 작업마다 새 컨테이너와 내부 Docker 데몬이 생기므로 `docker build`, `services:`, `container:`를 사용할 수 있습니다. 작업이 끝나면 컨테이너와 내부 이미지, 볼륨, 작업 디렉터리가 삭제됩니다.
- 서버당 매니저 하나를 실행합니다. 매니저를 재시작해도 Compose의 `queue` 볼륨에 있는 대기열과 현재 러너 기록을 이어받습니다.
- 매니저를 시작할 때와 매번 검색을 마치고 `RUNQUILL_POLL_INTERVAL`(기본 `30s`)이 지난 뒤 GitHub API에서 대기 작업을 찾습니다. 웹훅과 폴링으로 같은 GitHub 작업을 받으면 큐 기록 하나를 공유합니다. 이 값을 `0`으로 설정하면 웹훅만 사용합니다.

```sh
docker compose exec manager runquill status
```

실행 중인 러너 수와 서버별 상태·슬롯 사용량, GitHub 작업 링크를 한 번에 확인할 수 있습니다. `WAITING`은 바로 실행할 수 있는 작업의 대기 순서를 표시합니다. `RETRY`에는 다음 시도까지 남은 시간과 기록된 재시도 이유가 표시됩니다. 바이너리를 직접 실행하는 경우 매니저와 같은 큐 및 Docker 환경 설정으로 `runquill status`를 실행합니다.

스크립트나 모니터링 도구에서는 `docker compose exec -T manager runquill status --json`을 사용합니다. 스키마 버전 1에 `slot_limit`, `runners`, `hosts`, `jobs`가 들어 있습니다. 사용 중인 슬롯은 응답한 서버에서 관측한 값이고, 예약 슬롯에는 오프라인 서버와 소유 서버를 확인할 수 없는 러너 기록이 포함됩니다. Docker 호스트 구성이 충돌하면 작업 목록과 `docker_error`가 출력되고, `runners`는 `null`이며 명령은 실패 코드로 종료합니다.

매니저 로그는 `docker compose logs -f manager`로 확인합니다. 새 GitHub 러너 버전을 적용하려면 `docker build --pull -t runquill-runner:local runner`를 다시 실행합니다. 이미 실행 중인 작업은 사용하던 이미지로 마무리됩니다.

태그별 GHCR 이미지 설치와 업그레이드 절차는 [영문 설치 가이드](README.md#start-the-manager)에 있습니다. 이전 버전에서 업그레이드할 때는 Compose의 `queue` 볼륨을 유지하면 대기열과 실행 중인 러너 기록을 이어받습니다.

GitHub에서 작업이 대기 중이라면 `docker compose exec manager runquill diagnose OWNER/REPO`로 원인을 확인할 수 있습니다. App의 접근 권한, 마지막 저장소 폴링 결과, 작업 라벨, 재시도 이유, 슬롯 부족을 함께 보여줍니다.

큐 볼륨을 잃었거나 러너가 남아 있다면 `docker compose exec manager runquill gc --dry-run`으로 해당 컨테이너와 워커, 리스 상태를 확인합니다. 큐에 관련 작업 정보가 있으면 GitHub 작업 상태도 조회합니다. 큐나 워커가 응답하지 않으면 부분 조회임을 표시합니다.

폴링은 매번 접근 가능한 비공개 저장소에서 대기 중과 실행 중인 워크플로를 확인하며, 최근 활동이 있는 저장소부터 조회합니다. 요청됨, 대기, 보류 상태는 저장소 목록이 유지되는 동안 성공한 네 번의 조회 안에 순환 확인합니다. 작업별 타이밍에는 첫 발견 경로와 러너 등록, GitHub가 제공한 작업 시작 시각이 표시됩니다. 발견 지연은 GitHub 작업 생성 시각을 제공한 경우에만 표시됩니다.

로컬에서 대기열과 웹훅 테스트를 실행하려면 Go 1.26.11 이상에서 `go test -race ./...`를 사용합니다.

[CI](.github/workflows/ci.yml)는 amd64와 arm64 Linux에서 Go 테스트와 Docker-in-Docker 검증을 실행합니다. amd64에서는 임시 k3s 클러스터를 띄워 워커 중지 시 작업이 대기하고, 재개 시 러너가 생성되며, 작업 완료 후 Pod와 자격 증명 Secret이 제거되는지 확인합니다. 관리자와 뷰어에 별도 GitHub 계정을 쓰는 [OAuth 검증](panel_live_oauth_test.go)은 전용 OAuth 앱을 준비해 로컬에서 실행할 수 있습니다.

[릴리스 워크플로](.github/workflows/release.yml)는 이 저장소에 릴리스를 게시한 뒤 두 아키텍처의 깨끗한 Linux 컨테이너에서 공개 설치 스크립트로 해당 태그를 검증합니다.

Copyright © 2026 Runquill contributors. [Apache License 2.0](LICENSE)
