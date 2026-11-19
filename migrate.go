package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The rewriter reads only the small YAML subset GitHub workflow files use at the
// top, job and job-property levels. Anything else is left alone with a reason,
// because a wrong guess would silently move a job to a different runner.

const (
	maxRunsOnMappings = 50
	// Room left for runquill-<19 digit run ID>-<3 digit attempt>- and a -<3 digit matrix index>
	// inside the 100 character label limit webhook.go enforces.
	maxRouteSuffix = 100 - len("runquill-") - 19 - 1 - 3 - 1 - 4
)

const (
	outcomeChanged       = "changed"
	outcomeUnmapped      = "unmapped"
	outcomeExpression    = "expression"
	outcomeMapping       = "mapping"
	outcomeAnchor        = "anchor"
	outcomeSelfHosted    = "self-hosted"
	outcomeRunquill      = "runquill"
	outcomeReusable      = "reusable-call"
	outcomeWorkflowCall  = "workflow-call"
	outcomeNoRunsOn      = "no-runs-on"
	outcomeUnparseable   = "unparseable"
	outcomeInvalidJobID  = "job-id"
	reasonExpression     = "runs-on is an expression"
	reasonMapping        = "runs-on selects runners with group or labels"
	reasonAnchor         = "the job uses YAML anchors, aliases or merge keys"
	reasonSelfHosted     = "the job already targets self-hosted runners"
	reasonRunquill       = "the job already targets Runquill"
	reasonReusable       = "the job calls a reusable workflow"
	reasonWorkflowCall   = "the file is a reusable workflow (workflow_call), so labels would not be unique in the caller's run"
	reasonNoRunsOn       = "the job has no runs-on"
	reasonUnmapped       = "no mapping matches these runs-on labels"
	reasonMultiDocument  = "the file contains more than one YAML document"
	reasonTabs           = "the file indents with tabs"
	reasonCarriageReturn = "the file contains a carriage return outside a CRLF line ending"
	reasonEncoding       = "the file is not valid UTF-8"
)

