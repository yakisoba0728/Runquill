package app

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yakisoba0728/Runquill/internal/webui"
)

func TestPanelV2Routes(t *testing.T) {
	t.Setenv("RUNQUILL_PANEL_V2", "1")
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	access := panelAccessForTest(t, q)
	assets := webui.PanelV2()
	page, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	files, err := fs.Glob(assets, "assets/*")
	if err != nil || len(files) == 0 {
		t.Fatalf("panel v2 assets: %v, %v", files, err)
	}
	for _, mode := range []string{"demo", "live"} {
		t.Run(mode, func(t *testing.T) {
			demo := mode == "demo"
			handler := panelHTTPServer(nil, nil).Handler
			if !demo {
				handler = panelHTTPServer(newPanel(config{}), access).Handler
			}
			get := func(path string, status int) *httptest.ResponseRecorder {
				t.Helper()
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, access.cfg.origin+path, nil))
				if rec.Code != status {
					t.Errorf("%s: got HTTP %d, want %d", path, rec.Code, status)
				}
				for header, want := range map[string]string{
					"Cache-Control":           "no-store",
					"Content-Security-Policy": "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'",
					"Referrer-Policy":         "no-referrer",
					"X-Content-Type-Options":  "nosniff",
				} {
					if got := rec.Header().Get(header); got != want {
						t.Errorf("%s: %s = %q, want %q", path, header, got, want)
					}
				}
				return rec
			}
			wantPage := strings.Replace(string(page), `data-mode="demo"`, `data-mode="`+mode+`"`, 1)
			if rec := get("/", 200); rec.Body.String() != wantPage || !strings.Contains(wantPage, `data-mode="`+mode+`"`) {
				t.Error("root did not serve the embedded panel v2 page in the requested mode")
			}
			get("/theme.js", 200)
			for _, file := range files {
				status := http.StatusOK
				if !demo && strings.HasPrefix(file, "assets/demo-") {
					status = http.StatusNotFound
				}
				get("/"+file, status)
			}
			for _, path := range []string{"/assets/app.js", "/assets/panel-.js", "/assets/chunk-abc.map", "/assets/..%2Ftheme.js"} {
				get(path, 400)
			}
			for _, path := range []string{"/assets/chunk-missing.js", "/app.js", "/demo/jobs.js", "/index.html"} {
				get(path, 404)
			}
			if demo {
				get("/api/me", 404)
			} else {
				get("/api/me", 401)
			}
		})
	}
}

func TestPanelV2EmbeddedReferences(t *testing.T) {
	assets := webui.PanelV2()
	htmlRef := regexp.MustCompile(`\b(?:src|href)=["']\./([^"']+)["']`)
	chunkRef := regexp.MustCompile(`\./((?:panel|chunk|demo)-[A-Za-z0-9_-]+\.(?:js|css))`)
	scripts := regexp.MustCompile(`(?i)<script\b[^>]*>`)
	scriptSource := regexp.MustCompile(`(?i)\ssrc\s*=`)
	inlineStyle := regexp.MustCompile(`(?i)<style\b|\sstyle\s*=`)
	demoChunk := false
	if err := fs.WalkDir(assets, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		source, err := fs.ReadFile(assets, name)
		if err != nil {
			return err
		}
		pattern, dir := chunkRef, "assets/"
		if name == "index.html" {
			pattern, dir = htmlRef, ""
			if inlineStyle.Match(source) {
				t.Error("index.html contains inline style")
			}
			for _, script := range scripts.FindAll(source, -1) {
				if !scriptSource.Match(script) {
					t.Error("index.html contains inline script")
				}
			}
		}
		for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
			if _, err := fs.Stat(assets, dir+match[1]); err != nil {
				t.Errorf("%s refers to %s: %v", name, dir+match[1], err)
			}
		}
		if strings.HasPrefix(name, "assets/demo-") {
			demoChunk = true
		} else if strings.Contains(string(source), "runquill-demo-") {
			t.Errorf("%s contains sample fixtures outside a demo chunk", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !demoChunk {
		t.Error("panel v2 has no embedded demo chunk")
	}
}
