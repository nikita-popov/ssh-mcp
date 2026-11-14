package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kevinburke/ssh_config"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	version     = "1.1.1"
	dialTimeout = 15 * time.Second
	// Kept below lazy-mcp's exec_timeout (60s) so the caller sees a clear
	// "command timed out after 45s" instead of an opaque proxy-side
	// "context deadline exceeded" with the command still running.
	defaultTimeout   = 45 * time.Second
	maxTimeout       = 30 * time.Minute
	defaultMaxOutput = 256 * 1024
	maxMaxOutput     = 4 * 1024 * 1024
)

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   *Error `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
	Instructions    string       `json:"instructions,omitempty"`
}

type Capabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

type ToolsCapability struct{}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required"`
}

type Property struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Default     any    `json:"default,omitempty"`
}

type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

var (
	stdoutMu  sync.Mutex
	debugOn   = os.Getenv("SSH_MCP_DEBUG") != ""
	agentOnce sync.Once
	agentAuth ssh.AuthMethod
)

func debugf(format string, a ...any) {
	if debugOn {
		fmt.Fprintf(os.Stderr, "ssh-mcp: "+format+"\n", a...)
	}
}

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ssh-mcp: WARNING: "+format+"\n", a...)
}

func main() {
	serveStream(os.Stdin)
}

// serveStream reads JSON-RPC lines from in until EOF, serving each in its own
// goroutine, and does not return until every in-flight request has answered.
func serveStream(stdin io.Reader) {
	// Unbounded line reader: a JSON-RPC message larger than the old 1 MiB
	// scanner buffer used to make Scan() return false and kill the server silently.
	in := bufio.NewReaderSize(stdin, 64*1024)

	// Requests run in goroutines, so main must wait for them on EOF: returning
	// early killed in-flight work and silently dropped its response (a client
	// that closes stdin, e.g. `ssh-mcp < requests.jsonl`, got zero output).
	var inflight sync.WaitGroup
	defer inflight.Wait()

	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			inflight.Add(1)
			go func(l []byte) {
				defer inflight.Done()
				serveLine(l)
			}(line)
		}
		if err != nil {
			if err != io.EOF {
				debugf("stdin read error: %v", err)
			}
			debugf("stdin closed, draining in-flight requests")
			return
		}
	}
}

func serveLine(line []byte) {
	var req Request
	if err := json.Unmarshal(bytes.TrimSpace(line), &req); err != nil {
		debugf("bad json-rpc line (%d bytes): %v", len(line), err)
		return
	}

	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "ssh-mcp: panic handling %s: %v\n", req.Method, r)
			if req.ID != nil {
				writeResponse(errorResponse(req.ID, -32603, fmt.Sprintf("internal error: %v", r)))
			}
		}
	}()

	if resp := handleRequest(&req); resp != nil {
		writeResponse(resp)
	}
}

func writeResponse(resp *Response) {
	out, err := json.Marshal(resp)
	if err != nil {
		debugf("marshal response: %v", err)
		return
	}
	stdoutMu.Lock()
	defer stdoutMu.Unlock()
	os.Stdout.Write(append(out, '\n'))
}

func handleRequest(req *Request) *Response {
	// Notifications carry no id and must never be answered.
	isNotification := req.ID == nil

	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		pv := p.ProtocolVersion
		if pv == "" {
			pv = "2024-11-05"
		}
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: InitializeResult{
				ProtocolVersion: pv,
				Capabilities:    Capabilities{Tools: &ToolsCapability{}},
				ServerInfo:      ServerInfo{Name: "ssh-mcp", Version: version},
				Instructions:    "Runs one shell command per ssh_exec call. A non-zero remote exit code is reported as exit_code in the text (not as a tool error); tool errors mean the SSH transport itself failed. Output is capped at max_output_bytes (default 256 KiB, head+tail kept) - narrow with grep/head instead of dumping.",
			},
		}

	case "ping":
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: struct{}{}}

	case "resources/list":
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"resources": []any{}}}

	case "prompts/list":
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"prompts": []any{}}}

	case "notifications/initialized", "notifications/cancelled", "logging/setLevel":
		return nil

	case "tools/list":
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  ToolsListResult{Tools: toolCatalog()},
		}

	case "tools/call":
		var params CallToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorResponse(req.ID, -32602, "Invalid params")
		}
		return handleToolCall(req.ID, &params)

	default:
		if isNotification {
			return nil
		}
		return errorResponse(req.ID, -32601, "Method not found")
	}
}

