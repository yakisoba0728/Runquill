package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	mcpStdioMaxLine = mcpMaxBody
	mcpStdioWorkers = 2 // below the panel's mcpMaxInflight, so one proxy is not refused as busy
)

const mcpStdioUsage = `Usage: runquill mcp [--url ORIGIN] [--token-file FILE]
Serves the Runquill MCP tools on stdin and stdout for a coding agent, forwarding
each message to the panel's /mcp endpoint. The panel origin comes from --url or
RUNQUILL_URL and the fleet:read API token from --token-file or RUNQUILL_TOKEN.`

// mcpCommand runs a stdio MCP server that proxies every request to a remote
// panel. stdout carries only JSON-RPC messages; diagnostics go to stderr.
func mcpCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	for _, arg := range args {
		flagName, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if strings.HasPrefix(arg, "-") && flagName == "token" {
			fmt.Fprintln(stderr, "runquill: pass the API token with RUNQUILL_TOKEN or --token-file; a --token argument would be visible to other local users.")
			return 2
		}
	}
	options := &remoteOptions{stderr: stderr}
	flags := flag.NewFlagSet("runquill mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.url, "url", "", "panel origin (defaults to RUNQUILL_URL)")
	flags.StringVar(&options.tokenFile, "token-file", "", "file holding the API token (defaults to RUNQUILL_TOKEN)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, mcpStdioUsage)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	client, err := options.client()
	if err != nil {
		fmt.Fprintf(stderr, "runquill mcp: %v\n", err)
		return 2
	}
	p := &mcpProxy{client: client, out: bufio.NewWriter(stdout), log: stderr, inflight: make(map[string]context.CancelFunc),
		slots: make(chan struct{}, mcpStdioWorkers)}
	return p.run(stdin)
}

type mcpProxy struct {
	client *apiClient
	log    io.Writer
	slots  chan struct{}
	wg     sync.WaitGroup

	mu       sync.Mutex
	out      *bufio.Writer
	failed   bool
	inflight map[string]context.CancelFunc
	// legacy is the revision a legacy client negotiated with initialize; its
	// later requests carry no _meta, so the proxy supplies the version header.
	legacy string
}

// run reads one message per line until EOF, then lets requests in flight
// finish (each is bounded by the API client timeout) so piped input is answered.
func (p *mcpProxy) run(stdin io.Reader) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := bufio.NewReaderSize(stdin, 64<<10)
	for {
		line, tooLong, err := readMCPLine(reader)
		if tooLong {
			p.write(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: rpcInvalidRequest, Message: "The MCP message is larger than 64 KiB."}})
		} else if len(bytes.TrimSpace(line)) > 0 {
			p.handle(ctx, bytes.TrimSpace(line))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(p.log, "runquill mcp: read stdin: %v\n", err)
			}
			break
		}
	}
	p.wg.Wait()
	if p.failed {
		return 1
	}
	return 0
}

// readMCPLine returns one newline-delimited message, discarding the rest of a
// line past the size limit instead of buffering it.
func readMCPLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	tooLong := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > mcpStdioMaxLine+1 {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, tooLong, err
	}
}

type mcpEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type mcpEnvelopeParams struct {
	Meta      map[string]json.RawMessage `json:"_meta"`
	Name      string                     `json:"name"`
	URI       string                     `json:"uri"`
	RequestID json.RawMessage            `json:"requestId"`
}

func (p *mcpProxy) handle(ctx context.Context, line []byte) {
	var msg mcpEnvelope
	if line[0] != '{' || json.Unmarshal(line, &msg) != nil {
		p.write(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: rpcParseError, Message: "The message is not a JSON-RPC object."}})
		return
	}
	var params mcpEnvelopeParams
	if len(msg.Params) > 0 && msg.Params[0] == '{' {
		json.Unmarshal(msg.Params, &params)
	}
	if msg.ID == nil {
		// Cancellation is stdio-only; other notifications need no reply and
		// the HTTP endpoint would only acknowledge them.
		if msg.Method == "notifications/cancelled" && params.RequestID != nil {
			p.mu.Lock()
			if cancel := p.inflight[string(params.RequestID)]; cancel != nil {
				cancel()
			}
			p.mu.Unlock()
		}
		return
	}
	header := http.Header{}
	header.Set("Mcp-Method", msg.Method)
	var version string
	if raw, ok := params.Meta[mcpMetaVersion]; ok {
		json.Unmarshal(raw, &version)
	} else {
		p.mu.Lock()
		version = p.legacy
		p.mu.Unlock()
	}
	if version != "" {
		header.Set("MCP-Protocol-Version", version)
	}
	switch msg.Method {
	case "tools/call", "prompts/get":
		header.Set("Mcp-Name", mcpEncodeHeader(params.Name))
	case "resources/read":
		header.Set("Mcp-Name", mcpEncodeHeader(params.URI))
	}
	key := string(msg.ID)
	requestCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	if p.inflight[key] != nil {
		p.mu.Unlock()
		cancel()
		p.write(rpcResponse{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{Code: rpcInvalidRequest, Message: "A request with this id is already in flight."}})
		return
	}
	p.inflight[key] = cancel
	p.mu.Unlock()
	p.slots <- struct{}{}
	p.wg.Add(1)
	forward := func() {
		defer p.wg.Done()
		defer func() { <-p.slots }()
		defer cancel()
		response := p.forward(requestCtx, line, header, msg)
		p.mu.Lock()
		delete(p.inflight, key)
		p.mu.Unlock()
		if requestCtx.Err() != nil {
			return // cancelled: no further messages for this request
		}
		p.writeRaw(response)
	}
	if msg.Method == "initialize" {
		// Later legacy requests depend on the negotiated version.
		forward()
		return
	}
	go forward()
}

