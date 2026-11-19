package main

import (
	"database/sql"
	"testing"
)

// A database created before jobs:write keeps its tokens and accepts the new scope.
func TestPanelTokenScopeMigrationKeepsTokens(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/panel.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE panel_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT, digest BLOB NOT NULL UNIQUE, user_id INTEGER NOT NULL, name TEXT NOT NULL,
    scope TEXT NOT NULL CHECK (scope IN ('fleet:read','hosts:write')), expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL);
CREATE INDEX panel_tokens_user ON panel_tokens(user_id);
INSERT INTO panel_tokens (id, digest, user_id, name, scope, expires_at, created_at) VALUES (7, x'01', 3, 'old', 'hosts:write', 9, 1)`); err != nil {
		t.Fatal(err)
	}
	for range 2 { // The second run must be a no-op.
		if err := widenPanelTokenScopes(t.Context(), db); err != nil {
			t.Fatal(err)
		}
	}
	var name, scope string
	if err := db.QueryRow("SELECT name, scope FROM panel_tokens WHERE id = 7").Scan(&name, &scope); err != nil || name != "old" || scope != "hosts:write" {
		t.Fatalf("existing token lost: %q %q %v", name, scope, err)
	}
	result, err := db.Exec("INSERT INTO panel_tokens (digest, user_id, name, scope, expires_at, created_at) VALUES (x'02', 3, 'new', 'jobs:write', 9, 1)")
	if err != nil {
		t.Fatalf("jobs:write rejected after migration: %v", err)
	}
	if id, _ := result.LastInsertId(); id <= 7 {
		t.Fatalf("token id %d reused", id)
	}
	if _, err := db.Exec("INSERT INTO panel_tokens (digest, user_id, name, scope, expires_at, created_at) VALUES (x'03', 3, 'bad', 'admin', 9, 1)"); err == nil {
		t.Fatal("unknown scope accepted")
	}
}
