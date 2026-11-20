package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yakisoba0728/Runquill/examples"
)

func TestInitWorkflowWritesTemplateOnce(t *testing.T) {
	checkout := t.TempDir()
	if err := initWorkflow(checkout); err == nil {
		t.Fatal("expected a Git checkout to be required")
	}
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := initWorkflow(checkout); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(checkout, firstWorkflowPath)
	template, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(template, examples.FirstJob()) {
		t.Fatalf("expected the embedded first-job workflow, got %q (%v)", template, err)
	}
	existing := []byte("name: Existing workflow\n")
	if err := os.WriteFile(path, existing, 0644); err != nil {
		t.Fatal(err)
	}
	if err := initWorkflow(checkout); !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected existing workflow error, got %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(contents, existing) {
		t.Fatalf("expected the existing workflow to be preserved, got %q (%v)", contents, err)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected files after retry: %v (%v)", files, err)
	}
}

func TestWorkflowCommand(t *testing.T) {
	checkout := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(checkout)
	var out, errs bytes.Buffer
	for _, args := range [][]string{nil, {"init", "extra"}, {"unknown"}} {
		if code := workflowCommand(args, &out, &errs); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
	if code := workflowCommand([]string{"init"}, &out, &errs); code != 0 || !bytes.Contains(out.Bytes(), []byte(firstWorkflowPath)) {
		t.Fatalf("init: exit %d, %s %s", code, out.String(), errs.String())
	}
	if code := workflowCommand([]string{"init"}, &out, &errs); code != 1 {
		t.Fatalf("second init: exit %d", code)
	}
	out.Reset()
	// The generated first job must pass its own check.
	if code := workflowCommand([]string{"check"}, &out, &errs); code != 0 || !bytes.Contains(out.Bytes(), []byte("Checked 1 workflow files: 0 findings")) {
		t.Fatalf("check: exit %d, %s %s", code, out.String(), errs.String())
	}
}

func TestInitWorkflowRejectsLinkOutsideCheckout(t *testing.T) {
	checkout, external := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: linked-worktree\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(checkout, ".github")); err != nil {
		t.Fatal(err)
	}
	if err := initWorkflow(checkout); err == nil {
		t.Fatal("expected external workflow directory to be rejected")
	}
	if _, err := os.Stat(filepath.Join(external, "workflows")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected the external directory to stay untouched, got %v", err)
	}
}
