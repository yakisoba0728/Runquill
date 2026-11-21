# 러너 이미지

[Runquill README](../README.ko.md) · [English](runner-images.md)

모든 Runquill 러너는 러너 이미지로 시작합니다. 이 가이드는 게시된 OS 변형, 작업이 `runs-on`에서 변형을 고르는 방법, 매니저가 호스트에 이미지를 받는 방식, 캐시를 쓰는 풀의 도구 캐시, 더 이상 아무도 쓰지 않는 이미지와 캐시 볼륨의 정리를 설명합니다.

## OS 변형

릴리스는 amd64와 arm64용 러너 이미지를 다섯 가지 변형으로 게시합니다.

| 태그 | 기반 | 목록 이름 |
| --- | --- | --- |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3` | Ubuntu 24.04, 업스트림 GitHub Actions 러너 이미지 | `ubuntu24.04` |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-ubuntu22.04` | Ubuntu 22.04 | `ubuntu22.04` |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-ubuntu26.04` | Ubuntu 26.04 | `ubuntu26.04` |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-debian12` | Debian 12 | `debian12` |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-debian13` | Debian 13 | `debian13` |

모든 변형에는 다이제스트로 고정한 같은 Actions 러너와 Docker 바이너리, UID 1001의 `runner` 사용자가 들어 있으므로 권한 있는 풀, `privileged: false`를 설정한 풀, K3s `plain` 모드에서 모두 동작합니다. 소스에서 빌드하려면 `docker build runner`에 `--build-arg RUNQUILL_RUNNER_OS=NAME`을 전달합니다. CI는 모든 변형을 빌드하고 러너 사용자와 버전을 확인합니다. 중첩 Docker 테스트는 기본 변형에서만 실행합니다.

변형은 전체 공통 `RUNQUILL_RUNNER_IMAGE`, 풀의 `image`, 또는 작업이 이름으로 고르는 러너 이미지 목록의 항목으로 쓸 수 있습니다.

## 러너 이미지 목록

러너 이미지 목록은 이름을 이미지 참조에 대응시킵니다. 워크플로는 목록의 항목 이름만 지정할 수 있고 이미지 참조는 지정할 수 없습니다. 러너가 권한 있는 모드로 실행될 수 있으므로, 러너가 어떤 이미지로 시작할지는 관리자가 정합니다.

| 이름 | 이미지 | 출처 |
| --- | --- | --- |
| `ubuntu-latest` | 기본 러너 이미지(`RUNQUILL_RUNNER_IMAGE` 또는 패널의 기본 러너 이미지) | `default` |
| `ubuntu24.04` | `<release>-runner:<version>` | `release` |
| `ubuntu22.04`, `ubuntu26.04`, `debian12`, `debian13` | `<release>-runner:<version>-<name>` | `release` |

`release` 항목은 릴리스 빌드, 즉 Compose와 K3s가 실행하는 게시된 매니저 이미지와 단독 실행용 `runquill-linux-*` 바이너리에 있습니다. 소스 빌드에는 릴리스 이미지 이름이 없으므로 목록에 `ubuntu-latest`와 `ubuntu24.04`만 있고, 둘 다 기본 러너 이미지를 가리킵니다. 이때는 원하는 변형을 `RUNQUILL_RUNNER_IMAGES`로 추가합니다.

`RUNQUILL_RUNNER_IMAGES`는 `NAME: 이미지 참조` 형식의 JSON 객체입니다. 항목을 추가하고, 같은 이름의 기본 항목은 대체합니다.

```dotenv
RUNQUILL_RUNNER_IMAGES='{"ubuntu22.04":"ghcr.io/yakisoba0728/runquill-runner:v0.3.3-ubuntu22.04","ubuntu26.04":"ghcr.io/yakisoba0728/runquill-runner:v0.3.3-ubuntu26.04","debian12":"ghcr.io/yakisoba0728/runquill-runner:v0.3.3-debian12","debian13":"ghcr.io/yakisoba0728/runquill-runner:v0.3.3-debian13"}'
```

- 이름은 1~40자의 영문 소문자, 숫자, 점, 하이픈으로 이루어지고 영문자나 숫자로 시작합니다. 객체에는 이름을 최대 64개까지 넣을 수 있습니다.
- 이름은 점, 하이픈, 대소문자를 무시하고 비교하므로 `ubuntu22.04`와 `ubuntu-2204`는 같은 항목입니다. 같은 키로 정규화되는 이름 두 개는 거부합니다.
- 이미지는 스킴이나 인증 정보가 없는 Docker 이미지 참조여야 합니다. 대신 엔진을 레지스트리에 로그인시킵니다([비공개 레지스트리](#비공개-레지스트리) 참고).
- 값이 잘못되면 매니저가 시작 단계에서 멈춥니다. 새 목록을 적용하려면 변수를 바꾸고 매니저를 재시작합니다.

Compose에서는 [설정 오버레이](operations.ko.md#매니저-설정-추가)로 변수를 전달합니다. K3s에서는 `runquill-config` Secret에 추가합니다.

`runquill doctor`는 모든 항목을 `runner image os__LABEL__ = IMAGE (source)` 형식으로 출력하고, Docker 매니저라면 로컬 엔진에 그 이미지가 이미 있는지도 보여 줍니다. `GET /api/settings`는 같은 목록을 읽기 전용 `runner_images` 필드로 반환합니다.

## `runs-on`에서 OS 선택

작업은 자기 작업별 라벨 안에서, 선택 항목인 풀 선택자 뒤에 러너 이미지 목록의 항목을 고릅니다.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-os__ubuntu2204__build', github.run_id, github.run_attempt) }}
runs-on: ${{ format('runquill-{0}-{1}-pool__big__os__debian12__build', github.run_id, github.run_attempt) }}
runs-on: ${{ format('runquill-{0}-{1}-os__debian13__test-{2}', github.run_id, github.run_attempt, strategy.job-index) }}
```