func toolCatalog() []Tool {
	hostProp := Property{Type: "string", Description: "SSH target: user@host, user@host:port, or an alias from ~/.ssh/config"}
	passProp := Property{Type: "string", Description: "Password (optional). Prefer keys or SSH_AUTH_SOCK - never send a password unless the host really needs one"}
	timeoutProp := Property{Type: "number", Description: "Hard wall-clock limit for the whole call, seconds. The session is killed on expiry.", Default: int(defaultTimeout / time.Second)}
	maxOutProp := Property{Type: "number", Description: "Max bytes of combined stdout+stderr kept (head+tail).", Default: defaultMaxOutput}

	return []Tool{
		{
			Name:        "ssh_exec",
			Description: "Execute a command on a remote host via SSH. Returns exit_code, duration, stdout and stderr. Non-zero exit is NOT a tool error.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"host":             hostProp,
					"command":          {Type: "string", Description: "Shell command to run (passed to the remote shell; quote paths)"},
					"stdin":            {Type: "string", Description: "Optional text piped to the command's stdin"},
					"timeout_seconds":  timeoutProp,
					"max_output_bytes": maxOutProp,
					"password":         passProp,
				},
				Required: []string{"host", "command"},
			},
		},
		{
			Name:        "ssh_sftp",
			Description: "Copy a file via SFTP (needs the SFTP subsystem). Prefix the remote side with 'remote:'.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"host":     hostProp,
					"source":   {Type: "string", Description: "Source path, prefix 'remote:' for the remote side"},
					"dest":     {Type: "string", Description: "Destination path, prefix 'remote:' for the remote side"},
					"password": passProp,
				},
				Required: []string{"host", "source", "dest"},
			},
		},
		{
			Name:        "ssh_scp",
			Description: "Copy a file via the SCP protocol (works when SFTP is disabled). Prefix the remote side with 'remote:'.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"host":     hostProp,
					"source":   {Type: "string", Description: "Source path, prefix 'remote:' for the remote side"},
					"dest":     {Type: "string", Description: "Destination path, prefix 'remote:' for the remote side"},
					"password": passProp,
				},
				Required: []string{"host", "source", "dest"},
			},
		},
	}
}

func handleToolCall(id any, params *CallToolParams) *Response {
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	switch params.Name {
	case "ssh_exec":
		return handleExec(id, params)
	case "ssh_sftp":
		return handleSFTP(id, params)
	case "ssh_scp":
		return handleSCP(id, params)
	default:
		return toolError(id, "Unknown tool: "+params.Name)
	}
}

func handleExec(id any, params *CallToolParams) *Response {
	host := strArg(params, "host")
	command := strArg(params, "command")
	if host == "" || command == "" {
		return toolError(id, "Missing host or command")
	}

	timeout := durArg(params, "timeout_seconds", defaultTimeout, maxTimeout)
	maxOut := intArg(params, "max_output_bytes", defaultMaxOutput, maxMaxOutput)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res, err := executeSSH(ctx, host, command, strArg(params, "stdin"), strArg(params, "password"), maxOut, timeout)
	if err != nil {
		if res != nil && (res.Stdout != "" || res.Stderr != "") {
			return toolError(id, err.Error()+"\n--- partial output ---\n"+renderStreams(res))
		}
		return toolError(id, err.Error())
	}
	return toolSuccess(id, renderExec(res))
}

func handleSFTP(id any, params *CallToolParams) *Response {
	host := strArg(params, "host")
	source := strArg(params, "source")
	dest := strArg(params, "dest")
	if host == "" || source == "" || dest == "" {
		return toolError(id, "Missing host, source, or dest")
	}
	result, err := executeSFTP(host, source, dest, strArg(params, "password"))
	if err != nil {
		return toolError(id, err.Error())
	}
	return toolSuccess(id, result)
}

func handleSCP(id any, params *CallToolParams) *Response {
	host := strArg(params, "host")
	source := strArg(params, "source")
	dest := strArg(params, "dest")
	if host == "" || source == "" || dest == "" {
		return toolError(id, "Missing host, source, or dest")
	}
	result, err := executeSCP(host, source, dest, strArg(params, "password"))
	if err != nil {
		return toolError(id, err.Error())
	}
	return toolSuccess(id, result)
}

