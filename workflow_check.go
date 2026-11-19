package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	severityHigh    = "high"
	severityWarning = "warning"
	severityInfo    = "info"
	// Workflow files larger than this are not checked; GitHub's own limit is far lower in practice.
	maxCheckedWorkflow = 1 << 20
)

var (
	// The format() call may sit inside a conditional that picks a GitHub-hosted runner instead.
	runquillRoute = regexp.MustCompile(`format\(\s*'runquill-\{0\}-\{1\}-([A-Za-z0-9][A-Za-z0-9_-]*?)(-\{2\})?'\s*,\s*github\.run_id\s*,\s*github\.run_attempt\s*(,\s*strategy\.job-index\s*)?\)`)
	usesLine      = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s+(.*)$`)
	pinnedRef     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	dockerUsage   = regexp.MustCompile(`(^|[\s;&|(])(docker|docker-compose)(\s|$)|uses:\s+["']?docker://`)
	flowMapKey    = regexp.MustCompile(`[{,]\s*["']?([A-Za-z_]+)["']?\s*:`)
)

type workflowFinding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Job      string `json:"job,omitempty"`
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// workflowEvents lists the trigger names in the top-level on key.
func (d *wfDoc) workflowEvents() []string {
	on, ok := d.topProp("on")
	if !ok {
		return nil
	}
	var events []string
	switch on.value.form {
	case formPlain, formDouble, formSingle:
		return []string{on.value.text}
	case formFlowSeq:
		items, _, _ := flowItems(on.value.text)
		return items
	case formFlowMap:
		for _, m := range flowMapKey.FindAllStringSubmatch(on.value.text, -1) {
			events = append(events, m[1])
		}
		return events
	}
	indent := -1
	for _, i := range d.nested(on) {
		line := d.lines[i]
		if indent < 0 {
			indent = line.indent
		}
		if line.indent != indent {
			continue
		}
		text := d.text(i)
		if isSequenceItem(text, indent) {
			if item := parseInline(text, indent+1); item.form == formPlain || item.form == formDouble || item.form == formSingle {
				events = append(events, item.text)
			}
		} else if key, _, ok := parseYAMLKey(text, indent); ok {
			events = append(events, key)
		}
	}
	return events
}

type routeInfo struct {
	runquill, selfHosted bool
	pool, os             string
	problem              string
}

func checkRoute(labels []string, matrix bool) routeInfo {
	var info routeInfo
	for _, label := range labels {
		lower := strings.ToLower(label)
		switch {
		case strings.Contains(label, "${{") && runquillExpression.MatchString(label), strings.HasPrefix(lower, "runquill-"):
			info.runquill = true
		case lower == "self-hosted":
			info.selfHosted = true
		}
	}
	if !info.runquill {
		if info.selfHosted {
			info.problem = "runs-on targets self-hosted runners but is not a Runquill route, so Runquill will not start a runner for it"
		}
		return info
	}
	info.selfHosted = true
	if len(labels) != 1 {
		info.problem = "Runquill routes must be the job's only runs-on label"
		return info
	}
	m := runquillRoute.FindStringSubmatch(labels[0])
	if m == nil || !strings.HasPrefix(labels[0], "${{") || !strings.HasSuffix(labels[0], "}}") {
		info.problem = "runs-on mentions Runquill but does not build runquill-<run_id>-<run_attempt>-<suffix> with format(); Runquill will ignore the job"
		return info
	}
	if (m[2] != "") != (m[3] != "") {
		info.problem = "the {2} placeholder and strategy.job-index must be used together"
		return info
	}
	if matrix && m[2] == "" {
		info.problem = "matrix jobs need strategy.job-index in the Runquill label so each job gets its own runner"
		return info
	}
	suffix := strings.ToLower(m[1])
	info.pool = defaultWorkerPool
	pool, osName, _, err := splitRouteSuffix(suffix)
	if err != nil {
		info.problem = err.Error()
		return info
	}
	if pool != "" {
		info.pool = pool
	}
	info.os = osName
	if len(suffix) > maxRouteSuffix {
		info.problem = "the Runquill label can exceed 100 characters; shorten the suffix"
	}
	return info
}

func jobNeedsDocker(d *wfDoc, j *wfJob) bool {
	if _, ok := j.prop("container"); ok {
		return true
	}
	if _, ok := j.prop("services"); ok {
		return true
	}
	for i := j.bodyFrom; i < j.bodyTo; i++ {
		if d.lines[i].indent >= 0 && dockerUsage.MatchString(d.text(i)) {
			return true
		}
	}
	return false
}

// checkWorkflow runs the offline checks on one workflow file. pools is nil when
// no pool configuration is available.
func checkWorkflow(path string, src []byte, pools workerPools) []workflowFinding {
	return checkWorkflowImages(path, src, pools, nil)
}

// checkWorkflowImages also checks os__NAME__ selectors against the runner image catalogue when it is known.
func checkWorkflowImages(path string, src []byte, pools workerPools, images *runnerImages) []workflowFinding {
	var out []workflowFinding
	add := func(line int, job, severity, code, message string) {
		out = append(out, workflowFinding{File: path, Line: line, Job: job, Severity: severity, Code: code, Message: message})
	}
	d := parseWorkflow(src)
	if d.refusal != "" {
		add(1, "", severityWarning, "unparsed", "Could not check this file: "+d.refusal+".")
		return out
	}
	if _, ok := d.topProp("permissions"); !ok {
		add(1, "", severityWarning, "permissions", "The workflow does not set permissions, so jobs get the repository's default GITHUB_TOKEN permissions. Add a top-level permissions block.")
	}
	events := d.workflowEvents()
	for _, j := range d.jobs {
		line := j.line + 1
		if j.code != "" {
			add(line, j.id, severityInfo, "unparsed", "Could not check this job: "+j.refusal+".")
			continue
		}
		_, matrix := j.prop("strategy")
		_, reusable := j.prop("uses")
		var info routeInfo
		if p, ok := j.prop("runs-on"); ok {
			parsed := d.runsOn(p)
			line = parsed.line
			if parsed.code == "" || parsed.code == outcomeMapping {
				info = checkRoute(parsed.labels, matrix)
			}
			if parsed.code == outcomeMapping {
				info.selfHosted = true
			}
		}
		if info.problem != "" {
			add(line, j.id, severityWarning, "runs-on", info.problem+".")
		}
		if info.selfHosted {
			for _, event := range events {
				switch event {
				case "pull_request_target", "workflow_run":
					add(line, j.id, severityHigh, "untrusted-trigger", fmt.Sprintf("%s runs with repository secrets and write access, and this job runs on a self-hosted runner. Code from a fork can reach your runner hosts; use a GitHub-hosted runner for this workflow.", event))
				case "pull_request", "pull_request_review", "pull_request_review_comment":
					add(line, j.id, severityWarning, "fork-trigger", fmt.Sprintf("%s can run code from forks on this self-hosted runner if the repository allows fork pull request workflows. Keep that setting off or require approval for outside contributors.", event))
				}
			}
		}
		if info.runquill && info.problem == "" && pools != nil {
			pool, found := pools[info.pool]
			switch {
			case !found:
				add(line, j.id, severityWarning, "pool", fmt.Sprintf("Worker pool %q is not configured in RUNQUILL_WORKER_POOLS, so the job will wait.", info.pool))
			case pool.Privileged != nil && !*pool.Privileged && jobNeedsDocker(d, j):
				add(line, j.id, severityWarning, "docker", fmt.Sprintf("This job appears to need Docker (heuristic: container, services or docker commands), but worker pool %q runs without privileges and cannot use Docker.", info.pool))
			}
		}
		if info.runquill && info.problem == "" && info.os != "" && images != nil {
			if _, found := images.lookup(info.os); !found {
				add(line, j.id, severityWarning, "runner-image", fmt.Sprintf("Runner image %q is not in the runner image catalogue (release images and %s), so the job will wait.", info.os, runnerImagesEnv))
			} else if _, err := images.forJob("runquill-1-1-os__"+info.os+"__job", pools[info.pool]); err != nil {
				add(line, j.id, severityWarning, "runner-image", fmt.Sprintf("Worker pool %q does not allow runner image %q, so the job will wait.", info.pool, info.os))
			}
		}
		if _, ok := j.prop("timeout-minutes"); !ok && !reusable {
			add(j.line+1, j.id, severityWarning, "timeout", "The job has no timeout-minutes, so a stuck job can hold a runner for 6 hours.")
		}
		for i := j.line; i < j.bodyTo; i++ {
			if d.lines[i].indent < 0 {
				continue
			}
			m := usesLine.FindStringSubmatch(d.text(i))
			if m == nil {
				continue
			}
			value := parseInline(m[1], 0)
			if value.form != formPlain && value.form != formDouble && value.form != formSingle {
				continue
			}
			action := value.text
			if strings.HasPrefix(action, "./") || strings.HasPrefix(action, "docker://") || strings.Contains(action, "${{") {
				continue
			}
			owner, _, _ := strings.Cut(action, "/")
			owner = strings.ToLower(owner)
			_, ref, _ := strings.Cut(action, "@")
			if owner == "actions" || owner == "github" || pinnedRef.MatchString(ref) {
				continue
			}
			add(i+1, j.id, severityInfo, "unpinned-action", fmt.Sprintf("%s is not pinned to a full commit SHA.", action))
		}
	}
	slices.SortStableFunc(out, func(a, b workflowFinding) int { return a.Line - b.Line })
	return out
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
	findings := []workflowFinding{}
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
		findings = append(findings, checkWorkflowImages(name, data, pools, images)...)
	}
	high := 0
	for _, f := range findings {
		if f.Severity == severityHigh {
			high++
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(struct {
			Files    int               `json:"files"`
			Findings []workflowFinding `json:"findings"`
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
