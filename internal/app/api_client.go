package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	apiClientTimeout = 15 * time.Second
	apiMaxResponse   = 8 << 20
	apiMaxError      = 4 << 10
)

var apiTokenFormat = regexp.MustCompile(`^rqk_[A-Za-z0-9_-]{43}$`)

// apiError is a non-2xx panel answer. The panel writes errors as one plain
// sentence; Message holds it cleaned for a terminal.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the panel answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("the panel answered HTTP %d: %s", e.Status, e.Message)
}

// apiClient calls a remote panel with an API token. The token only ever goes
// into the Authorization header of requests to the configured origin.
type apiClient struct {
	base  *url.URL
	token string
	http  *http.Client
}

func newAPIClient(rawURL, token string) (*apiClient, error) {
	base, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery ||
		base.Fragment != "" || (base.Path != "" && base.Path != "/") ||
		(base.Scheme != "https" && !(base.Scheme == "http" && isPanelLoopback(base.Hostname()))) {
		return nil, errors.New("set RUNQUILL_URL or --url to the panel's HTTPS origin, or an HTTP loopback origin")
	}
	base.Path, base.RawPath = "", ""
	if !apiTokenFormat.MatchString(token) {
		return nil, errors.New("the API token is not a Runquill token (rqk_...); create one in the panel under API tokens")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = apiClientTimeout
	return &apiClient{base: base, token: token, http: &http.Client{
		Timeout:   apiClientTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// A redirect elsewhere could be a misconfigured proxy or a lure for the token.
			if req.URL.Scheme != base.Scheme || !strings.EqualFold(req.URL.Host, base.Host) {
				return errors.New("the panel redirected to another origin; refusing to follow it")
			}
			if len(via) >= 3 {
				return errors.New("the panel redirected too many times")
			}
			return nil
		},
	}}, nil
}

func (c *apiClient) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, []byte, error) {
	target := c.base.ResolveReference(&url.URL{Path: path, RawQuery: query.Encode()})
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "runquill-cli/"+version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("could not reach the panel: %w", err)
	}
	defer resp.Body.Close()
	limit := int64(apiMaxResponse)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		limit = apiMaxError
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("could not read the panel response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, &apiError{Status: resp.StatusCode, Message: apiErrorMessage(resp.Header.Get("Content-Type"), data[:min(int64(len(data)), limit)])}
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("the panel response exceeded %d MiB", apiMaxResponse>>20)
	}
	return resp, data, nil
}

// getAPIJSON returns the decoded value and the raw body, which --json prints unchanged.
func getAPIJSON[T any](ctx context.Context, c *apiClient, path string, query url.Values) (T, []byte, error) {
	var value T
	resp, data, err := c.do(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return value, nil, err
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "application/json" {
		return value, nil, errors.New("the panel did not answer with JSON; check RUNQUILL_URL")
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, nil, fmt.Errorf("could not decode the panel response: %w", err)
	}
	return value, data, nil
}

func apiErrorMessage(contentType string, body []byte) string {
	text := string(body)
	if mediaType, _, _ := mime.ParseMediaType(contentType); mediaType == "application/json" {
		var reply struct{ Error, Message string }
		if json.Unmarshal(body, &reply) == nil {
			text = cmp.Or(reply.Error, reply.Message)
		}
	}
	text, _, _ = strings.Cut(strings.TrimSpace(text), "\n")
	return terminalSafe(text, 300)
}

// terminalSafe drops control characters, so panel text cannot move the
// cursor or inject escape sequences, and caps the length in runes.
func terminalSafe(text string, limit int) string {
	var b strings.Builder
	count := 0
	for _, r := range text {
		if count == limit {
			b.WriteString("...")
			break
		}
		if unicode.IsControl(r) {
			r = ' '
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String())
}

type jobHistoryQuery struct {
	repository, outcome string
	since               time.Time
	limit               int
}

func (c *apiClient) jobHistory(ctx context.Context, q jobHistoryQuery) (panelHistoryPageResponse, []byte, error) {
	query := url.Values{}
	if q.repository != "" {
		query.Set("repository", q.repository)
	}
	if q.outcome != "" {
		query.Set("outcome", q.outcome)
	}
	if !q.since.IsZero() {
		query.Set("since", strconv.FormatInt(q.since.Unix(), 10))
	}
	if q.limit > 0 {
		query.Set("limit", strconv.Itoa(q.limit))
	}
	return getAPIJSON[panelHistoryPageResponse](ctx, c, "/api/jobs", query)
}

func (c *apiClient) job(ctx context.Context, id int64) (panelHistoryJob, []byte, error) {
	return getAPIJSON[panelHistoryJob](ctx, c, "/api/jobs/"+strconv.FormatInt(id, 10), nil)
}

func (c *apiClient) snapshot(ctx context.Context) (panelSnapshot, []byte, error) {
	return getAPIJSON[panelSnapshot](ctx, c, "/snapshot", nil)
}

func (c *apiClient) auditEvents(ctx context.Context, limit int) ([]panelAuditEvent, []byte, error) {
	return getAPIJSON[[]panelAuditEvent](ctx, c, "/api/audit", url.Values{"limit": {strconv.Itoa(limit)}})
}

func (c *apiClient) setHostCordon(ctx context.Context, host string, cordoned bool) error {
	_, _, err := c.do(ctx, http.MethodPost, "/api/hosts/cordon", nil, struct {
		Host     string `json:"host"`
		Cordoned bool   `json:"cordoned"`
	}{host, cordoned})
	return err
}

func (c *apiClient) changeJob(ctx context.Context, id int64, action string) error {
	_, _, err := c.do(ctx, http.MethodPost, "/api/jobs/"+strconv.FormatInt(id, 10)+"/"+action, nil, nil)
	return err
}