func strArg(p *CallToolParams, key string) string {
	v, ok := p.Arguments[key].(string)
	if !ok {
		return ""
	}
	return v
}

func intArg(p *CallToolParams, key string, def, max int) int {
	v, ok := p.Arguments[key]
	if !ok {
		return def
	}
	var n float64
	switch t := v.(type) {
	case float64:
		n = t
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return def
		}
		n = f
	default:
		return def
	}
	if n <= 0 {
		return def
	}
	if n > float64(max) {
		return max
	}
	return int(n)
}

func durArg(p *CallToolParams, key string, def, max time.Duration) time.Duration {
	secs := intArg(p, key, int(def/time.Second), int(max/time.Second))
	return time.Duration(secs) * time.Second
}

func toolError(id any, msg string) *Response {
	return &Response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  CallToolResult{Content: []Content{{Type: "text", Text: msg}}, IsError: true},
	}
}

func toolSuccess(id any, msg string) *Response {
	return &Response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  CallToolResult{Content: []Content{{Type: "text", Text: msg}}},
	}
}

func errorResponse(id any, code int, message string) *Response {
	return &Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &Error{Code: code, Message: message},
	}
}

// ---- output capping -------------------------------------------------------

// capWriter keeps the first half and the last half of a stream and counts the
// rest. Bounded memory no matter what the command prints.
type capWriter struct {
	mu        sync.Mutex
	head      []byte
	tail      []byte
	headLimit int
	tailLimit int
	total     int64
}

func newCapWriter(limit int) *capWriter {
	if limit < 1024 {
		limit = 1024
	}
	return &capWriter{headLimit: limit / 2, tailLimit: limit - limit/2}
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.total += int64(n)
	if room := w.headLimit - len(w.head); room > 0 {
		k := room
		if k > len(p) {
			k = len(p)
		}
		w.head = append(w.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		w.tail = append(w.tail, p...)
		if len(w.tail) > w.tailLimit {
			w.tail = w.tail[len(w.tail)-w.tailLimit:]
		}
	}
	return n, nil
}

func (w *capWriter) truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total > int64(w.headLimit+w.tailLimit)
}

func (w *capWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, 0, len(w.head)+len(w.tail)+64)
	out = append(out, w.head...)
	if w.total > int64(w.headLimit+len(w.tail)) {
		out = append(out, fmt.Sprintf("\n[... %d bytes elided ...]\n", w.total-int64(w.headLimit+len(w.tail)))...)
	}
	out = append(out, w.tail...)
	return out
}

func (w *capWriter) totals() (kept, total int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(w.headLimit + len(w.tail)), w.total
}

// ---- exec ----------------------------------------------------------------

type execResult struct {
	ExitCode   int
	DurationMS int64
	TimedOut   bool
	Timeout    time.Duration
	Stdout     string
	Stderr     string
	KeptBytes  int64
	TotalBytes int64
}

func renderExec(res *execResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\nduration_ms: %d\n", res.ExitCode, res.DurationMS)
	if res.TimedOut {
		fmt.Fprintf(&b, "timed_out: true (limit %s, remote command may still be running)\n", res.Timeout)
	}
	if res.KeptBytes < res.TotalBytes {
		fmt.Fprintf(&b, "output: %d of %d bytes kept (head+tail)\n", res.KeptBytes, res.TotalBytes)
	}
	b.WriteString(renderStreams(res))
	return b.String()
}