형식은 `os__NAME__JOB`과 `pool__POOL__os__NAME__JOB`입니다. 라우팅 라벨에는 점을 쓸 수 없으므로 이름에서 점을 뺍니다. `ubuntu2204`, `ubuntu2404`, `ubuntu2604`, `debian12`, `debian13`, `ubuntu-latest`처럼 씁니다. 라벨 형식은 1~40자의 영문 소문자, 숫자, 하이픈을 허용합니다. 이름을 비교할 때 점과 하이픈은 무시하므로 `ubuntu2204`와 `ubuntu-22-04`는 모두 `ubuntu22.04`를 고릅니다. `doctor`는 각 항목의 라벨 형식을 출력합니다.

`os__`가 없는 작업은 호스트의 이미지, 즉 풀의 `image`나 기본 러너 이미지로 실행합니다.

`runs-on: ubuntu-22.04`만 쓰는 방식은 의도적으로 지원하지 않습니다. Runquill은 다른 작업이 쓰지 않는 라벨로 일회용 러너를 등록해 러너 하나와 작업 하나를 짝짓습니다. 여러 작업이 같은 라벨을 쓰면 GitHub가 러너를 시작한 목적과 다른 작업을 그 러너에 줄 수 있고, GitHub 호스팅 러너와 같은 작업을 두고 경쟁하게 됩니다.

### 풀에서 고를 수 있는 OS 제한

풀의 선택 항목인 `images` 키에는 그 풀의 작업이 고를 수 있는 러너 이미지 목록 이름을 나열합니다.

```dotenv
RUNQUILL_WORKER_POOLS='{"build":{"hosts":["ssh://builder@worker.example.com"],"images":["ubuntu22.04","debian12"]}}'
```

키를 생략하면 모든 이름을 허용합니다. 이름은 목록 이름과 같은 규칙으로 검사하지만 목록에 실제로 있을 필요는 없으므로, 목록에서 항목을 지워도 저장된 설정을 불러오지 못하게 되지는 않습니다. 패널의 풀 편집기에는 이 목록을 위한 **허용할 러너 OS** 입력란이 있습니다.

### 작업이 이미지를 받지 못할 때

- 목록에 없거나 작업의 풀이 허용하지 않는 이름이면 작업은 `select runner image: runner image "ubuntu2004" is not in the runner image catalogue; add it to RUNQUILL_RUNNER_IMAGES` 같은 재시도 이유와 함께 대기합니다. 목록이나 풀을 고치면 이후 스케줄링에서 작업이 시작됩니다. 실패로 기록되는 경우는 24시간 기한이 지났을 때뿐입니다.
- 작업의 풀에 그 이미지를 시작할 수 있는 호스트가 없으면 이유에 그렇게 표시됩니다. 아웃바운드 `agent://` 워커는 설정된 이미지만 시작하므로, 고른 이미지가 그 이미지와 같을 때만 `os__` 작업을 맡습니다.
- 호스트가 이미지를 받는 동안 작업은 `waiting for runner image IMAGE on HOST`로 대기합니다. 이미지를 가진 호스트가 생길 때까지 GitHub 러너 등록은 만들지 않습니다.