// forward posts one message and returns the line to print: the panel's
// JSON-RPC answer verbatim, or a local -32603 error when there is none.
func (p *mcpProxy) forward(ctx context.Context, line []byte, header http.Header, msg mcpEnvelope) []byte {
	fail := func(message string) []byte {
		payload, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{Code: rpcInternalError, Message: message}})
		return payload
	}
	status, contentType, data, err := p.client.postMCP(ctx, line, header)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(p.log, "runquill mcp: %s: %v\n", terminalSafe(msg.Method, 64), err)
		}
		return fail("Could not reach the Runquill panel.")
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	var compact bytes.Buffer
	if mediaType == "application/json" && len(bytes.TrimSpace(data)) > 0 && bytes.TrimSpace(data)[0] == '{' &&
		json.Compact(&compact, data) == nil {
		var answer struct {
			ID    json.RawMessage `json:"id"`
			Error *rpcError       `json:"error"`
		}
		json.Unmarshal(data, &answer)
		if !bytes.Equal(answer.ID, msg.ID) {
			// An id-less error (busy, malformed id) would leave the client waiting.
			if answer.Error != nil {
				return fail(fmt.Sprintf("The Runquill panel answered HTTP %d: %s", status, terminalSafe(answer.Error.Message, 300)))
			}
			return fail(fmt.Sprintf("The Runquill panel answered HTTP %d.", status))
		}
		if msg.Method == "initialize" && status == http.StatusOK {
			var reply struct {
				Result struct {
					ProtocolVersion string `json:"protocolVersion"`
				} `json:"result"`
			}
			if json.Unmarshal(data, &reply) == nil {
				p.mu.Lock()
				p.legacy = reply.Result.ProtocolVersion
				p.mu.Unlock()
			}
		}
		return compact.Bytes()
	}
	message := apiErrorMessage(contentType, data[:min(len(data), apiMaxError)])
	if message == "" {
		return fail(fmt.Sprintf("The Runquill panel answered HTTP %d.", status))
	}
	return fail(fmt.Sprintf("The Runquill panel answered HTTP %d: %s", status, message))
}

func (p *mcpProxy) write(response rpcResponse) {
	payload, err := json.Marshal(response)
	if err == nil {
		p.writeRaw(payload)
	}
}

func (p *mcpProxy) writeRaw(line []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out.Write(line)
	p.out.WriteByte('\n')
	if err := p.out.Flush(); err != nil && !p.failed {
		p.failed = true
		fmt.Fprintf(p.log, "runquill mcp: write stdout: %v\n", err)
	}
}

// mcpEncodeHeader applies the spec's base64 sentinel to values that are not
// plain header-safe ASCII, or that look like the sentinel themselves.
func mcpEncodeHeader(value string) string {
	safe := value == strings.TrimSpace(value) &&
		!(strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?="))
	for i := 0; safe && i < len(value); i++ {
		safe = value[i] >= 0x20 && value[i] <= 0x7e
	}
	if safe {
		return value
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

// postMCP sends one JSON-RPC message to /mcp. Unlike do, it returns error
// statuses with their body, because MCP carries JSON-RPC errors in 4xx answers.
func (c *apiClient) postMCP(ctx context.Context, body []byte, header http.Header) (int, string, []byte, error) {
	target := c.base.ResolveReference(&url.URL{Path: "/mcp"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, "", nil, err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "runquill-mcp/"+version)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, apiMaxResponse+1))
	if err != nil {
		return 0, "", nil, err
	}
	if len(data) > apiMaxResponse {
		return 0, "", nil, fmt.Errorf("the panel response exceeded %d MiB", apiMaxResponse>>20)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), data, nil
}
