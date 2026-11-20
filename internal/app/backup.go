package app

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/yakisoba0728/Runquill/internal/backupseal"
)

const (
	queueArchiveID = 0x52514241 // RQBA
	queueFencedID  = 0x5251464e // RQFN
	queueBackupV1  = 1
)

const queueBackupMetadata = `CREATE TABLE runquill_backup (owner TEXT NOT NULL, app_id TEXT NOT NULL)`
const queueBackupOptionalJobColumns = "jit_requested_ms jit_registered_ms runner_launch_ms"

var (
	errQueueBackupIdentity          = errors.New("queue backup belongs to another GitHub owner or app")
	errBackupVerificationPassphrase = errors.New("could not load the backup passphrase for verification")
)

type queueBackupIdentity struct {
	owner, appID string
}

var queueBackupTables = []struct {
	name, columns string
	baseColumns   int
}{
	{"jobs", "sequence job_id repository repository_id installation_id route phase changed_at runner_name runner_id launched_at retry_at " +
		"retry_reason runner_host github_created_at discovered_at provisioned_at github_started_at discovery_source runner_cleaned_at runner_cleaned_host failure", 12},
	{"docker_hosts", "host cordoned untracked_slots", 2},
	{"repository_polls", "installation_id repository_id checked_at full_at active_at last_error", 6},
	{"operation_settings", "id revision policy", 3},
	{"worker_authorities", "host engine token_digest", 3},
}

func runQueueBackup(ctx context.Context, out io.Writer, command string, args []string) error {
	flags := flag.NewFlagSet("runquill "+command, flag.ExitOnError)
	path := env("RUNQUILL_DB", "runquill.sqlite3")
	passphraseFile := flags.String("passphrase-file", "", "read the backup passphrase from this file instead of RUNQUILL_BACKUP_PASSPHRASE")
	if command == "backup" {
		destination := flags.String("output", "", "write a new private SQLite queue backup")
		seal := flags.Bool("seal", false, "encrypt the backup with the backup passphrase")
		upload := flags.Bool("upload", false, "upload the backup to the configured S3-compatible bucket")
		flags.Parse(args)
		if (*destination == "" && !*upload) || flags.NArg() != 0 || (*passphraseFile != "" && !*seal) {
			return errors.New("usage: runquill backup --output FILE [--seal [--passphrase-file FILE]] or runquill backup --upload [--output FILE] [--seal [--passphrase-file FILE]]")
		}
		if *upload {
			return uploadBackupCommand(ctx, out, path, *destination, *seal, *passphraseFile)
		}
		if !*seal {
			return backupQueue(ctx, out, path, *destination)
		}
		return sealedBackupCommand(ctx, out, path, *destination, *passphraseFile)
	}

	source := flags.String("from", "", "prepare a recovery queue from this backup")
	remote := flags.String("from-s3", "", "prepare a recovery queue from this backup in the configured S3-compatible bucket")
	activate := flags.Bool("activate", false, "lift the recovery fence after stopping the previous fleet and checking its runners")
	flags.Parse(args)
	chosen := 0
	for _, set := range []bool{*source != "", *remote != "", *activate} {
		if set {
			chosen++
		}
	}
	if flags.NArg() != 0 || chosen != 1 || (*activate && *passphraseFile != "") {
		return errors.New("usage: runquill restore --from FILE [--passphrase-file FILE], runquill restore --from-s3 KEY [--passphrase-file FILE] or runquill restore --activate")
	}
	if *activate {
		return activateQueue(ctx, out, path)
	}
	if *remote != "" {
		return restoreFromS3Command(ctx, out, *remote, path, *passphraseFile)
	}
	return restoreQueueFrom(ctx, out, *source, path, *passphraseFile)
}

func sealedBackupCommand(ctx context.Context, out io.Writer, source, destination, passphraseFile string) error {
	identity, err := currentQueueBackupIdentity()
	if err != nil {
		return err
	}
	passphrase, err := backupPassphrase(os.Getenv, passphraseFile)
	if err != nil {
		return err
	}
	if err := sealedQueueBackup(ctx, source, destination, identity, passphrase); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Created sealed Runquill queue backup %s.\n", destination)
	return err
}

func backupQueue(ctx context.Context, out io.Writer, source, destination string) error {
	identity, err := currentQueueBackupIdentity()
	if err != nil {
		return err
	}
	if err := publishQueueCopy(ctx, source, destination, identity, queueArchiveID); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Created Runquill queue backup %s.\n", destination)
	return err
}

func restoreQueue(ctx context.Context, out io.Writer, source, destination string) error {
	return restoreQueueFrom(ctx, out, source, destination, "")
}