### Kubernetes

K3s에서는 고른 이미지가 러너 Pod의 이미지를 대체하고, kubelet이 다른 이미지처럼 받습니다. 비공개 레지스트리에는 `RUNQUILL_K8S_IMAGE_PULL_SECRET`을 사용합니다. K3s에는 워커 풀이 없으므로 `images`는 적용되지 않습니다.

### 워크플로 이전과 점검

패널의 **마이그레이션** 화면이 `runs-on`에 GitHub 호스팅 Ubuntu 버전 라벨(예: `ubuntu-22.04`)이 하나 있는 작업을 바꿀 때, 매니저의 러너 이미지 목록에 그 항목이 있으면 새 라벨에 `os__ubuntu2204__`를 넣어 OS를 유지합니다. `ubuntu-latest`는 기본 이미지이므로 선택자 없는 라벨로 바뀝니다. 작업 ID가 `os__`로 시작하면 선택자로 읽히므로 그 작업은 그대로 둡니다. [워크플로 이전](workflow-migration.ko.md)

`runquill workflow check`는 잘못된 `os__` 접미사를 `runs-on` 항목으로 보고하고, 목록에 없거나 작업의 풀이 허용하지 않는 이름은 `runner-image` 경고로 보고합니다. 명령을 실행하는 바이너리의 목록에 그 환경의 `RUNQUILL_RUNNER_IMAGES`를 더해 확인하므로, 매니저와 같은 `RUNQUILL_RUNNER_IMAGES`를 그 환경에도 설정합니다.

`runquill status`는 OS를 고른 작업 아래에 `os=NAME image=IMAGE`를 출력합니다. `status --json`, 패널의 대기열과 러너 표, `GET /snapshot`에는 `runner_os`와 `runner_image`가 있고, 작업 기록에는 `runner_os`가 있습니다.

## 이미지 자동 받기

매니저는 `local` 호스트와 `ssh://` 호스트에 러너 이미지를 직접 받습니다. 정책은 `RUNQUILL_IMAGE_PULL`로 정합니다.

| 값 | 동작 |
| --- | --- |
| `missing`(기본값) | 호스트에 없는 이미지를 받습니다. |
| `always` | 호스트에 이미 있는 이미지도 `RUNQUILL_IMAGE_PULL_INTERVAL`(기본값 `6h`, `1m`부터 `168h`까지)마다 다시 받습니다. 갱신에 실패하면 호스트에 있던 이미지를 계속 씁니다. |
| `never` | 아무것도 받지 않습니다. v0.3.1 이전처럼 각 호스트에 이미지가 미리 있어야 합니다. |

이미지 받기는 각 호스트에 설정된 이미지(기본 러너 이미지나 풀의 `image`)와, 대기 중인 작업이 그 호스트에서 고른 러너 이미지 목록의 이미지를 필요할 때 받습니다. 필요할 때 받는 이미지는 모든 호스트가 아니라 조건에 맞는 호스트 하나, 즉 가장 덜 붐비는 호스트나 이미 받고 있는 호스트에서 받으며, 마지막으로 요청한 작업 이후 한 시간이 지나면 요청 기록을 지웁니다.

- 30초마다 이미지를 확인하고, 동시에 최대 두 개, 호스트당 하나씩 받으며, 15분이 지나면 받기를 중단합니다. 실패하면 30초 뒤 다시 시도하고, 이후 대기 시간을 두 배씩 늘려 최대 10분까지 기다립니다. 그 사이 로컬 빌드처럼 호스트에 이미지가 생기면 바로 사용합니다.
- 호스트 자체의 이미지가 없어도 매니저는 시작합니다. 그 호스트는 사용할 수 없음으로 표시되고, 이미지 상태(`missing`, `pulling`, 또는 받기 오류와 함께 `failed`)가 패널의 **호스트와 풀** 화면과 `GET /snapshot`(`image_status`)에 나타납니다. `runquill status`는 별도 프로세스로 실행되므로 이미지가 있는지(`ready` 또는 `missing`)만 압니다. 그 호스트에서 실행될 작업은 `waiting for runner image IMAGE on HOST`로 대기합니다.
- 이미지 참조에 넣은 인증 정보는 로그와 받기 오류에서 지웁니다.

예외는 다음과 같습니다.

