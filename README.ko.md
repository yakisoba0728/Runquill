# Runquill

[English](README.md) · 한국어

개인 GitHub 계정이나 [조직](#github-조직-연결)의 비공개 저장소에서 요청한 Actions 작업을 내 서버에서 실행합니다. 기본 설치 범위인 **All repositories**를 고르면 이후에 만드는 비공개 저장소도 자동으로 감지합니다. 원한다면 [선택한 저장소만 연결](#선택한-저장소만-연결)할 수 있습니다.

[빠른 시작](#빠른-시작) · [첫 작업](#첫-작업-확인) · [샘플 패널](#샘플-패널) · [팀 패널](#웹-패널과-팀-권한) · [원격 CLI](docs/remote-cli.md) · [릴리스 다운로드](https://github.com/yakisoba0728/Runquill/releases)

## 빠른 시작

1. [Docker](#공개-릴리스-설치) 또는 [k3s](deploy/k3s/README.md)에 설치합니다. 비공개 GitHub App 생성을 승인하고 개인 계정에 **All repositories**로 설치합니다.
2. 비공개 저장소를 내려받은 디렉터리에서 설치한 실행 파일로 `"$HOME/runquill/runquill" workflow init`을 실행합니다. 생성한 워크플로를 기본 브랜치에 커밋한 뒤 GitHub에서 **Actions → Runquill first job → Run workflow**를 선택합니다. [첫 작업 실행 방법](#첫-작업-확인)
3. 작업 로그에서 `runquill-`로 시작하는 러너 이름을 확인합니다. 작업이 끝나면 매니저의 `runner removed` 로그 또는 [웹 패널](#웹-패널과-팀-권한)에서 정리 결과를 확인합니다.

| 실행 환경 | 러너와 작업 지원 범위 |
| --- | --- |
| [Docker](#공개-릴리스-설치) | 작업마다 일회용 컨테이너를 생성합니다. [풀](#워커-풀로-작업-분리)에서 `privileged`를 끄지 않았다면 `docker build`, 서비스 컨테이너, 작업 컨테이너를 지원합니다. [SSH 워커](#docker-작업-서버-추가)를 추가하면 여러 Docker 호스트가 대기열을 공유합니다. |
| [k3s](deploy/k3s/README.md) `plain` | 작업마다 권한을 제한한 Pod를 생성합니다. 셸 등 러너 Pod에서 직접 실행할 수 있는 단계를 지원합니다. |
| [k3s](deploy/k3s/README.md) `dind` | 작업마다 권한을 부여한 Docker-in-Docker Pod를 생성합니다. Docker 빌드, 서비스 컨테이너, 작업 컨테이너가 필요할 때 선택합니다. |

매니저 한 대와 영속 SQLite 대기열이 저장소 전체의 동시 실행 수를 제한합니다. 작업마다 새 파일시스템을 사용하는 저장소 범위 러너를 받습니다. 단, Docker [워커 풀에서 `cache`를 켜면](#워커-풀로-작업-분리) 저장소별 캐시 디렉터리는 작업 사이에 유지됩니다. 워크플로에서 GitHub 호스팅 러너도 선택할 수 있습니다.

**신뢰 범위** · 기본 설정의 Docker-in-Docker에는 `--privileged`가 필요합니다. 전용 서버에서 신뢰하는 코드만 실행하고, 신뢰하기 어려운 PR 코드는 GitHub 호스팅 러너로 보냅니다. GitHub App 키와 매니저의 Docker 소켓은 각각 설치 범위의 저장소와 호스트 Docker를 관리할 수 있으므로 접근을 제한합니다.

## 서버 설치

cgroup v2를 쓰는 amd64 또는 arm64 Linux 서버가 필요합니다. 그 서버에서 설치 스크립트를 받아 실행하면 호스트 준비, 릴리스 다운로드, 설치 마법사 실행까지 이어집니다.

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --dir "$HOME/runquill" --wizard
```

Docker 호스트 묶음마다 매니저 하나를 실행합니다. 폴링은 GitHub에 HTTPS로 연결할 수 있으면 공유기 뒤에서도 동작합니다.

### 호스트 준비

설치 스크립트는 Linux, cgroup v2, Docker Compose, Docker 접근 권한을 확인합니다. 호스트 설정이 필요하면 실행할 명령을 보여 주고 한 번 묻습니다. Ubuntu, Debian, Fedora, RHEL과 파생 배포판, Alpine에서는 빠진 Docker 패키지를 설치할 수 있습니다. systemd나 OpenRC가 없는 호스트에서는 `dockerd`를 직접 시작합니다. GitHub·GHCR 연결은 `--prepare-only`로 미리 확인할 수 있습니다.

Docker 저장소를 추가할 때는 서명 키를 검증합니다. 사용자에게 root와 같은 권한을 주는 `docker` 그룹을 추가하면, 현재 설치가 새 그룹 권한으로 이어집니다.

| 옵션 | 동작 |
| --- | --- |
| `--yes`, `-y` | 묻지 않고 목록의 변경을 적용합니다. 무인 설치에 씁니다 |
| `--no-install` | 시스템을 바꾸지 않습니다. 빠진 항목을 출력하고 실패합니다 |
| `--prepare-only` | 호스트만 준비하고 멈춥니다 |
| `--no-docker` | Docker 점검을 건너뜁니다. K3s 호스트 등에 씁니다 |
| `--no-tailscale` | 브라우저 설치에서 Tailscale 자동 설정을 건너뜁니다 |
| `--download-only` | 호스트 점검 없이 릴리스를 받아 검증만 합니다 |

### 직접 호스트 준비

호스트를 직접 준비하려면 [Docker 설치 가이드](https://docs.docker.com/engine/install/)에 따라 Docker Engine과 Compose 플러그인을 설치합니다. 그다음 Docker를 시작하고 사용자에게 접근 권한을 줍니다.

```sh
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"
```

로그아웃한 뒤 다시 로그인합니다. 이후 `sh install.sh --prepare-only --no-install`로 호스트를 바꾸지 않고 점검할 수 있습니다.

### 브라우저로 설치

`sh install.sh --dir "$HOME/runquill" --wizard`를 실행하고 필요하면 호스트 준비를 승인합니다. 출력된 링크를 열어 GitHub를 연결하고 배포합니다. 마법사가 호스트에 맞는 러너 한도를 채웁니다. 조직, 선택한 저장소, K3s, 사용자 지정 주소를 쓰려면 **고급 설정**을 엽니다.

설치 프로그램은 Tailscale을 준비하고 브라우저 로그인을 시작할 수 있습니다. Docker Compose에서 tailnet이 허용하면 마법사가 Serve로 패널을 비공개 연결하고 Funnel로 GitHub 웹훅을 받습니다. 폴링도 계속 사용할 수 있습니다. [브라우저 설치 가이드](docs/browser-setup.md)

마법사는 `127.0.0.1`에서 접속을 받습니다. SSH로 접속하면 내 컴퓨터에서 실행할 포트 포워딩 명령을 출력합니다.

```text
ssh -L 18080:127.0.0.1:18080 alice@203.0.113.10
```

이 SSH 세션을 열어 둔 채 출력된 `http://127.0.0.1:18080/?key=…` 주소를 내 컴퓨터의 브라우저로 엽니다. 양쪽 포트 번호는 같아야 합니다. 주소의 `key`는 이번 설치 세션의 비밀값입니다. 다른 호스트 이름이나 NAT 주소로 서버에 접속한다면 `ssh` 명령에 그 주소를 씁니다.

### 공개 릴리스 설치

브라우저 대신 터미널에서 설정하려면 `--wizard`를 빼고 실행합니다.

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --dir "$HOME/runquill"
```

1. 설치 프로그램이 출력한 브라우저 링크를 열어 비공개 GitHub App을 만듭니다. 개인 계정에 **All repositories**로 설치합니다.
2. 설치 프로그램이 Compose를 시작하고 `Preflight passed.`를 출력할 때까지 기다립니다.
3. 내 비공개 저장소에서 [첫 작업을 실행합니다](#첫-작업-확인).

설치 스크립트는 다운로드 체크섬을 확인하고 같은 버전의 이미지를 사용합니다. `--version v0.3.2`로 공개 버전을 지정하거나 `--download-only`로 바이너리와 배포 파일을 먼저 받을 수 있습니다.

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

### GitHub 조직 연결

조직 소유자 계정으로 설치를 진행한 뒤 생성된 GitHub App을 그 조직에 설치합니다. 매니저 한 인스턴스는 GitHub 계정 하나를 관리합니다. 계정이나 러너 등록 수준을 변경할 때는 새 디렉터리와 대기열을 사용합니다.

```sh
./runquill setup --organization YOUR_ORG --repository-scope selected
```

Compose용 공개 설치 스크립트에도 같은 옵션을 전달할 수 있습니다. k3s에서는 `setup`에 `--k3s`를 추가합니다. 기본적으로 작업을 요청한 저장소에 러너를 등록하며, 저장소 **Actions 읽기**와 **Administration 읽기·쓰기** 권한을 사용합니다. 기존 App을 사용한다면 `.env`에 `RUNQUILL_OWNER=YOUR_ORG`와 `RUNQUILL_OWNER_TYPE=organization`을 설정합니다. 조직의 패널 설정에는 실제 관리자의 GitHub 사용자 ID를 입력합니다. [GitHub App 등록](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)

조직 러너 그룹을 사용하려면 **선택한 저장소**만 허용하고 공개 저장소 접근을 끈 조직 소유 그룹을 만듭니다. 그룹의 GitHub 워크플로 단위 제한은 사용하지 않습니다. App이 접근할 수 있는 비공개 저장소만 그룹에 추가하고 `setup` 또는 설치 스크립트에 `--runner-group-id ID`를 전달합니다. 기존 App은 `RUNQUILL_RUNNER_GROUP_ID=ID`와 조직의 **Self-hosted runners 읽기·쓰기** 권한을 설정합니다. Runquill은 등록 전에 그룹 정책과 저장소 접근을 확인합니다. GitHub가 그룹 접근과 라벨로 작업을 배정하므로 서로의 워크플로를 신뢰하는 저장소끼리만 그룹을 공유하고 작업별 고유 라벨을 유지하세요. [GitHub 러너 그룹](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access)

`runquill doctor --repo YOUR_ORG/REPO`로 App과 그룹 범위를, `runquill diagnose YOUR_ORG/REPO`로 대기 작업을 확인할 수 있습니다.

### 매니저 시작

소스에서 설치할 때는 러너 이미지를 빌드한 뒤 매니저를 시작합니다.

```sh
docker build --pull -t runquill-runner:local runner
docker compose up -d --build
```

릴리스 실행 파일을 사용하면 `setup`이 같은 태그의 매니저와 러너 이미지 이름을 `.env`에 기록합니다. 없는 러너 이미지는 매니저가 `local`과 `ssh://` 호스트에 직접 받으므로, `setup`이 출력한 러너 이미지 다운로드 명령은 첫 작업 전에 이미지를 미리 받아 두는 선택 단계입니다. 다음 명령으로 매니저를 받아 시작합니다.

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

수동으로 작성한 `.env`에 릴리스 이미지를 사용하려면 예제에 있는 매니저와 러너 이미지 이름을 같은 버전 태그로 맞춥니다. 호스트에 러너 이미지가 없으면 매니저가 받아 옵니다.

기존 App에서 웹훅을 켜려면 **Workflow job** 이벤트를 구독하고 공개 HTTPS 주소를 등록합니다. `openssl rand -hex 32`로 생성한 시크릿을 GitHub와 `RUNQUILL_WEBHOOK_SECRET`에 같은 값으로 설정합니다. HTTPS 프록시에서 `/webhook`을 매니저의 8080 포트로 전달합니다. 같은 서버에서 Caddy를 실행한다면 아래 설정을 사용합니다.

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

NAT 뒤에 있는 워커는 [아웃바운드 워커](docs/outbound-workers.md)로 등록해 HTTPS로 매니저에 연결할 수 있습니다.

매니저 한 대에서 로컬 Docker와 SSH로 연결한 여러 Docker 서버를 사용합니다. 각 서버는 Linux의 rootful Docker, cgroup v2, Docker 소켓에 접근할 수 있는 SSH 계정이 필요합니다. `RUNQUILL_RUNNER_IMAGE`에 해당하는 이미지는 매니저가 이 연결로 각 서버에 받습니다.

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

`repositories`에 입력한 저장소만 해당 풀을 사용합니다. 필드를 생략하면 조건을 충족하는 모든 저장소가 접근할 수 있고, 빈 목록을 설정하면 어느 저장소도 접근할 수 없습니다. `cpus`와 `memory_mib`는 새 러너 컨테이너에 적용되는 Docker 자원 한도이며, 생략하면 Docker 기본값을 사용합니다. 선택 항목인 `image`로 풀마다 다른 Runquill 호환 이미지를 지정할 수 있습니다. 매니저가 풀의 각 호스트에 이 이미지를 받습니다. 레지스트리에 없는 이미지라면 해당 호스트에서 직접 빌드합니다. 전체 동시 실행 한도와 서버별 슬롯 한도도 적용됩니다.

작업 라벨의 접미사에 풀 이름을 지정합니다. 풀 접두사가 없는 작업은 `default`를 사용합니다. 매트릭스 빌드라면 작업별 라벨을 구분하는 `strategy.job-index`도 포함합니다.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-pool__build__compile-{2}', github.run_id, github.run_attempt, strategy.job-index) }}
```

대기 작업은 요청한 풀의 슬롯을 사용하며, 한 풀의 슬롯이 다 차도 다른 풀에서 작업을 시작할 수 있습니다. 서로 다른 신뢰 수준의 작업을 운영할 때는 전용 워커를 배치하고 해당 풀에 접근할 저장소를 제한하세요.

라벨에 설정되지 않은 풀을 지정했거나 풀이 허용하지 않는 저장소라면 작업은 `worker pool "build" is not configured` 같은 재시도 이유와 함께 대기합니다. 패널의 **설정**에서 풀을 고치면 매니저를 재시작하지 않아도 이후 스케줄링에서 작업이 시작됩니다. 실패로 기록되는 경우는 24시간 기한이 지났을 때뿐입니다.

풀에는 다음 선택 항목도 쓸 수 있습니다.

```dotenv
RUNQUILL_WORKER_POOLS='{"lint":{"hosts":["local"],"privileged":false,"cache":true,"max_per_repository":2,"images":["ubuntu22.04"]}}'
```

- `privileged`의 기본값은 `true`이며, 러너마다 `--privileged`와 내부 Docker 데몬을 사용합니다. `false`로 설정하면 러너를 UID 1001로 실행하고 모든 capability를 제거하며 `no-new-privileges`를 적용합니다. 이 풀의 작업은 `docker`, 서비스 컨테이너, 작업 컨테이너, `sudo`를 사용할 수 없습니다. 이런 풀에 사용자 지정 `image`를 쓰려면 `/home/runner`를 소유한 UID 1001 러너 사용자가 이미지에 있어야 합니다.
- `cache: true`는 `runquill-cache-<저장소 ID>`라는 Docker 이름 있는 볼륨을 `/home/runner/.cache`에 마운트합니다. 같은 호스트에서 실행되는 같은 저장소의 이후 작업이 내용을 재사용하고, 동시에 실행되는 작업도 같은 볼륨을 공유합니다. 풀에서 실행되는 해당 저장소의 모든 작업이 캐시를 읽고 바꿀 수 있으므로 완전히 새 파일시스템에서 시작하지 않습니다. 러너의 `RUNNER_TOOL_CACHE`도 이 볼륨 안을 가리키므로 `actions/setup-*`가 받은 도구도 남습니다. 볼륨은 호스트마다 따로 생기며, 30일 동안 어떤 러너도 쓰지 않은 볼륨은 매니저가 삭제합니다([정리](docs/runner-images.md#cleanup-of-unused-images-and-cache-volumes)). 목록은 `docker volume ls -f label=io.runquill.cache=1`로 확인합니다. 이 캐시는 [Docker 빌드 레이어 캐시](#다음-작업에서-docker-빌드-레이어-재사용)와 별개입니다.
- `max_per_repository`는 해당 풀에서 저장소 하나가 사용할 수 있는 러너 수를 제한하며, 그 풀에서는 `RUNQUILL_MAX_PER_REPOSITORY`보다 우선합니다. 둘 다 없으면 저장소 하나가 빈 슬롯을 모두 쓸 수 있습니다.
- `images`는 풀의 작업이 `os__NAME__`으로 고를 수 있는 [러너 OS](#러너-os-선택) 이름 목록입니다. 생략하면 모든 이름을 허용합니다.

`privileged: false` 또는 `cache: true`를 설정한 풀의 [아웃바운드 워커](docs/outbound-workers.md)는 매니저와 같은 Runquill 릴리스를 실행해야 합니다. 이전 버전 에이전트는 이 설정이 담긴 러너 작업을 거부하므로 매니저와 함께 워커도 업그레이드하세요.

관리자 패널의 풀 편집기에는 Docker-in-Docker와 공유 캐시 체크박스, 허용할 러너 OS 이름 입력란이 있습니다. 편집기는 저장할 때 풀의 `max_per_repository` 값을 유지하지만 바꾸지는 못합니다. 패널에서 설정을 한 번 저장하면 그 값이 `RUNQUILL_WORKER_POOLS`보다 우선합니다.

### 저장소 간 슬롯 공유

슬롯이 비면 실행 중인 러너가 가장 적은 저장소의 작업을 먼저 시작하고, 같으면 가장 오래 기다린 작업을 고릅니다. 같은 저장소 안에서는 발견한 순서대로 시작합니다. `RUNQUILL_MAX_PER_REPOSITORY`를 설정하면 매니저 전체에서 저장소마다 풀별 러너 수를 제한합니다. 값은 `0`(무제한, 기본값)부터 `10000`까지입니다. 한도에 걸린 작업은 대기열에 남고 다른 저장소의 작업을 막지 않습니다. k3s에서도 모든 러너가 하나의 풀을 공유하는 것으로 보고 같은 한도를 적용합니다.

`RUNQUILL_SCHEDULER_WORKERS`는 스케줄링 한 번에 병렬로 처리할 러너 시작과 정리 작업 수입니다. `1`(순차)부터 `32`까지 설정할 수 있고 기본값은 `4`입니다. Docker 호스트 하나에 대한 호출은 순서대로 처리합니다. Compose에서는 [오버레이](#매니저-설정-추가)로 이 값을 전달합니다.

### 러너 이미지와 컨테이너 엔진

릴리스는 Ubuntu 24.04 기반 `runquill-runner:<버전>`과 `-ubuntu22.04`, `-ubuntu26.04`, `-debian12`, `-debian13` 변형 이미지를 게시합니다. 매니저는 `local`과 `ssh://` 호스트에 없는 러너 이미지를 직접 받습니다(기본값 `RUNQUILL_IMAGE_PULL=missing`, 주기적으로 새로 받으려면 `always`). 아웃바운드 `agent://` 워커, 로컬에서 빌드한 이미지, `RUNQUILL_IMAGE_PULL=never` 설정에서는 이미지를 직접 준비합니다. 매니저는 자신이 받은 이미지와 30일 동안 어떤 러너도 쓰지 않은 캐시 볼륨도 삭제합니다. [러너 이미지](docs/runner-images.md) `RUNQUILL_CONTAINER_ENGINE=podman`을 설정하면 `local`과 `ssh://` 호스트에 Podman CLI를 사용하고, rootless 엔진의 러너는 Docker-in-Docker 없이 시작합니다. Podman과 rootless 엔진은 실제 호스트에서 아직 검증하지 않았습니다. [엔진과 이미지](docs/container-engines.md)

### 탄력적 러너 한도

`RUNQUILL_ELASTIC=observe` 또는 `apply`를 설정하면 `local` Docker 호스트에서 바쁜 러너가 다른 러너나 빈 슬롯에 약속되지 않은 CPU와 메모리를 풀의 `cpus`, `memory_mib`의 배수까지 빌려 씁니다. 한도는 풀 설정값 아래로 내려가지 않고 메모리는 줄이지 않습니다. `observe`는 결정만 기록하며 **워커와 풀** 화면에서 확인할 수 있습니다. 부하가 걸린 실제 호스트에서는 아직 검증하지 않았습니다. [탄력적 한도](docs/elastic-limits.md)

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

### 러너 OS 선택

같은 작업별 라벨 안에서 풀 선택자 뒤에 `os__NAME__`을 넣어 OS 변형을 고릅니다. 이름은 `ubuntu2204`, `ubuntu2604`, `debian12`, `debian13`처럼 점 없이 씁니다. 지정하지 않은 작업은 호스트의 이미지로 실행됩니다.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-os__ubuntu2204__build', github.run_id, github.run_attempt) }}
```

이름은 러너 이미지 목록에서 찾습니다. 릴리스 빌드에는 게시된 변형이 모두 들어 있고, `RUNQUILL_RUNNER_IMAGES`로 항목을 추가하거나 바꿀 수 있습니다. Runquill은 작업마다 고유한 라벨로 러너와 작업을 짝지으므로 `runs-on: ubuntu-22.04`처럼 GitHub 호스팅 라벨만 쓰는 방식은 지원하지 않습니다. [러너 이미지](docs/runner-images.md)

### 다음 작업에서 Docker 빌드 레이어 재사용

루트에 `Dockerfile`이 있는 저장소의 `.github/workflows/`에 [빌드 캐시 예제](examples/runquill-build-cache.yml)를 복사합니다. 기본 브랜치의 커밋을 빌드하고, 같은 작업의 이후 단계에서 결과 이미지를 사용할 수 있습니다. 새 러너는 GitHub Actions 캐시에서 이전 빌드의 레이어를 복원합니다. 이미지마다 `scope`를 하나씩 정해 캐시 입력과 출력 양쪽에 같은 값을 쓰세요. 캐시 저장에 실패해도 빌드 결과를 사용할 수 있습니다. [Docker GitHub Actions 캐시](https://docs.docker.com/build/cache/backends/gha/)

예제는 두 액션을 커밋 SHA로 고정하고 읽기 권한만 사용합니다. 캐시는 기본 브랜치에서 갱신합니다. 빌드 시크릿은 BuildKit 시크릿 마운트로 전달하면 캐시 레이어에 남지 않습니다. [Docker 빌드 시크릿](https://docs.docker.com/build/building/secrets/)

### 기존 워크플로 이전

관리자는 패널의 **이전** 화면에서 GitHub 호스팅 라벨을 쓰는 작업을 Runquill 라벨로 바꾸는 풀 리퀘스트를 열 수 있습니다. Contents, Pull requests, Workflows 쓰기 권한이 필요하며, 새 App에서는 `runquill setup --migrate`만 이 권한을 요청합니다. Runquill은 먼저 diff를 미리 보여주고 `runs-on`만 바꿉니다. 안전하게 바꿀 수 없는 작업은 이유와 함께 그대로 두며, 병합은 하지 않습니다. `ubuntu-22.04` 같은 GitHub 호스팅 Ubuntu 버전 라벨은 매니저의 러너 이미지 목록에 그 OS가 있으면 `os__` 선택자로 바뀝니다. 실제 GitHub 저장소에서는 아직 검증하지 않았습니다. [워크플로 이전](docs/workflow-migration.md)

`runquill workflow check`는 로컬 워크플로 파일에서 잘못된 Runquill 라벨, 설정되지 않은 풀과 러너 OS 이름, self-hosted 러너의 위험한 트리거, 권한과 시간 제한 누락, 고정되지 않은 액션을 찾습니다. 심각도가 high인 항목이 있으면 상태 코드 1로 종료합니다. `--strict`를 붙이면 항목이 하나라도 있을 때 1로 종료하고, `--json`은 결과를 JSON으로 출력합니다. [점검 코드](docs/workflow-migration.md#check-workflows-offline)

## K3s에서 실행

Kubernetes로 매니저와 SQLite 볼륨을 배포하고, 작업마다 노드 중 한 곳에 Pod를 하나씩 만듭니다. 설치, 자원 한도, 러너 모드는 [K3s 가이드](deploy/k3s/README.md)를 따릅니다.

## 운영과 업그레이드

### 샘플 패널

[GitHub Pages 데모](https://yakisoba0728.github.io/Runquill/)가 게시되면 첫 작업 정리, 웹훅 누락 후 복구, 워커 오프라인을 샘플 데이터로 체험할 수 있습니다. 로컬에서는 `"$HOME/runquill/runquill" panel --demo`를 실행하고 [127.0.0.1:8081](http://127.0.0.1:8081/)을 엽니다. 소스 체크아웃에서는 `go run . panel --demo`를 사용합니다.

데모를 게시하려면 **Settings → Pages → Source**에서 **GitHub Actions**를 선택하고 `main`에서 [Publish sample panel](.github/workflows/demo-pages.yml) 워크플로를 실행합니다. 워크플로는 데모의 HTML, CSS, 스크립트와 라이선스를 게시합니다. 배포 주소는 실행 결과에 표시됩니다. [GitHub Pages 설정](https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site)

### 웹 패널과 팀 권한

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

팀 사용자는 등록된 GitHub 계정으로 로그인합니다. 관리자는 GitHub 로그인이나 숫자 계정 ID로 팀원을 추가합니다. 로그인을 입력하면 패널이 GitHub 공개 사용자 API로 계정 ID를 확인하며, 개인 사용자 계정만 등록할 수 있습니다. 토큰은 1~90일 뒤 만료되고 권한 범위는 하나이며 패널에서 폐기할 수 있습니다. 토큰 원문은 발급 시 한 번만 표시됩니다. `Authorization: Bearer <token>` 헤더로 보냅니다. 토큰은 자기 범위의 경로만 호출할 수 있으므로 조회와 변경을 함께 하려면 토큰 두 개가 필요합니다. 러너 로그, 워크플로 이전, 백업 등 관리자 경로에는 어떤 토큰도 쓸 수 없습니다. 모든 경로는 [API 명세](api/openapi.yaml)에 있습니다.

| 범위 | 경로 |
| --- | --- |
| `fleet:read` | `GET /snapshot`, `GET /api/events`, `GET /api/jobs`, `GET /api/elastic`, `GET /api/usage`, `POST /mcp` |
| `hosts:write` | `POST /api/hosts/cordon`에 `{"host":"local","cordoned":true}`를 보냅니다. 배정을 멈춰도 실행 중인 러너는 계속 처리됩니다. |
| `jobs:write` | `POST /api/jobs/{id}/cancel`, `POST /api/jobs/{id}/retry` |

**작업 기록**은 최신 작업부터 대기, 실행 중, 종료, 실패 결과와 함께 보여줍니다. 저장소, 결과, 최근 1시간·24시간·7일로 필터링할 수 있습니다. 끝난 작업은 7일 동안 보관하고 작업 로그는 GitHub에 남습니다. `GET /api/jobs`도 같은 목록을 반환하며 `repository`, `outcome`, `since`와 `until`(Unix 초), `limit`(최대 100), 그리고 응답의 `next` 값을 `before`로 받습니다.

Operator와 Admin은 러너 없이 대기 중인 작업을 패널이나 `jobs:write` 토큰으로 관리할 수 있습니다. **지금 재시도**는 재시도 대기 시간을 없애 다음 스케줄링에서 작업을 시도하게 합니다. **작업 취소**는 작업을 `cancelled by @LOGIN` 사유로 실패 처리하고 러너를 시작하지 않습니다. GitHub의 실행은 GitHub에서 취소하거나 다시 실행할 때까지 대기 상태로 남습니다.

감사 기록에는 로그인, 팀, 토큰, 호스트, 작업, 설정, 백업 변경과 러너 로그 조회, 워크플로 이전이 실행한 사람의 로그인과 함께 남습니다. 작업 종류나 실행한 사람의 로그인 또는 ID로 필터링하고, 이전 기록을 페이지 단위로 불러올 수 있습니다. 설정 변경은 바뀐 항목마다 이전 값과 새 값을 기록하며, 이름이 비밀값으로 보이는 항목은 값을 남기지 않습니다.

패널은 Server-Sent Events로 실시간 갱신하고, 스트림을 쓸 수 없으면 폴링으로 전환합니다. Cmd+K 또는 Ctrl+K로 명령 팔레트를 열고, 밝은 테마·어두운 테마·시스템 설정 중에서 고를 수 있습니다. 화면별 기능과 사용 권한은 [패널 가이드](docs/panel.md)에서 확인하세요.

패널에서 작업을 선택하면 러너 라벨, 대기 또는 시작 원인, 최근 재시도 이유를 확인할 수 있습니다. 설정된 GitHub App 접근 범위는 **GitHub 연결**에 표시됩니다. 패널에는 발견한 작업만 표시되므로, 화면에 작업이 없으면 `runquill diagnose OWNER/REPO`로 GitHub App의 접근 범위, 작업의 GitHub 라벨, 이벤트 전달을 점검하세요. 같은 원인은 `runquill status --json`의 작업별 `diagnostic` 객체에 `code`와 `message`로도 표시됩니다.

### 사용량 통계

**사용량** 화면과 `GET /api/usage`는 끝난 작업의 수, 실패, 러너 사용 시간, 대기 시간을 UTC 날짜, 저장소, 풀, 호스트별로 보여줍니다. 작업은 끝난 UTC 날짜에 한 번만 집계합니다. 실패는 Runquill이 포기했거나 패널에서 취소한 작업이며 GitHub 작업 결과와는 다릅니다. 일별 합계는 대기열 DB에 400일 동안 보관하지만 대기열 백업에는 포함되지 않습니다. [자세히](docs/panel.md#usage-statistics)

### 러너 로그

Operator와 Admin은 **로그** 화면에서 작업을 맡은 러너의 컨테이너 출력을 보거나 이어서 받을 수 있습니다. GitHub 단계 로그가 아니라 러너 프로세스의 로그입니다. `local`과 `ssh://` 호스트의 러너만 지원하며 API 토큰으로는 볼 수 없습니다. 실제 운영 환경에서는 아직 검증하지 않았습니다. [자세히](docs/panel.md#runner-logs)

### 원격 CLI

다른 컴퓨터에서 패널 API로 `runquill jobs list`, `runners list`, `hosts list`, `pools list`, `hosts cordon`, `hosts uncordon`, `jobs cancel`, `jobs retry`를 실행할 수 있습니다. `RUNQUILL_URL`에 패널 주소를 지정하고 토큰은 `RUNQUILL_TOKEN`이나 `--token-file`로 전달합니다. 스크립트에서는 `--json`을 붙입니다. 명령마다 맞는 토큰 범위가 필요하며, 감사 기록은 어떤 토큰으로도 읽을 수 없으므로 `audit tail`은 항상 웹 패널을 안내합니다. [원격 CLI 가이드](docs/remote-cli.md)

### 코딩 에이전트용 MCP

`POST /mcp`는 운영 현황, 작업, 호스트, 사용량, 탄력 한도 결정을 읽기 전용 [Model Context Protocol](https://modelcontextprotocol.io/) 도구로 제공합니다. HTTP로 `fleet:read` 토큰을 보내거나, `runquill mcp`를 패널로 전달하는 로컬 stdio 서버로 실행합니다. 브라우저 세션은 항상 거부합니다. 실제 MCP 클라이언트로는 아직 검증하지 않았습니다. [MCP 가이드](docs/mcp.md)

### 상태 확인과 업그레이드

대기 작업 수, 러너 슬롯 사용량, 시작 및 정리 횟수를 모니터링하려면 별도 [Prometheus 메트릭 리스너](docs/metrics.md)를 활성화하세요.

```sh
docker compose exec manager runquill status
```

실행 중인 러너 수와 서버별 상태·슬롯 사용량, GitHub 작업 링크를 한 번에 확인할 수 있습니다. `WAITING`은 바로 실행할 수 있는 작업의 발견 순서를 표시하며, [저장소 간 슬롯 공유](#저장소-간-슬롯-공유) 때문에 실제 시작 순서는 다를 수 있습니다. `RETRY`에는 다음 시도까지 남은 시간과 기록된 재시도 이유가 표시됩니다. 바이너리를 직접 실행하는 경우 매니저와 같은 큐 및 러너 백엔드 설정으로 `runquill status`를 실행합니다.

운영 패널에서 대기 순서, 대기 시간, 재시도 이유를 확인하고 저장소, 작업 ID, 풀로 작업을 필터링할 수 있습니다. 워커와 러너 표에는 호스트 상태, 사용 중·예약 슬롯, 정비 상태, 실행 시간이 표시됩니다. 풀 용량은 전체 한도를 적용하기 전의 워커 슬롯을 보여주고, 개요에는 실제로 사용할 수 있는 슬롯 수가 표시됩니다.

관리자는 **설정**에서 전체 및 워커별 슬롯 한도, GitHub 폴링 주기, 러너 이미지, Docker 워커와 풀 정책을 바꿀 수 있습니다. 변경 내용은 대기열 DB와 감사 기록에 저장되며, 매니저를 재시작하지 않고 새 작업 할당에 적용됩니다. 초기값은 배포 설정에서 가져옵니다. SSH 워커는 배포 시 등록한 SSH 인증을 사용하고, 아웃바운드 에이전트에는 배포 시 등록한 인증 파일이 필요합니다. GitHub App, 패널 주소 및 Tailscale은 설치 마법사에서 설정합니다.

스크립트나 모니터링 도구에서는 `docker compose exec -T manager runquill status --json`을 사용합니다. 스키마 버전 1에 `slot_limit`, `runners`, `hosts`, `jobs`가 들어 있습니다. 사용 중인 슬롯은 응답한 서버에서 관측한 값이고, 예약 슬롯에는 오프라인 서버와 소유 서버를 확인할 수 없는 러너 기록이 포함됩니다. Docker 호스트 구성이 충돌하면 작업 목록과 `docker_error`가 출력되고, `runners`는 `null`이며 명령은 실패 코드로 종료합니다.

GitHub에서 작업이 대기 중이라면 `docker compose exec manager runquill diagnose OWNER/REPO`로 원인을 확인할 수 있습니다. 저장소 범위의 읽기 전용 토큰으로 App의 접근 권한을 확인하고, 마지막 저장소 폴링 결과, 잘못된 라벨, 재시도 이유, 워커 슬롯 부족을 함께 보여줍니다.

`docker compose exec manager runquill gc --dry-run`은 대기열 리스와 맞지 않는 Runquill 러너 컨테이너를 찾아 Docker 호스트와 상태를 보여줍니다. 큐에 관련 작업 정보가 있으면 GitHub 작업 상태도 조회하고, 큐나 호스트가 응답하지 않으면 부분 조회임을 표시합니다. 이 명령은 컨테이너와 큐 데이터를 읽기만 합니다.

정리하려면 `docker compose exec manager runquill gc --remove-orphans`를 실행합니다. 대기열 리스가 없고 작업을 실행하고 있을 수 없는 러너 컨테이너를 삭제하며, 남겨둔 러너는 이유와 함께 출력합니다. `--grace`(기본 `10m`, 최소 `1m`)보다 최근에 만든 러너는 건드리지 않으므로 시작 중인 러너는 안전합니다. 리스가 다른 호스트에 기록된 러너, 큐에 작업 기록이 없는데 실행 중인 러너, GitHub에서 작업이 끝나지 않았거나 러너가 사용 중으로 보이는 경우, GitHub에 연결할 수 없는 경우에는 러너를 남깁니다. 큐에 작업이 있는 러너는 GitHub 러너 등록을 먼저 삭제한 뒤 Docker ID로 컨테이너를 삭제합니다. `local`과 `ssh://` 호스트에서만 동작하며, `agent://` 워커와 Kubernetes 백엔드는 건너뛰었다고 표시합니다. 호스트 조회나 삭제에 실패하면 상태 코드 1로 종료합니다.

`docker compose exec manager runquill gc --unused --dry-run`은 Runquill이 받은 러너 이미지와 캐시 볼륨 중 `RUNQUILL_CLEANUP_AFTER`(기본 `30d`) 동안 어떤 러너도 쓰지 않은 항목을 보여줍니다. `--dry-run` 없이 실행하면 삭제하며, 매니저도 `RUNQUILL_CLEANUP_INTERVAL`마다 같은 기준으로 정리합니다. `--older-than`으로 다른 보존 기간을 지정할 수 있습니다. 직접 받거나 빌드한 이미지는 삭제하지 않습니다. [정리](docs/runner-images.md#cleanup-of-unused-images-and-cache-volumes)

- `RUNQUILL_MAX_RUNNERS`(기본 2)가 전체 저장소의 동시 실행 수를 정합니다. 정원이 차면 SQLite에 대기합니다. 생성에 실패한 작업은 15초 뒤 재시도하며, 그동안 다음 작업이 빈자리를 사용할 수 있습니다. GitHub는 24시간 넘게 대기한 self-hosted 작업을 실패 처리합니다. [GitHub 러너 배정 규칙](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#routing-precedence-for-self-hosted-runners)
- Runquill은 24시간 안에 시작하지 못한 작업을 사유와 마지막 재시도 오류와 함께 실패로 기록합니다. 패널에서 취소한 작업도 실패로 기록됩니다. 실패한 작업은 대기열로 돌아오지 않습니다. GitHub의 실행은 GitHub에서 취소하거나 다시 실행할 때까지 대기 상태로 남고, 다시 실행하면 새 작업이 생깁니다. 사유는 패널의 작업 기록에 표시되며, 기한이 지난 작업은 매니저 로그에도 `job failed`로 남습니다.
- 러너는 저장소 범위의 일회용 JIT 러너입니다. 기본 풀에서는 작업마다 새 컨테이너와 내부 Docker 데몬이 생기므로 `docker build`, `services:`, `container:`를 사용할 수 있습니다. 작업이 끝나면 컨테이너와 내부 이미지, 볼륨, 작업 디렉터리가 삭제됩니다. 풀에서 `cache`를 켰다면 저장소별 캐시 볼륨만 남고, 정리 보존 기간 동안 쓰이지 않으면 삭제됩니다.
- 매니저 로그는 `docker compose logs -f manager`로 확인합니다. 작업 단계 출력은 평소처럼 GitHub 워크플로에 남습니다. 소스 설치에서 새 GitHub 러너 버전을 적용하려면 `docker build --pull -t runquill-runner:local runner`를 다시 실행합니다. 이미 실행 중인 작업은 사용하던 이미지로 마무리됩니다. `RUNQUILL_LOG_FORMAT`을 `text` 또는 `json`으로 설정하면 구조화된 로그를 남기고, `RUNQUILL_LOG_LEVEL`은 `debug`, `info`, `warn`, `error` 중 하나로 설정합니다. 형식을 지정하지 않으면 기존 형태의 로그를 출력합니다. 잘못된 값을 넣으면 명령이 상태 코드 2로 종료합니다.
- 8080 포트의 `GET /healthz`는 대기열 DB가 응답하면 HTTP 200을 반환합니다. `GET /readyz`는 여기에 더해 최근 2분 안에 스케줄링이 성공했고 GitHub App 인증 실패가 없어야 준비 상태로 봅니다. 그렇지 않으면 HTTP 503과 함께 `scheduler`가 `starting`, `failing`, `stalled` 중 하나인 JSON을 반환하며, `database`와 `github_auth`에 나머지 점검 결과가 표시됩니다. 오류 상세는 매니저 로그에만 남습니다.
- 서버당 매니저 하나를 실행합니다. 원래 Python 버전에서 올라오는 경우를 포함해 업그레이드할 때는 Compose의 `queue` 볼륨을 유지합니다. 매니저를 재시작해도 대기열과 현재 러너 기록을 이어받습니다.
- 매니저를 시작할 때와 매번 검색을 마치고 `RUNQUILL_POLL_INTERVAL`(기본 `30s`)이 지난 뒤 GitHub API에서 대기 작업을 찾습니다. 폴링은 접근 가능한 저장소의 대기 중·실행 중 워크플로를 최근 활동 순으로 확인하며, 요청됨·대기·보류 상태는 저장소 목록이 유지되는 동안 성공한 네 번의 조회 안에 순환 확인합니다. 웹훅과 폴링으로 같은 GitHub 작업을 받으면 큐 기록 하나를 공유합니다. 이 값을 `0`으로 설정하면 웹훅만 사용합니다.
- 작업별 타이밍에는 첫 발견 경로, 러너 등록 성공, GitHub가 제공한 작업 시작 시각이 기록됩니다. 발견 지연은 GitHub가 작업 생성 시각을 제공한 경우에만 계산합니다. 대기 시간은 발견부터 첫 등록까지, 아직 대기 중이면 지금까지 기다린 시간입니다. 값이 없으면 `status --json`에서 생략합니다.

### 업그레이드

`--upgrade`를 지원하는 릴리스에서는 새 설치 스크립트를 받아 기존 설치 디렉터리를 지정합니다. `--dir`의 기본값은 `./runquill`입니다.

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --upgrade --dir "$HOME/runquill"
```

설치 스크립트는 체크섬을 검증하고 바뀐 릴리스 파일을 교체합니다. 이전 파일은 `.previous/`에 보관합니다. 마지막 업그레이드를 되돌리려면 `cp -p .previous/* .`를 실행합니다. `--upgrade`는 릴리스 파일만 다루며 `--version`, `--dir`, `--yes`, `--no-install`과 함께 쓸 수 있습니다.

설치 스크립트는 아무것도 재시작하지 않습니다. `.env`에 릴리스 이미지가 지정되어 있으면 새로 설정할 `RUNQUILL_MANAGER_IMAGE`와 `RUNQUILL_RUNNER_IMAGE` 값과 함께, 평소 쓰는 Compose 파일로 실행할 이미지 다운로드, `docker compose up -d --no-build`, `doctor` 명령을 출력합니다. 새 러너 이미지는 매니저가 `local`과 `ssh://` 호스트에 받습니다. 아웃바운드 워커에서는 새 이미지를 받아 두고 `runquill worker` 실행 파일도 업그레이드합니다. K3s 설치에서는 대신 `kubectl apply -f runquill-k3s.yaml`을 안내합니다.

`--upgrade`가 없는 릴리스나 수동 설치에서는 `.env`의 두 이미지 태그를 새 릴리스로 바꾸고 [매니저 시작](#매니저-시작)의 다운로드 명령을 다시 실행합니다.

### 매니저 설정 추가

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

`docker compose -f compose.yaml -f compose.settings.yaml up -d`로 적용합니다. 다른 오버레이도 함께 전달하고, 릴리스 이미지를 쓴다면 `--no-build`를 추가합니다. 예약 백업에는 백업 디렉터리 마운트와 암호 파일도 필요하므로 [백업 가이드](docs/backups.md#compose)의 오버레이를 사용합니다. 탄력적 한도 변수는 [탄력적 한도 가이드](docs/elastic-limits.md#compose)에 있습니다. 비공개 레지스트리에서 받으려면 [비공개 레지스트리](docs/runner-images.md#private-registries)처럼 Docker 로그인 정보를 매니저에 마운트하고 `DOCKER_CONFIG`를 설정합니다. 매니저 이미지에는 Docker CLI만 있으므로 Compose에서 `RUNQUILL_CONTAINER_ENGINE=podman`은 쓸 수 없습니다. k3s에서는 매니저가 `runquill-config` Secret의 모든 변수를 읽으므로 `.env`에 추가하고 Secret을 다시 만든 뒤 Deployment를 재시작합니다.

### 알림 보내기

`RUNQUILL_NOTIFY_URL`에 Slack 같은 서비스의 수신 웹훅 주소를 설정하면 운영 알림을 받습니다. 많은 서비스가 주소에 토큰을 넣으므로 이 주소는 비밀값으로 다룹니다. Runquill은 주소를 로그에 남기지 않습니다. 주소는 HTTPS여야 하며 HTTP는 루프백 호스트에만 허용합니다. 다른 호스트로의 리다이렉트는 따르지 않습니다.

| 이벤트 | 보내는 시점 |
| --- | --- |
| `queue_stalled`, `queue_recovered` | 러너 없이 대기 중인 가장 오래된 작업이 `RUNQUILL_NOTIFY_STALL_AFTER`(기본 `10m`)보다 오래 기다릴 때, 그리고 대기 시간이 기준 아래로 내려갈 때 |
| `host_offline`, `host_recovered` | Docker 호스트, 아웃바운드 워커, Kubernetes 백엔드의 상태를 조회하지 못할 때, 그리고 다시 조회될 때 |
| `job_failed` | 24시간 안에 시작하지 못한 작업을 Runquill이 실패로 기록할 때. 패널에서 취소한 작업은 알리지 않습니다. |

저장소별 한도를 설정한 경우 이미 실행 중인 러너가 있는 저장소의 작업은 대기 시간 계산에서 빠지므로, 한도 때문에 기다리는 작업만으로는 `queue_stalled`가 발생하지 않습니다. 문제가 계속되면 `RUNQUILL_NOTIFY_COOLDOWN`(기본 `30m`)마다 다시 알립니다. 복구 알림은 해당 문제를 알린 경우에만 한 번 보냅니다.

`RUNQUILL_NOTIFY_FORMAT=slack`(기본값)은 `text`와 함께 `source`, `kind`, `host`, `repository`, `job`(러너 라벨), `job_id`, `reason`, `waiting_seconds`, `timestamp`를 보냅니다. `{"text": ...}`만 받는 수신 서비스에는 `text`를 사용합니다. 이벤트마다 최대 세 번, 요청당 10초 제한으로 보내며 네트워크 오류와 HTTP 429, 5xx 응답에서 재시도합니다. 메모리에 최대 64개를 대기시키고, 가득 차면 가장 오래된 이벤트를 버립니다. 매니저가 종료될 때는 남은 알림 전송을 최대 10초 기다립니다. [메트릭](docs/metrics.md)을 켜면 `runquill_notifications_*` 카운터로 전송 결과를 확인할 수 있습니다.

### 대기열 백업과 복구

매니저를 실행한 채 SQLite 대기열을 백업할 수 있습니다. 볼륨을 잃어도 복구할 수 있도록 백업을 볼륨 밖으로 복사합니다.

```sh
umask 077
docker compose exec -T manager runquill backup --output /data/queue-backup.sqlite3
docker compose cp manager:/data/queue-backup.sqlite3 ./queue-backup.sqlite3
```

관리자는 패널에서도 같은 백업을 내려받을 수 있습니다. 다운로드 진행률이 표시되고, 큰 대기열은 최대 30분까지 걸릴 수 있으며, 백업 파일이 준비된 뒤에는 다른 패널 작업을 막지 않습니다.

백업은 버전 정보와 함께 소유자만 읽고 쓸 수 있는 새 파일에 저장합니다. 대기 작업, 러너 리스, 워커 정비 상태, 저장소 조회 기록, 패널에서 저장한 운영 설정이 포함됩니다. 저장소와 작업 정보가 들어 있으므로 비공개로 보관합니다. GitHub App PEM, 배포 설정, OAuth 비밀값은 별도로 백업합니다. 패널 사용자, 세션, 토큰, 감사 기록은 제외됩니다. 복구 후 패널을 사용하려면 `RUNQUILL_PANEL_BOOTSTRAP_ID`에 첫 관리자의 숫자 GitHub 계정 ID를 설정하고 팀 등록과 토큰 발급을 다시 진행합니다.

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

### 백업 예약, 암호화, 업로드

`RUNQUILL_BACKUP_INTERVAL`과 `RUNQUILL_BACKUP_DIR`을 설정하면 백업을 자동으로 만들고, `RUNQUILL_BACKUP_PASSPHRASE` 또는 `RUNQUILL_BACKUP_PASSPHRASE_FILE`을 설정하면 암호화합니다. `runquill backup --seal`로 같은 형식의 암호화 백업을 직접 만들 수 있고, `runquill restore --from`에 암호를 주면 복구합니다. `RUNQUILL_BACKUP_S3_*` 변수를 설정하면 매니저가 백업을 S3 호환 버킷에도 복사하며 `runquill restore --from-s3`로 복구합니다. `RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true`가 없으면 암호화하지 않은 백업은 올리지 않습니다. 실제 버킷으로는 아직 검증하지 않았습니다. [백업 가이드](docs/backups.md)

### 제거

먼저 호스트를 `cordon`하고 실행 중인 작업이 끝나기를 기다립니다. 다시 설치할 수도 있다면 [대기열을 백업](#대기열-백업과-복구)합니다. 설치 디렉터리에서 평소 쓰는 Compose 파일로 매니저를 내립니다. `-v`는 대기 작업, 리스, 패널에서 저장한 설정이 담긴 `queue` 볼륨을 삭제하고, `--rmi all`은 매니저 이미지를 삭제합니다.

```sh
docker compose -f compose.yaml -f compose.panel.yaml down -v --rmi all
```

러너 컨테이너, 캐시 볼륨, 러너 이미지는 Compose가 관리하지 않습니다. SSH 워커와 아웃바운드 워커를 포함한 각 Docker 호스트에서 남은 항목을 삭제합니다. 이미지는 `RUNQUILL_RUNNER_IMAGE`와 풀의 `image`에 지정한 이름, 작업이 고른 러너 이미지 목록의 이미지를 확인합니다.

```sh
docker ps -a --filter label=io.runquill.runner=1
docker rm -f $(docker ps -aq --filter label=io.runquill.runner=1)
docker volume rm $(docker volume ls -q -f label=io.runquill.cache=1)
docker image rm ghcr.io/yakisoba0728/runquill-runner:v0.3.3
```

그다음 `.env`, GitHub App 키, 토큰 파일이 들어 있는 설치 디렉터리를 삭제합니다. 직접 만든 Tailscale Funnel이나 Serve 리스너를 끄고, 아웃바운드 워커의 `runquill worker`도 중지합니다.

설치 스크립트가 호스트에 적용한 변경은 그대로 남습니다. 되돌리려면 `sudo gpasswd -d "$USER" docker`로 사용자를 `docker` 그룹에서 뺍니다. Docker는 배포판별 [Docker 제거 절차](https://docs.docker.com/engine/install/ubuntu/#uninstall-docker-engine)에 따라 삭제하고, `/etc/apt/keyrings/docker.asc`와 `/etc/apt/sources.list.d/docker.sources` 또는 `/etc/yum.repos.d/docker-ce.repo`도 지웁니다.

k3s에서는 `kubectl delete -f runquill-k3s.yaml`로 Runquill 네임스페이스 두 개와 그 안의 매니저, 러너 Pod, Secret, 대기열 PVC를 삭제합니다.

마지막으로 **Settings → Developer settings → GitHub Apps → *해당 App* → Advanced → Delete GitHub App**에서 GitHub App을 삭제합니다. 조직 App은 조직 설정에서 같은 메뉴를 사용합니다. App을 삭제하면 모든 설치에서 제거됩니다. 별도의 패널 OAuth App도 같은 방식으로 삭제하고, 저장소의 **Settings → Actions → Runners**에 남은 `runquill-` 러너가 있으면 삭제합니다. [GitHub App 삭제](https://docs.github.com/en/apps/maintaining-github-apps/deleting-a-github-app)

## 기능 검증 현황

이 문서의 기능은 Go 테스트로 확인하며, CI의 중첩 Docker 테스트와 K3s 수명 주기 점검은 실제 컨테이너를 실행합니다. 다음 기능은 구현되어 있지만 가짜 서버나 기록된 출력으로만 테스트했고, 실제 환경에서는 아직 실행하지 않았습니다.

- Podman과 rootless 컨테이너 엔진
- 실제 S3 버킷으로의 업로드와 복구
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

이 기능에서 문제를 발견하면 이슈로 알려주세요.

## 개발과 릴리스

Go 1.26.11 이상을 사용합니다. 매니저는 네이티브 바이너리로 빌드되며 외부 런타임 의존성은 Docker CLI입니다.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o runquill .
```

러너 Dockerfile은 GitHub Actions 러너 이미지를 다이제스트로 고정합니다. `--build-arg RUNQUILL_RUNNER_BASE=IMAGE`로 호환 이미지에 자체 도구를 추가할 수 있습니다. 다른 기반 이미지는 `--build-arg RUNQUILL_RUNNER_OS=`에 `ubuntu22.04`, `ubuntu26.04`, `debian12`, `debian13` 중 하나를 넣어 빌드합니다. Kubernetes `plain` 모드와 `privileged: false` 풀을 위해 러너 파일은 `/home/runner`에 두고 사용자 UID는 1001로 유지합니다.

러너 이미지를 빌드한 뒤 `RUNQUILL_TEST_DOCKER_IMAGE=runquill-runner:local go test -run '^TestDockerRunner$' -v`로 중첩 Docker 검증을 실행합니다. 로컬 GitHub 리스너 대역으로 Docker 빌드, PostgreSQL, 바인드 마운트, 컨테이너 삭제를 확인합니다.

[CI](.github/workflows/ci.yml)는 amd64와 arm64 Linux에서 Go 테스트와 중첩 Docker 검증을 실행합니다. amd64에서는 임시 k3s 클러스터도 띄워 배정을 멈추면 작업이 대기하고, 재개하면 러너가 시작되며, 작업이 끝나면 러너 Pod와 자격 증명 Secret이 삭제되는지 확인합니다. 관리자와 뷰어에 별도 GitHub 계정을 쓰는 [OAuth 검증](panel_live_oauth_test.go)은 전용 OAuth 앱을 준비해 로컬에서 실행할 수 있습니다.

버전 태그를 올리면 [릴리스 워크플로](.github/workflows/release.yml)가 CI를 실행하고, amd64와 arm64 이미지를 GHCR에 게시하며, 같은 버전의 Linux 바이너리와 체크섬을 릴리스에 첨부합니다. 이 저장소에 게시한 뒤에는 두 아키텍처의 깨끗한 Linux 컨테이너에서 공개 설치 스크립트로 해당 태그를 검증합니다. 처음 게시한 뒤 익명으로 받을 수 있어야 한다면 두 패키지를 공개로 바꿉니다. 워크플로는 저장소 소유자의 네임스페이스에 게시하므로 포크는 자체 이미지 이름을 사용합니다. [GitHub Container registry 접근](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

Copyright © 2026 Runquill contributors. [Apache License 2.0](LICENSE)
