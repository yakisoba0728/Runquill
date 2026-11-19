package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setTestRunEnv(t *testing.T) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"RUNQUILL_OWNER": "owner", "RUNQUILL_APP_ID": "1",
		"RUNQUILL_KEY_FILE": keyPath, "RUNQUILL_DB": filepath.Join(t.TempDir(), "queue.sqlite3")} {
		t.Setenv(name, value)
	}
}

func TestLoadConfigMaxPerRepository(t *testing.T) {
	setTestRunEnv(t)
	cfg, err := loadConfig()
	if err != nil || cfg.maxPerRepository != 0 || cfg.workers != 4 {
		t.Fatalf("defaults: %d, %d, %v", cfg.maxPerRepository, cfg.workers, err)
	}
	for _, value := range []string{"0", "33", "two"} {
		t.Setenv("RUNQUILL_SCHEDULER_WORKERS", value)
		if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "RUNQUILL_SCHEDULER_WORKERS") {
			t.Errorf("workers %q: %v", value, err)
		}
	}
	t.Setenv("RUNQUILL_SCHEDULER_WORKERS", "1")
	t.Setenv("RUNQUILL_MAX_PER_REPOSITORY", "2")
	if cfg, err := loadConfig(); err != nil || cfg.maxPerRepository != 2 {
		t.Fatalf("set: %d, %v", cfg.maxPerRepository, err)
	}
	for _, value := range []string{"-1", "x", "10001"} {
		t.Setenv("RUNQUILL_MAX_PER_REPOSITORY", value)
		if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "RUNQUILL_MAX_PER_REPOSITORY") {
			t.Errorf("%q: %v", value, err)
		}
	}
}

func TestRunRejectsInvalidNotifyConfigWithoutTheURL(t *testing.T) {
	setTestRunEnv(t)
	const secret = "http://hooks.example.com/services/T000/B000/secret-token"
	t.Setenv("RUNQUILL_NOTIFY_URL", secret)
	err := run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "RUNQUILL_NOTIFY_") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("run: %v", err)
	}
	if _, statErr := os.Stat(os.Getenv("RUNQUILL_DB")); !os.IsNotExist(statErr) {
		t.Fatalf("startup continued past an invalid notify configuration: %v", statErr)
	}
}
