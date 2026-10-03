package archil

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"net/http"

	"github.com/coder/websocket"
)

const stdinChunkBytes = 1 << 20

// Stream identifies which of a process's output streams a frame came from.
type Stream string

const (
	Stdout Stream = "stdout"
	Stderr Stream = "stderr"
)

// Output is one chunk of process output, carrying the absolute offset it starts
// at so a reader can detect a gap or resume after a disconnect.
type Output struct {
	Stream Stream
	Offset uint64
	Data   []byte
}

// Result is a process's terminal state.
type Result struct {
	Status     string
	ExitCode   *int
	ExitReason string
	Stdout     string
	Stderr     string
}

func (r *Result) OK() bool { return r.ExitCode != nil && *r.ExitCode == 0 }

// Process is a live connection to one process inside a sandbox.
type Process struct {
	client    *Client
	sandboxID string

	mu      sync.Mutex
	conn    *websocket.Conn
	id      string
	cursor  uint64
	result  *Result
	stdout  strings.Builder
	stderr  strings.Builder
	collect bool
	onOut   func(Output)

	ready   chan struct{}
	done    chan struct{}
	readyOK bool
	readyE  error
	recvE   error
}

func (p *Process) ID() string { return p.id }

// Cursor is the output offset reached so far, for resuming with Attach.
func (p *Process) Cursor() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cursor
}

// connection mints a short-lived signed WebSocket URL. The signature is in the
// URL, so the socket itself carries no credential.
func (c *Client) connection(ctx context.Context, sandboxID string) (string, error) {
	var out struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.do(ctx, "POST", "/api/sandboxes/"+sandboxID+"/connections", nil, struct{}{}, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("archil: sandbox %s returned no connection URL", sandboxID)
	}
	return out.URL, nil
}

func (c *Client) dial(ctx context.Context, sandboxID string) (*websocket.Conn, error) {
	u, err := c.connection(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: &http.Client{Timeout: 0}})
	if err != nil {
		return nil, fmt.Errorf("archil: dialing sandbox %s: %w", sandboxID, err)
	}
	conn.SetReadLimit(64 << 20)
	return conn, nil
}

// RunOptions configures a process start.
type RunOptions struct {
	Cwd            string
	Env            map[string]string
	TimeoutSeconds int
	// CollectOutput accumulates stdout and stderr into the Result. Leave it off
	// for a long-lived process whose output would grow without bound.
	CollectOutput bool
	OnOutput      func(Output)
}

// Run starts a process and returns once the runtime has acknowledged it.
func (c *Client) Run(ctx context.Context, sandboxID, command string, opts RunOptions) (*Process, error) {
	// env must marshal as a map, never null: a nil typed map stored in an `any`
	// is not == nil, so copy into a non-nil map rather than testing the field.
	env := map[string]string{}
	for k, v := range opts.Env {
		env[k] = v
	}
	req := map[string]any{
		"type":     "start",
		"command":  command,
		"terminal": false,
		"env":      env,
	}
	if opts.Cwd != "" {
		req["cwd"] = opts.Cwd
	}
	if opts.TimeoutSeconds > 0 {
		req["timeout_seconds"] = opts.TimeoutSeconds
	}
	return c.startProcess(ctx, sandboxID, req, "started", 0, opts)
}

// Attach reconnects to an existing process, resuming output from offset.
func (c *Client) Attach(ctx context.Context, sandboxID, processID string, offset uint64, opts RunOptions) (*Process, error) {
	req := map[string]any{"type": "attach", "process_id": processID, "offset": offset}
	return c.startProcess(ctx, sandboxID, req, "attached", offset, opts)
}

