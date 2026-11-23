# Prometheus 메트릭

[Runquill README](../README.ko.md) · [English](metrics.md)

Runquill은 운영 현황 전체의 집계 메트릭을 전용 HTTP 리스너로 내보낼 수 있습니다. `RUNQUILL_METRICS_ADDR`를 설정하면 켜집니다. 로컬 리스너는 인증 없이 동작합니다. 주소는 IP와 포트를 그대로 적습니다.

```sh
RUNQUILL_METRICS_ADDR=127.0.0.1:9090 ./runquill
curl --fail http://127.0.0.1:9090/metrics
```

매니저를 시작할 때는 평소의 GitHub App과 실행 환경 설정을 그대로 사용합니다. 메트릭 리스너는 패널, 웹훅과 별개로 동작합니다.

루프백이 아닌 주소에 바인딩하려면 `RUNQUILL_METRICS_TOKEN_FILE`이 필요합니다. 이 파일에 전용 무작위 토큰을 넣고, 수집할 때 bearer 토큰으로 보냅니다. 포트는 수집기만 접근할 수 있게 제한하고, 요청이 네트워크를 거친다면 신뢰하는 역프록시에서 TLS를 종료합니다. 토큰을 교체한 뒤에는 매니저를 재시작합니다.

Compose에서는 `compose.yaml` 옆에 토큰 파일을 만듭니다.

```sh
(umask 077; set -C; openssl rand -hex 32 > metrics-token)
```

다음 오버레이를 `compose.metrics.yaml`로 저장합니다. Docker는 이 포트를 호스트의 루프백 인터페이스에 게시합니다.

```yaml
services:
  manager:
    environment:
      RUNQUILL_METRICS_ADDR: 0.0.0.0:9090
      RUNQUILL_METRICS_TOKEN_FILE: /run/secrets/metrics-token
    secrets:
      - metrics-token
    ports:
      - "127.0.0.1:9090:9090"

secrets:
  metrics-token:
    file: ./metrics-token
```

`docker compose -f compose.yaml -f compose.metrics.yaml up -d`로 적용합니다. 이미 쓰고 있는 SSH나 패널 오버레이도 함께 지정합니다. 게시된 이미지로 설치했다면 `--no-build`를 추가합니다.

호스트에서 실행하는 Prometheus의 수집 설정을 구성합니다. Prometheus가 파일 경로를 읽을 수 있어야 합니다.

```yaml
scrape_configs:
  - job_name: runquill
    static_configs:
      - targets: ["127.0.0.1:9090"]
    authorization:
      credentials_file: /path/to/runquill/metrics-token
```

K3s에서는 매니저 설정에 `RUNQUILL_METRICS_ADDR=127.0.0.1:9090`을 추가하고 매니저를 재시작합니다. 같은 Pod의 수집기를 쓰거나, 로컬에서 확인할 때는 `kubectl -n runquill-system port-forward deployment/runquill 9090:9090`을 사용합니다. 루프백이 아닌 주소에서 수집하려면 토큰을 Kubernetes Secret으로 마운트하고 네트워크 접근을 수집기로 제한합니다.

메트릭 값은 운영 현황 전체를 나타냅니다. 샘플에는 저장소, 작업, 러너 식별자가 없습니다. 아래의 탄력적 한도 게이지만 호스트 라벨을 가지며, 작업 단계 히스토그램만 풀과 백엔드 라벨을 가집니다.

| 게이지 | 의미 |
| --- | --- |
| `runquill_queue_ready_jobs`, `runquill_queue_retry_jobs` | 러너 없이 대기 중인 작업을 재시도 대기 시간이 지났는지에 따라 나눈 수입니다. |
| `runquill_queue_oldest_job_age_seconds` | 러너 없이 대기 중인 가장 오래된 작업의 대기 시간입니다. |
| `runquill_runner_slot_limit`, `runquill_runner_occupied_slots`, `runquill_runner_reserved_slots` | 전체 한도, 사용 중으로 관측한 슬롯, 오프라인 호스트나 호스트가 정해지지 않은 리스를 위해 예약한 슬롯입니다. |
| `runquill_runners_running`, `runquill_hosts_offline` | 실행 중으로 관측한 러너 런타임과, 상태를 읽지 못한 호스트 수입니다. |
| `runquill_fleet_snapshot_timestamp_seconds` | 마지막으로 성공한 측정 시각입니다. 첫 측정이 준비될 때까지 게이지는 나타나지 않습니다. |

카운터 `runquill_runner_starts_total`, `runquill_runner_launch_errors_total`, `runquill_runner_lease_releases_total`, `runquill_scheduler_tick_errors_total`은 매니저를 재시작하면 초기화됩니다. 리스 해제에는 재시도 전 정리가 포함되며, 성공적으로 끝나지 않은 작업이 포함될 수 있습니다.

