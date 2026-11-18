package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

const wizardLogLimit = 32 << 10

var wizardANSI = regexp.MustCompile("\\x1b\\[[0-?]*[ -/]*[@-~]")

type wizardProgress struct {
	mu     sync.Mutex
	stage  string
	tail   []byte
	line   []byte
	skip   bool
	redact *strings.Replacer
}

func (p *wizardProgress) reset(values map[string]string) {
	var pairs []string
	for _, field := range []string{"RUNQUILL_PANEL_GITHUB_CLIENT_SECRET", "RUNQUILL_WEBHOOK_SECRET", "RUNQUILL_WORKER_TOKEN"} {
		if value := values[field]; value != "" {
			pairs = append(pairs, value, "[redacted]")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stage, p.tail, p.line, p.skip = "", nil, nil, false
	p.redact = strings.NewReplacer(pairs...)
}

func (p *wizardProgress) mark(stage string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stage = stage
}

// Emit complete lines so secrets split across command writes can be redacted.
func (p *wizardProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	size := len(data)
	for len(data) > 0 {
		end := bytes.IndexAny(data, "\r\n")
		part := data
		if end >= 0 {
			part = data[:end]
		}
		if len(p.line)+len(part) > wizardLogLimit {
			p.line, p.skip = nil, true
		}
		if !p.skip {
			p.line = append(p.line, part...)
		}
		if end < 0 {
			break
		}
		text := wizardANSI.ReplaceAllString(string(p.line), "")
		if p.redact != nil {
			text = p.redact.Replace(text)
		}
		if p.skip {
			text = "[oversized log line omitted]"
		}
		if text != "" {
			p.tail = append(p.tail, text...)
			p.tail = append(p.tail, '\n')
			for len(p.tail) > wizardLogLimit {
				drop := bytes.IndexByte(p.tail, '\n') + 1
				p.tail = p.tail[drop:]
			}
		}
		p.line, p.skip = p.line[:0], false
		data = data[end+1:]
	}
	return size, nil
}

func (p *wizardProgress) snapshot() (stage, log string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stage, string(p.tail)
}

func (web *setupWizard) status(w http.ResponseWriter) {
	web.mu.Lock()
	phase, step, install := web.phase, web.step, web.installURL != ""
	web.mu.Unlock()
	stage, log := web.progress.snapshot()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Phase   string `json:"phase"`
		Step    string `json:"step"`
		Install bool   `json:"install"`
		Stage   string `json:"stage"`
		Log     string `json:"log"`
	}{phase, step, install, stage, log})
}