func renderStreams(res *execResult) string {
	var b strings.Builder
	b.WriteString("--- stdout ---\n")
	b.WriteString(res.Stdout)
	if res.Stdout != "" && !strings.HasSuffix(res.Stdout, "\n") {
		b.WriteString("\n")
	}
	if res.Stderr != "" {
		b.WriteString("--- stderr ---\n")
		b.WriteString(res.Stderr)
		if !strings.HasSuffix(res.Stderr, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func executeSSH(ctx context.Context, host, command, stdin, password string, maxOut int, timeout time.Duration) (*execResult, error) {
	client, err := connectSSH(host, password)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	outW := newCapWriter(maxOut)
	errW := newCapWriter(maxOut)
	session.Stdout = outW
	session.Stderr = errW
	if stdin != "" {
		session.Stdin = strings.NewReader(stdin)
	}

	start := time.Now()
	if err := session.Start(command); err != nil {
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		timedOut = true
		// Closing the channel alone does NOT stop the remote command: it just
		// detaches it, leaving an orphan (verified: `sleep 60` survived). Send a
		// real signal, escalating to SIGKILL, before tearing the session down.
		_ = session.Signal(ssh.SIGTERM)
		select {
		case waitErr = <-done:
		case <-time.After(2 * time.Second):
			_ = session.Signal(ssh.SIGKILL)
			select {
			case waitErr = <-done:
			case <-time.After(2 * time.Second):
			}
		}
		_ = client.Close()
	}

	res := &execResult{
		DurationMS: time.Since(start).Milliseconds(),
		TimedOut:   timedOut,
		Timeout:    timeout,
		Stdout:     string(outW.bytes()),
		Stderr:     string(errW.bytes()),
	}
	res.KeptBytes = int64(len(res.Stdout) + len(res.Stderr))
	_, t1 := outW.totals()
	_, t2 := errW.totals()
	res.TotalBytes = t1 + t2

	if timedOut {
		res.ExitCode = -1
		return res, fmt.Errorf("command timed out after %s - remote command signalled (SIGTERM, then SIGKILL after 2s)", timeout)
	}

	if waitErr != nil {
		var exitErr *ssh.ExitError
		if errors.As(waitErr, &exitErr) {
			// Remote command ran and failed: that is a result, not a tool error.
			res.ExitCode = exitErr.ExitStatus()
			return res, nil
		}
		return res, fmt.Errorf("ssh session failed: %w", waitErr)
	}
	return res, nil
}

// ---- sftp / scp -----------------------------------------------------------

func splitRemoteArg(source, dest string) (srcRemote, dstRemote bool, s, d string, err error) {
	srcRemote = strings.HasPrefix(source, "remote:")
	dstRemote = strings.HasPrefix(dest, "remote:")
	s = strings.TrimPrefix(source, "remote:")
	d = strings.TrimPrefix(dest, "remote:")
	if srcRemote && dstRemote {
		return false, false, "", "", fmt.Errorf("both source and dest cannot be remote")
	}
	if !srcRemote && !dstRemote {
		return false, false, "", "", fmt.Errorf("either source or dest must be remote")
	}
	return srcRemote, dstRemote, s, d, nil
}

func executeSFTP(host, source, dest, password string) (string, error) {
	srcRemote, _, source, dest, err := splitRemoteArg(source, dest)
	if err != nil {
		return "", err
	}

	client, err := connectSSH(host, password)
	if err != nil {
		return "", err
	}
	defer client.Close()

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return "", fmt.Errorf("failed to create SFTP client (host may have the subsystem disabled - try ssh_scp): %w", err)
	}
	defer sftpClient.Close()

	if srcRemote {
		return downloadFile(sftpClient, source, dest)
	}
	return uploadFile(sftpClient, source, dest)
}

func uploadFile(sftpClient *sftp.Client, local, remote string) (string, error) {
	localAbs := absLocal(local)
	src, err := os.Open(localAbs)
	if err != nil {
		return "", fmt.Errorf("failed to open local file: %w", err)
	}
	defer src.Close()

	// Create() fails on a missing parent dir; make the remote side explicit.
	if dir := filepath.ToSlash(filepath.Dir(remote)); dir != "." && dir != "/" {
		_ = sftpClient.MkdirAll(dir)
	}

	dst, err := sftpClient.Create(remote)
	if err != nil {
		return "", fmt.Errorf("failed to create remote file: %w", err)
	}
	defer dst.Close()

	n, err := io.Copy(dst, src)
	if err != nil {
		return "", fmt.Errorf("failed to copy: %w", err)
	}
	return fmt.Sprintf("Uploaded %d bytes: %s -> %s", n, localAbs, remote), nil
}

func downloadFile(sftpClient *sftp.Client, remote, local string) (string, error) {
	localAbs := absLocal(local)
	src, err := sftpClient.Open(remote)
	if err != nil {
		return "", fmt.Errorf("failed to open remote file: %w", err)
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(localAbs), 0o755); err != nil {
		return "", fmt.Errorf("failed to create local dir: %w", err)
	}
	dst, err := os.Create(localAbs)
	if err != nil {
		return "", fmt.Errorf("failed to create local file: %w", err)
	}
	defer dst.Close()

	n, err := io.Copy(dst, src)
	if err != nil {
		return "", fmt.Errorf("failed to copy: %w", err)
	}
	return fmt.Sprintf("Downloaded %d bytes: %s -> %s", n, remote, localAbs), nil
}

func executeSCP(host, source, dest, password string) (string, error) {
	srcRemote, _, source, dest, err := splitRemoteArg(source, dest)
	if err != nil {
		return "", err
	}

	client, err := connectSSH(host, password)
	if err != nil {
		return "", err
	}
	defer client.Close()

	if srcRemote {
		return scpDownload(client, source, dest)
	}
	return scpUpload(client, source, dest)
}

func scpUpload(client *ssh.Client, local, remote string) (string, error) {
	localAbs := absLocal(local)
	f, err := os.Open(localAbs)
	if err != nil {
		return "", fmt.Errorf("failed to open local file: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("failed to get stdin: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("failed to get stdout: %w", err)
	}

	// Remote path is parsed by the remote shell: it must be quoted.
	if err := session.Start("scp -t " + shq(remote)); err != nil {
		return "", fmt.Errorf("failed to start scp: %w", err)
	}

	buf := make([]byte, 1)
	if _, err := io.ReadFull(stdout, buf); err != nil {
		return "", fmt.Errorf("scp handshake failed: %w", err)
	}

	fmt.Fprintf(stdin, "C0644 %d %s\n", stat.Size(), filepath.Base(localAbs))
	if _, err := io.ReadFull(stdout, buf); err != nil {
		return "", fmt.Errorf("scp header rejected: %w", err)
	}

	n, err := io.Copy(stdin, f)
	if err != nil {
		return "", fmt.Errorf("failed to copy: %w", err)
	}

	fmt.Fprint(stdin, "\x00")
	if _, err := io.ReadFull(stdout, buf); err != nil {
		return "", fmt.Errorf("scp transfer not acknowledged: %w", err)
	}
	stdin.Close()
	if err := session.Wait(); err != nil {
		return "", fmt.Errorf("scp failed: %w", err)
	}

	return fmt.Sprintf("Uploaded %d bytes: %s -> %s", n, localAbs, remote), nil
}

func scpDownload(client *ssh.Client, remote, local string) (string, error) {
	localAbs := absLocal(local)

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("failed to get stdin: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("failed to get stdout: %w", err)
	}

	if err := session.Start("scp -f " + shq(remote)); err != nil {
		return "", fmt.Errorf("failed to start scp: %w", err)
	}

	fmt.Fprint(stdin, "\x00")

	reader := bufio.NewReader(stdout)
	header, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read header: %w", err)
	}
	if strings.HasPrefix(header, "\x01") || strings.HasPrefix(header, "\x02") {
		return "", fmt.Errorf("remote scp error: %s", strings.TrimSpace(header[1:]))
	}

	// C0644 <size> <name>  -- name may contain spaces, so SplitN(3).
	fields := strings.SplitN(strings.TrimSuffix(header, "\n"), " ", 3)
	if len(fields) < 3 || !strings.HasPrefix(fields[0], "C") {
		return "", fmt.Errorf("unexpected scp header: %q", header)
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("bad size in scp header: %q", header)
	}

	fmt.Fprint(stdin, "\x00")

	if err := os.MkdirAll(filepath.Dir(localAbs), 0o755); err != nil {
		return "", fmt.Errorf("failed to create local dir: %w", err)
	}
	f, err := os.Create(localAbs)
	if err != nil {
		return "", fmt.Errorf("failed to create local file: %w", err)
	}
	defer f.Close()

	n, err := io.CopyN(f, reader, size)
	if err != nil {
		return "", fmt.Errorf("failed to copy: %w", err)
	}

	if _, err := reader.ReadByte(); err != nil {
		return "", fmt.Errorf("missing scp trailer: %w", err)
	}
	fmt.Fprint(stdin, "\x00")
	stdin.Close()
	if err := session.Wait(); err != nil {
		return "", fmt.Errorf("scp failed: %w", err)
	}

	return fmt.Sprintf("Downloaded %d bytes: %s -> %s", n, remote, localAbs), nil
}

// ---- host / auth ----------------------------------------------------------

func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func absLocal(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// parseHost resolves user@host[:port] plus ~/.ssh/config.
func parseHost(host string) (user, addr, identityFile string) {
	user = os.Getenv("USER")
	if user == "" {
		if u, err := osuser.Current(); err == nil {
			user = u.Username
		}
	}
	// An explicit user@ in the host argument outranks User from ssh config,
	// exactly like the ssh(1) command line.
	explicitUser := false
	rest := host
	if at := strings.LastIndex(rest, "@"); at != -1 {
		user = rest[:at]
		explicitUser = true
		rest = rest[at+1:]
	}

	port := ""
	if h, p, err := net.SplitHostPort(rest); err == nil {
		rest, port = h, p
	}

	alias := rest
	if home, err := os.UserHomeDir(); err == nil {
		if cfg := loadSSHConfig(home); cfg != nil {
			if v, _ := cfg.Get(rest, "User"); v != "" && !explicitUser {
				user = v
			}
			if hn, _ := cfg.Get(rest, "HostName"); hn != "" {
				alias = hn
			}
			if port == "" {
				if p, _ := cfg.Get(rest, "Port"); p != "" {
					port = p
				}
			}
			if ids, err := cfg.GetAll(rest, "IdentityFile"); err == nil && len(ids) > 0 {
				identityFile = expandHome(ids[0], home)
			}
		}
	}

	if port == "" {
		port = "22"
	}
	return user, net.JoinHostPort(alias, port), identityFile
}

func loadSSHConfig(home string) *ssh_config.Config {
	f, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	defer f.Close()
	cfg, err := ssh_config.Decode(f)
	if err != nil {
		debugf("cannot parse ssh config: %v", err)
		return nil
	}
	return cfg
}

func expandHome(p, home string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// hostKeyCallback is strict only when SSH_MCP_KNOWN_HOSTS is set, so existing
// deployments keep working. Point it at ~/.ssh/known_hosts to get real
// protection against a spoofed host.
func hostKeyCallback() ssh.HostKeyCallback {
	path := os.Getenv("SSH_MCP_KNOWN_HOSTS")
	if path == "" {
		return ssh.InsecureIgnoreHostKey()
	}
	path = expandHome(path, homeDir())
	cb, err := knownhosts.New(path)
	if err != nil {
		warnf("cannot use known_hosts %q (%v) - falling back to no host key verification", path, err)
		return ssh.InsecureIgnoreHostKey()
	}
	return cb
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func getAuthMethods(password, identityFile string) ([]ssh.AuthMethod, error) {
	if password != "" {
		return []ssh.AuthMethod{
			ssh.Password(password),
			ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
				}
				return answers, nil
			}),
		}, nil
	}

	var methods []ssh.AuthMethod
	if m := sshAgentAuth(); m != nil {
		methods = append(methods, m)
	}

	passphrase := os.Getenv("SSH_MCP_KEY_PASSPHRASE")
	for _, keyFile := range keyCandidates(identityFile) {
		data, err := os.ReadFile(keyFile)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			if passphrase == "" {
				debugf("%s: %v (set SSH_MCP_KEY_PASSPHRASE for encrypted keys)", keyFile, err)
				continue
			}
			signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
			if err != nil {
				debugf("%s: %v", keyFile, err)
				continue
			}
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no usable SSH auth: no password given, SSH_AUTH_SOCK has no usable agent, and no unencrypted key found in %v", keyCandidates(identityFile))
	}
	return methods, nil
}

// sshAgentAuth connects to the agent once per process instead of once per call.
func sshAgentAuth() ssh.AuthMethod {
	agentOnce.Do(func() {
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			debugf("agent dial %s: %v", sock, err)
			return
		}
		agentAuth = ssh.PublicKeysCallback(agent.NewClient(conn).Signers)
	})
	return agentAuth
}

func keyCandidates(identityFile string) []string {
	home := homeDir()
	keys := []string{}
	if identityFile != "" {
		keys = append(keys, identityFile)
	}
	return append(keys,
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".ssh", "id_rsa"),
		filepath.Join(home, ".ssh", "id_ecdsa"),
	)
}

func connectSSH(host, password string) (*ssh.Client, error) {
	user, addr, identityFile := parseHost(host)

	authMethods, err := getAuthMethods(password, identityFile)
	if err != nil {
		return nil, err
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback(),
		Timeout:         dialTimeout,
	}

	start := time.Now()
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s as %s: %w", addr, user, err)
	}
	debugf("connected %s as %s in %s", addr, user, time.Since(start).Round(time.Millisecond))
	return client, nil
}
