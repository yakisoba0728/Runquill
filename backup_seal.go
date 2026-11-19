package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Sealed backup format, version 1. Integers are big-endian.
//
//	magic       8 bytes  "RQSEALED"
//	version     1 byte   1
//	kdf         1 byte   1 = PBKDF2-HMAC-SHA256, 32-byte key for AES-256-GCM
//	iterations  4 bytes  600000 to 10000000
//	salt length 1 byte   16 to 64
//	salt        salt length bytes
//	chunk size  4 bytes  plaintext bytes per chunk, 4096 to 1048576
//
// The header is followed by one or more chunks. Each chunk is the AES-256-GCM
// ciphertext of its plaintext followed by the 16-byte tag. Every chunk but
// the last carries exactly chunk-size plaintext bytes; the last carries 0 to
// chunk-size bytes, so even an empty payload has one chunk. The 12-byte nonce
// is the 8-byte chunk index, three zero bytes and a final flag (1 on the last
// chunk only). Every chunk authenticates the whole header as additional data.
// The key is unique per file through its random salt, so counter nonces never
// repeat under one key. A reordered, dropped, appended or truncated chunk, a
// changed header, or a wrong passphrase all fail authentication.
const (
	sealMagic         = "RQSEALED"
	sealVersion       = 1
	sealKDFPBKDF2     = 1
	sealIterations    = 600_000
	sealMinIterations = 600_000
	sealMaxIterations = 10_000_000
	sealSaltSize      = 32
	sealMinSalt       = 16
	sealMaxSalt       = 64
	sealChunkSize     = 64 << 10
	sealMinChunk      = 4 << 10
	sealMaxChunk      = 1 << 20
	sealTagSize       = 16
	minPassphrase     = 12
	maxPassphrase     = 4 << 10
)

var (
	errSealedAuth   = errors.New("the sealed backup is damaged or truncated, or the passphrase is wrong")
	errSealedHeader = errors.New("the sealed backup header is invalid")
)

// sealKey is replaceable so tests can memoize the deliberately slow derivation.
var sealKey = func(passphrase []byte, salt []byte, iterations int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, string(passphrase), salt, iterations, 32)
}

func sealHeader(salt []byte, iterations, chunk int) []byte {
	header := make([]byte, 0, len(sealMagic)+11+len(salt))
	header = append(header, sealMagic...)
	header = append(header, sealVersion, sealKDFPBKDF2)
	header = binary.BigEndian.AppendUint32(header, uint32(iterations))
	header = append(header, byte(len(salt)))
	header = append(header, salt...)
	return binary.BigEndian.AppendUint32(header, uint32(chunk))
}