var (
	jobRouteSuffix     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	yamlPlainKey       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$|^<<$`)
	runquillExpression = regexp.MustCompile(`(?i)runquill-`)
	workflowCallWord   = regexp.MustCompile(`(^|[^A-Za-z0-9_-])workflow_call([^A-Za-z0-9_-]|$)`)
)

// runsOnMapping moves jobs whose runs-on label set equals From to a Runquill pool.
// The runs-on value itself is generated, because webhook.go only accepts per-job
// labels built from the run ID, the run attempt and a suffix unique to the job.
type runsOnMapping struct {
	From []string `json:"from"`
	Pool string   `json:"pool,omitempty"`
}

type jobOutcome struct {
	File    string   `json:"file,omitempty"`
	Job     string   `json:"job"`
	Line    int      `json:"line"`
	Labels  []string `json:"labels,omitempty"`
	Changed bool     `json:"changed"`
	RunsOn  string   `json:"runs_on,omitempty"`
	Code    string   `json:"code"`
	Reason  string   `json:"reason,omitempty"`
}

type byteEdit struct {
	start, end int
	text       string
}

type rewriteResult struct {
	Output  []byte
	Jobs    []jobOutcome
	Refused string
	edits   []byteEdit
}

func (r rewriteResult) changed() bool {
	return len(r.edits) > 0
}

func labelSetKey(labels []string) string {
	set := make([]string, 0, len(labels))
	for _, label := range labels {
		set = append(set, strings.ToLower(label))
	}
	slices.Sort(set)
	return strings.Join(slices.Compact(set), "\x00")
}

func validateRunsOnMapping(mapping []runsOnMapping) (map[string]string, error) {
	if len(mapping) == 0 {
		return nil, errors.New("add at least one runs-on mapping")
	}
	if len(mapping) > maxRunsOnMappings {
		return nil, fmt.Errorf("use at most %d runs-on mappings", maxRunsOnMappings)
	}
	pools := make(map[string]string, len(mapping))
	for _, m := range mapping {
		if len(m.From) == 0 || len(m.From) > 10 {
			return nil, errors.New("each mapping needs between 1 and 10 runs-on labels")
		}
		for _, label := range m.From {
			lower := strings.ToLower(label)
			if label == "" || len(label) > 100 || strings.ContainsFunc(label, func(r rune) bool { return r < ' ' || r == 0x7f }) ||
				strings.Contains(label, "${{") || strings.TrimSpace(label) != label {
				return nil, errors.New("runs-on labels must be 1 to 100 printable characters without expressions")
			}
			if lower == "self-hosted" || strings.HasPrefix(lower, "runquill-") {
				return nil, errors.New("mappings move jobs from GitHub-hosted labels; self-hosted and Runquill labels are left alone")
			}
		}
		if m.Pool != "" && !workerPoolName.MatchString(m.Pool) {
			return nil, errors.New("pool names use lowercase letters, digits and hyphens")
		}
		key := labelSetKey(m.From)
		if _, dup := pools[key]; dup {
			return nil, errors.New("each runs-on label set can be mapped only once")
		}
		pools[key] = m.Pool
	}
	return pools, nil
}

// runquillRunsOn renders the per-job route label for a job ID and pool.
func runquillRunsOn(jobID, pool string, matrix bool) (string, error) {
	suffix := strings.ToLower(jobID)
	if !jobRouteSuffix.MatchString(suffix) {
		return "", errors.New("Runquill labels need a job ID that starts with a letter or digit")
	}
	if strings.HasPrefix(suffix, "pool__") {
		// worker_pools.go would read the job ID itself as a pool selector.
		return "", errors.New("a job ID starting with pool__ would select a worker pool")
	}
	if pool != "" && pool != defaultWorkerPool {
		suffix = "pool__" + pool + "__" + suffix
	}
	if len(suffix) > maxRouteSuffix {
		return "", fmt.Errorf("the job ID is too long for a Runquill label (at most %d characters with the pool)", maxRouteSuffix)
	}
	if matrix {
		return "${{ format('runquill-{0}-{1}-" + suffix + "-{2}', github.run_id, github.run_attempt, strategy.job-index) }}", nil
	}
	return "${{ format('runquill-{0}-{1}-" + suffix + "', github.run_id, github.run_attempt) }}", nil
}

type yamlLine struct {
	start, end, next int // content [start,end) excludes the line ending; next starts the following line
	indent           int // leading spaces; -1 for blank and comment-only lines
	tabbed           bool
}

type valueForm int

const (
	formEmpty valueForm = iota
	formPlain
	formDouble
	formSingle
	formFlowSeq
	formFlowMap
	formBlockScalar
	formAnchor
	formTag
	formIncomplete
	formInvalid
)

type inlineValue struct {
	form       valueForm
	start, end int // offsets within the line's content
	text       string
	comment    bool
}

type wfProp struct {
	key       string
	line, end int // end is the line index after this property's nested lines
	colon     int // offset of ':' within the line
	value     inlineValue
}

type wfJob struct {
	id       string
	line     int
	props    []wfProp
	code     string
	refusal  string
	bodyFrom int
	bodyTo   int
}

func (j *wfJob) prop(key string) (wfProp, bool) {
	for _, p := range j.props {
		if p.key == key {
			return p, true
		}
	}
	return wfProp{}, false
}

func (j *wfJob) refuse(code, reason string) {
	if j.code == "" {
		j.code, j.refusal = code, reason
	}
}

type wfDoc struct {
	src     []byte
	lines   []yamlLine
	top     []wfProp
	jobs    []*wfJob
	refusal string
}

func (d *wfDoc) text(i int) string {
	return string(d.src[d.lines[i].start:d.lines[i].end])
}

func (d *wfDoc) topProp(key string) (wfProp, bool) {
	for _, p := range d.top {
		if p.key == key {
			return p, true
		}
	}
	return wfProp{}, false
}

// parseInline classifies the single-line value that starts at from.
func parseInline(text string, from int) inlineValue {
	i := from
	for i < len(text) && text[i] == ' ' {
		i++
	}
	v := inlineValue{start: i, end: i}
	if i == len(text) {
		return v
	}
	if text[i] == '#' {
		if i == from && from > 0 {
			return inlineValue{form: formInvalid}
		}
		v.comment = true
		return v
	}
	rest := func(j int) inlineValue {
		k := j
		for k < len(text) && (text[k] == ' ' || text[k] == '\t') {
			k++
		}
		if k < len(text) && (text[k] != '#' || k == j) {
			return inlineValue{form: formInvalid}
		}
		v.end, v.comment = j, k < len(text)
		return v
	}
	switch c := text[i]; c {
	case '"':
		for j := i + 1; j < len(text); j++ {
			if text[j] == '\\' {
				return inlineValue{form: formInvalid}
			}
			if text[j] == '"' {
				v.form, v.text = formDouble, text[i+1:j]
				return rest(j + 1)
			}
		}
		return inlineValue{form: formIncomplete}
	case '\'':
		for j := i + 1; j < len(text); j++ {
			if text[j] != '\'' {
				continue
			}
			if j+1 < len(text) && text[j+1] == '\'' {
				j++
				continue
			}
			v.form, v.text = formSingle, strings.ReplaceAll(text[i+1:j], "''", "'")
			return rest(j + 1)
		}
		return inlineValue{form: formIncomplete}
	case '[', '{':
		end := flowEnd(text, i)
		if end < 0 {
			return inlineValue{form: formIncomplete}
		}
		v.form, v.text = formFlowSeq, text[i:end]
		if c == '{' {
			v.form = formFlowMap
		}
		return rest(end)
	case '|', '>':
		v.form = formBlockScalar
		return v
	case '&', '*':
		v.form = formAnchor
		return v
	case '!':
		v.form = formTag
		return v
	case '%', '@', '`', ']', '}', ',':
		return inlineValue{form: formInvalid}
	}
	end := len(text)
	for j := i + 1; j < len(text); j++ {
		if text[j] == '#' && (text[j-1] == ' ' || text[j-1] == '\t') {
			end, v.comment = j, true
			break
		}
	}
	for end > i && (text[end-1] == ' ' || text[end-1] == '\t') {
		end--
	}
	v.form, v.end, v.text = formPlain, end, text[i:end]
	if strings.Contains(v.text, ": ") || strings.HasSuffix(v.text, ":") || strings.HasPrefix(v.text, "- ") || v.text == "-" {
		return inlineValue{form: formInvalid}
	}
	return v
}

