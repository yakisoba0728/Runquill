package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func workerEnrollCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("runquill worker enroll", flag.ContinueOnError)
	name := flags.String("name", os.Getenv("RUNQUILL_WORKER_NAME"), "configured agent host name")
	manager := flags.String("manager", os.Getenv("RUNQUILL_WORKER_URL"), "manager HTTPS origin")
	tokenFile := flags.String("token-file", os.Getenv("RUNQUILL_WORKER_TOKEN_FILE"), "private file for the new worker token")
	code := flags.String("code", "", "one-time enrollment code from the Runquill panel")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !validHex32(*code) || *tokenFile == "" {
		return errors.New("use runquill worker enroll --name NAME --manager ORIGIN --token-file FILE --code CODE")
	}
	endpoint, err := workerEndpoint(*manager, *name)
	if err != nil {
		return err
	}
	target := docker{worker: *name}
	engine, err := target.engineID(ctx)
	if err != nil {
		return err
	}
	if !validEngineID(engine) {
		return errors.New("Docker reported an invalid engine ID")
	}
	if err := prepareWorkerToken(*tokenFile); err != nil {
		return err
	}
	token, release, err := lockWorkerToken(*tokenFile)
	if err != nil {
		return err
	}
	defer release()
	body, err := json.Marshal(workerAuthority{Engine: engine, Digest: workerDigest(token)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(endpoint, "/poll")+"/enroll", bytes.NewReader(body))
	if err != nil {
		return errors.New("build worker enrollment request")
	}
	req.Header.Set("Authorization", "Bearer "+*code)
	req.Header.Set("Content-Type", "application/json")
	res, err := workerHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("contact manager for enrollment: %w. If the manager accepted the code, start the worker with the saved token file", err)
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusUnauthorized:
		return errors.New("worker enrollment code was rejected. If the manager already accepted it, start the worker with the saved token file")
	case http.StatusConflict:
		return errors.New("this Docker engine or worker credential is already in use")
	default:
		return fmt.Errorf("worker enrollment returned HTTP %d", res.StatusCode)
	}
}

func prepareWorkerToken(filename string) error {
	_, err := os.Lstat(filename)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Keep the token locally before contacting the manager, so a lost reply is recoverable.
	if errors.Is(err, os.ErrNotExist) {
		if err := writeSetupFile(filename, []byte(workerSecret()+"\n")); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("write private worker token: %w", err)
		}
	}
	dir, err := os.Open(filepath.Dir(filename))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
