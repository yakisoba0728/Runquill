# Cloudflare Tunnel로 Runquill 연결

[Runquill README](../README.ko.md) · [English](cloudflare-tunnel.md)

Docker Compose로 설치한 Runquill의 운영 패널과 GitHub App 웹훅을 HTTPS 주소 하나에 연결합니다. Linux 서버의 cloudflared가 Cloudflare에 아웃바운드로 연결하며, 패널과 웹훅은 서버의 루프백 주소에 바인딩된 상태로 운영합니다.

## 터널 만들기

Cloudflare에서 관리하는 도메인과 Cloudflare 엣지 인증서가 적용된 호스트 이름을 사용합니다. 예제 주소는 `runner-ci.example.com`입니다. [Cloudflare 패키지 저장소](https://pkg.cloudflare.com/)에서 배포판에 맞는 `cloudflared`를 설치합니다. 터널을 관리할 사용자로 아래 명령을 실행하고 브라우저에서 로그인 URL을 열어 도메인을 승인합니다.

```sh
cloudflared tunnel login
cloudflared tunnel create runquill
cloudflared tunnel route dns runquill runner-ci.example.com
```

터널 생성 명령이 출력한 UUID를 `TUNNEL_ID`에 넣습니다. `~/.cloudflared/cert.pem`에는 계정의 터널을 관리할 권한이 있습니다. 터널 연결용 JSON과 함께 비공개로 보관합니다.

```sh
TUNNEL_ID='YOUR_TUNNEL_UUID'
sudo install -d -m 0755 /etc/cloudflared
sudo install -m 0600 "$HOME/.cloudflared/$TUNNEL_ID.json" "/etc/cloudflared/$TUNNEL_ID.json"
sudoedit /etc/cloudflared/config.yml
```

설정 파일에 아래 규칙을 넣습니다. `YOUR_TUNNEL_UUID`와 호스트 이름은 각각 두 곳 모두 바꿉니다. cloudflared는 규칙을 위에서부터 검사하므로 정확히 `/webhook`에 일치하는 규칙을 먼저 둡니다.

```yaml
tunnel: YOUR_TUNNEL_UUID
credentials-file: /etc/cloudflared/YOUR_TUNNEL_UUID.json
ingress:
  - hostname: runner-ci.example.com
    path: ^/webhook$
    service: http://127.0.0.1:8080
  - hostname: runner-ci.example.com
    service: http://127.0.0.1:8081
  - service: http_status:404
```

라우팅을 검증한 뒤 systemd 서비스로 등록합니다. 이미 cloudflared 서비스가 실행 중인 서버에서는 기존 서비스 설정에 규칙을 추가하고 검증한 뒤 그 서비스를 재시작합니다.

```sh
sudo cloudflared tunnel --config /etc/cloudflared/config.yml ingress validate
sudo cloudflared tunnel --config /etc/cloudflared/config.yml ingress rule https://runner-ci.example.com/webhook
sudo cloudflared --config /etc/cloudflared/config.yml service install
systemctl is-enabled cloudflared.service
systemctl is-active cloudflared.service
```

[Cloudflare 로컬 관리 터널 문서](https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/create-local-tunnel/)

## Runquill과 GitHub App 설정

새 설치에서는 [브라우저 마법사](install.ko.md#공개-릴리스-설치)를 `--no-tailscale`과 함께 실행합니다. **고급 설정**에서 **패널 주소**에 `https://runner-ci.example.com`, **공개 웹훅 URL**에 `https://runner-ci.example.com/webhook`을 입력합니다. 마법사를 따라 GitHub App을 만들고 설치합니다.

기존 설치에서는 같은 App과 대기열을 사용합니다. 패널 로그인에 쓰는 App의 콜백 URL로 `https://runner-ci.example.com/auth/github/callback`을 추가합니다. 러너 관리용 App의 웹훅을 활성화하고 `/webhook`을 포함한 URL을 입력한 뒤 **Workflow job** 이벤트를 구독합니다. GitHub와 매니저에는 같은 웹훅 시크릿을 설정합니다. 이미 쓰던 시크릿이 있다면 그대로 사용합니다.

설치 디렉터리의 `.env`에 다음 값을 적용합니다.

```dotenv
RUNQUILL_PANEL_URL=https://runner-ci.example.com
RUNQUILL_WEBHOOK_URL=https://runner-ci.example.com/webhook
RUNQUILL_POLL_INTERVAL=30s
```

같은 파일의 `RUNQUILL_WEBHOOK_SECRET`도 설정합니다. 패널에 이미 폴링 간격을 저장했다면 패널 **설정**에서 해당 값을 바꿉니다. 기존 설치 디렉터리에서 배포에 쓰던 모든 Compose 파일을 지정해 매니저를 다시 띄웁니다.

```sh
cd "$HOME/runquill"
docker compose -f compose.yaml -f compose.panel.yaml up -d --no-build
```

패널은 GitHub OAuth로 로그인하고, 웹훅은 GitHub 서명을 검증합니다. 패널에 Cloudflare Access를 적용할 경우 GitHub가 이벤트를 보낼 수 있도록 정확히 `/webhook` 경로는 브라우저 로그인 정책에서 제외합니다.

[GitHub App 웹훅 설정 문서](https://docs.github.com/en/webhooks/using-webhooks/creating-webhooks)

## 공개 주소와 실제 작업 확인

서버 외부의 클라이언트에서 확인합니다.

```sh
curl -fsS -o /dev/null -w '%{http_code}\n' https://runner-ci.example.com/
curl -sS -o /dev/null -w '%{http_code}\n' -d '{}' https://runner-ci.example.com/webhook
```

패널은 `200`, 서명 없이 호출한 웹훅은 `401`이 정상입니다. 등록한 GitHub 계정으로 패널에 로그인합니다. 다음으로 App이 접근할 수 있는 비공개 저장소에서 [첫 작업을 실행](workflows.ko.md#첫-작업-확인)합니다. GitHub App 설정의 **Advanced → Recent deliveries**에서 해당 작업의 `workflow_job` 전달 응답이 `202`인지 확인합니다. 매니저 로그에서 같은 작업 ID에 대해 `source=webhook`이 붙은 `queued job received`, `runner started`, `runner removed`를 찾습니다. GitHub에서 작업 성공과 러너 등록 해제도 확인합니다.

[재부팅 점검](operations.ko.md#재부팅-후-확인)으로 자동 시작을 확인할 수 있습니다. 폴링 없이 검증할 때는 [웹훅 전용 확인](operations.ko.md#웹훅만으로-작업-수신-확인)을 따릅니다.