// flowEnd returns the offset after the bracket that closes text[open], or -1.
func flowEnd(text string, open int) int {
	depth := 0
	for j := open; j < len(text); j++ {
		switch text[j] {
		case '"':
			k := strings.IndexByte(text[j+1:], '"')
			if k < 0 || strings.Contains(text[j+1:j+1+k], `\`) {
				return -1
			}
			j += k + 1
		case '\'':
			k := j + 1
			for ; k < len(text); k++ {
				if text[k] == '\'' {
					if k+1 < len(text) && text[k+1] == '\'' {
						k++
						continue
					}
					break
				}
			}
			if k >= len(text) {
				return -1
			}
			j = k
		case '#':
			if j > 0 && (text[j-1] == ' ' || text[j-1] == '\t') {
				return -1
			}
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return -1
}

// parseYAMLKey reads "key:" at the start of text and returns the key and the colon offset.
func parseYAMLKey(text string, at int) (string, int, bool) {
	s := text[at:]
	var key string
	var colon int
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		end := strings.IndexByte(s[1:], s[0])
		if end < 0 {
			return "", 0, false
		}
		key, colon = s[1:end+1], end+2
		if strings.ContainsAny(key, `\'"`) || colon >= len(s) || s[colon] != ':' {
			return "", 0, false
		}
	} else {
		colon = -1
		for i := 0; i < len(s); i++ {
			if s[i] == ':' && (i+1 == len(s) || s[i+1] == ' ') {
				colon = i
				break
			}
		}
		if colon < 0 {
			return "", 0, false
		}
		key = strings.TrimRight(s[:colon], " ")
	}
	if !yamlPlainKey.MatchString(key) || colon+1 < len(s) && s[colon+1] != ' ' {
		return "", 0, false
	}
	return key, at + colon, true
}

func isSequenceItem(text string, indent int) bool {
	return strings.HasPrefix(text[indent:], "- ") || text[indent:] == "-"
}

func parseWorkflow(src []byte) *wfDoc {
	d := &wfDoc{src: src}
	if !utf8.Valid(src) {
		d.refusal = reasonEncoding
		return d
	}
	at := 0
	if bytes.HasPrefix(src, []byte("\xEF\xBB\xBF")) {
		at = 3
	}
	for at < len(src) {
		line := yamlLine{start: at}
		nl := bytes.IndexByte(src[at:], '\n')
		if nl < 0 {
			line.end, line.next = len(src), len(src)
		} else {
			line.end, line.next = at+nl, at+nl+1
			if line.end > at && src[line.end-1] == '\r' {
				line.end--
			}
		}
		content := src[line.start:line.end]
		if bytes.IndexByte(content, '\r') >= 0 {
			d.refusal = reasonCarriageReturn
			return d
		}
		spaces := 0
		for spaces < len(content) && content[spaces] == ' ' {
			spaces++
		}
		trimmed := bytes.TrimLeft(content, " \t")
		line.indent = spaces
		if len(trimmed) == 0 || trimmed[0] == '#' {
			line.indent = -1
		} else {
			line.tabbed = spaces < len(content) && content[spaces] == '\t'
		}
		d.lines = append(d.lines, line)
		at = line.next
	}
	d.parseTop()
	if d.refusal == "" {
		d.parseJobs()
	}
	return d
}

func (d *wfDoc) parseTop() {
	started := false
	for i, line := range d.lines {
		if line.indent < 0 {
			continue
		}
		text := d.text(i)
		if line.indent > 0 {
			if line.tabbed && len(d.top) == 0 {
				d.refusal = reasonTabs
				return
			}
			if len(d.top) == 0 {
				d.refusal = fmt.Sprintf("line %d is indented before any top-level key", i+1)
				return
			}
			continue
		}
		if line.tabbed {
			d.refusal = reasonTabs
			return
		}
		switch {
		case text == "---" || strings.HasPrefix(text, "--- ") || text == "...":
			if started || text != "---" {
				d.refusal = reasonMultiDocument
				return
			}
			started = true
			continue
		case strings.HasPrefix(text, "%"):
			d.refusal = "the file uses YAML directives"
			return
		case isSequenceItem(text, 0):
			if len(d.top) == 0 || d.top[len(d.top)-1].value.form != formEmpty {
				d.refusal = fmt.Sprintf("line %d is not a top-level key", i+1)
				return
			}
			continue
		}
		started = true
		key, colon, ok := parseYAMLKey(text, 0)
		if !ok {
			d.refusal = fmt.Sprintf("line %d is not a top-level key the scanner understands", i+1)
			return
		}
		if _, dup := d.topProp(key); dup {
			d.refusal = fmt.Sprintf("the top-level key %q appears more than once", key)
			return
		}
		value := parseInline(text, colon+1)
		if value.form == formIncomplete || value.form == formInvalid {
			d.refusal = fmt.Sprintf("line %d has a value the scanner cannot read on one line", i+1)
			return
		}
		if len(d.top) > 0 {
			d.top[len(d.top)-1].end = i
		}
		d.top = append(d.top, wfProp{key: key, line: i, end: len(d.lines), colon: colon, value: value})
	}
}

func (d *wfDoc) parseJobs() {
	jobs, ok := d.topProp("jobs")
	if !ok {
		return
	}
	switch jobs.value.form {
	case formEmpty:
	case formAnchor:
		d.refusal = "jobs uses YAML anchors or aliases"
		return
	default:
		d.refusal = "jobs is not a block mapping"
		return
	}
	jobIndent := -1
	var current *wfJob
	seen := make(map[string]bool)
	for i := jobs.line + 1; i < jobs.end; i++ {
		line := d.lines[i]
		if line.indent < 0 {
			continue
		}
		if jobIndent < 0 {
			jobIndent = line.indent
		}
		if line.tabbed && line.indent <= jobIndent {
			d.refusal = reasonTabs
			return
		}
		if line.indent > jobIndent {
			continue
		}
		text := d.text(i)
		if line.indent < jobIndent || isSequenceItem(text, line.indent) {
			d.refusal = fmt.Sprintf("line %d does not continue the jobs mapping", i+1)
			return
		}
		id, colon, ok := parseYAMLKey(text, line.indent)
		if !ok || id == "<<" {
			d.refusal = fmt.Sprintf("line %d is not a job the scanner understands", i+1)
			return
		}
		if seen[id] {
			d.refusal = fmt.Sprintf("the job %q appears more than once", id)
			return
		}
		seen[id] = true
		if current != nil {
			current.bodyTo = i
		}
		current = &wfJob{id: id, line: i, bodyFrom: i + 1, bodyTo: jobs.end}
		d.jobs = append(d.jobs, current)
		switch parseInline(text, colon+1).form {
		case formEmpty:
		case formAnchor:
			current.refuse(outcomeAnchor, reasonAnchor)
		default:
			current.refuse(outcomeUnparseable, "the job is not a block mapping")
		}
	}
	for _, j := range d.jobs {
		if d.parseJobBody(j); d.refusal != "" {
			return
		}
	}
}

func (d *wfDoc) parseJobBody(j *wfJob) {
	indent := -1
	for i := j.bodyFrom; i < j.bodyTo; i++ {
		line := d.lines[i]
		if line.indent < 0 {
			continue
		}
		if indent < 0 {
			indent = line.indent
		}
		if line.tabbed && line.indent <= indent {
			d.refusal = reasonTabs
			return
		}
		if line.indent > indent {
			if len(j.props) == 0 {
				j.refuse(outcomeUnparseable, fmt.Sprintf("line %d has inconsistent indentation", i+1))
			}
			continue
		}
		if line.indent < indent {
			j.refuse(outcomeUnparseable, fmt.Sprintf("line %d has inconsistent indentation", i+1))
			continue
		}
		text := d.text(i)
		if isSequenceItem(text, indent) {
			if len(j.props) == 0 || j.props[len(j.props)-1].value.form != formEmpty {
				j.refuse(outcomeUnparseable, fmt.Sprintf("line %d is not a job setting", i+1))
			}
			continue
		}
		key, colon, ok := parseYAMLKey(text, indent)
		if !ok {
			j.refuse(outcomeUnparseable, fmt.Sprintf("line %d is not a job setting the scanner understands", i+1))
			continue
		}
		if key == "<<" {
			j.refuse(outcomeAnchor, reasonAnchor)
			continue
		}
		if _, dup := j.prop(key); dup {
			j.refuse(outcomeUnparseable, fmt.Sprintf("%s appears more than once in the job", key))
			continue
		}
		value := parseInline(text, colon+1)
		if value.form == formIncomplete || value.form == formInvalid {
			j.refuse(outcomeUnparseable, fmt.Sprintf("line %d has a value the scanner cannot read on one line", i+1))
		}
		if len(j.props) > 0 {
			j.props[len(j.props)-1].end = i
		}
		j.props = append(j.props, wfProp{key: key, line: i, end: j.bodyTo, colon: colon, value: value})
	}
}

// nested reports the significant lines inside a property's value after its key line.
func (d *wfDoc) nested(p wfProp) []int {
	var out []int
	for i := p.line + 1; i < p.end; i++ {
		if d.lines[i].indent >= 0 {
			out = append(out, i)
		}
	}
	return out
}

type parsedRunsOn struct {
	labels  []string
	line    int
	edit    func(string) []byteEdit
	code    string
	refusal string
}

func (d *wfDoc) runsOn(p wfProp) parsedRunsOn {
	out := parsedRunsOn{line: p.line + 1}
	refuse := func(code, reason string) parsedRunsOn {
		out.code, out.refusal = code, reason
		return out
	}
	lineStart := d.lines[p.line].start
	v := p.value
	nested := d.nested(p)
	switch v.form {
	case formAnchor:
		return refuse(outcomeAnchor, reasonAnchor)
	case formFlowMap:
		return refuse(outcomeMapping, reasonMapping)
	case formBlockScalar, formTag, formIncomplete, formInvalid:
		return refuse(outcomeUnparseable, "runs-on uses a YAML form the scanner does not rewrite")
	case formPlain, formDouble, formSingle, formFlowSeq:
		if len(nested) > 0 {
			return refuse(outcomeUnparseable, "runs-on continues on the next line")
		}
	}
	switch v.form {
	case formPlain, formDouble, formSingle:
		out.labels = []string{v.text}
		style := map[valueForm]byte{formDouble: '"', formSingle: '\''}[v.form]
		out.edit = func(value string) []byteEdit {
			return []byteEdit{{lineStart + v.start, lineStart + v.end, quoteYAML(value, style)}}
		}
		return out
	case formFlowSeq:
		if strings.Contains(v.text, "${{") {
			// Expressions may contain commas; classification only needs to see one.
			out.labels = []string{v.text}
			return out
		}
		items, code, reason := flowItems(v.text)
		if code != "" {
			return refuse(code, reason)
		}
		out.labels = items
		out.edit = func(value string) []byteEdit {
			return []byteEdit{{lineStart + v.start, lineStart + v.end, value}}
		}
		return out
	}
	if len(nested) == 0 {
		return refuse(outcomeUnparseable, "runs-on is empty")
	}
	first := d.lines[nested[0]]
	if !isSequenceItem(d.text(nested[0]), first.indent) {
		if _, _, isKey := parseYAMLKey(d.text(nested[0]), first.indent); isKey {
			return refuse(outcomeMapping, reasonMapping)
		}
		return refuse(outcomeUnparseable, "runs-on continues on the next line")
	}
	var deletes []byteEdit
	for _, i := range nested {
		line := d.lines[i]
		text := d.text(i)
		if line.tabbed || line.indent != first.indent || !isSequenceItem(text, line.indent) {
			return refuse(outcomeUnparseable, "runs-on has a nested list")
		}
		item := parseInline(text, line.indent+1)
		switch item.form {
		case formPlain, formDouble, formSingle:
		case formAnchor:
			return refuse(outcomeAnchor, reasonAnchor)
		case formFlowMap:
			return refuse(outcomeMapping, reasonMapping)
		default:
			return refuse(outcomeUnparseable, "a runs-on list item uses a YAML form the scanner does not rewrite")
		}
		if item.comment {
			return refuse(outcomeUnparseable, "a runs-on list item has a comment that rewriting would drop")
		}
		out.labels = append(out.labels, item.text)
		if n := len(deletes); n > 0 && deletes[n-1].end == line.start {
			deletes[n-1].end = line.next
		} else {
			deletes = append(deletes, byteEdit{line.start, line.next, ""})
		}
	}
	if last := nested[len(nested)-1]; d.lines[last].next == d.lines[last].end {
		// The file ends on the last item without a line ending; keep it that way by
		// removing the terminator before the final deleted run instead.
		run := &deletes[len(deletes)-1]
		for i := last; i > p.line; i-- {
			if d.lines[i].start == run.start {
				run.start = d.lines[i-1].end
				break
			}
		}
	}
	merged := deletes
	colon := lineStart + p.colon + 1
	out.edit = func(value string) []byteEdit {
		return append([]byteEdit{{colon, colon, " " + value}}, merged...)
	}
	return out
}

func flowItems(text string) ([]string, string, string) {
	inner := strings.TrimSpace(text[1 : len(text)-1])
	if inner == "" {
		return nil, outcomeUnparseable, "runs-on is empty"
	}
	var items []string
	for raw := range strings.SplitSeq(inner, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		item := parseInline(raw, 0)
		switch item.form {
		case formPlain, formDouble, formSingle:
		case formAnchor:
			return nil, outcomeAnchor, reasonAnchor
		default:
			return nil, outcomeUnparseable, "runs-on has a nested or unusual list item"
		}
		if item.end != len(raw) || strings.ContainsAny(item.text, "[]{}") {
			return nil, outcomeUnparseable, "runs-on has a nested or unusual list item"
		}
		items = append(items, item.text)
	}
	if len(items) == 0 {
		return nil, outcomeUnparseable, "runs-on is empty"
	}
	return items, "", ""
}

func quoteYAML(value string, style byte) string {
	switch style {
	case '"':
		return `"` + value + `"`
	case '\'':
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return value
}

// classifyLabels returns the outcome for labels that rewriting must leave alone.
func classifyLabels(labels []string) (string, string) {
	for _, label := range labels {
		if strings.Contains(label, "${{") {
			if runquillExpression.MatchString(label) {
				return outcomeRunquill, reasonRunquill
			}
			return outcomeExpression, reasonExpression
		}
	}
	for _, label := range labels {
		lower := strings.ToLower(label)
		if strings.HasPrefix(lower, "runquill-") {
			return outcomeRunquill, reasonRunquill
		}
		if lower == "self-hosted" {
			return outcomeSelfHosted, reasonSelfHosted
		}
	}
	return "", ""
}

// reusableDefinition reports whether the workflow can be called from another workflow.
func (d *wfDoc) reusableDefinition() bool {
	on, ok := d.topProp("on")
	if !ok {
		return false
	}
	for i := on.line; i < on.end; i++ {
		if d.lines[i].indent < 0 {
			continue
		}
		text := d.text(i)
		if i == on.line {
			text = text[on.colon+1:]
		}
		if before, _, found := strings.Cut(text, " #"); found {
			text = before
		}
		if workflowCallWord.MatchString(text) {
			return true
		}
	}
	return false
}

func rewriteWorkflow(src []byte, mapping []runsOnMapping) (rewriteResult, error) {
	pools, err := validateRunsOnMapping(mapping)
	if err != nil {
		return rewriteResult{}, err
	}
	res := rewriteWith(src, pools)
	if !res.changed() {
		return res, nil
	}
	// Re-read the output: every rewritten job must now read as Runquill and nothing else may move.
	again := rewriteWith(res.Output, pools)
	if again.Refused != "" || again.changed() || len(again.Jobs) != len(res.Jobs) {
		return rewriteResult{Output: src, Refused: "the rewritten file did not pass the consistency check"}, nil
	}
	for i, job := range res.Jobs {
		next := again.Jobs[i]
		if next.Job != job.Job || job.Changed && (next.Code != outcomeRunquill || len(next.Labels) != 1 || next.Labels[0] != job.RunsOn) ||
			!job.Changed && next.Code != job.Code {
			return rewriteResult{Output: src, Refused: "the rewritten file did not pass the consistency check"}, nil
		}
	}
	return res, nil
}

func rewriteWith(src []byte, pools map[string]string) rewriteResult {
	res := rewriteResult{Output: src}
	d := parseWorkflow(src)
	if d.refusal != "" {
		res.Refused = d.refusal
		return res
	}
	reusable := d.reusableDefinition()
	lowered := make(map[string]int)
	for _, j := range d.jobs {
		lowered[strings.ToLower(j.id)]++
	}
	for _, j := range d.jobs {
		outcome := jobOutcome{Job: j.id, Line: j.line + 1}
		var edits []byteEdit
		_, matrix := j.prop("strategy")
		runsOn, hasRunsOn := j.prop("runs-on")
		_, uses := j.prop("uses")
		var parsed parsedRunsOn
		if hasRunsOn {
			parsed = d.runsOn(runsOn)
			outcome.Line, outcome.Labels = parsed.line, parsed.labels
		}
		code, reason := classifyLabels(parsed.labels)
		switch {
		case j.code != "":
			outcome.Code, outcome.Reason = j.code, j.refusal
		case uses:
			outcome.Code, outcome.Reason = outcomeReusable, reasonReusable
		case !hasRunsOn:
			outcome.Code, outcome.Reason = outcomeNoRunsOn, reasonNoRunsOn
		case parsed.code != "":
			outcome.Code, outcome.Reason = parsed.code, parsed.refusal
		case code != "":
			outcome.Code, outcome.Reason = code, reason
		default:
			pool, mapped := pools[labelSetKey(parsed.labels)]
			value, labelErr := runquillRunsOn(j.id, pool, matrix)
			switch {
			case !mapped:
				outcome.Code, outcome.Reason = outcomeUnmapped, reasonUnmapped
			case reusable:
				outcome.Code, outcome.Reason = outcomeWorkflowCall, reasonWorkflowCall
			case labelErr != nil:
				outcome.Code, outcome.Reason = outcomeInvalidJobID, labelErr.Error()
			case lowered[strings.ToLower(j.id)] > 1:
				outcome.Code, outcome.Reason = outcomeInvalidJobID, "another job ID differs only in letter case, so their Runquill labels would collide"
			default:
				outcome.Code, outcome.Changed, outcome.RunsOn = outcomeChanged, true, value
				edits = parsed.edit(value)
			}
		}
		res.Jobs = append(res.Jobs, outcome)
		res.edits = append(res.edits, edits...)
	}
	if len(res.edits) > 0 {
		res.Output = applyEdits(src, res.edits)
	}
	return res
}

func applyEdits(src []byte, edits []byteEdit) []byte {
	slices.SortStableFunc(edits, func(a, b byteEdit) int { return a.start - b.start })
	out := make([]byte, 0, len(src)+256)
	at := 0
	for _, e := range edits {
		out = append(out, src[at:e.start]...)
		out = append(out, e.text...)
		at = e.end
	}
	return append(out, src[at:]...)
}

// unifiedDiff renders a line diff with three lines of context for display.
func unifiedDiff(path string, before, after []byte) string {
	if bytes.Equal(before, after) {
		return ""
	}
	a, b := diffLines(before), diffLines(after)
	ops := myersDiff(a, b)
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", path, path)
	const context = 3
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-context, 0)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				end = min(end+context, len(ops))
				break
			}
			end = run
		}
		aStart, bStart, aCount, bCount := ops[start].a, ops[start].b, 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				aCount++
			}
			if op.kind != '-' {
				bCount++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aCount), hunkRange(bStart, bCount))
		for _, op := range ops[start:end] {
			text := b
			index := op.b
			if op.kind == '-' {
				text, index = a, op.a
			}
			out.WriteByte(op.kind)
			out.WriteString(strings.TrimSuffix(strings.TrimSuffix(text[index], "\n"), "\r"))
			out.WriteByte('\n')
			if !strings.HasSuffix(text[index], "\n") {
				out.WriteString("\\ No newline at end of file\n")
			}
		}
		i = end
	}
	return out.String()
}

