package app

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/yakisoba0728/Runquill/examples"
)

const firstWorkflowPath = ".github/workflows/runquill-first-job.yml"

// workflowCommand runs the workflow subcommands and returns the process exit status.
func workflowCommand(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && args[0] == "init":
		if err := initWorkflow("."); err != nil {
			fmt.Fprintln(stderr, "runquill workflow init:", err)
			return 1
		}
		fmt.Fprintf(stdout, "Created %s. Commit it to your private repository's default branch, then run Runquill first job from the GitHub Actions tab.\n", firstWorkflowPath)
		return 0
	case len(args) > 0 && args[0] == "check":
		return workflowCheckCommand(args[1:], ".", stdout, stderr, os.Getenv)
	}
	fmt.Fprintln(stderr, "Usage: runquill workflow init | runquill workflow check [--json] [--strict] [PATH...]")
	return 2
}

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
	if err := root.WriteFile(staged, examples.FirstJob(), 0644); err != nil {
		return err
	}
	// Publish the complete template without replacing an existing workflow.
	err = root.Link(staged, firstWorkflowPath)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s: %w", firstWorkflowPath, os.ErrExist)
	}
	return err
}
