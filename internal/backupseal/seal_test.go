package backupseal

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"
)

const testPassphrase = "correct horse battery staple"

// memoizeSealKey keeps the real KDF but avoids repeating it under -race.
func memoizeSealKey(t *testing.T) {
	t.Helper()
	real := sealKey
	var mu sync.Mutex
	cache := map[string][]byte{}
	sealKey = func(passphrase, salt []byte, iterations int) ([]byte, error) {
		id := string(passphrase) + "\x00" + string(salt) + "\x00" + string(binary.BigEndian.AppendUint32(nil, uint32(iterations)))
		mu.Lock()
		defer mu.Unlock()
		if key, ok := cache[id]; ok {
			return key, nil
		}
		key, err := real(passphrase, salt, iterations)
		if err == nil {
			cache[id] = key
		}
		return key, err
	}
	t.Cleanup(func() { sealKey = real })
}

func sealBytes(t testing.TB, payload []byte) []byte {
	t.Helper()
	var sealed bytes.Buffer
	if err := Encrypt(&sealed, bytes.NewReader(payload), []byte(testPassphrase)); err != nil {
		t.Fatal(err)
	}
	return sealed.Bytes()
}

func TestBackupSealRoundTripAndFraming(t *testing.T) {
	memoizeSealKey(t)
	for _, size := range []int{0, 1, sealChunkSize, 2*sealChunkSize + 5} {
		payload := make([]byte, size)
		rand.Read(payload)
		sealed := sealBytes(t, payload)
		header := len(Magic) + 7 + sealSaltSize + 4
		chunks := max(1, (size+sealChunkSize-1)/sealChunkSize)
		if want := header + size + chunks*sealTagSize; len(sealed) != want {
			t.Fatalf("size %d: sealed length %d, want %d", size, len(sealed), want)
		}
		if !bytes.HasPrefix(sealed, []byte(Magic)) {
			t.Fatal("missing magic")
		}
		var opened bytes.Buffer
		if err := Decrypt(&opened, bytes.NewReader(sealed), []byte(testPassphrase)); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(opened.Bytes(), payload) {
			t.Fatalf("size %d: round trip changed the payload", size)
		}
		if size > 0 && bytes.Contains(sealed, payload[:min(size, 32)]) && size >= 16 {
			t.Fatal("payload appears in the sealed file")
		}
	}
}

func TestBackupSealUsesTheDocumentedKDF(t *testing.T) {
	sealed := sealBytes(t, []byte("payload"))
	if got := binary.BigEndian.Uint32(sealed[10:14]); got != sealIterations || sealIterations < 600_000 {
		t.Fatalf("iterations %d", got)
	}
	if err := Decrypt(io.Discard, bytes.NewReader(sealed), []byte("wrong passphrase!")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

func TestBackupUnsealRejectsTamperingInEveryRegion(t *testing.T) {
	memoizeSealKey(t)
	payload := make([]byte, 2*sealChunkSize+100)
	rand.Read(payload)
	sealed := sealBytes(t, payload)
	header := len(Magic) + 7 + sealSaltSize + 4
	regions := map[string]int{
		"magic": 0, "version": 8, "kdf": 9, "iterations": 13, "salt length": 14, "salt": 15,
		"chunk size": header - 1, "first chunk": header + 10, "first tag": header + sealChunkSize + 3,
		"middle chunk": header + sealChunkSize + sealTagSize + 50, "last chunk": len(sealed) - sealTagSize - 2, "last tag": len(sealed) - 1,
	}
	for region, offset := range regions {
		damaged := bytes.Clone(sealed)
		damaged[offset] ^= 1
		var opened bytes.Buffer
		if err := Decrypt(&opened, bytes.NewReader(damaged), []byte(testPassphrase)); err == nil {
			t.Fatalf("%s: accepted a flipped bit", region)
		}
	}
	for name, cut := range map[string][]byte{
		"empty": nil, "half header": sealed[:10], "header only": sealed[:header],
		"chunk boundary": sealed[:header+sealChunkSize+sealTagSize], "mid chunk": sealed[:header+500],
		"last byte": sealed[:len(sealed)-1], "trailing data": append(bytes.Clone(sealed), 0),
		"dropped chunk": append(bytes.Clone(sealed[:header]), sealed[header+sealChunkSize+sealTagSize:]...),
	} {
		if err := Decrypt(io.Discard, bytes.NewReader(cut), []byte(testPassphrase)); err == nil {
			t.Fatalf("%s: accepted a truncated or extended file", name)
		}
	}
	if err := Decrypt(io.Discard, bytes.NewReader(sealed), []byte("wrong passphrase!")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

func TestBackupUnsealBoundsHeaderParameters(t *testing.T) {
	memoizeSealKey(t)
	sealed := sealBytes(t, []byte("x"))
	for name, change := range map[string]func([]byte){
		"low iterations":  func(b []byte) { binary.BigEndian.PutUint32(b[10:], 1000) },
		"high iterations": func(b []byte) { binary.BigEndian.PutUint32(b[10:], 1<<31) },
		"short salt":      func(b []byte) { b[14] = 4 },
		"huge chunk":      func(b []byte) { binary.BigEndian.PutUint32(b[15+sealSaltSize:], 1<<30) },
		"version":         func(b []byte) { b[8] = 2 },
	} {
		damaged := bytes.Clone(sealed)
		change(damaged)
		err := Decrypt(io.Discard, bytes.NewReader(damaged), []byte(testPassphrase))
		if err == nil || errors.Is(err, ErrAuthentication) {
			t.Fatalf("%s: header was not rejected before key derivation: %v", name, err)
		}
	}
}

func TestBackupUnsealAuthenticatesValidHeaderChanges(t *testing.T) {
	memoizeSealKey(t)
	sealed := sealBytes(t, []byte("short payload"))
	// Both sizes put the short payload in one chunk and use the same key.
	binary.BigEndian.PutUint32(sealed[len(Magic)+7+sealSaltSize:], sealMinChunk)
	if err := Decrypt(io.Discard, bytes.NewReader(sealed), []byte(testPassphrase)); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("changed chunk size: %v", err)
	}
}

func FuzzBackupDecrypt(f *testing.F) {
	// Keep header parsing and GCM in the fuzz loop without re-running PBKDF2.
	real := sealKey
	sealKey = func([]byte, []byte, int) ([]byte, error) { return make([]byte, 32), nil }
	f.Cleanup(func() { sealKey = real })
	sealed := sealBytes(f, []byte("payload"))
	f.Add([]byte{}, testPassphrase)
	f.Add(sealBytes(f, nil), testPassphrase)
	f.Add(sealed, testPassphrase)
	f.Add(sealed[:len(sealed)-1], testPassphrase)
	f.Add(sealed, "")
	f.Fuzz(func(t *testing.T, data []byte, passphrase string) {
		_ = Decrypt(io.Discard, bytes.NewReader(data), []byte(passphrase))
	})
}
