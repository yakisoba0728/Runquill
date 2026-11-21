# Runquill 개발, 테스트 및 릴리스

[Runquill README](../README.ko.md) · [English](development.md)

## 저장소 구조

| 경로 | 역할 |
| --- | --- |
| `main.go` | `go build .`, `go run .`에서 사용하는 실행 진입점입니다. |
| `internal/app/` | CLI, 매니저, 대기열, 러너 백엔드, GitHub 연동, 패널 API와 통합 테스트를 둡니다. |
| `internal/workflow/`, `internal/route/` | 오프라인 워크플로 검사와 변환, 스케줄러도 쓰는 러너 라벨 규칙을 담당합니다. |
| `internal/webui/` | 바이너리에 포함되는 운영 패널과 설치 마법사 파일입니다. 릴리스 데모에는 패널 파일을 사용합니다. |
| `internal/backupseal/` | 암호화된 백업 형식과 스트리밍 암·복호화를 담당합니다. |
| `runner/`, `deploy/`, `api/`, `examples/` | 러너 이미지, K3s 매니페스트, OpenAPI 명세, 워크플로 예제입니다. 설치 스크립트와 Compose 파일은 루트에 둡니다. |

## 개발과 릴리스

Go 1.26.11 이상을 사용합니다. 매니저는 네이티브 바이너리로 빌드되며 외부 런타임 의존성은 Docker CLI입니다.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o runquill .
```

매니저와 워커는 Linux에서만 실행하며 테스트도 Linux에서 실행합니다. Windows와 macOS 빌드는 [원격 CLI](remote-cli.md)와 `runquill workflow check`용이며 CI가 두 OS의 교차 컴파일을 확인합니다.

러너 Dockerfile은 GitHub Actions 러너 이미지를 다이제스트로 고정합니다. `--build-arg RUNQUILL_RUNNER_BASE=IMAGE`로 호환 이미지에 자체 도구를 추가할 수 있습니다. 다른 기반 이미지는 `--build-arg RUNQUILL_RUNNER_OS=`에 `ubuntu22.04`, `ubuntu26.04`, `debian12`, `debian13` 중 하나를 넣어 빌드합니다. Kubernetes `plain` 모드와 `privileged: false` 풀을 위해 러너 파일은 `/home/runner`에 두고 사용자 UID는 1001로 유지합니다.

러너 이미지를 빌드한 뒤 `RUNQUILL_TEST_DOCKER_IMAGE=runquill-runner:local go test ./internal/app -run '^TestDockerRunner$' -v`로 중첩 Docker 검증을 실행합니다. 로컬 GitHub 리스너 대역으로 Docker 빌드, PostgreSQL, 바인드 마운트, 컨테이너 삭제를 확인합니다.

[CI](../.github/workflows/ci.yml)는 amd64와 arm64 Linux에서 Go 테스트와 중첩 Docker 검증을 실행합니다. amd64에서는 임시 k3s 클러스터도 띄워 배정을 멈추면 작업이 대기하고, 재개하면 러너가 시작되며, 작업이 끝나면 러너 Pod와 자격 증명 Secret이 삭제되는지 확인합니다. 관리자와 뷰어에 별도 GitHub 계정을 쓰는 [OAuth 검증](../internal/app/panel_live_oauth_test.go)은 전용 OAuth 앱을 준비해 로컬에서 실행할 수 있습니다.

버전 태그를 올리면 [릴리스 워크플로](../.github/workflows/release.yml)가 CI를 실행하고, amd64와 arm64 이미지를 GHCR에 게시하며, 같은 버전의 Linux 바이너리와 체크섬을 릴리스에 첨부합니다. 이 저장소에 게시한 뒤에는 두 아키텍처의 깨끗한 Linux 컨테이너에서 공개 설치 스크립트로 해당 태그를 검증합니다. 처음 게시한 뒤 익명으로 받을 수 있어야 한다면 두 패키지를 공개로 바꿉니다. 워크플로는 저장소 소유자의 네임스페이스에 게시하므로 포크는 자체 이미지 이름을 사용합니다. [GitHub Container registry 접근](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

## 설치와 업그레이드 검증

Linux에서 셸 문법과 호스트 준비 테스트를 실행합니다. 오프라인 테스트는 가짜 파일시스템과 대체 명령을 사용합니다.

```sh
sh -n install.sh
sh .github/scripts/test-install-offline.sh dash
sh .github/scripts/test-install-offline.sh bash
```

Docker와 Compose가 있다면 CI와 동일한 버전 조합으로 업그레이드를 검증할 수 있습니다.

```sh
python3 .github/scripts/smoke-docker-upgrade.py v0.3.4 v0.4.0
```

이 검증은 이전 릴리스에서 패널 설정과 대기 작업을 만들고 같은 대기열 볼륨으로 업그레이드합니다. 그런 다음 별도의 Docker 워커를 등록해 원래 작업의 완료와 러너 정리를 확인합니다. 릴리스 워크플로는 게시 후 직전 공개 릴리스와 새 태그를 비교합니다. 이 업그레이드 테스트는 로컬 GitHub API 대역을 사용합니다.

## 실제 GitHub App 웹훅 검증

기존 배포를 검사하려면 [운영 가이드](operations.ko.md#웹훅만으로-작업-수신-확인)를 따릅니다. [실제 웹훅 스모크 테스트](../.github/scripts/smoke-live-webhook.py)는 검증용 매니저, 별도 Docker 워커, HTTPS 릴레이를 임시로 만듭니다. Docker, Python 3, OpenSSL, curl이 설치된 Linux에서 비공개 테스트 저장소의 기본 브랜치를 대상으로 수동 실행합니다. 먼저 Runquill 소스에서 `docker build -t runquill-live-manager:ci .`로 매니저를 빌드합니다.

개인 계정에 전용 GitHub App을 만들고 **선택한 저장소**로 테스트 저장소 하나에만 설치합니다. 테스트 App에 초기 웹훅 URL을 설정하고 HTTPS 검증을 켭니다. 본문 형식은 JSON을 선택하고 **Workflow job** 이벤트를 구독합니다. 다른 매니저나 러너가 이 저장소의 작업을 가져가지 않도록 합니다. 테스트 저장소의 기본 브랜치에 `.github/workflows/runquill-webhook-job.yml`을 추가합니다. `workflow_dispatch`에 필수 문자열 입력 `nonce`를 정의하고, [작업별 Runquill 라벨](workflows.ko.md#워크플로에서-러너-선택)을 쓰는 짧은 작업 하나를 둡니다.

`GITHUB_REPOSITORY=OWNER/REPO`, `GITHUB_REF=refs/heads/DEFAULT_BRANCH`와 대상 Actions를 조회하고 실행할 수 있는 `GITHUB_TOKEN`을 전달합니다. `RUNQUILL_APP_ID`, `RUNQUILL_APP_PRIVATE_KEY`, `RUNQUILL_WEBHOOK_SECRET`은 보호된 CI 시크릿이나 변수에서 공급합니다. Runquill 소스에서 `python3 .github/scripts/smoke-live-webhook.py`를 실행합니다. 폴링을 끈 상태에서 서명된 대기·완료 웹훅과 Docker 워커 실행, 컨테이너 삭제, 러너 등록 해제를 확인합니다. 스크립트가 테스트 App의 웹훅 URL을 임시로 바꾸고 종료 시 복원합니다. 중단에 대비해 같은 러너의 항상 실행되는 단계에서 `--restore-webhook`으로 복원할 수도 있습니다.
