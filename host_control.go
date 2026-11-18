package main

import (
	"context"
	"fmt"
	"io"
)

func setHostCordon(ctx context.Context, out io.Writer, name string, cordoned bool) error {
	fleet, err := newDockerFleet("", env("RUNQUILL_DOCKER_HOSTS", "local"))
	if err != nil {
		return err
	}
	if _, err := fleet.host(name); err != nil {
		return err
	}
	path := env("RUNQUILL_DB", "runquill.sqlite3")
	db, err := openQueueDB(path, "rw")
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `
        INSERT INTO docker_hosts (host, cordoned) VALUES (?, ?)
        ON CONFLICT (host) DO UPDATE SET cordoned = excluded.cordoned`, name, cordoned)
	if err != nil {
		return fmt.Errorf("update Docker host state in %s (start the manager to initialize the queue): %w", path, err)
	}
	mode := "uncordoned"
	if cordoned {
		mode = "cordoned"
	}
	_, err = fmt.Fprintf(out, "Docker host %s %s.\n", name, mode)
	return err
}
