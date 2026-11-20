package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var panelOperation = make(chan struct{}, 1)

// Large queues need longer than the panel's 10s write timeout to copy and send.
const panelBackupTimeout = 30 * time.Minute

func panelOperationContext(w http.ResponseWriter, r *http.Request, timeout time.Duration) (context.Context, func(), bool) {
	select {
	case panelOperation <- struct{}{}:
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		return ctx, sync.OnceFunc(func() { cancel(); <-panelOperation }), true
	default:
		http.Error(w, "Another operation is running. Try again shortly.", http.StatusTooManyRequests)
		return nil, nil, false
	}
}

func (a *panelAccess) downloadBackup(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	// The server's write deadline started when the request headers arrived.
	// Recorders and other writers without deadlines report ErrNotSupported.
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(panelBackupTimeout))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		http.Error(w, "Could not prepare the queue backup.", http.StatusServiceUnavailable)
		return
	}
	ctx, done, ok := panelOperationContext(w, r, panelBackupTimeout)
	if !ok {
		return
	}
	defer done()
	dir, err := os.MkdirTemp("", "runquill-panel-backup-")
	if err != nil {
		http.Error(w, "Could not prepare the queue backup.", http.StatusServiceUnavailable)
		return
	}
	defer os.RemoveAll(dir)
	name := "runquill-queue-backup.sqlite3"
	path := filepath.Join(dir, name)
	if err := backupQueue(ctx, io.Discard, env("RUNQUILL_DB", "runquill.sqlite3"), path); err != nil {
		http.Error(w, "Could not create the queue backup.", http.StatusServiceUnavailable)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.Error(w, "Could not open the backup.", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	if err := a.change(ctx, user, "queue.backup", "", func(*sql.Tx) error { return nil }); err != nil {
		http.Error(w, "Could not authorize the backup download.", http.StatusForbidden)
		return
	}
	// The copy is complete and audited; a slow download must not block other operations.
	done()
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	http.ServeContent(w, r, name, time.Time{}, file)
}

func panelDiagnose(w http.ResponseWriter, r *http.Request, _ panelIdentity) {
	var request struct{ Repository string }
	if !decodePanelJSON(w, r, &request) {
		return
	}
	ctx, done, ok := panelOperationContext(w, r, 8*time.Second)
	if !ok {
		return
	}
	defer done()
	var report bytes.Buffer
	err := diagnose(ctx, &report, request.Repository)
	if err != nil {
		report.WriteString("\n" + err.Error())
	}
	panelJSON(w, http.StatusOK, struct {
		OK     bool   `json:"ok"`
		Report string `json:"report"`
	}{err == nil, report.String()})
}