func sealAEAD(passphrase, salt []byte, iterations int) (cipher.AEAD, error) {
	key, err := sealKey(passphrase, salt, iterations)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealNonce(nonce []byte, index uint64, final bool) {
	clear(nonce)
	binary.BigEndian.PutUint64(nonce, index)
	if final {
		nonce[11] = 1
	}
}

// sealBackup encrypts src into dst with constant memory.
func sealBackup(dst io.Writer, src io.Reader, passphrase []byte) error {
	if err := checkSealPassphrase(passphrase); err != nil {
		return err
	}
	salt := make([]byte, sealSaltSize)
	rand.Read(salt)
	header := sealHeader(salt, sealIterations, sealChunkSize)
	aead, err := sealAEAD(passphrase, salt, sealIterations)
	if err != nil {
		return err
	}
	if _, err := dst.Write(header); err != nil {
		return err
	}
	in := bufio.NewReaderSize(src, sealChunkSize)
	plain := make([]byte, sealChunkSize)
	out := make([]byte, 0, sealChunkSize+sealTagSize)
	nonce := make([]byte, aead.NonceSize())
	for index := uint64(0); ; index++ {
		n, err := io.ReadFull(in, plain)
		final := false
		switch {
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
			final = true
		case err != nil:
			return err
		default:
			if _, err := in.Peek(1); errors.Is(err, io.EOF) {
				final = true
			} else if err != nil {
				return err
			}
		}
		sealNonce(nonce, index, final)
		if _, err := dst.Write(aead.Seal(out[:0], nonce, plain[:n], header)); err != nil {
			return err
		}
		if final {
			return nil
		}
	}
}

// unsealBackup authenticates and decrypts src into dst. dst receives
// plaintext before the end is verified, so callers write to a private
// temporary file and only use it once this returns nil.
func unsealBackup(dst io.Writer, src io.Reader, passphrase []byte) error {
	if len(passphrase) == 0 {
		return errSealPassphrase
	}
	in := bufio.NewReaderSize(src, sealMaxChunk+sealTagSize)
	header, salt, iterations, chunk, err := readSealHeader(in)
	if err != nil {
		return err
	}
	aead, err := sealAEAD(passphrase, salt, iterations)
	if err != nil {
		return err
	}
	sealed := make([]byte, chunk+sealTagSize)
	plain := make([]byte, 0, chunk)
	nonce := make([]byte, aead.NonceSize())
	for index := uint64(0); ; index++ {
		n, err := io.ReadFull(in, sealed)
		final := false
		switch {
		case errors.Is(err, io.EOF):
			return errSealedAuth
		case errors.Is(err, io.ErrUnexpectedEOF):
			final = true
		case err != nil:
			return err
		default:
			if _, err := in.Peek(1); errors.Is(err, io.EOF) {
				final = true
			} else if err != nil {
				return err
			}
		}
		if n < sealTagSize {
			return errSealedAuth
		}
		sealNonce(nonce, index, final)
		opened, err := aead.Open(plain[:0], nonce, sealed[:n], header)
		if err != nil {
			return errSealedAuth
		}
		if _, err := dst.Write(opened); err != nil {
			return err
		}
		if final {
			return nil
		}
	}
}

func readSealHeader(in io.Reader) (header, salt []byte, iterations, chunk int, err error) {
	fixed := make([]byte, len(sealMagic)+7)
	if _, err := io.ReadFull(in, fixed); err != nil {
		return nil, nil, 0, 0, errSealedHeader
	}
	if string(fixed[:len(sealMagic)]) != sealMagic {
		return nil, nil, 0, 0, errors.New("the file is not a sealed Runquill backup")
	}
	rest := fixed[len(sealMagic):]
	if rest[0] != sealVersion {
		return nil, nil, 0, 0, fmt.Errorf("unsupported sealed backup version %d", rest[0])
	}
	if rest[1] != sealKDFPBKDF2 {
		return nil, nil, 0, 0, errSealedHeader
	}
	iterations = int(binary.BigEndian.Uint32(rest[2:6]))
	saltSize := int(rest[6])
	if iterations < sealMinIterations || iterations > sealMaxIterations || saltSize < sealMinSalt || saltSize > sealMaxSalt {
		return nil, nil, 0, 0, errSealedHeader
	}
	tail := make([]byte, saltSize+4)
	if _, err := io.ReadFull(in, tail); err != nil {
		return nil, nil, 0, 0, errSealedHeader
	}
	chunk = int(binary.BigEndian.Uint32(tail[saltSize:]))
	if chunk < sealMinChunk || chunk > sealMaxChunk {
		return nil, nil, 0, 0, errSealedHeader
	}
	return append(fixed, tail...), tail[:saltSize], iterations, chunk, nil
}

var errSealPassphrase = errors.New("set RUNQUILL_BACKUP_PASSPHRASE or --passphrase-file to seal or open a sealed backup")

func checkSealPassphrase(passphrase []byte) error {
	if len(passphrase) == 0 {
		return errSealPassphrase
	}
	if len(passphrase) < minPassphrase {
		return fmt.Errorf("the backup passphrase must be at least %d characters", minPassphrase)
	}
	return nil
}

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
	magic := make([]byte, len(sealMagic))
	if _, err := io.ReadFull(f, magic); err != nil {
		return false, nil
	}
	return string(magic) == sealMagic, nil
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
	if err := sealBackup(out, io.TeeReader(contextReader{ctx, src}, sum), passphrase); err != nil {
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
	if err := unsealBackup(opened, contextReader{ctx, check}, passphrase); err != nil {
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
	if err := unsealBackup(out, contextReader{ctx, src}, passphrase); err != nil {
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
	if err := checkSealPassphrase(passphrase); err != nil {
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
