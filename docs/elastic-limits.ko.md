# 탄력적 러너 한도

[Runquill README](../README.ko.md) · [English](elastic-limits.md)

`cpus`나 `memory_mib`를 설정한 [워커 풀](workers.ko.md#워커-풀로-작업-분리)은 러너마다 고정된 Docker 자원 한도를 줍니다. 탄력적 한도를 쓰면 바쁜 러너가 다른 러너나 대기 작업에 약속되지 않은 호스트 용량을 빌려 쓰고, 한가해지면 돌려줍니다.

탄력적 한도는 매니저의 `local` Docker 호스트에서, 풀에 `cpus`나 `memory_mib` 또는 둘 다 설정된 러너에만 적용됩니다. `ssh://` 호스트, 아웃바운드 `agent://` 워커, Kubernetes는 건드리지 않으며, 매니저는 건너뛴 호스트를 로그에 남깁니다.

## 모드

| `RUNQUILL_ELASTIC` | 동작 |
| --- | --- |
| `off`(기본값) | 한도를 바꾸지 않습니다. `RUNQUILL_JOB_RESOURCES=off`가 아니면 [작업 자원](#작업-자원) 기록을 위해 러너는 계속 측정합니다. |
| `observe` | 러너를 측정하고 내릴 결정을 기록하지만 컨테이너는 바꾸지 않습니다. |
| `apply` | 결정을 기록하고 `docker update`로 적용합니다. |

`observe`로 시작해서 패널의 **호스트** 화면, `GET /api/elastic`, MCP `elastic_decisions` 도구에서 결정을 검토하세요.

## 설정

| 변수 | 기본값 | 범위 | 동작 |
| --- | --- | --- | --- |
| `RUNQUILL_ELASTIC_INTERVAL` | `10s` | `1s`–`1h` | 러너를 측정하는 주기입니다. 작업 자원 기록에도 쓰입니다. |
| `RUNQUILL_ELASTIC_COOLDOWN` | 측정 주기 세 번 | `1s`–`1h`, 측정 주기 이상 | 러너 하나의 CPU나 메모리를 두 번 바꾸는 사이의 최소 간격입니다. |
| `RUNQUILL_ELASTIC_HOST_RESERVE_CPUS` | `1` | `0` 이상 | 러너에 빌려주지 않을 CPU입니다. |
| `RUNQUILL_ELASTIC_HOST_RESERVE_MEMORY_MIB` | `512` | MiB 단위 정수 | 러너에 빌려주지 않을 메모리입니다. |
| `RUNQUILL_ELASTIC_MEMORY_TRIGGER` | `0.9` | `0.5`–`0.99` | 러너가 메모리를 더 받기 전에 메모리 한도 중 사용해야 하는 비율입니다. |
| `RUNQUILL_ELASTIC_MAX_MULTIPLE` | `2` | `1`–`16` | 어떤 한도도 풀이 보장하는 값의 이 배수를 넘지 않습니다. |

값이 잘못되면 매니저가 시작 단계에서 멈춥니다.

## 규칙

풀의 `cpus`와 `memory_mib`는 각 러너의 보장값입니다. 매니저는 측정할 때마다 호스트 여유분을 계산합니다. 호스트의 CPU와 메모리에서 예비분, 모든 러너의 보장값, 호스트의 빈 슬롯에 해당하는 보장값을 뺀 값입니다. 빈 슬롯의 보장값은 그 호스트에서 아직 시작될 수 있는 작업을 위해 예약해 둡니다.

- CPU 한도의 90% 이상을 쓰는 러너는 아직 빌려주지 않은 여유분에서 보장값의 절반까지 더 빌립니다.
- CPU 사용량이 보장값의 절반 아래인 러너는 빌린 만큼을 돌려줍니다.
- 여유분이 빌려준 양보다 줄어들면 가장 많이 빌린 러너부터 쿨다운(`RUNQUILL_ELASTIC_COOLDOWN`)을 기다리지 않고 바로 CPU를 돌려줍니다.
- 메모리 기준을 넘은 러너는 아직 내주지 않은 여유분에서 보장값의 4분의 1(최소 64 MiB)을 더 받습니다. 러너가 실행되는 동안 메모리는 줄이지 않습니다. 스왑은 항상 메모리 한도와 같게 맞춥니다.
- 한도는 보장값 아래로 내려가지 않고, 보장값의 `RUNQUILL_ELASTIC_MAX_MULTIPLE`배를 넘지 않습니다.
- 이 매니저가 라벨을 붙인 러너 컨테이너만 바꿉니다. 여기서는 `RUNQUILL_CONTAINER_ENGINE=podman`이어도 매니저가 항상 `docker` CLI를 사용합니다.

패널은 최근 결정 200개를 메모리에 보관합니다. [메트릭](metrics.ko.md)을 켜면 호스트별 게이지와 결정 카운터를 내보냅니다.

## Compose

매니저는 마운트한 Docker 소켓으로 `local` 호스트에 접근합니다. 변수는 [오버레이](operations.ko.md#매니저-설정-추가)로 전달합니다.

```yaml
services:
  manager:
    environment:
      RUNQUILL_ELASTIC: ${RUNQUILL_ELASTIC:-off}
      RUNQUILL_ELASTIC_INTERVAL: ${RUNQUILL_ELASTIC_INTERVAL:-}
      RUNQUILL_ELASTIC_COOLDOWN: ${RUNQUILL_ELASTIC_COOLDOWN:-}
      RUNQUILL_ELASTIC_HOST_RESERVE_CPUS: ${RUNQUILL_ELASTIC_HOST_RESERVE_CPUS:-}
      RUNQUILL_ELASTIC_HOST_RESERVE_MEMORY_MIB: ${RUNQUILL_ELASTIC_HOST_RESERVE_MEMORY_MIB:-}
      RUNQUILL_ELASTIC_MEMORY_TRIGGER: ${RUNQUILL_ELASTIC_MEMORY_TRIGGER:-}
      RUNQUILL_ELASTIC_MAX_MULTIPLE: ${RUNQUILL_ELASTIC_MAX_MULTIPLE:-}
      RUNQUILL_JOB_RESOURCES: ${RUNQUILL_JOB_RESOURCES:-}
```

## 작업 자원

같은 측정 단계에서 작업마다 CPU와 메모리 사용량을 기록하므로, 어떤 풀이 너무 작거나 큰지 알 수 있습니다. 기본으로 켜져 있고 `RUNQUILL_ELASTIC=off`여도 동작하며, `cpus`나 `memory_mib`가 없는 풀을 포함해 `local` Docker 호스트의 모든 러너를 측정합니다. 끄려면 `RUNQUILL_JOB_RESOURCES=off`로 설정합니다. `on`이나 `off`가 아닌 값이면 매니저가 시작 단계에서 멈춥니다. 측정은 항상 `docker` CLI로 하므로 `RUNQUILL_CONTAINER_ENGINE=podman`이면 `RUNQUILL_JOB_RESOURCES=on`을 설정하고 `docker` 명령이 Podman 소켓에 닿을 때만 켜집니다. `off`는 측정만 멈추며, 종료된 러너의 메모리 부족 종료 확인은 계속합니다.

| 기록하는 값 | 기록하는 곳 |
| --- | --- |
| 측정 횟수, 코어 단위 CPU 최고값과 평균, MiB 단위 메모리 최고값 | `local` Docker 호스트만 |
| CPU와 메모리 한도 | 처음 측정했을 때의 풀 보장값과, 측정 중 가장 높았던 실제 한도입니다. 보장값을 따로 두므로 탄력적 한도가 한도를 올려도 너무 작은 풀이 가려지지 않습니다. `0`은 무제한입니다. |
| 메모리 부족으로 종료됨 | `local`과 `ssh://` Docker 호스트는 종료된 러너의 `State.OOMKilled`, Kubernetes는 러너 컨테이너의 `OOMKilled` 사유로 판단합니다. 아웃바운드 `agent://` 워커에서는 기록하지 않습니다. |

호스트가 기록하지 않는 항목은 0으로 보여 주지 않고 출력에서 뺍니다.

탄력적 한도가 꺼져 있으면 측정할 때마다 `docker ps`, `docker inspect`를 실행하고, 러너가 실행 중이면 모든 러너에 대해 `docker stats --no-stream`을 한 번 실행합니다. `docker info`는 건너뛰고 컨테이너는 바꾸지 않습니다. `docker stats` 한 번은 데몬을 1–2초 동안 사용하므로, 바쁜 호스트라면 `RUNQUILL_ELASTIC_INTERVAL`을 늘리세요.

CPU 평균은 러너가 작업을 받기 전 유휴 시간을 포함한 러너의 전체 수명에 걸친 값입니다. 최고값은 이 측정값 가운데 가장 큰 값이며 cgroup의 최고 사용량이 아닙니다. 두 측정 사이에 잠깐 튄 사용량이나 빠르게 일어난 메모리 부족 종료는 실제보다 낮은 최고값으로 남을 수 있습니다. 메모리 부족 표시는 엔진이 보고한 값입니다. Docker 29는 러너 안의 프로세스 하나라도 메모리 부족으로 종료되면, 러너 자체는 정상 종료했더라도 이 값을 설정합니다. 다른 엔진과 버전은 확인하지 않았습니다.

작업 하나는 `runquill jobs show JOB_ID`, `GET /api/jobs/{id}`, MCP `get_job` 도구로 봅니다. 최근 작업을 한도와 비교하려면 `runquill usage --rightsizing`, `GET /api/usage/rightsizing`, MCP `job_rightsizing` 도구를 씁니다. 최근 7일 동안 끝난 작업을 풀별로, `--group job`이면 저장소와 러너 라벨별로 묶고 자원마다 판정을 붙입니다.

| 판정 | 뜻 |
| --- | --- |
| `tight` | 최고값이 한도의 80%를 넘었거나, 메모리의 경우 러너가 메모리 부족으로 종료됐습니다. |
| `oversized` | 측정한 작업이 3개 이상이고 모든 최고값이 한도의 25% 미만입니다. |
| `ok` | 둘 다 아닙니다. |
| `too_few_jobs` | 한도가 있는 측정 작업이 3개 미만이고 `tight`도 아닙니다. |
| `unlimited` | 측정한 작업 중 한도가 있는 작업이 없습니다. |

기록은 작업과 같이 7일 동안 보관하며 큐 백업에는 들어가지 않습니다.

계획 규칙은 단위 테스트로, Docker 호출은 가짜 Docker 명령을 쓰는 테스트로 확인했습니다. 부하가 걸린 실제 Docker 호스트에서는 아직 탄력적 한도를 실행하지 않았습니다.
