// Package backupseal implements the sealed Runquill backup format.
package backupseal

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
	Magic             = "RQSEALED"
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
)

var (
	ErrAuthentication = errors.New("the sealed backup is damaged or truncated, or the passphrase is wrong")
	ErrHeader         = errors.New("the sealed backup header is invalid")
)

// sealKey is replaceable so tests can memoize the deliberately slow derivation.
var sealKey = func(passphrase []byte, salt []byte, iterations int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, string(passphrase), salt, iterations, 32)
}

func sealHeader(salt []byte, iterations, chunk int) []byte {
	header := make([]byte, 0, len(Magic)+11+len(salt))
	header = append(header, Magic...)
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

// Encrypt encrypts src into dst with constant memory.
func Encrypt(dst io.Writer, src io.Reader, passphrase []byte) error {
	if err := CheckPassphrase(passphrase); err != nil {
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

// Decrypt authenticates and decrypts src into dst. dst receives
// plaintext before the end is verified, so callers write to a private
// temporary file and only use it once this returns nil.
func Decrypt(dst io.Writer, src io.Reader, passphrase []byte) error {
	if len(passphrase) == 0 {
		return ErrMissingPassphrase
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
			return ErrAuthentication
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
			return ErrAuthentication
		}
		sealNonce(nonce, index, final)
		opened, err := aead.Open(plain[:0], nonce, sealed[:n], header)
		if err != nil {
			return ErrAuthentication
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
	fixed := make([]byte, len(Magic)+7)
	if _, err := io.ReadFull(in, fixed); err != nil {
		return nil, nil, 0, 0, ErrHeader
	}
	if string(fixed[:len(Magic)]) != Magic {
		return nil, nil, 0, 0, errors.New("the file is not a sealed Runquill backup")
	}
	rest := fixed[len(Magic):]
	if rest[0] != sealVersion {
		return nil, nil, 0, 0, fmt.Errorf("unsupported sealed backup version %d", rest[0])
	}
	if rest[1] != sealKDFPBKDF2 {
		return nil, nil, 0, 0, ErrHeader
	}
	iterations = int(binary.BigEndian.Uint32(rest[2:6]))
	saltSize := int(rest[6])
	if iterations < sealMinIterations || iterations > sealMaxIterations || saltSize < sealMinSalt || saltSize > sealMaxSalt {
		return nil, nil, 0, 0, ErrHeader
	}
	tail := make([]byte, saltSize+4)
	if _, err := io.ReadFull(in, tail); err != nil {
		return nil, nil, 0, 0, ErrHeader
	}
	chunk = int(binary.BigEndian.Uint32(tail[saltSize:]))
	if chunk < sealMinChunk || chunk > sealMaxChunk {
		return nil, nil, 0, 0, ErrHeader
	}
	return append(fixed, tail...), tail[:saltSize], iterations, chunk, nil
}

var ErrMissingPassphrase = errors.New("set RUNQUILL_BACKUP_PASSPHRASE or --passphrase-file to seal or open a sealed backup")

func CheckPassphrase(passphrase []byte) error {
	if len(passphrase) == 0 {
		return ErrMissingPassphrase
	}
	if len(passphrase) < minPassphrase {
		return fmt.Errorf("the backup passphrase must be at least %d characters", minPassphrase)
	}
	return nil
}