// restoreQueueFrom accepts a plain or sealed backup. A sealed one is fully
// authenticated into a private temporary file before the restore starts.
func restoreQueueFrom(ctx context.Context, out io.Writer, source, destination, passphraseFile string) error {
	identity, err := currentQueueBackupIdentity()
	if err != nil {
		return err
	}
	path, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	lock, err := lockQueue(path)
	if err != nil {
		return err
	}
	defer lock.Close()
	sealed, err := isSealedBackup(source)
	if err != nil {
		return err
	}
	if sealed {
		passphrase, err := backupPassphrase(os.Getenv, passphraseFile)
		if err != nil {
			return err
		}
		if len(passphrase) == 0 {
			return backupseal.ErrMissingPassphrase
		}
		if err := checkNewQueuePath(path); err != nil {
			return err
		}
		staging, err := os.MkdirTemp(filepath.Dir(path), ".runquill-unseal-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		plain := filepath.Join(staging, "queue.sqlite3")
		if err := unsealFileTo(ctx, source, plain, passphrase); err != nil {
			return err
		}
		source = plain
	}
	if err := publishQueueCopy(ctx, source, path, identity, queueFencedID); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Prepared recovery queue %s. Stop the prior manager, check for outstanding runners, then run restore --activate with RUNQUILL_DB set to this queue.\n", path)
	return err
}

func publishQueueCopy(ctx context.Context, source, destination string, identity queueBackupIdentity, artifactID int) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	if err := validateQueueFile(source); err != nil {
		return err
	}
	if err := checkNewQueuePath(destination); err != nil {
		return err
	}

	parent := filepath.Dir(destination)
	staging, err := os.MkdirTemp(parent, ".runquill-queue-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	target := filepath.Join(staging, "queue.sqlite3")
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := copyQueueFile(ctx, source, target, identity, artifactID); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkNewQueuePath(destination); err != nil {
		return err
	}
	// Link publishes a complete snapshot without overwriting another writer's file.
	if err := os.Link(target, destination); err != nil {
		return fmt.Errorf("publish queue file: %w", err)
	}
	dir, err := os.Open(parent)
	if err != nil {
		return fmt.Errorf("published %s but could not open its directory for sync: %w", destination, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("published %s but could not sync its directory: %w", destination, err)
	}
	return nil
}

func checkNewQueuePath(path string) error {
	// A leftover journal can apply an old database's writes to a newly published file.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("%s already exists. Choose a new database path", path+suffix)
			}
			return err
		}
	}
	return nil
}

