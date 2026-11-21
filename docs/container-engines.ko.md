# 컨테이너 엔진과 러너 이미지

[Runquill README](../README.ko.md) · [English](container-engines.md)

## 엔진 선택

`RUNQUILL_CONTAINER_ENGINE`은 매니저가 `local` 호스트와 `ssh://` 호스트에 사용할 CLI를 고릅니다. 값은 `docker`(기본값) 또는 `podman`입니다. 매니저 하나는 이 호스트 전체에 엔진 하나를 사용합니다. 아웃바운드 워커는 자기 환경에서 이 변수를 읽습니다. Kubernetes 모드에서는 `docker` 이외의 값을 거부합니다.

- Compose용으로 게시한 매니저 이미지에는 Docker CLI만 들어 있습니다. Podman을 쓰려면 `podman` CLI가 설치된 머신에서 단독 실행용 `runquill` 바이너리를 실행합니다.
- 경로가 없는 Podman `ssh://` 호스트에는 `/run/podman/podman.sock`으로 연결합니다. 다른 소켓이 필요하면 호스트 항목 뒤에 경로를 붙입니다. 예를 들어 rootless 사용자는 `ssh://builder@worker.example.com/run/user/1000/podman/podman.sock`처럼 씁니다.
- Podman 러너는 `k8s-file`로 로그를 남기며, 크기 한도는 10 MB이고 순환 개수는 지정하지 않습니다. 캐시 볼륨은 러너보다 먼저 `podman volume create`로 만듭니다.
- [러너 로그](panel.ko.md#러너-로그)와 [탄력적 한도](elastic-limits.ko.md)는 항상 `docker` CLI를 호출하므로 Podman 호스트를 지원하지 않습니다.

`runquill doctor`는 호스트마다 엔진, 버전, 플랫폼, cgroup 버전, rootful 또는 rootless 여부를 보고합니다.

## Rootless 엔진

Runquill은 러너를 시작하기 전에 각 엔진이 rootless인지 확인하며, `doctor`도 같은 결과를 보고합니다.

- rootless 호스트의 러너는 plain 모드로 시작합니다. `privileged: false`를 설정한 풀과 같이 UID 1001로 실행하고, 모든 capability를 제거하고, `no-new-privileges`를 적용하며, Docker-in-Docker를 쓰지 않습니다. 이 호스트의 작업은 `docker`, 서비스 컨테이너, 작업 컨테이너, `sudo`를 사용할 수 없습니다.
- rootless 호스트에서 `"privileged": true`를 명시한 풀은 오류와 함께 거부합니다. 그런 환경에서는 Docker-in-Docker가 안정적으로 동작하지 않기 때문입니다.

Podman과 rootless 엔진은 구현되어 있고, 기록한 엔진 출력을 사용하는 단위 테스트로 확인했습니다. 실제 Podman이나 rootless 호스트에서는 아직 실행하지 않았습니다. 지원하고 테스트한 구성은 cgroup v2를 쓰는 Linux의 rootful Docker입니다.

## 러너 이미지 변형

릴리스는 amd64와 arm64용 러너 이미지를 다섯 가지 변형으로 게시합니다.

| 태그 | 기반 |
| --- | --- |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3` | Ubuntu 24.04, 업스트림 GitHub Actions 러너 이미지 |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-ubuntu22.04` | Ubuntu 22.04 |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-ubuntu26.04` | Ubuntu 26.04 |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-debian12` | Debian 12 |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-debian13` | Debian 13 |

모든 변형에는 같은 버전으로 고정한 Actions 러너와 Docker 바이너리, UID 1001의 `runner` 사용자가 들어 있습니다. 소스에서 빌드하려면 `docker build runner`에 `--build-arg RUNQUILL_RUNNER_OS=ubuntu22.04`, `ubuntu26.04`, `debian12`, `debian13` 중 하나를 전달합니다. 변형은 전체 공통 `RUNQUILL_RUNNER_IMAGE`나 풀의 `image`로 쓰거나, 작업이 `os__NAME__` 라벨로 고르게 할 수 있습니다. [러너 이미지](runner-images.ko.md)

CI는 모든 변형을 빌드하고 러너 사용자와 버전을 확인합니다. 중첩 Docker 테스트는 기본 변형에서만 실행합니다.

## 호스트별 이미지

매니저는 `local` 호스트와 `ssh://` 호스트에 없는 러너 이미지를 직접 받고(`RUNQUILL_IMAGE_PULL=missing`, 기본값), 작업이 러너 이미지 목록의 이미지를 고르면 그 이미지도 받습니다. 러너는 여전히 `--pull never`로 시작하므로, 호스트는 러너를 시작하면서 이미지를 받지 않고 이미지가 준비될 때까지 기다립니다. `runquill doctor`는 다른 플랫폼용으로 빌드한 이미지를 보고하고, 자동 받기가 켜져 있으면 없는 이미지를 대기 중으로 보고합니다.

매니저가 이미지를 받을 수 없는 아웃바운드 `agent://` 워커, `runquill-runner:local`처럼 로컬에서 빌드한 이미지, `RUNQUILL_IMAGE_PULL=never`를 설정한 모든 호스트에서는 이미지를 직접 준비합니다. 이 경우 이미지가 없는 `local` 또는 `ssh://` 호스트는 사용할 수 없음으로 표시됩니다. Podman에서는 `podman` CLI와 그 CLI의 레지스트리 로그인으로 이미지를 받습니다. [이미지 자동 받기](runner-images.ko.md#이미지-자동-받기)
