package main

import (
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const firstWorkflowPath = ".github/workflows/runquill-first-job.yml"

//go:embed examples/runquill-first-job.yml
var firstWorkflow []byte

func initWorkflow(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	git, err := root.Lstat(".git")
	if errors.Is(err, os.ErrNotExist) || err == nil && !git.IsDir() && !git.Mode().IsRegular() {
		return errors.New("run this command from the root of a Git checkout")
	}
	if err != nil {
		return err
	}

	workflows := filepath.Dir(firstWorkflowPath)
	if err := root.MkdirAll(workflows, 0755); err != nil {
		return err
	}
	staged := filepath.Join(workflows, ".runquill-"+rand.Text())
	defer root.Remove(staged)
	if err := root.WriteFile(staged, firstWorkflow, 0644); err != nil {
		return err
	}
	// Publish the complete template without replacing an existing workflow.
	err = root.Link(staged, firstWorkflowPath)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s: %w", firstWorkflowPath, os.ErrExist)
	}
	return err
}