func copyQueueFile(ctx context.Context, source, target string, identity queueBackupIdentity, artifactID int) error {
	db, err := openQueueDB(target, "rw")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	if err := (&queue{DB: db}).migrate(); err != nil {
		return err
	}
	sourceURI := url.URL{Scheme: "file", Path: source, RawQuery: "mode=ro"}
	if _, err := db.ExecContext(ctx, "PRAGMA trusted_schema=OFF"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "ATTACH DATABASE ? AS source", sourceURI.String()); err != nil {
		return fmt.Errorf("open source queue: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var sourceID int
	if err := tx.QueryRowContext(ctx, "PRAGMA source.application_id").Scan(&sourceID); err != nil {
		return err
	}
	if artifactID == queueFencedID {
		if err := validateQueueArchive(ctx, tx, "source", sourceID, identity); err != nil {
			return err
		}
	} else if sourceID != 0 {
		return errors.New("back up an active Runquill queue; this file has another format")
	}

	// Copy a single read snapshot of the allowlisted queue data. Do not copy enrollment codes or other credentials.
	for _, table := range queueBackupTables {
		columns := strings.Fields(table.columns)
		required := table.baseColumns
		if artifactID == queueFencedID {
			required = len(columns)
		}
		if table.name == "jobs" {
			// Earlier version 1 backups have no stage timing columns.
			columns = append(columns, strings.Fields(queueBackupOptionalJobColumns)...)
		}
		if err := copyQueueTable(ctx, tx, table.name, columns, required, artifactID == queueFencedID); err != nil {
			return fmt.Errorf("copy queue table %s: %w", table.name, err)
		}
	}
	var foreignOwner bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM jobs
	    WHERE instr(repository, '/') = 0 OR lower(substr(repository, 1, instr(repository, '/') - 1)) <> ?)`, identity.owner).
		Scan(&foreignOwner); err != nil {
		return err
	}
	if foreignOwner {
		return errors.New("queue contains jobs for another account. Check RUNQUILL_DB and RUNQUILL_OWNER")
	}
	if _, err := tx.ExecContext(ctx, queueBackupMetadata); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO runquill_backup VALUES (?, ?)", identity.owner, identity.appID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d; PRAGMA user_version=%d", artifactID, queueBackupV1)); err != nil {
		return err
	}
	return tx.Commit()
}

func copyQueueTable(ctx context.Context, tx *sql.Tx, name string, columns []string, required int, fullSchema bool) error {
	var kind string
	err := tx.QueryRowContext(ctx, "SELECT type FROM pragma_table_list(?) WHERE schema='source' AND name=?", name, name).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) && (name == "operation_settings" || name == "worker_authorities" || name != "jobs" && !fullSchema) {
		return nil
	}
	if err != nil {
		return err
	}
	if kind != "table" {
		return errors.New("expected a regular SQLite table")
	}
	rows, err := tx.QueryContext(ctx, "SELECT name FROM pragma_table_info(?, 'source')", name)
	if err != nil {
		return err
	}
	defer rows.Close()
	available := make(map[string]bool)
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return err
		}
		available[column] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	present := make([]string, 0, len(columns))
	for index, column := range columns {
		if available[column] {
			present = append(present, column)
		} else if index < required {
			return fmt.Errorf("missing queue column %s", column)
		}
	}
	list := strings.Join(present, ",")
	_, err = tx.ExecContext(ctx, "INSERT INTO main."+name+" ("+list+") SELECT "+list+" FROM source."+name)
	return err
}

// validateQueueArchive checks the archive attached as schema ("source" or "main").
func validateQueueArchive(ctx context.Context, tx *sql.Tx, schema string, sourceID int, identity queueBackupIdentity) error {
	if sourceID != queueArchiveID {
		return errors.New("input is not a Runquill queue backup")
	}
	var format int
	if err := tx.QueryRowContext(ctx, "PRAGMA "+schema+".user_version").Scan(&format); err != nil {
		return err
	}
	if format != queueBackupV1 {
		return fmt.Errorf("unsupported Runquill queue backup version %d", format)
	}
	var check string
	if err := tx.QueryRowContext(ctx, "PRAGMA "+schema+".integrity_check(1)").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return errors.New("queue backup failed the SQLite integrity check")
	}
	return checkQueueBackupIdentity(ctx, tx, schema, identity)
}

// verifyQueueBackup applies the restore checks to a finished backup file.
func verifyQueueBackup(ctx context.Context, path string, identity queueBackupIdentity) error {
	if err := validateQueueFile(path); err != nil {
		return err
	}
	db, err := openQueueDB(path, "ro")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA trusted_schema=OFF"); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int
	if err := tx.QueryRowContext(ctx, "PRAGMA main.application_id").Scan(&id); err != nil {
		return fmt.Errorf("verify queue backup: %w", err)
	}
	if err := validateQueueArchive(ctx, tx, "main", id, identity); err != nil {
		return fmt.Errorf("verify queue backup: %w", err)
	}
	return nil
}

func verifyLocalBackup(ctx context.Context, path string) error {
	identity, err := currentQueueBackupIdentity()
	if err != nil {
		return err
	}
	if strings.HasSuffix(path, ".sealed") {
		passphrase, err := backupPassphrase(os.Getenv, "")
		if err != nil || backupseal.CheckPassphrase(passphrase) != nil {
			return errBackupVerificationPassphrase
		}
		dir, err := os.MkdirTemp("", "runquill-verify-backup-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		plain := filepath.Join(dir, "queue.sqlite3")
		if err := unsealFileTo(ctx, path, plain, passphrase); err != nil {
			return err
		}
		path = plain
	}
	return verifyQueueBackup(ctx, path, identity)
}

func currentQueueBackupIdentity() (queueBackupIdentity, error) {
	id := queueBackupIdentity{
		owner: strings.ToLower(strings.TrimSpace(os.Getenv("RUNQUILL_OWNER"))),
		appID: strings.TrimSpace(os.Getenv("RUNQUILL_APP_ID")),
	}
	if !setupLogin.MatchString(id.owner) || id.appID == "" {
		return id, errors.New("set RUNQUILL_OWNER and RUNQUILL_APP_ID to identify the queue's GitHub App")
	}
	return id, nil
}

func checkQueueBackupIdentity(ctx context.Context, tx *sql.Tx, database string, expected queueBackupIdentity) error {
	var owner, appID string
	if err := tx.QueryRowContext(ctx, "SELECT owner,app_id FROM "+database+".runquill_backup").Scan(&owner, &appID); err != nil {
		return fmt.Errorf("read queue backup identity: %w", err)
	}
	if owner != expected.owner || appID != expected.appID {
		return errQueueBackupIdentity
	}
	return nil
}

func validateQueueFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("expected a SQLite queue file at %s", path)
	}
	return nil
}

func checkQueueReady(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	db, err := openQueueDB(path, "ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var id int
	if err := db.QueryRow("PRAGMA application_id").Scan(&id); err != nil {
		return err
	}
	switch id {
	case 0:
		return nil
	case queueArchiveID:
		return errors.New("restore the Runquill queue backup to a new queue before starting the manager")
	case queueFencedID:
		return errors.New("the restored queue is fenced. Stop the prior manager and check its runners before running restore --activate")
	default:
		return errors.New("the database has an unexpected SQLite application ID")
	}
}

func activateQueue(ctx context.Context, out io.Writer, path string) error {
	identity, err := currentQueueBackupIdentity()
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	lock, err := lockQueue(path)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := validateQueueFile(path); err != nil {
		return err
	}
	db, err := openQueueDB(path, "rw")
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id, format int
	if err := tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&id); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&format); err != nil {
		return err
	}
	if id != queueFencedID || format != queueBackupV1 {
		return errors.New("activate a prepared Runquill recovery queue")
	}
	if err := checkQueueBackupIdentity(ctx, tx, "main", identity); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE runquill_backup; PRAGMA application_id=0; PRAGMA user_version=0"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Activated recovery queue %s. Start the manager with this RUNQUILL_DB.\n", path)
	return err
}
