# 아웃바운드 Docker 워커

[Runquill README](../README.ko.md) · [English](outbound-workers.md)

워커는 HTTPS로 Runquill 매니저를 폴링하고 자기 Docker 엔진에서 작업을 실행합니다. 매니저와 GitHub로 나가는 연결만 있으면 됩니다. 각 워커에는 Linux amd64 또는 arm64, cgroup v2를 쓰는 rootful Docker, 로컬에 미리 받아 둔 Runquill 러너 이미지, Runquill 실행 파일이 필요합니다. Docker 백엔드와 마찬가지로 신뢰하는 워크플로 전용 엔진을 사용하세요.

## 패널에서 등록

워커용 슬롯을 미리 설정하고, HTTPS로 `/worker/v1/*`를 제공하는 매니저 origin을 지정합니다. 이 주소는 패널 origin과 달라도 됩니다. 기존 `local`과 `ssh://` 대상을 쓰고 있다면 그대로 둡니다.

```dotenv
RUNQUILL_DOCKER_HOSTS=local,agent://worker-a
RUNQUILL_DOCKER_HOST_LIMITS=agent://worker-a=1
RUNQUILL_WORKER_ENROLLMENT_URL=https://workers.example.com
```

Compose에서는 `compose.yaml`과 `compose.panel.yaml`, 그리고 현재 설치에서 쓰는 오버레이를 함께 적용합니다. 워커 인증 오버레이는 `workers.json` 등록에 필요합니다. 신뢰하는 프록시는 [연결](#연결)에 설명한 대로 구성합니다. 패널 **설정**에 워커 목록이 이미 저장되어 있으면 매니저는 시작할 때 새 `agent://` 호스트를 그 목록에 더하고 저장된 다른 워커는 그대로 둡니다. 자세한 내용은 [화면 이동](panel.ko.md#화면-이동)을 참고하세요.

패널 관리자로 **호스트와 풀**을 열고 해당 슬롯에서 **워커 등록**을 누른 뒤, 표시된 명령을 워커에서 실행합니다. 이 명령은 워커의 Docker 엔진을 등록하고, 현재 디렉터리에 비공개 토큰 파일을 만들고, 그 호스트에 설정된 이미지로 워커를 시작합니다. 시작하기 전에 그 이미지를 미리 받아 둡니다. 코드는 10분 뒤 만료되고 한 번만 쓸 수 있으며, 새 코드를 발급하면 대체됩니다. 선택한 머신의 운영자에게만 공유하세요.

토큰 파일을 보관하고, 서비스 관리자에서는 같은 디렉터리에서 표시된 재시작 명령을 사용합니다. 등록 요청의 응답을 받지 못했더라도 매니저가 코드를 받아들였다면 재시작 명령이 저장된 토큰으로 연결할 수 있습니다. 첫 인증 정보를 기다리는 설정된 슬롯은 사용할 수 없는 상태로 남지만, 쓰지 않는 용량을 예약하지는 않습니다.

워커 인증 정보는 해당 Docker 엔진과 대조해 확인합니다. 매니저는 토큰 해시를 대기열에 저장하며, 대기열 백업에는 이 연결 정보가 포함됩니다. 대기 중인 등록 코드는 백업에서 제외됩니다. 워커 토큰 파일은 따로 보관하세요. `RUNQUILL_WORKER_ENROLLMENT_URL`을 제거하면 코드 발급이 꺼지지만, 등록된 워커는 기존 인증 정보를 계속 사용합니다.

## 수동 등록

워커에서 비공개 토큰 파일을 만들고 Docker 엔진 ID와 토큰 해시를 구합니다. 엔진 ID와 해시를 매니저 관리자에게 전달합니다.

```sh
(umask 077 && openssl rand -hex 32 >"$PWD/worker-a.token")
docker info --format '{{.ID}}'
tr -d '\r\n' <"$PWD/worker-a.token" | sha256sum
```

매니저에서 받은 값을 그대로 넣어 `workers.json`을 만듭니다. 토큰 원문은 워커에만 남습니다.

```json
{
  "worker-a": {
    "engine_id": "WORKER_ENGINE_ID",
    "token_sha256": "WORKER_TOKEN_SHA256"
  }
}
```

매니저 설정에 워커를 추가합니다. `local`이나 기존 SSH 대상을 계속 쓰려면 목록에 남겨 둡니다. 대상마다 별도의 엔진이 필요합니다. 서버별 한도는 워커가 오프라인일 때 예약하는 용량의 상한입니다.

```dotenv
RUNQUILL_DOCKER_HOSTS=agent://worker-a
RUNQUILL_DOCKER_HOST_LIMITS=agent://worker-a=1
RUNQUILL_WORKER_AUTH_FILE=./workers.json
```

Compose에서는 아웃바운드 워커 오버레이를 켭니다.

```sh
docker compose -f compose.yaml -f compose.worker.yaml up -d
```

## 연결

신뢰하는 HTTPS 역프록시로 `/worker/v1/*`를 매니저의 HTTP 리스너에 전달합니다. 매니저의 HTTP 포트는 루프백에 바인딩하거나 그 프록시만 접근할 수 있게 둡니다. 프록시는 `X-Forwarded-Proto`를 실제 외부 스킴으로 덮어써야 합니다. Runquill은 루프백 피어나, 명시적으로 설정한 단 하나의 `RUNQUILL_WORKER_PROXY_IP`에서 온 이 헤더만 받아들입니다. 이 값에는 매니저가 보는 프록시의 출발지 IP를 설정합니다. 호스트의 Compose 포트로 프록시한다면 Docker 브리지 게이트웨이일 수 있습니다. 다른 클라이언트가 그 피어로 위장하지 못하도록 백엔드 리스너 접근을 제한하세요. 프록시 로그에는 인증 헤더와 응답 본문을 남기지 마세요.

워커에서는 워커 이름, 매니저의 HTTPS origin, 비공개 토큰 파일을 지정해 Runquill을 시작합니다. 매니저나 풀에서 이 호스트에 설정한 이미지와 같은 이미지를 사용합니다. 평소 쓰는 신뢰할 수 있는 이미지 공급 경로로 그 이미지를 먼저 로드하거나 받아 둡니다. 매니저는 `local` 호스트와 `ssh://` 호스트에서만 러너 이미지를 받고, 아웃바운드 워커에서는 받지 않습니다. 워커는 이 이미지만 시작하므로 `os__NAME__` 라벨로 다른 [러너 이미지 목록의 이미지](runner-images.ko.md)를 고른 작업은 맡지 않으며, 매니저의 [사용하지 않는 이미지와 캐시 볼륨 정리](runner-images.ko.md#사용하지-않는-이미지와-캐시-볼륨-정리)도 이 워커를 건너뜁니다.

```sh
RUNQUILL_RUNNER_IMAGE=runquill-runner:local \
  runquill worker --name worker-a --manager https://runquill.example.com \
  --token-file "$PWD/worker-a.token"
```

`--name`, `--manager`, `--token-file`을 생략하면 각각 `RUNQUILL_WORKER_NAME`, `RUNQUILL_WORKER_URL`, `RUNQUILL_WORKER_TOKEN_FILE` 값을 씁니다. 워커는 서버 인증서를 검증합니다. 로컬 테스트에는 루프백 IP를 직접 적은 HTTP 주소를 쓸 수 있습니다. 서비스 관리자로 워커 프로세스를 계속 실행하세요. 워커는 토큰 파일 옆의 파일을 잠가 두 프로세스가 같은 인증 정보를 동시에 쓰지 못하게 합니다. 워커마다 고유한 토큰과 이름을 사용합니다.

워커에서도 매니저와 같은 Runquill 릴리스를 실행합니다. `privileged: false`나 `cache: true`를 설정한 풀의 워커는 매니저와 함께 업그레이드해야 합니다. 이전 버전 워커는 이 설정이 담긴 러너 작업을 거부하기 때문입니다. 캐시 볼륨은 워커 자신의 엔진에 있습니다.

워커는 `runquill status`나 패널에서 확인합니다. 기존의 예약, 슬롯, 풀, `cordon` 동작이 그대로 적용됩니다. 연결이 끊겼다가 돌아오면 Runquill은 새 인벤토리를 받은 뒤에 그 워커의 이전 러너 리스를 처리합니다. 재시작한 워커는 최대 45초 뒤 이전 세션을 이어받을 수 있습니다. 작업이 정상적으로 끝나면 매니저는 확인된 Docker ID로 워커 소유의 컨테이너를 삭제합니다.

Docker 통합 테스트는 패널의 코드 발급, CLI 등록, 서명된 테스트용 웹훅, 실제 컨테이너에서의 작업 실행, 삭제까지 확인합니다. 로컬 GitHub API 테스트 서버를 사용하며, CI에서 두 Linux 아키텍처 모두에서 실행합니다.

## 인증 정보 교체와 엔진 교체

인증 정보를 교체하거나 엔진을 바꾸기 전에 `agent://worker-a`를 `cordon`하고 작업이 모두 끝나기를 기다립니다. 워커를 멈추고 새 토큰을 만든 뒤, `workers.json`의 해시를 갱신하고 매니저를 재시작합니다. 그다음 워커를 재시작하고 `uncordon`합니다. 패널에서 등록한 호스트에 수동 인증 정보를 설정하면, 매니저를 재시작할 때 저장된 연결 정보를 대체합니다. 토큰만 교체할 때는 이름과 엔진 ID를 그대로 유지합니다. 엔진을 바꿀 때는 작업이 모두 끝난 뒤 엔진 ID를 갱신합니다. 해결되지 않은 리스가 남은 상태로 워커를 영구히 폐기하면, 매니저가 그 워커를 대조할 수 있을 때까지 그 리스는 예약된 채로 남습니다. 낯선 러너를 로컬에서 삭제하기 전에 `runquill gc --dry-run` 결과를 검토하세요. `runquill gc --remove-orphans`는 아웃바운드 워커를 건너뜁니다.