func hunkRange(start, count int) string {
	if count == 0 {
		return strconv.Itoa(start) + ",0"
	}
	if count == 1 {
		return strconv.Itoa(start + 1)
	}
	return strconv.Itoa(start+1) + "," + strconv.Itoa(count)
}

func diffLines(data []byte) []string {
	var lines []string
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			lines = append(lines, string(data))
			break
		}
		lines = append(lines, string(data[:i+1]))
		data = data[i+1:]
	}
	return lines
}

type diffOp struct {
	kind byte
	a, b int // indexes in the old and new lines (positions for insertions and deletions)
}

// myersDiff computes a shortest edit script between two line lists.
func myersDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	maxD := n + m
	offset := maxD + 1
	v := make([]int, 2*maxD+3)
	// Each step keeps only the diagonals it can reach, so memory grows with the edit size.
	var trace [][]int
	for d := 0; d <= maxD; d++ {
		trace = append(trace, slices.Clone(v[offset-d-1:offset+d+2]))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || k != d && v[offset+k-1] < v[offset+k+1] {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return backtrack(trace, x, y, d)
			}
		}
	}
	return nil
}

func backtrack(trace [][]int, x, y, d int) []diffOp {
	var ops []diffOp
	for ; d > 0; d-- {
		v := func(k int) int { return trace[d][k+d+1] }
		k := x - y
		var prevK int
		if k == -d || k != d && v(k-1) < v(k+1) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x, y = x-1, y-1
			ops = append(ops, diffOp{' ', x, y})
		}
		if x == prevX {
			y--
			ops = append(ops, diffOp{'+', x, y})
		} else {
			x--
			ops = append(ops, diffOp{'-', x, y})
		}
	}
	for x > 0 && y > 0 {
		x, y = x-1, y-1
		ops = append(ops, diffOp{' ', x, y})
	}
	slices.Reverse(ops)
	return ops
}
