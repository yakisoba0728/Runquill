# 탄력적 러너 한도

[Runquill README](../README.ko.md) · [English](elastic-limits.md)

`cpus`나 `memory_mib`를 설정한 [워커 풀](workers.ko.md#워커-풀로-작업-분리)은 러너마다 고정된 Docker 자원 한도를 줍니다. 탄력적 한도를 쓰면 바쁜 러너가 다른 러너나 대기 작업에 약속되지 않은 호스트 용량을 빌려 쓰고, 한가해지면 돌려줍니다.

탄력적 한도는 매니저의 `local` Docker 호스트에서, 풀에 `cpus`나 `memory_mib` 또는 둘 다 설정된 러너에만 적용됩니다. `ssh://` 호스트, 아웃바운드 `agent://` 워커, Kubernetes는 건드리지 않으며, 매니저는 건너뛴 호스트를 로그에 남깁니다.

## 모드

| `RUNQUILL_ELASTIC` | 동작 |
| --- | --- |
| `off`(기본값) | 측정하지 않습니다. |
| `observe` | 러너를 측정하고 내릴 결정을 기록하지만 컨테이너는 바꾸지 않습니다. |
| `apply` | 결정을 기록하고 `docker update`로 적용합니다. |

`observe`로 시작해서 패널의 **호스트와 풀** 화면, `GET /api/elastic`, MCP `elastic_decisions` 도구에서 결정을 검토하세요.

## 설정

| 변수 | 기본값 | 범위 | 동작 |
| --- | --- | --- | --- |
| `RUNQUILL_ELASTIC_INTERVAL` | `10s` | `1s`–`1h` | 러너를 측정하는 주기입니다. |
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
```

계획 규칙은 단위 테스트로, Docker 호출은 가짜 Docker 명령을 쓰는 테스트로 확인했습니다. 부하가 걸린 실제 Docker 호스트에서는 아직 탄력적 한도를 실행하지 않았습니다.
