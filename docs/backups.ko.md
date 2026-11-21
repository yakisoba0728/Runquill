# 예약·암호화·S3 백업

[Runquill README](../README.ko.md) · [English](backups.md)

대기열 백업은 작업, 러너 리스, 호스트 정비 상태, 저장소 조회 기록, 운영 설정을 일관된 시점으로 복사한 SQLite 파일입니다. 등록된 아웃바운드 워커의 호스트 ID, 엔진 ID, 토큰 해시도 들어 있습니다. 워커 등록 코드와 관측 정보, 패널 사용자, 세션, 토큰, 감사 기록, [사용량 통계](panel.ko.md#사용량-통계)는 제외됩니다. 기본적인 `runquill backup`과 `runquill restore` 절차는 [대기열 백업과 복구](operations.ko.md#대기열-백업과-복구)에 있습니다. 이 가이드는 여기에 암호화, 예약 실행, 외부 저장소 복사를 더합니다.

## 암호화 백업

`runquill backup --output FILE --seal`은 `RUNQUILL_BACKUP_PASSPHRASE`, `RUNQUILL_BACKUP_PASSPHRASE_FILE`, `--passphrase-file` 중 하나로 받은 12자 이상의 암호로 백업을 암호화합니다. 암호를 명령 인수로 받지는 않습니다. 두 변수 중 하나만 설정하세요. 암호 파일은 줄바꿈으로 끝나도 됩니다.

암호화 파일은 AES-256-GCM을 사용합니다. 키는 암호와 무작위 32바이트 솔트로 PBKDF2-HMAC-SHA256(600,000회 반복)을 거쳐 만들므로 파일마다 키가 다릅니다. 데이터는 64 KiB 단위 청크로 암호화합니다. 각 청크는 파일 헤더, 청크 위치, 마지막 청크 여부와 함께 인증합니다. 암호가 틀리거나, 헤더가 바뀌었거나, 청크 순서가 바뀌었거나 빠지거나 덧붙거나 잘렸으면 데이터를 쓰기 전에 복구가 실패합니다. 다만 파일이 Runquill 백업이라는 사실과 대략적인 크기는 드러납니다. 암호화하거나 해독하는 동안 Runquill은 평문 사본을 대상 경로 옆의 비공개 임시 디렉터리에만 쓰고 작업이 끝나면 삭제합니다. 암호는 비밀 저장소에 보관하세요. 잃어버린 암호는 복구할 수 없습니다.

패널의 **대기열 백업 다운로드**는 암호화하지 않습니다.

## 예약 백업

다음 변수를 설정하면 매니저가 시작 1분 뒤에 백업을 만들고, 이후 지정한 주기마다 한 번씩 만듭니다.

| 변수 | 기본값 | 동작 |
| --- | --- | --- |
| `RUNQUILL_BACKUP_INTERVAL` | 미설정(끔) | `24h`처럼 `1m` 이상의 Go duration 값입니다. |
| `RUNQUILL_BACKUP_DIR` | 필수 | 백업을 저장할 디렉터리입니다. 권한 `0700`으로 만듭니다. |
| `RUNQUILL_BACKUP_KEEP` | `7` | 보관할 로컬 사본 수입니다. `1`부터 `1000`까지이며 오래된 사본부터 삭제합니다. |
| `RUNQUILL_BACKUP_PASSPHRASE` | 미설정 | 모든 예약 백업을 암호화합니다. |
| `RUNQUILL_BACKUP_PASSPHRASE_FILE` | 미설정 | 대신 파일에서 암호를 읽습니다. |

파일 이름은 `runquill-queue-YYYYMMDDTHHMMSSZ.sqlite3`이고, 암호화한 파일에는 `.sealed`가 붙습니다. 각 파일은 게시하기 전에 매니저의 `RUNQUILL_OWNER`와 `RUNQUILL_APP_ID`로 확인하며, 이 이름 형식의 파일만 정리 대상입니다. 설정이 잘못되면 매니저가 시작 단계에서 멈춥니다. 실패한 실행은 로그에 남고 패널의 **백업** 화면에 표시되며, 다음 주기에 다시 시도합니다.

관리자는 **백업** 화면의 **최신 백업 검증**으로 보관 중인 가장 최근 로컬 사본을 확인할 수 있습니다. 매니저는 설정된 암호로 암호화 사본을 인증하고 SQLite 형식, 무결성, GitHub App 정보를 확인합니다. 검증해도 원본 파일과 사용 중인 대기열은 바뀌지 않습니다. 패널에는 매니저 시작 이후 마지막 검증의 파일, 시각, 결과가 표시됩니다. 완료되거나 실패한 검증은 감사 기록에 `queue.backup.verify` 시도로 남습니다.

## S3 호환 스토리지에 업로드

다음 변수를 추가하면 예약 백업을 매번 버킷에도 복사합니다.

| 변수 | 기본값 | 동작 |
| --- | --- | --- |
| `RUNQUILL_BACKUP_S3_ENDPOINT` | 필수 | `https://s3.eu-central-1.amazonaws.com`처럼 경로가 없는 엔드포인트 URL입니다. HTTP는 루프백 주소에만 허용합니다. |
| `RUNQUILL_BACKUP_S3_BUCKET` | 필수 | 버킷 이름입니다. |
| `RUNQUILL_BACKUP_S3_REGION` | `us-east-1` | 서명에 쓰는 리전입니다. |
| `RUNQUILL_BACKUP_S3_PREFIX` | 비어 있음 | `runquill/` 같은 키 접두사입니다. |
| `RUNQUILL_BACKUP_S3_ACCESS_KEY_ID` | 필수 | 액세스 키입니다. |
| `RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY` | | 시크릿 키입니다. 이 변수나 파일 변수 중 하나를 설정합니다. |
| `RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY_FILE` | | 파일에서 시크릿 키를 읽습니다. |
| `RUNQUILL_BACKUP_S3_PATH_STYLE` | `false` | `bucket.endpoint/key` 대신 `endpoint/bucket/key`를 씁니다. 직접 운영하는 스토리지에서 흔히 필요합니다. |
| `RUNQUILL_BACKUP_S3_KEEP` | `7` | 접두사 아래에 보관할 객체 수입니다. `0`부터 `1000`까지이며 `0`이면 삭제하지 않습니다. |
| `RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT` | `false` | 암호화하지 않은 백업의 업로드를 허용합니다. |

Runquill은 암호화하지 않은 백업을 올리지 않습니다. 예약과 버킷을 설정했는데 암호가 없으면 `RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true`가 아닌 한 매니저가 시작하지 않으며, 허용한 경우에도 암호화하지 않은 업로드마다 경고를 남깁니다. `--seal` 없이 `runquill backup --upload`를 실행해도 같은 이유로 거부합니다. 업로드는 AWS Signature Version 4를 사용하고 리다이렉트를 따르지 않으며, 객체 크기를 비교해 완료를 확인합니다. 매번 실행한 뒤 매니저는 보관 중인 로컬 백업 중 아직 업로드를 확인하지 못한 것을 오래된 순서로 올리고, `RUNQUILL_BACKUP_S3_KEEP`을 넘는 가장 오래된 Runquill 객체를 삭제합니다. 실패한 업로드는 다음 실행에서 다시 시도합니다.

`runquill backup --upload --seal`은 같은 설정으로 백업 하나를 만들어 업로드합니다. 로컬 사본도 남기려면 `--output FILE`을 추가합니다.

암호화 예약 업로드와 복구 과정은 인증을 켠 로컬 SeaweedFS S3 서비스에서도 테스트했습니다. 사용 중인 S3 호환 서비스를 확인하려면 위의 S3 연결 변수를 테스트 버킷으로 설정하고 `RUNQUILL_TEST_BACKUP_S3=1 go test ./internal/app -run '^TestLiveS3BackupRestore$' -v`를 실행합니다. 이 스모크 테스트는 고유한 하위 접두사 아래에 합성 백업 하나를 쓰고, 이를 복구해 대기열을 확인한 뒤 그 테스트 객체만 삭제합니다. 해당 객체를 쓰고 읽고 삭제할 권한이 필요합니다.

## Compose

Compose는 기본적으로 이 변수들을 전달하지 않고 백업 디렉터리도 마운트하지 않습니다. `compose.yaml` 옆에 `compose.backups.yaml` 같은 오버레이를 저장하고, `(umask 077; openssl rand -base64 32 > backup-passphrase)`로 `backup-passphrase`를 만든 뒤 그 사본을 서버 밖에도 보관합니다.

```yaml
services:
  manager:
    environment:
      RUNQUILL_BACKUP_INTERVAL: ${RUNQUILL_BACKUP_INTERVAL:-24h}
      RUNQUILL_BACKUP_DIR: /backups
      RUNQUILL_BACKUP_KEEP: ${RUNQUILL_BACKUP_KEEP:-7}
      RUNQUILL_BACKUP_PASSPHRASE_FILE: /run/secrets/backup-passphrase
      RUNQUILL_BACKUP_S3_ENDPOINT: ${RUNQUILL_BACKUP_S3_ENDPOINT:-}
      RUNQUILL_BACKUP_S3_BUCKET: ${RUNQUILL_BACKUP_S3_BUCKET:-}
      RUNQUILL_BACKUP_S3_REGION: ${RUNQUILL_BACKUP_S3_REGION:-}
      RUNQUILL_BACKUP_S3_PREFIX: ${RUNQUILL_BACKUP_S3_PREFIX:-}
      RUNQUILL_BACKUP_S3_ACCESS_KEY_ID: ${RUNQUILL_BACKUP_S3_ACCESS_KEY_ID:-}
      RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY: ${RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY:-}
      RUNQUILL_BACKUP_S3_PATH_STYLE: ${RUNQUILL_BACKUP_S3_PATH_STYLE:-}
      RUNQUILL_BACKUP_S3_KEEP: ${RUNQUILL_BACKUP_S3_KEEP:-}
    volumes:
      - ./backups:/backups
    secrets:
      - backup-passphrase

secrets:
  backup-passphrase:
    file: ./backup-passphrase
```

`docker compose -f compose.yaml -f compose.backups.yaml up -d`로 적용합니다. 다른 오버레이도 함께 지정하고, 게시된 이미지를 쓴다면 `--no-build`를 추가합니다. 업로드하지 않는다면 S3 줄은 지웁니다. K3s에서는 변수를 `runquill-config` Secret에 추가하고 백업 디렉터리는 별도 볼륨에 둡니다.

## 복구

일반 복구와 똑같이, 매니저 시작을 차단한 새 대기열 파일을 준비합니다. 백업과 같은 `RUNQUILL_OWNER`와 `RUNQUILL_APP_ID`로 실행한 뒤 [대기열 백업과 복구](operations.ko.md#대기열-백업과-복구)의 활성화 절차를 따릅니다.

암호화 파일에서 복구할 때는 환경 변수나 `--passphrase-file`로 암호를 전달합니다. Runquill은 헤더를 보고 암호화 형식을 알아냅니다. 위 오버레이를 쓰면 백업 디렉터리와 암호가 이미 마운트되어 있습니다.

```sh
docker compose -f compose.yaml -f compose.backups.yaml run --rm --no-deps -e RUNQUILL_DB=/data/recovered.sqlite3 \
    manager restore --from /backups/runquill-queue-20260101T000000Z.sqlite3.sealed
```

S3에서 복구할 때는 `RUNQUILL_BACKUP_S3_PREFIX` 아래의 파일 이름이나 전체 키로 객체를 지정합니다. Runquill은 객체를 대상 경로 옆에 내려받아 같은 방식으로 복구합니다.

```sh
docker compose -f compose.yaml -f compose.backups.yaml run --rm --no-deps -e RUNQUILL_DB=/data/recovered.sqlite3 \
    manager restore --from-s3 runquill-queue-20260101T000000Z.sqlite3.sealed
```

S3에서 복구할 때는 S3 변수와 암호만 있으면 됩니다. `RUNQUILL_BACKUP_INTERVAL`은 필요하지 않습니다.