[알림](operations.ko.md#알림-보내기)을 설정하면 `runquill_notifications_sent_total`, `runquill_notifications_failed_total`, `runquill_notifications_dropped_total`이 각각 웹훅에 전달한 이벤트, 모든 시도 후에도 전달하지 못한 이벤트, 메모리 대기열이 가득 찼거나 매니저가 종료 중이어서 버린 이벤트를 셉니다. 이 카운터는 `RUNQUILL_NOTIFY_URL`이 없으면 나타나지 않으며, 역시 매니저를 재시작하면 초기화됩니다.

[탄력적 한도](elastic-limits.ko.md) 루프는 적용한 결정과 관측만 한 결정을 세는 `runquill_elastic_changes_total{applied="true"|"false"}`, Docker가 거부한 변경을 세는 `runquill_elastic_update_errors_total`을 추가합니다. `RUNQUILL_ELASTIC`이 `observe` 또는 `apply`이면 `runquill_elastic_surplus_cpus`, `runquill_elastic_lent_cpus`, `runquill_elastic_surplus_memory_bytes`, `runquill_elastic_extra_memory_bytes`, `runquill_elastic_held_slots` 게이지가 관리 중인 로컬 호스트마다 `host` 라벨을 붙여 값을 보고합니다. 단, 그 호스트의 풀에 `cpus`나 `memory_mib`가 설정되어 있어야 합니다. 이 카운터들도 매니저를 재시작하면 초기화됩니다.

## 작업 단계 히스토그램

다섯 개의 히스토그램이 작업의 지연을 단계별로 나눕니다. 느린 원인이 스케줄러, GitHub, 이미지 받기, 작업 자체 중 어디에 있는지 구분할 수 있습니다. 각 샘플에는 `pool`(작업 라벨의 워커 풀, 풀 선택자가 없으면 `default`, K3s에서는 빈 값)과 `backend`(`docker` 또는 `kubernetes`) 라벨이 붙습니다.

| 히스토그램 | 단계 | 작업 `timing` 필드 |
| --- | --- | --- |
| `runquill_job_scheduler_wait_seconds` | Runquill이 작업을 발견한 시각부터 러너 리스를 잡은 시각까지입니다. 빈 슬롯 대기, 재시도 지연, 저장소별 상한 대기, 그리고 Docker 호스트에서는 러너 이미지를 받을 때까지의 대기가 포함됩니다. | `scheduler_wait_millis` |
| `runquill_job_jit_registration_seconds` | GitHub가 JIT 러너 설정을 만드는 데 걸린 시간입니다. | `jit_registration_millis` |
| `runquill_job_runtime_launch_seconds` | 등록부터 런타임이 컨테이너를 시작하거나(Docker 호스트, 이미 받아 둔 이미지 사용) Pod를 만들 때까지(Kubernetes)입니다. | `runtime_launch_millis` |
| `runquill_job_github_assign_seconds` | 러너 시작부터 GitHub가 작업을 진행 중으로 보고할 때까지입니다. 러너 시작, 연결, 작업 배정이 포함되며, Kubernetes에서는 kubelet이 이미지를 받고 Pod를 시작하는 시간도 포함됩니다. | `launch_to_github_start_seconds` |
| `runquill_job_execution_seconds` | GitHub가 보고한 작업 시작부터 완료까지입니다. | `execution_seconds` |

버킷은 앞의 네 단계가 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1800, 3600초이고, 실행 단계가 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 21600초입니다. 모두 `+Inf`가 붙습니다.

작업은 GitHub가 완료를 보고한 뒤 러너 리스를 해제할 때 한 번 관측하며, 그 작업을 실행한 러너의 시각을 씁니다. 따라서 아직 대기열에 있는 작업은 `runquill_queue_oldest_job_age_seconds`에만 나타납니다. 러너를 받지 못한 작업, 작업이 대기 중일 때 해제된 러너, 시각이 빠진 단계는 관측하지 않습니다. GitHub 배정과 실행 단계는 1초 정밀도인 GitHub 시각을 쓰며, 배정 단계는 이를 매니저 시계와 비교하므로 차이가 음수이면 건너뜁니다. 히스토그램은 매니저를 재시작하면 초기화됩니다. 작업 하나의 같은 단계별 시간은 `GET /api/jobs/{id}`의 `timing` 객체와 `runquill jobs show`에서 볼 수 있습니다.

예를 들어 지난 한 시간 동안 시작 단계별 90번째 백분위수는 다음과 같습니다.

```promql
histogram_quantile(0.9, sum by (le, pool) (rate(runquill_job_runtime_launch_seconds_bucket[1h])))
histogram_quantile(0.9, sum by (le, pool) (rate(runquill_job_scheduler_wait_seconds_bucket[1h])))
```

## 쿼리 예

예를 들어 다음 쿼리는 실행할 수 있는 작업이 있는데 설정한 슬롯이 모두 사용 중이거나 예약된 매니저를 찾습니다. 오래된 값을 찾으려면 `time() - runquill_fleet_snapshot_timestamp_seconds`를 사용합니다.

```promql
runquill_queue_ready_jobs > 0
and (runquill_runner_occupied_slots + runquill_runner_reserved_slots >= runquill_runner_slot_limit)
```

[Prometheus 수집 설정](https://prometheus.io/docs/prometheus/latest/configuration/configuration/)
