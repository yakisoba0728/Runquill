# Runquill 개발, 테스트 및 릴리스

[Runquill README](../README.ko.md) · [English](development.md)

## 저장소 구조

| 경로 | 역할 |
| --- | --- |
| `main.go` | `go build .`, `go run .`에서 사용하는 실행 진입점입니다. |
| `internal/app/` | CLI, 매니저, 대기열, 러너 백엔드, GitHub 연동, 패널 API와 통합 테스트를 둡니다. |
| `internal/workflow/`, `internal/route/` | 오프라인 워크플로 검사와 변환, 스케줄러도 쓰는 러너 라벨 규칙을 담당합니다. |
| `internal/webui/` | 바이너리에 포함되는 설치 마법사와 패널 파일입니다. v2 소스는 `panel/`, 커밋한 빌드 결과는 `panelv2/`, 이전 패널은 `panelweb/`에 있습니다. |
| `internal/backupseal/` | 암호화된 백업 형식과 스트리밍 암·복호화를 담당합니다. |
| `runner/`, `deploy/`, `api/`, `examples/` | 러너 이미지, K3s 매니페스트, OpenAPI 명세, 워크플로 예제입니다. 설치 스크립트와 Compose 파일은 루트에 둡니다. |

## 개발과 릴리스

Go 1.26.11 이상을 사용합니다. 매니저는 네이티브 바이너리로 빌드되며 외부 런타임 의존성은 Docker CLI입니다.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o runquill .
```

매니저와 워커는 Linux에서만 실행하며 테스트도 Linux에서 실행합니다. Windows와 macOS 빌드는 [원격 CLI](remote-cli.ko.md)와 `runquill workflow check`용이며 CI가 두 OS의 교차 컴파일을 확인합니다.

러너 Dockerfile은 GitHub Actions 러너 이미지와 모든 OS 기반 이미지를 다이제스트로 고정하고, 매니저 Dockerfile도 Go와 Docker CLI 이미지를 같은 방식으로 고정합니다. `--build-arg RUNQUILL_RUNNER_BASE=IMAGE`로 호환 이미지에 자체 도구를 추가할 수 있습니다. 다른 기반 이미지는 `--build-arg RUNQUILL_RUNNER_OS=`에 `ubuntu22.04`, `ubuntu26.04`, `debian12`, `debian13` 중 하나를 넣어 빌드합니다. Kubernetes `plain` 모드와 `privileged: false` 풀을 위해 러너 파일은 `/home/runner`에 두고 사용자 UID는 1001로 유지합니다.

러너 이미지를 빌드한 뒤 `RUNQUILL_TEST_DOCKER_IMAGE=runquill-runner:local go test ./internal/app -run '^TestDockerRunner$' -v`로 중첩 Docker 검증을 실행합니다. 로컬 GitHub 리스너 대역으로 Docker 빌드, PostgreSQL, 바인드 마운트, 컨테이너 삭제를 확인합니다. 같은 이미지로 `-run '^TestDockerWarmRunner$'`를 실행하면 두 러너 모드에서 [미리 띄운 러너](workers.ko.md#러너를-미리-띄워-두기)를 시작하고 각각에 JIT 설정을 넘겨 봅니다.

[CI](../.github/workflows/ci.yml)는 Go 점검을 amd64와 arm64 네이티브 이미지 빌드, 중첩 Docker 검증과 병렬로 실행합니다. amd64 런타임 점검에서는 K3s도 실행해 호스트 정비, 러너 배정, Pod와 Secret 정리를 확인합니다. 러너 이미지나 그 빌드 워크플로가 바뀌면 추가 OS 빌드 4개도 실행합니다. 관리자와 뷰어에 별도 GitHub 계정을 쓰는 [OAuth 검증](../internal/app/panel_live_oauth_test.go)은 전용 OAuth 앱을 준비해 실행할 수 있습니다.

버전 태그를 올리면 [릴리스 워크플로](../.github/workflows/release.yml)가 아키텍처와 OS별 네이티브 작업에서 각 이미지를 한 번씩 빌드합니다. 테스트한 이미지를 GHCR에 게시하고, 단독 실행 파일은 같은 매니저 이미지에서 꺼냅니다. 그런 다음 두 아키텍처에서 공개 설치 스크립트로 게시된 릴리스를 검증합니다. 포크는 자기 GitHub 소유자의 네임스페이스에 게시합니다. 익명으로 받을 수 있게 하려면 두 패키지를 공개로 바꿉니다. [GitHub Container registry 접근](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

## 의존성 업데이트

[Dependabot](../.github/dependabot.yml)은 GitHub Actions, Go 모듈, 패널 npm 패키지, 두 Dockerfile의 Docker 기반 이미지마다 매주 묶음 풀 리퀘스트 하나를 엽니다. 고정한 태그의 다이제스트는 갱신하지만 러너 OS 릴리스, Go 마이너 버전, Docker CLI 메이저 버전을 바꾸는 태그 변경은 건너뜁니다. 이런 변경은 `releaseRunnerOS`, 워크플로, 문서와 함께 직접 합니다.

npm 풀 리퀘스트에서는 브랜치를 체크아웃해 `internal/webui/panel`에서 `npm ci && npm run build`를 실행하고, 다시 빌드한 `internal/webui/panelv2/` 결과물을 푸시합니다. 그러지 않으면 CI의 커밋된 에셋 검사가 실패합니다.

Docker 다이제스트를 직접 갱신하려면 고정한 태그마다 다이제스트를 조회해 해당 `FROM` 줄의 `sha256`을 바꿉니다.

```sh
docker buildx imagetools inspect ubuntu:22.04 --format '{{json .Manifest.Digest}}'
```

## 웹 패널 빌드와 테스트

[`.node-version`](../internal/webui/panel/.node-version)에 고정된 버전을 사용합니다. 현재 Node 26.13.1입니다. Go 빌드는 커밋된 결과를 바이너리에 포함하므로 Node가 필요 없습니다.

```sh
cd internal/webui/panel
npm ci
npm run typecheck
npm test
npm run build
npm run size
npx playwright install
npx playwright test
```

패널 소스가 바뀌면 `internal/webui/panelv2/`의 결과도 함께 커밋합니다. CI는 다시 빌드한 결과가 커밋된 파일과 정확히 일치하는지 확인하고, `go run . panel --demo`를 띄워 Chromium, Firefox, WebKit에서 Playwright를 실행합니다. Ubuntu에서는 `npx playwright install --with-deps`로 시스템 라이브러리도 설치합니다. GitHub Pages 샘플과 릴리스의 `runquill-panel-demo.tar.gz`는 같은 v2 결과를 사용합니다. 릴리스 데모를 열려면 압축을 풀고 `panelv2/` 디렉터리를 HTTP로 서빙합니다. [패널 개발 안내](../internal/webui/panel/README.md)

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

이 검증은 이전 릴리스에서 패널 설정과 대기 작업을 만들고 같은 대기열 볼륨으로 업그레이드합니다. 그런 다음 별도의 Docker 워커를 등록해 원래 작업의 완료와 러너 정리를 확인합니다. 릴리스 워크플로는 게시 후 직전 공개 릴리스에서 새 태그로의 업그레이드를 검증합니다. 이 업그레이드 테스트는 로컬 GitHub API 대역을 사용합니다.

## 실제 GitHub App 웹훅 검증

기존 배포를 검사하려면 [운영 가이드](operations.ko.md#웹훅만으로-작업-수신-확인)를 따릅니다. [실제 웹훅 스모크 테스트](../.github/scripts/smoke-live-webhook.py)는 검증용 매니저, 별도 Docker 워커, HTTPS 릴레이를 임시로 만듭니다. Docker, Python 3, OpenSSL, curl이 설치된 Linux에서 비공개 테스트 저장소의 기본 브랜치를 대상으로 수동 실행합니다. 먼저 Runquill 소스에서 `docker build -t runquill-live-manager:ci .`로 매니저를 빌드합니다.

개인 계정에 전용 GitHub App을 만들고 **선택한 저장소**로 테스트 저장소 하나에만 설치합니다. 테스트 App에 초기 웹훅 URL을 설정하고 HTTPS 검증을 켭니다. 본문 형식은 JSON을 선택하고 **Workflow job** 이벤트를 구독합니다. 다른 매니저나 러너가 이 저장소의 작업을 가져가지 않도록 합니다. 테스트 저장소의 기본 브랜치에 `.github/workflows/runquill-webhook-job.yml`을 추가합니다. `workflow_dispatch`에 필수 문자열 입력 `nonce`를 정의하고, [작업별 Runquill 라벨](workflows.ko.md#워크플로에서-러너-선택)을 쓰는 짧은 작업 하나를 둡니다.

`GITHUB_REPOSITORY=OWNER/REPO`, `GITHUB_REF=refs/heads/DEFAULT_BRANCH`와 대상 Actions를 조회하고 실행할 수 있는 `GITHUB_TOKEN`을 전달합니다. `RUNQUILL_APP_ID`, `RUNQUILL_APP_PRIVATE_KEY`, `RUNQUILL_WEBHOOK_SECRET`은 보호된 CI 시크릿이나 변수에서 공급합니다. Runquill 소스에서 `python3 .github/scripts/smoke-live-webhook.py`를 실행합니다. 폴링을 끈 상태에서 서명된 대기·완료 웹훅과 Docker 워커 실행, 컨테이너 삭제, 러너 등록 해제를 확인합니다. 스크립트가 테스트 App의 웹훅 URL을 임시로 바꾸고 종료 시 복원합니다. 중단에 대비해 같은 러너의 항상 실행되는 단계에서 `--restore-webhook`으로 복원할 수도 있습니다.
