package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yakisoba0728/Runquill/internal/workflow"
)

// Workflow files larger than this are not checked.
const maxCheckedWorkflow = 1 << 20

func workflowCheckOptions(pools workerPools, images *runnerImages) workflow.CheckOptions {
	var options workflow.CheckOptions
	if pools != nil {
		options.Pools = make(map[string]bool, len(pools))
		for name, pool := range pools {
			options.Pools[name] = pool.Privileged == nil || *pool.Privileged
		}
	}
	if images != nil {
		options.CheckImage = func(pool, name string) (known, allowed bool) {
			_, known = images.lookup(name)
			return known, pools[pool].allowsImage(name)
		}
	}
	return options
}

func workerPoolsFromEnv(getenv func(string) string) (workerPools, error) {
	raw := strings.TrimSpace(getenv("RUNQUILL_WORKER_POOLS"))
	if raw == "" {
		return nil, nil
	}
	var pools workerPools
	if err := json.Unmarshal([]byte(raw), &pools); err != nil || len(pools) == 0 {
		return nil, errors.New("RUNQUILL_WORKER_POOLS is not a JSON object of pools; pool checks are skipped")
	}
	if _, ok := pools[defaultWorkerPool]; !ok {
		pools[defaultWorkerPool] = workerPool{}
	}
	return pools, nil
}

// savedWorkerPools reads the pools saved in the panel from RUNQUILL_DB without changing it.
// It reports false when the database is missing, unreadable or has no saved settings.
func savedWorkerPools(ctx context.Context, dir string, getenv func(string) string, envPools workerPools) (workerPools, string, bool) {
	path := strings.TrimSpace(getenv("RUNQUILL_DB"))
	if path == "" {
		return nil, "", false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, "", false
	}
	db, err := openQueueDB(path, "ro")
	if err != nil {
		return nil, "", false
	}
	defer db.Close()
	policy, saved, err := readOperationPolicy(ctx, db, operationPolicy{Pools: envPools})
	if err != nil || !saved {
		return nil, "", false
	}
	pools := policy.Pools
	if len(pools) == 0 {
		return nil, path, true
	}
	if _, ok := pools[defaultWorkerPool]; !ok {
		pools[defaultWorkerPool] = workerPool{}
	}
	return pools, path, true
}

func workflowFiles(dir string, args []string) ([]string, error) {
	if len(args) == 0 {
		args = []string{filepath.Join(dir, ".github", "workflows")}
	}
	var files []string
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, arg)
			continue
		}
		entries, err := os.ReadDir(arg)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			ext := filepath.Ext(entry.Name())
			if entry.Type().IsRegular() && (ext == ".yml" || ext == ".yaml") {
				files = append(files, filepath.Join(arg, entry.Name()))
			}
		}
	}
	return files, nil
}

// workflowCheckCommand implements runquill workflow check. It reads local files only.
func workflowCheckCommand(args []string, dir string, stdout, stderr io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("runquill workflow check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "print findings as JSON")
	strict := flags.Bool("strict", false, "exit with status 1 for any finding, not only high severity")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: runquill workflow check [--json] [--strict] [PATH...]")
		fmt.Fprintln(stderr, "Check workflow files offline. PATH defaults to .github/workflows.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	pools, poolErr := workerPoolsFromEnv(getenv)
	poolSource := ""
	if saved, path, ok := savedWorkerPools(context.Background(), dir, getenv, pools); ok {
		pools, poolSource = saved, "the operation settings saved in the panel"
		if saved != nil {
			poolErr = nil
		}
		fmt.Fprintf(stderr, "Using the operation settings saved in %s (read-only) for pool checks.\n", path)
	}
	if poolErr != nil {
		fmt.Fprintln(stderr, poolErr)
	}
	images, imagesErr := buildRunnerImages(getenv(runnerImagesEnv), releaseImage, version, "")
	if imagesErr != nil {
		fmt.Fprintln(stderr, imagesErr.Error()+"; runner image checks are skipped")
	}
	paths := flags.Args()
	for i, path := range paths {
		if !filepath.IsAbs(path) {
			paths[i] = filepath.Join(dir, path)
		}
	}
	files, err := workflowFiles(dir, paths)
	if errors.Is(err, fs.ErrNotExist) && len(flags.Args()) == 0 {
		err = errors.New("no .github/workflows directory here; run this from a repository root or pass workflow files")
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	options := workflowCheckOptions(pools, images)
	options.PoolSource = poolSource
	findings := []workflow.Finding{}
	for _, file := range files {
		data, err := readWorkflowFile(file)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		name := file
		if rel, err := filepath.Rel(dir, file); err == nil && !strings.HasPrefix(rel, "..") {
			name = filepath.ToSlash(rel)
		}
		findings = append(findings, workflow.Check(name, data, options)...)
	}
	high := 0
	for _, f := range findings {
		if f.Severity == workflow.SeverityHigh {
			high++
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(struct {
			Files    int                `json:"files"`
			Findings []workflow.Finding `json:"findings"`
		}{len(files), findings})
	} else {
		for _, f := range findings {
			job := ""
			if f.Job != "" {
				job = " job " + f.Job + ":"
			}
			fmt.Fprintf(stdout, "%s:%d: %s [%s]%s %s\n", f.File, f.Line, f.Severity, f.Code, job, f.Message)
		}
		fmt.Fprintf(stdout, "Checked %d workflow files: %d findings, %d high severity.\n", len(files), len(findings), high)
	}
	if high > 0 || *strict && len(findings) > 0 {
		return 1
	}
	return 0
}

func readWorkflowFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCheckedWorkflow+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCheckedWorkflow {
		return nil, fmt.Errorf("%s is larger than 1 MiB", path)
	}
	return data, nil
}
