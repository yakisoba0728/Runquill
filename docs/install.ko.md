# Runquill 설치

[Runquill README](../README.ko.md) · [English](install.md)

## 공개 릴리스 설치

cgroup v2를 쓰는 amd64 또는 arm64 Linux 서버가 필요합니다. 그 서버에서 설치 스크립트를 받아 실행하면 호스트 준비, 릴리스 다운로드, 설치 마법사 실행까지 이어집니다.

```sh
cd ~ && curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh && sh install.sh --wizard
```

Docker 호스트 묶음마다 매니저 하나를 실행합니다. 폴링은 GitHub에 HTTPS로 연결할 수 있으면 공유기 뒤에서도 동작합니다.

위 명령은 `$HOME/runquill`에 설치합니다. 이미 설치한 서버의 버전을 올릴 때는 [업그레이드](operations.ko.md#업그레이드)를 사용하세요. 자동 Tailscale 연결을 건너뛰려면 `--no-tailscale`을 붙입니다. [Cloudflare Tunnel 가이드](cloudflare-tunnel.ko.md)는 별도 HTTPS 주소로 패널과 웹훅을 연결하는 절차를 설명합니다.

| 연결 방식 | 설정 |
| --- | --- |
| 폴링과 로컬 또는 SSH 패널 | `--wizard --no-tailscale`로 실행하고 공개 웹훅 URL을 비워 둡니다. |
| Tailscale Serve와 Funnel | 기본 마법사를 사용하고 tailnet의 HTTPS 및 Funnel 권한을 확인합니다. |
| 내 도메인의 공개 HTTPS | [Cloudflare Tunnel](cloudflare-tunnel.ko.md) 또는 [HTTPS 프록시](#기존-github-app-또는-웹훅-사용)를 구성하고 마법사에 URL을 입력합니다. |

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

설치 프로그램은 Tailscale을 준비하고 브라우저 로그인을 시작할 수 있습니다. Docker Compose에서 tailnet이 허용하면 마법사가 Serve로 패널을 비공개 연결하고 Funnel로 GitHub 웹훅을 받습니다. 폴링도 계속 사용할 수 있습니다. [브라우저 설치 가이드](browser-setup.ko.md)

마법사는 `127.0.0.1`에서 접속을 받습니다. SSH로 접속하면 내 컴퓨터에서 실행할 포트 포워딩 명령을 출력합니다.

```text
ssh -L 18080:127.0.0.1:18080 alice@203.0.113.10
```

이 SSH 세션을 열어 둔 채 출력된 `http://127.0.0.1:18080/?key=…` 주소를 내 컴퓨터의 브라우저로 엽니다. 양쪽 포트 번호는 같아야 합니다. 주소의 `key`는 이번 설치 세션의 비밀값입니다. 다른 호스트 이름이나 NAT 주소로 서버에 접속한다면 `ssh` 명령에 그 주소를 씁니다.

마법사에 **배포 완료**가 표시되고 운영 패널이 열리면 서버에서 설치 마법사를 실행한 터미널에서 `Ctrl+C`로 종료할 수 있습니다. 매니저 컨테이너는 백그라운드에서 계속 실행됩니다. 패널을 SSH 포워딩으로 열었다면 패널용 SSH 세션은 사용하는 동안 유지하세요.

### 터미널에서 설치

브라우저 대신 터미널에서 설정하려면 `--wizard`를 빼고 실행합니다.

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --dir "$HOME/runquill"
```

1. 설치 프로그램이 출력한 브라우저 링크를 열어 비공개 GitHub App을 만듭니다. 개인 계정에 **All repositories**로 설치합니다.
2. 설치 프로그램이 Compose를 시작하고 `Preflight passed.`를 출력할 때까지 기다립니다.
3. 내 비공개 저장소에서 [첫 작업을 실행합니다](workflows.ko.md#첫-작업-확인).

설치 스크립트는 다운로드 체크섬을 확인하고 같은 버전의 이미지를 사용합니다. `--version v0.4.0`로 공개 버전을 지정하거나 `--download-only`로 바이너리와 배포 파일을 먼저 받을 수 있습니다.

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

두 파일을 저장한 뒤 중단했다면 `setup`을 다시 실행해 App 설치를 이어갈 수 있습니다. 생성된 설정은 GitHub API 폴링을 사용하므로 공개 HTTPS 주소 없이도 동작합니다.

[GitHub App 매니페스트](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest) · [GitHub App 설치](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app)

### 선택한 저장소만 연결

새 설치에서 일부 비공개 저장소만 연결하려면 아래 옵션을 사용합니다.

```sh
sh install.sh --dir "$HOME/runquill" --repository-scope selected
# 설치 디렉터리에 실행 파일이 있는 경우
./runquill setup --repository-scope selected
```

GitHub에서 **Only select repositories**를 고르고 사용할 비공개 저장소를 선택합니다. 저장소 목록은 GitHub가 관리하고, `setup`은 `.env`에 `RUNQUILL_REPOSITORY_SCOPE=selected`를 기록합니다. 저장소 추가·제거는 GitHub App 설치 설정에서 합니다. 폴링을 켰다면 매니저를 재시작하지 않아도 다음 폴링에서 새로 승인한 저장소의 작업을 발견할 수 있습니다. 웹훅 전용 설정에서는 GitHub가 보내는 이벤트를 통해 작업을 감지합니다. 첫 작업을 실행하기 전 매니저 환경에서 `runquill doctor --repo OWNER/REPO`로 접근을 확인하세요. [GitHub App 저장소 접근 변경](https://docs.github.com/en/apps/using-github-apps/reviewing-and-modifying-installed-github-apps)

기존 앱의 설치 범위를 바꾸려면 GitHub 설정을 변경한 뒤 매니저 설정의 `RUNQUILL_REPOSITORY_SCOPE`도 `all` 또는 `selected`로 맞춥니다. Compose 매니저를 다시 만들거나 k3s 설정 Secret을 갱신하고 Deployment를 재시작하세요. 이 값을 생략하면 기본값은 `all`입니다.

### GitHub 조직 연결

조직 소유자 계정으로 설치를 진행한 뒤 생성된 GitHub App을 그 조직에 설치합니다. 매니저 한 인스턴스는 GitHub 계정 하나를 관리합니다. 계정이나 러너 등록 수준을 변경할 때는 새 디렉터리와 대기열을 사용합니다.

```sh
./runquill setup --organization YOUR_ORG --repository-scope selected
```

Compose용 공개 설치 스크립트에도 같은 옵션을 전달할 수 있습니다. k3s에서는 `setup`에 `--k3s`를 추가합니다. 기본적으로 작업을 요청한 저장소에 러너를 등록하며, 저장소 **Actions 읽기**와 **Administration 읽기·쓰기** 권한을 사용합니다. 기존 App을 사용한다면 `.env`에 `RUNQUILL_OWNER=YOUR_ORG`와 `RUNQUILL_OWNER_TYPE=organization`을 설정합니다. 조직의 패널 설정에는 실제 관리자의 GitHub 사용자 ID를 입력합니다. [GitHub App 등록](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)

조직 러너 그룹을 사용하려면 **선택한 저장소**만 허용하고 공개 저장소 접근을 끈 조직 소유 그룹을 만듭니다. 그룹의 GitHub 워크플로 단위 제한은 사용하지 않습니다. App이 접근할 수 있는 비공개 저장소만 그룹에 추가하고 `setup` 또는 설치 스크립트에 `--runner-group-id ID`를 전달합니다. [`RUNQUILL_ALLOW_PUBLIC_REPOSITORIES=true`](workflows.ko.md#공개-저장소에서-작업-실행)를 설정했다면 공개 저장소를 허용하고 공개 저장소만 넣은 별도 그룹을 사용합니다. 기존 App은 `.env`에 `RUNQUILL_RUNNER_GROUP_ID=ID`를 설정하고 조직의 **Self-hosted runners 읽기·쓰기** 권한을 부여합니다. Runquill은 러너를 등록할 때마다 먼저 그룹 정책과 저장소 접근을 확인합니다. 워크플로에서 러너 그룹을 지정한다면 설정한 그룹 이름을 쓰고, 작업별 라벨은 그대로 유지합니다. GitHub는 그룹 접근과 일치하는 라벨로 조직 러너에 작업을 배정하므로, 서로의 워크플로를 신뢰하는 저장소끼리만 그룹을 공유하세요. [GitHub 러너 그룹](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access) · [조직 러너 API](https://docs.github.com/en/rest/actions/self-hosted-runners#create-configuration-for-a-just-in-time-runner-for-an-organization)

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

### 기존 GitHub App 또는 웹훅 사용

앱을 수동으로 만들 때는 **Only on this account**를 선택하고 **Actions → Read-only**와 **Administration → Read and write** 권한을 부여합니다. 발급한 PEM 키를 보관하고 앱을 개인 계정에 설치합니다. [환경 변수 예제](../.env.example)를 `.env`로 복사해 GitHub 로그인, App ID, PEM 키 경로를 입력하고 두 파일의 권한을 `0600`으로 설정합니다. **Only select repositories**로 설치했다면 `RUNQUILL_REPOSITORY_SCOPE=selected`를 설정합니다. 폴링으로 연결할 때는 앱의 웹훅을 끄고 `RUNQUILL_WEBHOOK_SECRET`을 비워둡니다. [GitHub App 설치](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app) · [저장소 러너 JIT API](https://docs.github.com/en/rest/actions/self-hosted-runners#create-configuration-for-a-just-in-time-runner-for-a-repository)

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