func (c *Client) startProcess(ctx context.Context, sandboxID string, req map[string]any, expect string, cursor uint64, opts RunOptions) (*Process, error) {
	conn, err := c.dial(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	p := &Process{
		client:    c,
		sandboxID: sandboxID,
		conn:      conn,
		cursor:    cursor,
		collect:   opts.CollectOutput,
		onOut:     opts.OnOutput,
		ready:     make(chan struct{}),
		done:      make(chan struct{}),
	}
	if id, ok := req["process_id"].(string); ok {
		p.id = id
	}
	go p.receive(expect)
	if err := p.sendJSON(ctx, req); err != nil {
		conn.Close(websocket.StatusInternalError, "send failed")
		return nil, err
	}
	select {
	case <-p.ready:
		p.mu.Lock()
		rerr := p.readyE
		p.mu.Unlock()
		if rerr != nil {
			conn.Close(websocket.StatusInternalError, "not ready")
			return nil, rerr
		}
		return p, nil
	case <-ctx.Done():
		conn.Close(websocket.StatusInternalError, "cancelled")
		return nil, ctx.Err()
	}
}

func (p *Process) receive(expect string) {
	defer close(p.done)
	for {
		typ, data, err := p.conn.Read(context.Background())
		if err != nil {
			p.mu.Lock()
			p.recvE = err
			p.mu.Unlock()
			p.failReady(fmt.Errorf("archil: process connection closed before ready: %w", err))
			return
		}
		if typ == websocket.MessageText {
			if err := p.handleControl(data, expect); err != nil {
				p.mu.Lock()
				p.recvE = err
				p.mu.Unlock()
				p.failReady(err)
				return
			}
			continue
		}
		out, err := p.handleOutput(data)
		if err != nil {
			p.mu.Lock()
			p.recvE = err
			p.mu.Unlock()
			p.failReady(err)
			return
		}
		if out != nil && p.onOut != nil {
			p.onOut(*out)
		}
	}
}

func (p *Process) handleControl(data []byte, expect string) error {
	var ev struct {
		Type       string `json:"type"`
		ProcessID  string `json:"process_id"`
		Status     string `json:"status"`
		ExitCode   *int   `json:"exit_code"`
		ExitReason string `json:"exit_reason"`
		Cursor     uint64 `json:"cursor"`
		Error      string `json:"error"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("archil: bad control message: %w", err)
	}
	switch ev.Type {
	case "started", "attached":
		if ev.Type != expect {
			return fmt.Errorf("archil: expected %s, got %s", expect, ev.Type)
		}
		p.mu.Lock()
		if p.id != "" && p.id != ev.ProcessID {
			p.mu.Unlock()
			return fmt.Errorf("archil: runtime returned a different process id")
		}
		p.id = ev.ProcessID
		p.readyOK = true
		p.mu.Unlock()
		p.closeReady()
	case "exit":
		p.mu.Lock()
		if p.cursor < ev.Cursor {
			p.cursor = ev.Cursor
		}
		p.result = &Result{
			Status: ev.Status, ExitCode: ev.ExitCode, ExitReason: ev.ExitReason,
			Stdout: p.stdout.String(), Stderr: p.stderr.String(),
		}
		// A process that exits immediately reports `exit` and the socket then
		// closes. That is a successful start, so the close must not be read as a
		// failure to become ready.
		p.readyOK = true
		p.mu.Unlock()
		p.closeReady()
	case "error":
		return fmt.Errorf("archil: %s: %s", ev.Error, ev.Message)
	}
	return nil
}

// handleOutput decodes one output frame: a stream tag byte, an 8-byte big-endian
// absolute offset, then payload. Frames may overlap after a reattach, so anything
// already behind the cursor is dropped.
func (p *Process) handleOutput(frame []byte) (*Output, error) {
	if len(frame) < 9 || (frame[0] != 1 && frame[0] != 2) {
		return nil, fmt.Errorf("archil: invalid process output frame")
	}
	offset := binary.BigEndian.Uint64(frame[1:9])
	payload := frame[9:]
	end := offset + uint64(len(payload))
	p.mu.Lock()
	defer p.mu.Unlock()
	if end <= p.cursor {
		return nil, nil
	}
	if offset > p.cursor {
		p.cursor = offset
	}
	skip := uint64(0)
	if p.cursor > offset {
		skip = p.cursor - offset
	}
	unread := payload[skip:]
	unreadOffset := offset + skip
	p.cursor = end
	stream := Stdout
	if frame[0] == 2 {
		stream = Stderr
	}
	if p.collect {
		if stream == Stdout {
			p.stdout.Write(unread)
		} else {
			p.stderr.Write(unread)
		}
	}
	return &Output{Stream: stream, Offset: unreadOffset, Data: unread}, nil
}

func (p *Process) closeReady() {
	select {
	case <-p.ready:
	default:
		close(p.ready)
	}
}

func (p *Process) failReady(err error) {
	p.mu.Lock()
	if p.readyE == nil && !p.readyOK {
		p.readyE = err
	}
	p.mu.Unlock()
	p.closeReady()
}

func (p *Process) sendJSON(ctx context.Context, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return p.conn.Write(ctx, websocket.MessageText, raw)
}

// SendInput writes to the process's stdin, chunked to stay under the frame limit.
func (p *Process) SendInput(ctx context.Context, data []byte) error {
	for off := 0; off < len(data); off += stdinChunkBytes {
		end := min(off+stdinChunkBytes, len(data))
		if err := p.conn.Write(ctx, websocket.MessageBinary, data[off:end]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Process) CloseStdin(ctx context.Context) error {
	return p.sendJSON(ctx, map[string]any{"type": "close_stdin"})
}

// Wait blocks until the process exits or its connection drops.
func (p *Process) Wait(ctx context.Context) (*Result, error) {
	select {
	case <-p.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.result != nil {
		return p.result, nil
	}
	if p.recvE != nil {
		return nil, fmt.Errorf("archil: process connection failed: %w", p.recvE)
	}
	return nil, fmt.Errorf("archil: process connection closed before exit")
}

func (p *Process) Disconnect() {
	p.conn.Close(websocket.StatusNormalClosure, "")
}

// Kill terminates the process over a separate control connection, since the
// process's own socket may be streaming.
func (p *Process) Kill(ctx context.Context) error {
	return p.client.control(ctx, p.sandboxID, map[string]any{"type": "kill", "process_id": p.id})
}

func (c *Client) control(ctx context.Context, sandboxID string, req map[string]any) error {
	conn, err := c.dial(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageBinary, raw)
}

// Exec runs a command to completion and returns its result.
func (c *Client) Exec(ctx context.Context, sandboxID, command string) (*Result, error) {
	p, err := c.Run(ctx, sandboxID, command, RunOptions{CollectOutput: true})
	if err != nil {
		return nil, err
	}
	defer p.Disconnect()
	return p.Wait(ctx)
}

// ExecOK runs a command and fails unless it exits zero, folding stderr into the error.
func (c *Client) ExecOK(ctx context.Context, sandboxID, command string) (string, error) {
	res, err := c.Exec(ctx, sandboxID, command)
	if err != nil {
		return "", err
	}
	if !res.OK() {
		code := -1
		if res.ExitCode != nil {
			code = *res.ExitCode
		}
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		return res.Stdout, fmt.Errorf("archil: command failed (exit %d): %s", code, detail)
	}
	return res.Stdout, nil
}

// uploadScript writes stdin to a temporary file and moves it into place, so a
// partial transfer never leaves a half-written file at the target path.
const uploadScript = `set -eu
mkdir -p "$ARCHIL_FILE_PARENT"
trap 'rm -f "$ARCHIL_FILE_TEMP"' EXIT HUP INT TERM
: > "$ARCHIL_FILE_TEMP"
cat > "$ARCHIL_FILE_TEMP"
chmod "$ARCHIL_FILE_MODE" "$ARCHIL_FILE_TEMP"
mv -f "$ARCHIL_FILE_TEMP" "$ARCHIL_FILE_TARGET"
trap - EXIT HUP INT TERM`

// UploadFile streams content to a path inside the sandbox with the given mode.
func (c *Client) UploadFile(ctx context.Context, sandboxID, target string, mode string, content io.Reader) error {
	parent := target[:strings.LastIndex(target, "/")+1]
	if parent == "" {
		parent = "/"
	}
	env := map[string]string{
		"ARCHIL_FILE_TARGET": target,
		"ARCHIL_FILE_PARENT": strings.TrimSuffix(parent, "/"),
		"ARCHIL_FILE_TEMP":   target + ".sc-upload",
		"ARCHIL_FILE_MODE":   mode,
	}
	if env["ARCHIL_FILE_PARENT"] == "" {
		env["ARCHIL_FILE_PARENT"] = "/"
	}
	p, err := c.Run(ctx, sandboxID, uploadScript, RunOptions{Env: env, CollectOutput: true})
	if err != nil {
		return err
	}
	defer p.Disconnect()
	buf := make([]byte, stdinChunkBytes)
	for {
		n, rerr := content.Read(buf)
		if n > 0 {
			if err := p.SendInput(ctx, buf[:n]); err != nil {
				return fmt.Errorf("archil: uploading %s: %w", target, err)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("archil: reading upload source: %w", rerr)
		}
	}
	if err := p.CloseStdin(ctx); err != nil {
		return err
	}
	res, err := p.Wait(ctx)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("archil: uploading %s failed: %s", target, strings.TrimSpace(res.Stderr))
	}
	return nil
}
