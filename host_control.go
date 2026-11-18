package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
)

func setHostCordon(ctx context.Context, out io.Writer, name string, cordoned bool) error {
	label := "Docker host"
	if env("RUNQUILL_BACKEND", "docker") == "kubernetes" {
		label = "Kubernetes runner pool"
		if name != kubeHost {
			return fmt.Errorf("unknown Kubernetes runner pool %q", name)
		}
	} else {
		fleet, err := newDockerFleet("", env("RUNQUILL_DOCKER_HOSTS", "local"))
		if err != nil {
			return err
		}
		if _, err := fleet.host(name); err != nil {
			return err
		}
	}
	path := env("RUNQUILL_DB", "runquill.sqlite3")
	db, err := openQueueDB(path, "rw")
	if err != nil {
		return err
	}
	defer db.Close()
	if err := writeHostCordon(ctx, db, name, cordoned); err != nil {
		return fmt.Errorf("update runner host state in %s (start the manager to initialize the queue): %w", path, err)
	}
	mode := "uncordoned"
	if cordoned {
		mode = "cordoned"
	}
	_, err = fmt.Fprintf(out, "%s %s %s.\n", label, name, mode)
	return err
}

func writeHostCordon(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, name string, cordoned bool) error {
	_, err := db.ExecContext(ctx, `
        INSERT INTO docker_hosts (host, cordoned) VALUES (?, ?)
        ON CONFLICT (host) DO UPDATE SET cordoned = excluded.cordoned`, name, cordoned)
	return err
}
