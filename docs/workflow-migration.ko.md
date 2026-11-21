# 워크플로 이전과 점검

[Runquill README](../README.ko.md) · [English](workflow-migration.md)

Runquill은 `ubuntu-latest` 같은 GitHub 호스팅 라벨을 쓰는 기존 작업을 풀 리퀘스트를 열어 Runquill 라벨로 옮길 수 있고, 커밋하기 전에 워크플로 파일을 오프라인으로 점검할 수 있습니다.

## 권한

이전 풀 리퀘스트를 열려면 GitHub App에 **Contents**, **Pull requests**, **Workflows** 읽기·쓰기 권한이 필요합니다. Runquill은 기본적으로 이 권한을 요청하지 않습니다.

- 새 App이라면 `runquill setup --migrate`를 실행합니다. 설정 과정에서 App 매니페스트에 세 권한을 추가합니다. 설치 스크립트와 브라우저 마법사에는 이 옵션이 없습니다.
- 기존 App이라면 GitHub의 App 설정에서 세 권한을 추가한 뒤, App 설치에서 새 권한을 승인합니다.

패널의 **마이그레이션** 화면에서 빠진 권한을 확인할 수 있습니다. 미리보기에는 Contents 읽기 권한만 필요합니다.

## 패널에서 이전

이전은 패널의 **마이그레이션** 화면에서 관리자만 쓸 수 있습니다. API 토큰으로는 쓸 수 없습니다.

1. **권한**에서 App 설치를 확인합니다.
2. **저장소와 라벨**에서 설정된 계정 소유의 비공개 저장소 하나와 대응 규칙을 최대 50개까지 입력합니다. 각 규칙에는 `runs-on` 라벨을 1~10개 적고, 대상 풀은 선택해서 적습니다. 라벨은 대소문자와 순서에 관계없이 집합으로 비교합니다.
3. **미리보기**는 기본 브랜치의 `.github/workflows` 바로 아래에 있는 모든 `.yml`, `.yaml` 파일(최대 50개, 파일당 최대 512 KiB)을 읽고 파일마다 diff를, 작업마다 결과를 보여 줍니다. 아무것도 쓰지 않습니다.
4. **풀 리퀘스트 열기**는 쓰기 토큰으로 미리보기를 다시 실행하고, 결과가 검토한 미리보기와 여전히 같을 때만 진행합니다. 기본 브랜치에서 `runquill/migrate-runs-on-<hash>` 브랜치를 만들고, 바뀐 파일을 각각 커밋한 뒤, 바뀐 작업과 그대로 둔 작업을 나열한 풀 리퀘스트 하나를 엽니다.

요청, 열린 풀 리퀘스트, 실패는 감사 기록에 남습니다. 대상 풀이 준비되면 풀 리퀘스트는 직접 병합합니다.

## 바뀌는 것과 바뀌지 않는 것