- 아웃바운드 `agent://` 워커는 미리 받아 둔 자기 이미지를 씁니다. 매니저가 그곳에서는 이미지를 받을 수 없으므로 워커에 직접 이미지를 로드합니다. [아웃바운드 워커](outbound-workers.ko.md)
- `runquill-runner:local`처럼 로컬에서 빌드한 이미지는 어느 레지스트리에도 없습니다. 필요한 모든 호스트에서 빌드합니다. `missing`이면 있는 로컬 이미지를 그대로 씁니다. `always`이면 주기마다 갱신이 실패하고 로컬 이미지를 계속 쓰므로, 로컬 빌드에는 `missing`이나 `never`를 권장합니다.
- `never`이면 아무것도 받지 않습니다. 이미지가 없는 호스트는 사용할 수 없음으로 표시되고, 러너 이미지 목록의 이미지를 고른 작업은 이미지가 생길 때까지 기다립니다. Runquill은 최대 15초마다 이미지를 확인합니다.
- K3s에서는 kubelet이 이미지를 받습니다. `RUNQUILL_IMAGE_PULL`은 여전히 검사하지만 효과는 없습니다.

`runquill doctor`는 자동 받기가 켜져 있으면 없는 이미지를 점검 실패가 아닌 대기 중으로 보고합니다. Compose에서는 [설정 오버레이](operations.ko.md#매니저-설정-추가)로 변수를 설정합니다.

## 비공개 레지스트리

매니저는 SSH 호스트에 `--host ssh://…`를 붙여 `docker pull`을 직접 실행하므로, 레지스트리 로그인 정보는 호스트가 아니라 매니저의 Docker 클라이언트 설정에 있어야 합니다. 레지스트리의 `auths` 항목이 담긴 `config.json`이 있는 디렉터리를 만들고(매니저 이미지 안에서는 credential helper를 쓸 수 없습니다), 매니저에 읽기 전용으로 마운트한 뒤 `DOCKER_CONFIG`가 그 디렉터리를 가리키게 합니다.

```yaml
services:
  manager:
    environment:
      DOCKER_CONFIG: /run/docker-config
    volumes:
      - ./docker-config:/run/docker-config:ro
```

이 디렉터리는 GitHub App 키처럼 보호하세요. 아웃바운드 워커는 매니저를 위해 아무것도 받지 않으므로 별도의 이미지 공급 경로가 필요합니다. K3s에서는 `RUNQUILL_K8S_IMAGE_PULL_SECRET`을 사용합니다. 비공개 레지스트리는 아직 실제로 실행해 보지 않았습니다.

## 도구 캐시

`cache: true`를 설정한 풀에서는 러너마다 저장소의 기존 `runquill-cache-<저장소 ID>` 볼륨을 `/home/runner/.cache`에 마운트해 패키지와 빌드 도구 캐시로 씁니다. `/home/runner/.cache/tools`에 마운트하는 별도 볼륨은 `RUNNER_TOOL_CACHE`를 그 저장소와 러너의 실제 이미지 ID, 플랫폼 단위로 나눕니다. 같은 호스트의 작업은 이 값이 같으면 `actions/setup-*`가 받은 도구를 재사용합니다. 작업은 마운트된 캐시를 읽고 수정할 수 있습니다. 태그가 새 이미지 ID를 가리키게 되면 도구 캐시도 새로 시작합니다.

업그레이드 후 각 이미지의 첫 작업은 빈 도구 캐시로 시작합니다. 이전에 공유하던 도구 파일은 새 마운트 아래의 저장소 볼륨에 남고, 저장소의 다른 캐시 파일도 계속 쓸 수 있습니다. 캐시는 `local`, `ssh://`, 아웃바운드 `agent://` 워커에서 동작합니다. 워커마다 새 도구 캐시 구조를 쓰려면 아웃바운드 워커를 업그레이드합니다.

Docker 통합 테스트는 도구 체인 표식 파일로 캐시 재사용과 격리를 확인합니다. 실제 `actions/setup-*` 작업은 아직 별도로 검증해야 합니다.

## 사용하지 않는 이미지와 캐시 볼륨 정리

매니저는 `local` 호스트와 `ssh://` 호스트에서 한동안 아무도 쓰지 않은 러너 이미지와 캐시 볼륨을 삭제합니다. 호스트마다 각 이미지와 캐시 볼륨으로 마지막으로 러너를 시작한 시각과, 자신의 이미지 받기 기능이 받은 이미지를 기록합니다.

| 변수 | 기본값 | 동작 |
| --- | --- | --- |
| `RUNQUILL_CLEANUP_AFTER` | `30d` | 두 종류 모두의 보존 기간입니다. `off`나 `0`이면 자동 정리를 끕니다. |
| `RUNQUILL_CLEANUP_IMAGES_AFTER` | 위의 값 | 이미지만의 보존 기간, 또는 `off`입니다. |
| `RUNQUILL_CLEANUP_CACHE_AFTER` | 위의 값 | 캐시 볼륨만의 보존 기간, 또는 `off`입니다. |
| `RUNQUILL_CLEANUP_INTERVAL` | `6h` | 정리 간격으로 `5m`부터 `7d`까지입니다. 첫 정리는 시작할 때 실행합니다. |

보존 기간은 `720h` 같은 Go duration이나 `30d` 같은 정수 일수이며, `1h` 이상이어야 합니다.

삭제하는 대상은 다음 두 가지뿐입니다.

- `io.runquill.cache=1` 라벨이 있고 이름이 `runquill-cache-<숫자>` 또는 `runquill-cache-<숫자>-tools-<해시>`이며, 어떤 컨테이너도 마운트하지 않고 보존 기간 동안 쓰이지 않은 캐시 볼륨
- Runquill의 이미지 받기 기능이 그 호스트에 받았고 받았을 때의 이미지 ID를 그대로 가지고 있으며, 어떤 설정도 어떤 컨테이너도 쓰지 않고, 보존 기간 동안 아무도 러너를 시작하지 않은 이미지

다음은 절대 삭제하지 않습니다.

- 설치 스크립트나 `docker pull`로 받은 이미지를 포함해 직접 받거나 빌드한 이미지, 로컬에서 다시 빌드해 다른 이미지를 가리키게 된 태그
- 기본 러너 이미지, 모든 풀의 `image`, 최근 한 시간 안에 대기 중인 작업이 요청한 러너 이미지 목록의 이미지, 이들 중 하나와 ID가 같은 이미지
- 어떤 컨테이너든 사용 중인 이미지와 볼륨, 받는 중인 이미지
- 건너뛰는 아웃바운드 `agent://` 워커와 K3s의 모든 항목

삭제할 때는 각 객체를 먼저 다시 확인하고, 이미지는 참조로 강제 옵션 없이 삭제하며, 엔진이 거부하면 그대로 둡니다. 한 호스트가 실패해도 다른 호스트는 계속 정리합니다.

**업그레이드 후 첫 시작.** Runquill에 기록이 없는 객체는 정리 과정에서 처음 발견한 시점부터 추적합니다. 따라서 기존 캐시 볼륨은 업그레이드 후 최소한 보존 기간 한 번 동안 유지되고, 업그레이드 전에 받은 이미지는 Runquill의 이미지 받기 기능이 받은 것이 아니므로 삭제하지 않습니다.

같은 기준으로 직접 실행할 수 있습니다.

```sh
docker compose exec manager runquill gc --unused --dry-run
docker compose exec manager runquill gc --unused
docker compose exec manager runquill gc --unused --older-than 7d
```

`--dry-run`은 호스트, 종류, 이름, 미사용 기간, 크기를 보여 주며 기록을 포함해 아무것도 바꾸지 않습니다. 이 옵션 없이 실행하면 삭제하고 객체마다 결과를 출력합니다. `--older-than`은 두 종류 모두의 보존 기간을 대신하며, `RUNQUILL_CLEANUP_AFTER=off`일 때는 반드시 지정해야 합니다. `--unused`는 `--remove-orphans`와 함께 쓸 수 없습니다.

`runquill doctor`는 정리 정책과 함께, `local`과 `ssh://` 호스트마다 보유한 캐시 볼륨과 Runquill이 받은 이미지의 수와 크기를 출력합니다.

## 기능 검증 현황

개발자 컴퓨터의 실제 Docker로 확인한 항목: 레지스트리에서 이미지 받기, 실제 이미지와 볼륨을 대상으로 한 정리, 다섯 가지 OS 변형 모두의 러너 실행. Docker 스모크 테스트는 같은 저장소에 대해 Ubuntu 24.04와 Debian 13을 번갈아 실행하면서 각 이미지가 자기 도구 캐시만 재사용하는지, 패키지 캐시는 계속 공유되는지 확인합니다. plain 모드와 권한 있는 모드 모두에서 실행합니다.

아직 확인하지 않은 항목: 도구 캐시를 쓰는 실제 `actions/setup-*` 작업, `ssh://` 호스트에서 필요할 때 이미지 받기, 비공개 레지스트리, `os__` 선택자를 쓰는 아웃바운드 `agent://` 워커.
