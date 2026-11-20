package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/yakisoba0728/Runquill/internal/backupseal"
)

const maxPassphrase = 4 << 10

// backupPassphrase reads the passphrase from a file, RUNQUILL_BACKUP_PASSPHRASE
// or RUNQUILL_BACKUP_PASSPHRASE_FILE. It is never accepted on the command line.
func backupPassphrase(getenv func(string) string, file string) ([]byte, error) {
	value, fromEnv := getenv("RUNQUILL_BACKUP_PASSPHRASE"), getenv("RUNQUILL_BACKUP_PASSPHRASE_FILE")
	if file == "" {
		if value != "" && fromEnv != "" {
			return nil, errors.New("set only one of RUNQUILL_BACKUP_PASSPHRASE and RUNQUILL_BACKUP_PASSPHRASE_FILE")
		}
		if value != "" {
			return []byte(value), nil
		}
		file = fromEnv
	}
	if file == "" {
		return nil, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("read the backup passphrase file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPassphrase+1))
	if err != nil {
		return nil, errors.New("read the backup passphrase file")
	}
	if len(data) > maxPassphrase {
		return nil, errors.New("the backup passphrase file is too large")
	}
	return bytes.TrimRight(data, "\r\n"), nil
}

func isSealedBackup(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	magic := make([]byte, len(backupseal.Magic))
	if _, err := io.ReadFull(f, magic); err != nil {
		return false, nil
	}
	return string(magic) == backupseal.Magic, nil
}

// sealFileTo seals plain into target and confirms the sealed file decrypts
// to exactly the bytes that were sealed.
func sealFileTo(ctx context.Context, plain, target string, passphrase []byte) error {
	src, err := os.Open(plain)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	sum := sha256.New()
	if err := backupseal.Encrypt(out, io.TeeReader(contextReader{ctx, src}, sum), passphrase); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	check, err := os.Open(target)
	if err != nil {
		return err
	}
	defer check.Close()
	opened := sha256.New()
	if err := backupseal.Decrypt(opened, contextReader{ctx, check}, passphrase); err != nil {
		return fmt.Errorf("verify sealed backup: %w", err)
	}
	if !bytes.Equal(sum.Sum(nil), opened.Sum(nil)) {
		return errors.New("verify sealed backup: contents differ")
	}
	return nil
}

// unsealFileTo writes the verified plaintext of a sealed file to target.
func unsealFileTo(ctx context.Context, sealed, target string, passphrase []byte) error {
	src, err := os.Open(sealed)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := backupseal.Decrypt(out, contextReader{ctx, src}, passphrase); err != nil {
		return err
	}
	return out.Close()
}

// publishFile links a complete file into place without replacing anything.
func publishFile(target, destination string) error {
	if err := os.Link(target, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists. Choose a new path", destination)
		}
		return fmt.Errorf("publish backup file: %w", err)
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return fmt.Errorf("published %s but could not open its directory for sync: %w", destination, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("published %s but could not sync its directory: %w", destination, err)
	}
	return nil
}

// sealedQueueBackup writes a verified, sealed snapshot of the queue.
func sealedQueueBackup(ctx context.Context, source, destination string, identity queueBackupIdentity, passphrase []byte) error {
	if err := backupseal.CheckPassphrase(passphrase); err != nil {
		return err
	}
	destination, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("%s already exists. Choose a new path", destination)
		}
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".runquill-seal-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	plain := filepath.Join(staging, "queue.sqlite3")
	if err := publishQueueCopy(ctx, source, plain, identity, queueArchiveID); err != nil {
		return err
	}
	if err := verifyQueueBackup(ctx, plain, identity); err != nil {
		return err
	}
	sealed := filepath.Join(staging, "queue.sealed")
	if err := sealFileTo(ctx, plain, sealed, passphrase); err != nil {
		return err
	}
	return publishFile(sealed, destination)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// redactSecrets removes configured secrets from text bound for logs or status.
func redactSecrets(text string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}
