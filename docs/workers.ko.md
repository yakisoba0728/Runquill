# Docker 워커와 풀

[Runquill README](../README.ko.md) · [English](workers.md)

## Docker 작업 서버 추가

매니저 한 대가 사용할 Docker 엔진을 `.env`에 나열합니다. 각 서버는 Linux의 rootful Docker, cgroup v2, Docker 소켓에 접근할 수 있는 SSH 계정이 필요합니다. `RUNQUILL_RUNNER_IMAGE`에 해당하는 이미지는 매니저가 이 연결로 각 서버에 받습니다. 전용 SSH 키를 쓰고, `known_hosts`를 채울 때 서버의 호스트 키를 확인합니다.

```dotenv
RUNQUILL_DOCKER_HOSTS=local,ssh://builder@worker.example.com
RUNQUILL_MAX_RUNNERS=3
RUNQUILL_DOCKER_HOST_LIMITS=local=2,ssh://builder@worker.example.com=1
RUNQUILL_SSH_DIR=/path/to/runquill-ssh
```

지정한 디렉터리에 전용 SSH 키, `known_hosts`, SSH `config`를 넣습니다. `config`에는 `IdentityFile /root/.ssh/id_ed25519`처럼 컨테이너 안의 키 경로와 `BatchMode yes`, `StrictHostKeyChecking yes`를 설정합니다.

```sh
docker compose -f compose.yaml -f compose.ssh.yaml up -d
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill doctor
```

매니저는 배정 가능한 서버 중 사용 중인 슬롯이 가장 적은 곳에 새 러너를 배치합니다. `RUNQUILL_MAX_RUNNERS`는 전체 한도이고, `RUNQUILL_DOCKER_HOST_LIMITS`는 서버별 한도입니다. 서버별 설정에서는 `RUNQUILL_DOCKER_HOSTS`에 적은 서버 이름을 그대로 씁니다.

서버 연결이 끊기면 해당 서버의 러너 리스와 마지막으로 확인한 미추적 러너 슬롯을 전체 한도에 반영합니다. 정상 서버는 남는 슬롯으로 작업을 계속 받습니다. 연결한 적이 없는 서버의 슬롯은 서버별 한도만큼 예약하며, 서버별 한도가 없으면 전체 한도만큼 예약합니다.

정비 전 `cordon`을 실행하면 해당 서버의 새 작업 배정이 멈추고 기존 작업은 계속 실행됩니다. `status`에서 서버 연결 상태, 정비 상태, 슬롯 사용량을 확인할 수 있습니다. 서버를 목록에서 빼기 전에는 해당 서버의 슬롯이 모두 비었는지 확인하세요. 새 작업을 다시 받으려면 `uncordon`을 실행합니다. 정비 상태는 대기열 볼륨에 저장되므로 매니저를 재시작해도 유지됩니다.

```sh
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill host cordon ssh://builder@worker.example.com
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill status
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill host uncordon ssh://builder@worker.example.com
```

NAT 뒤에 있는 워커는 HTTPS로 매니저에 연결하는 [아웃바운드 워커](outbound-workers.ko.md)로 추가합니다.

## 워커 풀로 작업 분리

`.env`에 `RUNQUILL_WORKER_POOLS` JSON 객체를 설정하면 워커 풀을 사용합니다. 키는 풀 이름이고, `hosts`에는 `RUNQUILL_DOCKER_HOSTS`에 적은 이름을 그대로 사용합니다. 호스트는 풀 하나에 속하고, 목록에 없는 호스트는 `default` 풀에 속합니다. 풀 설정은 `default`에도 적용할 수 있습니다.

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

풀에는 다음 네 가지 선택 항목도 쓸 수 있습니다.

```dotenv
RUNQUILL_WORKER_POOLS='{"lint":{"hosts":["local"],"privileged":false,"cache":true,"max_per_repository":2,"images":["ubuntu22.04"]}}'
```