규칙에 맞는 작업마다 `runs-on` 값만 작업별 Runquill 라벨로 바꿉니다. 매트릭스 작업에는 `strategy.job-index`를 추가하고, `default`가 아닌 풀에는 `pool__NAME__` 접두사를 붙입니다. 작업의 라벨에 `ubuntu-22.04` 같은 GitHub 호스팅 Ubuntu 버전 라벨이 정확히 하나 있고 매니저의 [러너 이미지 목록](runner-images.ko.md#러너-이미지-목록)에 그 항목이 있으면, 작업이 OS를 유지하도록 라벨에 `os__ubuntu2204__`도 붙입니다. `ubuntu-latest`는 기본 이미지이므로 `os__` 선택자를 붙이지 않습니다. 다른 줄은 바꾸지 않으며, 다시 쓴 파일을 파싱했을 때 같은 작업이 나와야 받아들입니다.

Runquill은 병합하지 않고, 기존 브랜치에 푸시하지 않고, `.github/workflows` 밖에는 쓰지 않으며, 공개 저장소, App 설치 범위 밖의 저장소, 다른 소유자의 저장소는 건드리지 않습니다. 커밋이나 풀 리퀘스트가 실패하면, 자신이 만든 브랜치를 그 사이 다른 누구도 옮기지 않았을 때만 삭제합니다.

다음과 같은 작업은 바꾸지 않고, 미리보기에 이유를 표시합니다.

- 라벨에 맞는 규칙이 없는 경우
- `runs-on`이 표현식이거나, `group` 또는 `labels`를 쓰거나, 이미 self-hosted나 Runquill 러너를 대상으로 하거나, 없는 경우
- 작업이 재사용 워크플로를 호출하거나, 파일 자체가 재사용 워크플로(`workflow_call`)인 경우
- 작업이 YAML 앵커나, 스캐너가 정확히 읽을 수 없는 형식을 쓰는 경우
- 작업 ID로 Runquill 라벨을 만들 수 없거나, `pool__`이나 `os__`로 시작하거나, 다른 작업 ID와 대소문자만 다른 경우

파일이 너무 크거나, YAML 문서가 여러 개 있거나, 탭으로 들여썼거나, 불필요한 캐리지 리턴이 있거나, 올바른 UTF-8이 아니면 파일 전체를 바꾸지 않습니다.

App에 권한이 없거나, 저장소의 워크플로 파일이 50개를 넘거나, 미리보기 이후 파일이 바뀌었거나, 맞는 작업이 없거나, 같은 변경을 위한 이전 브랜치가 이미 있거나, 설치의 저장소 접근 범위가 `RUNQUILL_REPOSITORY_SCOPE`와 다르면 요청을 거부합니다.

다시 쓰기 로직은 단위 테스트로, GitHub 호출은 가짜 GitHub API를 쓰는 테스트로 확인했습니다. 실제 GitHub에서는 아직 이전을 실행하지 않았으므로 첫 풀 리퀘스트의 diff를 꼼꼼히 검토하세요.

## 오프라인 워크플로 점검

`runquill workflow check`는 로컬 워크플로 파일을 읽고 문제를 보고합니다. 네트워크 요청은 하지 않습니다.

```sh
runquill workflow check                 # .github/workflows in the current directory
runquill workflow check --json path/to/workflow.yml
```

| 코드 | 심각도 | 내용 |
| --- | --- | --- |
| `untrusted-trigger` | high | self-hosted 작업이 `pull_request_target`이나 `workflow_run`에서 실행됩니다. |
| `fork-trigger` | warning | self-hosted 작업이 포크 코드를 실행할 수 있는 `pull_request`나 리뷰 이벤트에서 실행됩니다. |
| `runs-on` | warning | Runquill 라벨 형식이 잘못되었습니다. 유일한 라벨이 아니거나, `format()`으로 만들지 않았거나, `strategy.job-index`가 없는 매트릭스 작업(`strategy`에 `matrix` 키가 있는 작업)이거나, 풀이나 `os__` 접미사가 잘못되었거나, 100자를 넘을 수 있는 라벨입니다. Runquill이 처리하지 않을 self-hosted 작업도 표시합니다. |
| `duplicate-suffix` | warning | 같은 파일의 두 작업이 같은 Runquill 라벨을 만들어, 어느 작업이든 다른 작업의 러너를 가져갈 수 있습니다. `pool__`과 `os__` 선택자를 포함한 소문자 접미사 전체를 비교하며, `strategy.job-index`를 쓰는 매트릭스 접미사는 `test-1` 같은 고정 접미사와도 일치하는 것으로 봅니다. |
| `pool` | warning | 라벨이 설정된 워커 풀에 없는 풀을 지정합니다. |
| `runner-image` | warning | 라벨이 러너 이미지 목록에 없는 `os__NAME__` 이미지를 고르거나, 풀의 `images` 목록이 허용하지 않는 이미지를 고릅니다. |
| `docker` | warning | 작업이 Docker를 쓰는 것으로 보이지만(추정), 풀에 `privileged: false`가 설정되어 있습니다. |
| `permissions` | warning | 워크플로에 최상위 `permissions` 블록이 없습니다. |
| `timeout` | warning | 작업에 `timeout-minutes`가 없습니다. `uses:`로 재사용 워크플로를 호출하는 작업에는 보고하지 않습니다. |
| `unpinned-action` | info | `actions/`와 `github/` 밖의 액션이 전체 커밋 SHA로 고정되어 있지 않습니다. |
| `unparsed` | warning 또는 info | 파일이나 작업을 점검할 수 없었습니다. |

풀 점검은 매니저가 적용할 워커 풀을 사용합니다. `RUNQUILL_DB`가 패널에서 저장한 운영 설정이 있는 기존 대기열 데이터베이스를 가리키면, 명령은 이를 읽기 전용으로 열어 그 풀을 사용하고 표준 오류에 안내를 출력합니다. 이렇게 하려면 매니저 환경에서 실행합니다. 그렇지 않거나 데이터베이스를 읽을 수 없으면, 명령의 환경에 `RUNQUILL_WORKER_POOLS`가 설정되어 있을 때만 풀 점검을 실행합니다. `runner-image` 점검은 명령을 실행하는 바이너리에 내장된 러너 이미지 목록에 그 환경의 `RUNQUILL_RUNNER_IMAGES`를 더해 사용합니다. 릴리스 빌드는 게시된 변형을 모두 압니다. 매니저에 `RUNQUILL_RUNNER_IMAGES`를 설정했다면 명령에도 같은 값을 설정합니다. [러너 이미지](runner-images.ko.md)

high 심각도 항목이 있으면, 또는 `--strict`에서는 어떤 항목이든 있으면 상태 코드 1로 종료합니다. 디렉터리가 없거나, 파일을 읽을 수 없거나, 1 MiB보다 큰 파일이 있으면 상태 코드 2로 종료합니다. 그 밖에는 0으로 종료합니다. `--json`은 `{"files": N, "findings": [...]}`를 출력하며, 각 항목에는 `file`, `line`, 선택 항목인 `job`, `severity`, `code`, `message`가 있습니다.
