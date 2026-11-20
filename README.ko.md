# Runquill

[English](README.md) · 한국어

Runquill은 비공개 저장소의 GitHub Actions 작업을 내 Linux 서버에서 실행합니다. GitHub App으로 웹훅이나 폴링을 통해 작업을 받고, 지정한 작업마다 일회용 러너를 띄운 뒤 완료되면 정리합니다.

[빠른 시작](#빠른-시작) · [첫 작업](#첫-작업-확인) · [샘플 패널](https://yakisoba0728.github.io/Runquill/) · [릴리스](https://github.com/yakisoba0728/Runquill/releases)

## 빠른 시작

### 공개 릴리스 설치

cgroup v2를 쓰는 amd64 또는 arm64 Linux 서버에서 실행하세요.

```sh
cd ~ && curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh && sh install.sh --wizard
```

출력된 URL을 엽니다. SSH로 접속했다면 내 컴퓨터에서 안내된 포트 포워딩 명령을 먼저 실행합니다. 마법사에서 GitHub를 연결하고 **전체 저장소** 또는 **선택한 저장소**에 App을 설치한 뒤 배포합니다. 설치 URL은 비공개로 보관하세요. 폴링이나 별도 터널을 쓸 때는 `--no-tailscale`을 붙여 Tailscale 자동 설정을 건너뛸 수 있습니다.

[설치 가이드](docs/install.ko.md) · [설치 마법사](docs/browser-setup.md) · [Cloudflare Tunnel](docs/cloudflare-tunnel.ko.md)

### 첫 작업 확인

[첫 작업 워크플로](examples/runquill-first-job.yml)를 App이 접근할 수 있는 비공개 저장소의 `.github/workflows/`에 복사합니다. 기본 브랜치에 커밋한 뒤 **Actions → Runquill first job → Run workflow**를 누릅니다. **Show the runner**에 `runquill-`로 시작하는 이름이 나오고, 작업이 끝나면 패널이나 매니저 로그에 러너 정리 기록이 남습니다.

기존 워크플로에서 Runquill로 실행할 작업의 `runs-on`은 이렇게 바꿉니다.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-build', github.run_id, github.run_attempt) }}
```

작업마다 `build`, `test`처럼 다른 접미사를 붙입니다. 매트릭스 작업은 `strategy.job-index`도 추가합니다. 같은 계정에 App을 **전체 저장소** 범위로 설치했다면 다른 비공개 저장소에 Runquill 시크릿이나 웹훅을 따로 추가할 필요가 없습니다.

[워크플로와 매트릭스 예제](docs/workflows.ko.md) · [기존 워크플로 이전](docs/workflow-migration.md)

## 실행 환경 선택

| 실행 환경 | 러너 |
| --- | --- |
| Docker Compose | 작업당 컨테이너 하나. 권한 있는 풀에서 Docker 빌드, 서비스 컨테이너, 작업 컨테이너를 실행할 수 있습니다. |
| K3s `plain` | 작업당 제한된 Pod 하나. Pod에서 직접 실행 가능한 단계를 지원합니다. |
| K3s `dind` | 작업당 권한 있는 Docker-in-Docker Pod 하나. Docker 빌드, 서비스 컨테이너, 작업 컨테이너를 지원합니다. |

워커 그룹당 매니저 하나를 실행하고 SQLite 대기열은 영구 스토리지에 보관합니다. 신뢰하는 작업 전용 호스트에서 운영하세요. 매니저는 Docker 데몬을 제어할 수 있고, GitHub App 키에는 설치 범위 안의 저장소에 접근할 수 있는 권한이 있습니다.

[Docker 워커와 풀](docs/workers.ko.md) · [K3s 배포](deploy/k3s/README.md)

## 문서

| 할 일 | 문서 |
| --- | --- |
| 설치, 저장소 접근 범위 선택, 조직 App 등록 | [설치](docs/install.ko.md) |
| 공개 HTTPS로 패널 열기와 서명된 웹훅 받기 | [Cloudflare Tunnel](docs/cloudflare-tunnel.ko.md), [Tailscale와 기타 프록시](docs/install.ko.md#기존-github-app-또는-웹훅-사용) |
| 작업 실행, 기존 워크플로 이전, 러너 OS 선택 | [워크플로](docs/workflows.ko.md), [마이그레이션](docs/workflow-migration.md), [러너 이미지와 캐시](docs/runner-images.md) |
| 워커 추가와 용량 설정 | [워커와 풀](docs/workers.ko.md), [아웃바운드 워커](docs/outbound-workers.md), [컨테이너 엔진](docs/container-engines.md), [탄력적 한도](docs/elastic-limits.md) |
| 상태 확인, 문제 해결, 업그레이드, 백업 | [운영](docs/operations.ko.md), [웹 패널](docs/panel.md), [백업](docs/backups.md), [메트릭](docs/metrics.md) |
| 스크립트나 코딩 에이전트로 상태 조회 | [원격 CLI](docs/remote-cli.md), [MCP](docs/mcp.md), [패널 API](api/openapi.yaml) |
| 저장소 개발과 기여 | [AGENTS.md](AGENTS.md), [개발과 릴리스](docs/development.ko.md) |

[Apache License 2.0](LICENSE)