- `privileged`의 기본값은 `true`이며, 러너마다 `--privileged`와 내부 Docker 데몬을 사용합니다. `false`로 설정하면 러너를 UID 1001로 실행하고 모든 capability를 제거하며 `no-new-privileges`를 적용합니다. 이 풀의 작업은 `docker`, 서비스 컨테이너, 작업 컨테이너, `sudo`를 사용할 수 없습니다. 이런 풀에 사용자 지정 `image`를 쓰려면 `/home/runner`를 소유한 UID 1001 러너 사용자가 이미지에 있어야 합니다.
- `cache: true`는 `runquill-cache-<저장소 ID>`라는 Docker 볼륨을 `/home/runner/.cache`에 마운트합니다. 같은 호스트의 같은 저장소 작업은 패키지와 빌드 도구 캐시를 공유하며, 러너가 이 볼륨으로 옮기는 npm, Go 모듈, NuGet, Bun 캐시도 여기에 포함됩니다. `RUNNER_TOOL_CACHE` 경로인 `/home/runner/.cache/tools`에는 저장소, 이미지 ID, 플랫폼별 볼륨을 따로 마운트하므로 `actions/setup-*`가 받은 도구는 같은 이미지를 쓰는 작업에서 재사용합니다. 각 호스트는 자체 볼륨을 유지합니다. 로컬과 SSH 호스트에서는 매니저가 30일 동안 러너가 쓰지 않은 볼륨을 삭제합니다. 목록은 `docker volume ls -f label=io.runquill.cache=1`로 확인합니다. [도구 캐시와 정리](runner-images.ko.md#도구-캐시) · [Docker 빌드 레이어 캐시](workflows.ko.md#다음-작업에서-docker-빌드-레이어-재사용)
- `max_per_repository`는 해당 풀에서 저장소 하나가 사용할 수 있는 러너 수를 제한하며, 그 풀에서는 `RUNQUILL_MAX_PER_REPOSITORY`보다 우선합니다. 둘 다 없으면 저장소 하나가 빈 슬롯을 모두 쓸 수 있습니다.
- `images`는 풀의 작업이 `os__NAME__`으로 고를 수 있는 [러너 OS](workflows.ko.md#러너-os-선택) 이름 목록입니다. 생략하면 모든 이름을 허용합니다.

`privileged: false` 또는 `cache: true`를 설정한 풀의 [아웃바운드 워커](outbound-workers.ko.md)는 매니저와 같은 Runquill 릴리스를 실행해야 합니다. 이전 버전 에이전트는 이 설정이 담긴 러너 작업을 거부하므로 매니저와 함께 워커도 업그레이드하세요.

관리자 패널의 풀 편집기에는 Docker-in-Docker와 공유 캐시 체크박스, 허용할 러너 OS 이름 입력란이 있습니다. 편집기는 저장할 때 풀의 `max_per_repository`, `warm`, `warm_max_idle_minutes` 값을 유지하지만 바꾸지는 못합니다. 패널에서 설정을 한 번 저장하면 그 값이 `RUNQUILL_WORKER_POOLS`보다 우선합니다.

## 러너를 미리 띄워 두기

일반 러너는 컨테이너가 시작될 때까지, Docker-in-Docker라면 자체 Docker 데몬이 준비될 때까지도 기다려야 합니다. 풀에 `warm`을 설정하면 풀의 `local`·`ssh://` 호스트마다 그 수만큼 러너를 미리 시작해 둡니다. 미리 띄운 러너는 작업도 자격 증명도 없이 대기합니다. 작업이 그 호스트에 배정되면 매니저가 러너 하나의 이름을 작업 러너 이름으로 바꾸고 작업의 일회용 JIT 설정을 넣어 주므로, 작업은 컨테이너와 데몬 시작을 건너뜁니다.

```dotenv
RUNQUILL_WORKER_POOLS='{"default":{"warm":2,"warm_max_idle_minutes":30}}'
```

- `warm`은 호스트당 개수이며 `0`(끔, 기본값)부터 `100`까지입니다. `warm_max_idle_minutes`가 지나도록 작업을 받지 못한 러너는 새것으로 교체되며 최대 `1440`입니다. 기본값은 `30`입니다.
- 미리 띄운 러너는 빈 슬롯만 채웁니다. 대기하는 동안에는 `RUNQUILL_MAX_RUNNERS`, 호스트 제한, 저장소 제한에 포함되지 않습니다. 작업이 가져간 러너는 다른 러너처럼 슬롯을 차지하며, 작업이나 전역 제한에 슬롯이 필요하면 매니저가 미리 띄운 러너를 제거합니다. `os__NAME__`으로 다른 이미지를 고른 작업은 새로 시작하면서 미리 띄운 러너 하나를 정리하므로, 그 스케줄러 패스가 끝날 때까지 호스트에 제한보다 컨테이너가 하나 많을 수 있습니다.
- 매니저는 스케줄러 패스마다 호스트당 최대 하나씩 러너를 미리 띄웁니다. cordon되었거나 연결되지 않았거나 러너 이미지를 기다리는 호스트는 건너뛰며, 호스트를 cordon하면 대기 중인 러너도 제거합니다. 이미지가 이 기능을 지원하지 않는 등의 이유로 러너가 일찍 종료되면 매니저는 그 호스트에서 15초부터 두 배씩 최대 10분까지 다시 시도를 미룹니다.
- 작업은 풀의 현재 이미지 이름, CPU·메모리 제한, Docker-in-Docker 모드, 대기 시간과 일치하는 러너만 가져갑니다. 일치하지 않는 러너는 매니저가 교체합니다. 같은 태그가 새 이미지를 가리키게 되면 러너가 대기 시간에 도달해 교체될 때 반영됩니다.
- 재시작한 매니저는 이미 실행 중인 러너를 그대로 이어받습니다. 매니저가 없으면 러너는 대기 시간보다 5분 뒤에 스스로 종료됩니다.
- 캐시 볼륨은 작업 저장소의 것인데 작업이 오기 전에는 저장소를 알 수 없으므로 `warm`은 `cache: true`와 함께 쓸 수 없습니다. 아웃바운드 `agent://` 워커에는 `warm`을 설정할 수 없고, K3s는 워커 풀을 쓰지 않습니다.
- 이 기능에는 이번 릴리스 이후의 러너 이미지가 필요합니다. 풀의 사용자 지정 이미지는 기본 엔트리포인트를 유지해야 합니다.
- 미리 띄운 러너도 다른 러너처럼 작업을 최대 하나만 받고 끝나면 제거됩니다. 대기 중에도 메모리를 쓰고 Docker-in-Docker라면 데몬도 실행 중이므로 `warm`은 호스트 용량 안에서 정하세요.
- 미리 띄운 러너의 이름은 `runquill-warm-…`입니다. `status`, 패널, `gc --dry-run`에는 나오지 않으며 `gc --remove-orphans`도 건드리지 않습니다. `docker ps --filter name=runquill-warm-`으로 확인하세요. Podman에서는 아직 시험하지 않았습니다.

## 저장소 간 슬롯 공유

슬롯이 비면 실행 중인 러너가 가장 적은 저장소의 작업을 먼저 시작하고, 같으면 가장 오래 기다린 작업을 고릅니다. 같은 저장소 안에서는 발견한 순서대로 시작합니다. `RUNQUILL_MAX_PER_REPOSITORY`를 설정하면 매니저 전체에서 저장소마다 풀별 러너 수를 제한합니다. 값은 `0`(무제한, 기본값)부터 `10000`까지입니다. 한도에 걸린 작업은 대기열에 남고 다른 저장소의 작업을 막지 않습니다. k3s에서도 모든 러너가 하나의 풀을 공유하는 것으로 보고 같은 한도를 적용합니다.

`RUNQUILL_SCHEDULER_WORKERS`는 스케줄링 한 번에 병렬로 처리할 러너 시작과 정리 작업 수입니다. `1`(순차)부터 `32`까지 설정할 수 있고 기본값은 `4`입니다. Docker 호스트 하나에 대한 호출은 순서대로 처리합니다. Compose에서는 [오버레이](operations.ko.md#매니저-설정-추가)로 이 값을 전달합니다.

## 러너 이미지와 컨테이너 엔진

릴리스는 Ubuntu 24.04 기반 `runquill-runner:<버전>`과 `-ubuntu22.04`, `-ubuntu26.04`, `-debian12`, `-debian13` 변형 이미지를 게시합니다. 매니저는 `local`과 `ssh://` 호스트에 없는 러너 이미지를 직접 받습니다(기본값 `RUNQUILL_IMAGE_PULL=missing`, 주기적으로 새로 받으려면 `always`). 아웃바운드 `agent://` 워커, 로컬에서 빌드한 이미지, `RUNQUILL_IMAGE_PULL=never` 설정에서는 이미지를 직접 준비합니다. 매니저는 자신이 받은 이미지와 30일 동안 어떤 러너도 쓰지 않은 캐시 볼륨도 삭제합니다. [러너 이미지](runner-images.ko.md) `RUNQUILL_CONTAINER_ENGINE=podman`을 설정하면 `local`과 `ssh://` 호스트에 Podman CLI를 사용하고, rootless 엔진의 러너는 Docker-in-Docker 없이 시작합니다. Podman과 rootless 엔진은 실제 호스트에서 아직 검증하지 않았습니다. [엔진과 이미지](container-engines.ko.md)

## 탄력적 러너 한도

`RUNQUILL_ELASTIC=observe` 또는 `apply`를 설정하면 `local` Docker 호스트에서 바쁜 러너가 다른 러너나 빈 슬롯에 약속되지 않은 CPU와 메모리를 풀의 `cpus`, `memory_mib`의 배수까지 빌려 씁니다. 한도는 풀 설정값 아래로 내려가지 않고 메모리는 줄이지 않습니다. `observe`는 결정만 기록하며 **호스트와 풀** 화면에서 확인할 수 있습니다. 부하가 걸린 실제 호스트에서는 아직 검증하지 않았습니다. [탄력적 한도](elastic-limits.ko.md)
