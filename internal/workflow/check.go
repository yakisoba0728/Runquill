package workflow

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/yakisoba0728/Runquill/internal/route"
)

const (
	SeverityHigh    = "high"
	severityWarning = "warning"
	severityInfo    = "info"
)

var (
	// The format() call may sit inside a conditional that picks a GitHub-hosted runner instead.
	runquillRoute = regexp.MustCompile(`format\(\s*'runquill-\{0\}-\{1\}-([A-Za-z0-9][A-Za-z0-9_-]*?)(-\{2\})?'\s*,\s*github\.run_id\s*,\s*github\.run_attempt\s*(,\s*strategy\.job-index\s*)?\)`)
	usesLine      = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s+(.*)$`)
	pinnedRef     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	dockerUsage   = regexp.MustCompile(`(^|[\s;&|(])(docker|docker-compose)(\s|$)|uses:\s+["']?docker://`)
	flowMapKey    = regexp.MustCompile(`[{,]\s*["']?([A-Za-z_]+)["']?\s*:`)
)

type Finding struct {
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
	info.pool = route.DefaultPool
	pool, osName, _, err := route.SplitSuffix(suffix)
	if err != nil {
		info.problem = err.Error()
		return info
	}
	if pool != "" {
		info.pool = pool
	}
	info.os = osName
	if len(suffix) > route.MaxSuffix {
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

// IsCommitSHA reports whether ref is a full lowercase Git commit SHA.
func IsCommitSHA(ref string) bool { return pinnedRef.MatchString(ref) }

// CheckOptions provides the runner configuration known to the caller.
type CheckOptions struct {
	// Pools maps pool names to whether they support Docker. Nil skips pool checks.
	Pools map[string]bool
	// CheckImage reports whether an image is known and allowed by the pool.
	// Nil skips checks against the image catalogue.
	CheckImage func(pool, osName string) (known, allowed bool)
}

// Check checks a workflow without accessing GitHub.
func Check(path string, src []byte, options CheckOptions) []Finding {
	var out []Finding
	add := func(line int, job, severity, code, message string) {
		out = append(out, Finding{File: path, Line: line, Job: job, Severity: severity, Code: code, Message: message})
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
					add(line, j.id, SeverityHigh, "untrusted-trigger", fmt.Sprintf("%s runs with repository secrets and write access, and this job runs on a self-hosted runner. Code from a fork can reach your runner hosts; use a GitHub-hosted runner for this workflow.", event))
				case "pull_request", "pull_request_review", "pull_request_review_comment":
					add(line, j.id, severityWarning, "fork-trigger", fmt.Sprintf("%s can run code from forks on this self-hosted runner if the repository allows fork pull request workflows. Keep that setting off or require approval for outside contributors.", event))
				}
			}
		}
		if info.runquill && info.problem == "" && options.Pools != nil {
			dockerAllowed, found := options.Pools[info.pool]
			switch {
			case !found:
				add(line, j.id, severityWarning, "pool", fmt.Sprintf("Worker pool %q is not configured in RUNQUILL_WORKER_POOLS, so the job will wait.", info.pool))
			case !dockerAllowed && jobNeedsDocker(d, j):
				add(line, j.id, severityWarning, "docker", fmt.Sprintf("This job appears to need Docker (heuristic: container, services or docker commands), but worker pool %q runs without privileges and cannot use Docker.", info.pool))
			}
		}
		if info.runquill && info.problem == "" && info.os != "" && options.CheckImage != nil {
			known, allowed := options.CheckImage(info.pool, info.os)
			if !known {
				add(line, j.id, severityWarning, "runner-image", fmt.Sprintf("Runner image %q is not in the runner image catalogue (release images and RUNQUILL_RUNNER_IMAGES), so the job will wait.", info.os))
			} else if !allowed {
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
			if owner == "actions" || owner == "github" || IsCommitSHA(ref) {
				continue
			}
			add(i+1, j.id, severityInfo, "unpinned-action", fmt.Sprintf("%s is not pinned to a full commit SHA.", action))
		}
	}
	slices.SortStableFunc(out, func(a, b Finding) int { return a.Line - b.Line })
	return out
}
