# 브라우저로 설치

[Runquill README](../README.ko.md) · [English](browser-setup.md)

Linux 호스트에서 설치 스크립트를 실행하고 출력된 설치 링크를 엽니다.

```sh
cd ~ && curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh && sh install.sh --wizard
```

1. 저장소 접근 범위를 고르고 **GitHub 연결**을 누릅니다. 마법사가 보여 주는 접근 범위로 GitHub App을 승인하고 설치합니다. 개인 계정이라면 마법사가 설치 소유자를 패널의 첫 관리자로 등록합니다.
2. **배포 및 사전 점검**을 누릅니다. 매니저가 시작되면 패널을 열거나 첫 워크플로를 내려받습니다.

기본 설정은 로컬 Docker Compose, 개인 GitHub 계정, 호스트에 맞게 추천된 러너 한도를 사용합니다. GitHub 조직, K3s 컨텍스트, 사용자 지정 주소, 다른 한도를 쓰려면 **고급 설정**을 엽니다. K3s에서는 같은 릴리스의 `runquill-k3s.yaml`과, 그 안의 리소스를 만들 권한이 있는 컨텍스트를 사용합니다. 로컬 패널은 포트 포워딩으로 열거나, 기존 HTTPS 인그레스를 지정합니다.

K3s 마법사는 `kube-system` 네임스페이스의 UID를 읽어 클러스터를 식별합니다. 중단된 배포를 이어 가려면 같은 설치 디렉터리에서 마법사를 다시 열고, 같은 클러스터와 GitHub App에 해당하는 컨텍스트를 선택합니다. 마법사는 네임스페이스에 기록한 설정 ID를 확인한 뒤 진행합니다. 이전 버전 마법사가 설정 ID 없이 네임스페이스를 만들었다면 K3s 수동 설치 절차로 배포를 마칩니다. [K3s 설치](../deploy/k3s/README.md#install)

릴리스 파일이 이미 있다면 설치 디렉터리에서 `./runquill wizard`를 실행합니다. 기존 Compose 설치에서는 `./runquill wizard --check-compose`가 읽기 전용 사전 점검을 실행합니다. SSH로 접속했다면 출력된 포워딩 명령을 사용하고 설치 링크는 비공개로 보관하세요.

배포가 끝나고 실제 패널이 응답하면 Ctrl+C로 마법사를 종료합니다. SSH 포워딩으로 연 패널은 사용하는 동안 포워딩 세션을 유지해야 합니다. 내 공개 HTTPS 주소를 쓰려면 [Cloudflare Tunnel](cloudflare-tunnel.ko.md)을 참고하세요.

## Tailscale

Compose 마법사에서 호스트를 준비할 때, 지원하는 Linux 배포판이라면 Tailscale을 설치하고 데몬을 시작하고 로컬 operator를 설정할 수 있습니다. 안내가 나오면 Tailscale 로그인 링크를 열어 tailnet에 참여합니다. tailnet에서 MagicDNS와 HTTPS를 켜야 합니다. Funnel에는 노드의 `funnel` 권한도 필요합니다.

마법사는 사용할 수 있으면 다음 경로를 구성합니다. 폴링은 30초 간격으로 계속 켜져 있으며 Tailscale 없이도 동작합니다.

| 항목 | 접근 | 리스너 | 대상 |
| --- | --- | --- | --- |
| Serve 패널 | 허용된 tailnet 피어 | HTTPS 9443 | `127.0.0.1:8081` |
| Funnel 웹훅 | GitHub와 인터넷 | HTTPS 8443, `/webhook` | `127.0.0.1:8080/webhook` |

자동 설정을 건너뛰려면 `install.sh --wizard --no-tailscale`을 사용하고, 패널과 웹훅 주소를 직접 정하려면 **고급 설정**을 사용합니다. 마법사는 기존 리스너를 그대로 두고 GitHub 웹훅 서명과 패널 OAuth를 사용합니다. [Tailscale Serve](https://tailscale.com/docs/reference/tailscale-cli/serve) · [Tailscale Funnel](https://tailscale.com/docs/features/tailscale-funnel)
