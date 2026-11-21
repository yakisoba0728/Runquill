# Scheduled, sealed and S3 backups

[Runquill README](../README.md) · [한국어](backups.ko.md)

A queue backup is a consistent SQLite copy of jobs, runner leases, host maintenance state, repository scan history and operational settings. It includes enrolled outbound workers' host and engine IDs and token digests. It excludes worker enrollment codes and observations, panel users, sessions, tokens, audit records and [usage statistics](panel.md#usage-statistics). The basic `runquill backup` and `runquill restore` flow is in [Back up and recover the queue](operations.md#back-up-and-recover-the-queue). This guide adds encryption, a schedule and offsite copies.

## Sealed backups

`runquill backup --output FILE --seal` encrypts the backup with a passphrase of at least 12 characters from `RUNQUILL_BACKUP_PASSPHRASE`, `RUNQUILL_BACKUP_PASSPHRASE_FILE` or `--passphrase-file`. The passphrase is never accepted as an argument. Set only one of the two variables. A passphrase file may end with a newline.

A sealed file uses AES-256-GCM with a key derived by PBKDF2-HMAC-SHA256 (600,000 iterations) from the passphrase and a random 32-byte salt, so every file has its own key. The data is encrypted in 64 KiB chunks. Each chunk is authenticated together with the file header, its position and whether it is the last one. A wrong passphrase, a changed header, or a reordered, dropped, appended or truncated chunk makes the restore fail before any data is used. The file still reveals that it is a Runquill backup and its approximate size. While sealing or unsealing, Runquill writes the plaintext copy only to a private temporary directory beside the destination and deletes it afterwards. Keep the passphrase in your secret store: a lost passphrase cannot be recovered.

The panel's **Download queue backup** is never sealed.

## Scheduled backups

The manager takes a backup one minute after start and then once per interval when these variables are set.

| Variable | Default | Effect |
| --- | --- | --- |
| `RUNQUILL_BACKUP_INTERVAL` | unset (off) | A Go duration of at least `1m`, such as `24h`. |
| `RUNQUILL_BACKUP_DIR` | required | Directory for the backups, created with mode `0700`. |
| `RUNQUILL_BACKUP_KEEP` | `7` | Local copies to keep, `1` to `1000`. Older ones are deleted. |
| `RUNQUILL_BACKUP_PASSPHRASE` | unset | Seal every scheduled backup. |
| `RUNQUILL_BACKUP_PASSPHRASE_FILE` | unset | Read the passphrase from a file instead. |

Files are named `runquill-queue-YYYYMMDDTHHMMSSZ.sqlite3`, with `.sealed` appended when sealed. Each one is checked against the manager's `RUNQUILL_OWNER` and `RUNQUILL_APP_ID` before it is published, and only files with this name pattern are pruned. Invalid settings stop the manager at startup. A failed run is logged, shown on the panel's **Backups** screen and retried at the next interval.

On **Backups**, an administrator can use **Verify latest backup** to check the most recent retained local copy. The manager authenticates a sealed copy with its configured passphrase and checks the SQLite format, integrity and GitHub App identity. The check keeps the original file and active queue intact. The panel shows the file, time and result from the latest verification since startup. Completed and failed checks are recorded as `queue.backup.verify` attempts in the audit log.

## Upload to S3-compatible storage

Add these variables to copy each scheduled backup to a bucket.

| Variable | Default | Effect |
| --- | --- | --- |
| `RUNQUILL_BACKUP_S3_ENDPOINT` | required | Endpoint URL without a path, such as `https://s3.eu-central-1.amazonaws.com`. HTTP is allowed only for a loopback address. |
| `RUNQUILL_BACKUP_S3_BUCKET` | required | Bucket name. |
| `RUNQUILL_BACKUP_S3_REGION` | `us-east-1` | Signing region. |
| `RUNQUILL_BACKUP_S3_PREFIX` | empty | Key prefix such as `runquill/`. |
| `RUNQUILL_BACKUP_S3_ACCESS_KEY_ID` | required | Access key. |
| `RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY` | | Secret key. Set this or the file variant. |
| `RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY_FILE` | | Read the secret key from a file. |
| `RUNQUILL_BACKUP_S3_PATH_STYLE` | `false` | Use `endpoint/bucket/key` instead of `bucket.endpoint/key`, as many self-hosted stores need. |
| `RUNQUILL_BACKUP_S3_KEEP` | `7` | Objects to keep under the prefix, `0` to `1000`. `0` never deletes. |
| `RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT` | `false` | Allow unsealed uploads. |

Runquill refuses to upload an unsealed backup. With a schedule and a bucket configured but no passphrase, the manager does not start unless `RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true`, and then it logs a warning for every unsealed upload. `runquill backup --upload` without `--seal` is refused the same way. Uploads use AWS Signature Version 4, never follow redirects, and are confirmed by comparing the object size. After each run the manager uploads any kept local backup not yet confirmed, oldest first, then deletes the oldest Runquill objects beyond `RUNQUILL_BACKUP_S3_KEEP`. A failed upload is retried at the next run.

`runquill backup --upload --seal` makes and uploads a single backup with the same settings. Add `--output FILE` to keep a local copy too.

The scheduled sealed upload and restore path has also been tested against an authenticated local SeaweedFS S3 service. To test your own S3-compatible service, set the S3 connection variables above for a test bucket and run `RUNQUILL_TEST_BACKUP_S3=1 go test ./internal/app -run '^TestLiveS3BackupRestore$' -v`. The smoke test writes one synthetic backup under a unique subprefix, restores and checks its queue, and deletes only that test object. It needs permission to put, read and delete that object.

## Compose

Compose does not pass these variables or mount a backup directory by default. Save an overlay such as `compose.backups.yaml` beside `compose.yaml`, create `backup-passphrase` with `(umask 077; openssl rand -base64 32 > backup-passphrase)`, and keep a copy of it outside the server.

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

Apply it with `docker compose -f compose.yaml -f compose.backups.yaml up -d`, adding your other overlays and `--no-build` for published images. Remove the S3 lines if you do not upload. On K3s, add the variables to the `runquill-config` Secret and store the backup directory on a separate volume.

## Restore

A restore prepares a new, fenced queue file, exactly like a plain restore. Run it with the same `RUNQUILL_OWNER` and `RUNQUILL_APP_ID` as the backup, then follow the activation steps in [Back up and recover the queue](operations.md#back-up-and-recover-the-queue).

From a sealed file, provide the passphrase through the environment or `--passphrase-file`. Runquill detects the sealed format by its header. With the overlay above, the backup directory and passphrase are already mounted.

```sh
docker compose -f compose.yaml -f compose.backups.yaml run --rm --no-deps -e RUNQUILL_DB=/data/recovered.sqlite3 \
    manager restore --from /backups/runquill-queue-20260101T000000Z.sqlite3.sealed
```

From S3, name the object by its file name under `RUNQUILL_BACKUP_S3_PREFIX` or by its full key. Runquill downloads it beside the destination and restores it the same way.

```sh
docker compose -f compose.yaml -f compose.backups.yaml run --rm --no-deps -e RUNQUILL_DB=/data/recovered.sqlite3 \
    manager restore --from-s3 runquill-queue-20260101T000000Z.sqlite3.sealed
```

Restore from S3 needs only the S3 variables and the passphrase. It does not need `RUNQUILL_BACKUP_INTERVAL`.
