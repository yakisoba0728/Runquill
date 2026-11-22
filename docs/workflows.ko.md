# GitHub Actions에서 Runquill 사용

[Runquill README](../README.ko.md) · [English](workflows.md)

## 사용할 저장소 연결

- **전체 저장소**로 설치했다면 같은 개인 계정이나 조직 소유의 다른 비공개 저장소에도 아래 워크플로를 적용할 수 있습니다. 공개 저장소는 [아래 설정](#공개-저장소에서-작업-실행)을 켜야 합니다. Runquill 인증 정보와 웹훅은 App 설치에 속하므로 저장소마다 Runquill 시크릿이나 웹훅을 추가할 필요가 없습니다.
- **선택한 저장소**로 설치했다면 GitHub의 App 설치 설정에서 대상 저장소를 추가합니다.
- 다른 조직이 소유한 저장소에서 쓰려면 해당 조직에 [별도 설치](install.ko.md#github-조직-연결)를 구성합니다.

## 첫 작업 확인

비공개 Git 저장소를 내려받은 디렉터리의 루트에서 [첫 작업용 워크플로](../examples/runquill-first-job.yml)를 만듭니다. 빠른 시작과 설치 위치가 다르다면 경로를 맞춰 실행합니다.

```sh
"$HOME/runquill/runquill" workflow init
```

이 명령은 `.github/workflows/runquill-first-job.yml`을 생성하고 같은 경로의 기존 파일을 보호합니다. `workflow init`이 추가되기 전 버전을 쓰거나 실행 파일이 다른 서버에 있다면 링크의 YAML을 내 저장소의 `.github/workflows/`에 직접 복사합니다.

워크플로를 저장소의 기본 브랜치에 커밋하고 푸시하거나 기존 리뷰 절차로 병합합니다. GitHub에서 **Actions → Runquill first job → Run workflow**를 누릅니다. **Show the runner** 단계에 `runquill-`로 시작하는 이름이 출력됩니다. 이 작업은 쉘만 사용하므로 애플리케이션 의존성 없이 러너 배정을 먼저 확인할 수 있습니다. [GitHub 수동 실행 요구사항](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow)

서버에서는 처음 배포할 때 사용한 Compose 파일로 App이 저장소에 접근할 수 있는지 확인하고 러너 정리 과정을 지켜봅니다. 아래 명령은 패널 오버레이를 사용하는 설치 기준입니다.

```sh
docker compose -f compose.yaml -f compose.panel.yaml exec -T manager runquill doctor --repo OWNER/REPO
docker compose -f compose.yaml -f compose.panel.yaml logs -f manager
```

`doctor`는 로컬 설정, Docker Engine, 러너 이미지, GitHub App 설치와 권한, 설정했다면 웹훅 설정까지 점검합니다. 점검에 실패한 항목이 있으면 상태 코드 1로 종료합니다. 작업이 성공하면 매니저 로그에서 `runner removed`를 확인하고, 저장소의 **Settings → Actions → Runners**에서 해당 러너가 사라졌는지 확인합니다. Docker 빌드와 서비스 컨테이너까지 확인하려면 [Docker·PostgreSQL 검증 워크플로](../examples/runquill-smoke.yml)를 실행합니다.

## 워크플로에서 러너 선택

[스모크 워크플로](../examples/runquill-smoke.yml)는 기본값으로 내 서버를 사용합니다. **Run workflow**에서 `github_hosted`를 선택하면 `ubuntu-latest`를 사용합니다. [병렬 작업 예제](../examples/runquill-parallel.yml)에는 같은 옵션을 쓰는 단일 작업 1개와 매트릭스 작업 2개가 있습니다. `.github/workflows/`에 복사해 수동 실행하면 각 작업 로그에서 배정된 러너 이름을 볼 수 있습니다.

라벨에는 워크플로 실행 ID, 실행 시도 번호, 해당 실행에서 작업마다 고유한 접미사를 넣습니다. 작업 ID는 식에 직접 적습니다. 예를 들어 `check` 작업은 다음 식을 사용합니다.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-check', github.run_id, github.run_attempt) }}
```

매트릭스 작업은 작업 ID 뒤에 `strategy.job-index`를 붙입니다. 인덱스는 0부터 시작하므로 예제의 `test` 작업은 `test-0`, `test-1`로 구분됩니다. 워크플로를 다시 실행하면 `run_attempt`가 바뀝니다.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-test-{2}', github.run_id, github.run_attempt, strategy.job-index) }}
```

이 식을 `runs-on`의 단일 라벨로 사용합니다. 접미사는 영문자나 숫자로 시작하고 이후에는 하이픈과 밑줄도 쓸 수 있습니다. 라벨 전체는 100자 이내여야 합니다. Runquill은 작업마다 요청한 라벨로 일회용 러너를 등록하고, GitHub는 라벨이 일치하는 러너에 작업을 배정합니다. [GitHub Actions 컨텍스트 문서](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts)

기존 워크플로에서는 옮길 작업의 `runs-on`만 바꾸고 테스트를 실행합니다. 풀을 지정하려면 [워커 풀 규칙](workers.ko.md#워커-풀로-작업-분리)을 따르세요. 신규 워크플로에 붙여 넣을 완성된 YAML은 [첫 작업 예제](../examples/runquill-first-job.yml)에 있습니다.

### 러너 OS 선택

같은 작업별 라벨 안에서 풀 선택자 뒤에 `os__NAME__`을 넣어 OS 변형을 고릅니다. 이름은 `ubuntu2204`, `ubuntu2604`, `debian12`, `debian13`처럼 점 없이 씁니다. 지정하지 않은 작업은 호스트의 이미지로 실행됩니다.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-os__ubuntu2204__build', github.run_id, github.run_attempt) }}
```

이름은 러너 이미지 목록에서 찾습니다. 릴리스 빌드에는 게시된 변형이 모두 들어 있고, `RUNQUILL_RUNNER_IMAGES`로 항목을 추가하거나 바꿀 수 있습니다. Runquill은 작업마다 고유한 라벨로 러너와 작업을 짝지으므로 `runs-on: ubuntu-22.04`처럼 GitHub 호스팅 라벨만 쓰는 방식은 지원하지 않습니다. [러너 이미지](runner-images.ko.md)

### 다음 작업에서 Docker 빌드 레이어 재사용

루트에 `Dockerfile`이 있는 저장소의 `.github/workflows/`에 [빌드 캐시 예제](../examples/runquill-build-cache.yml)를 복사합니다. 기본 브랜치의 커밋을 빌드하고, 같은 작업의 이후 단계에서 결과 이미지를 사용할 수 있습니다. 새 러너는 GitHub Actions 캐시에서 이전 빌드의 레이어를 복원할 수 있습니다. 이미지마다 `scope`를 하나씩 정해 캐시 입력과 출력 양쪽에 같은 값을 쓰세요. 캐시 저장에 실패해도 빌드 결과를 사용할 수 있습니다. [Docker GitHub Actions 캐시](https://docs.docker.com/build/cache/backends/gha/)

예제는 두 액션을 커밋 SHA로 고정하고 읽기 권한만 사용합니다. 캐시는 기본 브랜치에서 갱신합니다. 빌드 시크릿은 BuildKit 시크릿 마운트로 전달하면 캐시 레이어에 남지 않습니다. [Docker 빌드 시크릿](https://docs.docker.com/build/building/secrets/)

### 기존 워크플로 이전

관리자는 패널의 **마이그레이션** 화면에서 GitHub 호스팅 라벨을 쓰는 작업을 Runquill 라벨로 바꾸는 풀 리퀘스트를 열 수 있습니다. Contents, Pull requests, Workflows 쓰기 권한이 필요하며, 새 App에서는 `runquill setup --migrate`만 이 권한을 요청합니다. Runquill은 먼저 diff를 미리 보여주고 `runs-on`만 바꿉니다. 안전하게 바꿀 수 없는 작업은 이유와 함께 그대로 두며, 병합은 하지 않습니다. `ubuntu-22.04` 같은 GitHub 호스팅 Ubuntu 버전 라벨은 매니저의 러너 이미지 목록에 그 OS가 있으면 `os__` 선택자로 바뀝니다. 실제 GitHub 저장소에서는 아직 검증하지 않았습니다. [워크플로 이전](workflow-migration.ko.md)

`runquill workflow check`는 로컬 워크플로 파일에서 잘못된 Runquill 라벨, 같은 파일의 여러 작업이 함께 쓰는 접미사, 설정되지 않은 풀과 러너 OS 이름, self-hosted 러너의 위험한 트리거, 권한과 시간 제한 누락, 고정되지 않은 액션을 찾습니다. 심각도가 high인 항목이 있으면 상태 코드 1로 종료합니다. `--strict`를 붙이면 항목이 하나라도 있을 때 1로 종료하고, `--json`은 결과를 JSON으로 출력합니다. [점검 코드](workflow-migration.ko.md#오프라인-워크플로-점검)

## 공개 저장소에서 작업 실행

배포에서 `RUNQUILL_ALLOW_PUBLIC_REPOSITORIES=true`를 설정하지 않으면 Runquill은 공개 저장소의 작업을 무시합니다. 값을 `.env`에 두고 [Compose 오버레이](operations.ko.md#매니저-설정-추가)로 전달하거나 K3s의 `runquill-config` Secret에 추가합니다. 패널의 설정 화면에서는 바꿀 수 없으며, 켜져 있으면 패널 개요, `runquill status --json`, `runquill doctor`에 표시됩니다. 조직의 internal 저장소는 계속 제외합니다.

설정을 켜면 `RUNQUILL_OWNER`가 소유한 공개 저장소의 작업도 받고, 소유자, 저장소 ID, 보관(archived)·비활성 상태, App 설치 범위 같은 나머지 검사는 그대로 적용합니다.

### 모든 외부 기여자 승인 요구

공개 저장소에 러너를 띄우기 전에 Runquill은 저장소의 포크 풀 리퀘스트 승인 정책을 읽습니다. 정책은 `all_external_contributors`여야 합니다. 저장소의 **Settings → Actions → General**에서 포크 풀 리퀘스트 워크플로 승인 항목을 모든 외부 기여자 승인 요구로 설정합니다. 설정하기 전이나 GitHub App이 정책을 읽을 수 없는 동안에는 작업이 패널에 이유를 남기고 대기하며, 점점 긴 간격으로 다시 확인합니다. 정책을 읽으려면 저장소 **Administration** 읽기 권한이 필요합니다. 저장소에 러너를 등록하는 App에는 이미 있고, 조직 러너 그룹용으로 등록한 App에는 추가해야 합니다. 공개 저장소에서는 `runquill diagnose OWNER/REPO`가 결과를 보여 줍니다.

### 포크 실행이 러너를 쓰지 못하게 하는 방법

공개 저장소는 누구나 포크한 뒤 풀 리퀘스트에서 `runs-on`을 Runquill 라벨로 바꿀 수 있고, 라벨은 워크플로 파일과 실행 ID로 예측할 수 있습니다. GitHub는 같은 라벨로 대기 중인 저장소의 어떤 작업에도 Runquill 러너를 배정하므로, 포크 작업이 Runquill이 받은 작업의 라벨을 노릴 수 있습니다. Runquill은 두 가지로 막습니다.

- 작업 자체의 워크플로 실행이 포크나 삭제된 포크에서 왔다면 패널에 이유를 남기고 작업을 실패 처리합니다.
- 러너를 등록하기 전마다 저장소의 queued, in_progress, waiting 상태 워크플로 실행을 조회합니다. 포크에서 온 실행에 `runquill-` 라벨을 쓰는 미완료 작업이 있으면 그 실행 번호를 이유로 남기고 작업을 대기시킵니다. 그 실행을 취소하면 진행합니다.

러너를 띄울 때마다 승인 정책 조회 1회와 실행 목록 조회 3회, 미완료 포크 실행마다 작업 목록 조회가 추가됩니다. 비공개 저장소에서는 이 호출을 하지 않습니다. 남는 위험도 있습니다. 관리자가 Runquill 라벨을 쓰는 포크 풀 리퀘스트를 승인하면 그 실행이 끝나거나 취소될 때까지 공개 저장소 작업은 대기합니다. 승인하기 전에 `runs-on` 변경을 확인하세요. 검사와 러너가 작업을 잡는 사이의 몇 초 동안 승인된 포크 작업이 러너를 가져갈 수 있으며, Runquill은 러너가 다른 작업을 잡은 경우를 감지하지 않습니다.

### 원본 저장소에서 실행되는 워크플로

저장소 밖의 사람이 계기를 만들었더라도 GitHub가 원본 저장소에서 시작한 워크플로 실행은 그대로 실행합니다. `pull_request_target`, `workflow_run`, `issue_comment`, `issues`, `pull_request_review`, `pull_request_review_comment`, `discussion`, `discussion_comment`, `fork`, `watch`는 GitHub 계정만 있으면 누구나 원본 저장소의 워크플로를 시작할 수 있고, 이 워크플로는 저장소 시크릿을 읽을 수 있습니다. Runquill 라벨을 쓰는 작업에서는 풀 리퀘스트 코드를 체크아웃하거나 빌드하거나 실행하지 마세요. 이슈나 풀 리퀘스트의 제목·본문 같은 이벤트 값을 `run:` 스크립트에 그대로 넣으면 스크립트 인젝션이 되므로 `env:` 변수로 전달합니다. 오프라인 `fork-trigger` 점검은 일부 위험한 트리거를 알려 주지만 저장소가 공개인지는 알 수 없습니다.

### 공개 저장소 작업 분리

- 전용 호스트에서 `privileged: false`이고 `cache: true`를 쓰지 않으며 `repositories`를 공개 저장소로 제한한 비특권 [워커 풀](workers.ko.md#워커-풀로-작업-분리)로 실행합니다. 같은 저장소의 작업은 캐시 볼륨을 공유하므로 캐시를 쓰는 풀에서는 한 작업이 다음 작업을 오염시킬 수 있습니다.
- [조직 러너 그룹](install.ko.md#github-조직-연결)을 쓴다면 공개 저장소 전용 그룹을 만들고 공개 저장소를 허용합니다. 비공개 저장소를 그 그룹에 섞지 마세요. GitHub는 공개 저장소를 허용하지 않는 그룹에 공개 저장소의 작업을 배정하지 않으며, Runquill은 공개 저장소를 허용하지 않는데 공개 저장소가 들어 있는 그룹을 오류로 알립니다.

[GitHub self-hosted 러너 보안 강화](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions#hardening-for-self-hosted-runners) · [스크립트 인젝션](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions#understanding-the-risk-of-script-injections)
