package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCapWriterKeepsHeadAndTail(t *testing.T) {
	w := newCapWriter(4096)
	head := strings.Repeat("a", 200000)
	tail := strings.Repeat("z", 200000)
	w.Write([]byte(head))
	w.Write([]byte(tail))

	got := string(w.bytes())
	if len(got) > 4096+128 {
		t.Fatalf("kept %d bytes, want <= ~4096", len(got))
	}
	if !strings.HasPrefix(got, "aaa") {
		t.Fatalf("head not kept: %q", got[:10])
	}
	if !strings.HasSuffix(got, "zzz") {
		t.Fatalf("tail not kept: %q", got[len(got)-10:])
	}
	if !strings.Contains(got, "bytes elided") {
		t.Fatalf("no elision marker in %q", got)
	}
	if !w.truncated() {
		t.Fatal("truncated() = false, want true")
	}
	_, total := w.totals()
	if total != 400000 {
		t.Fatalf("total = %d, want 400000", total)
	}
}

func TestCapWriterSmallOutputUntouched(t *testing.T) {
	w := newCapWriter(4096)
	w.Write([]byte("hello\n"))
	if got := string(w.bytes()); got != "hello\n" {
		t.Fatalf("got %q", got)
	}
	if w.truncated() {
		t.Fatal("truncated() = true for small output")
	}
}

func TestParseHostSSHConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "Host db\n  HostName 10.0.0.5\n  User postgres\n  Port 2222\n  IdentityFile ~/.ssh/id_db\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	user, addr, id := parseHost("db")
	if user != "postgres" {
		t.Errorf("user = %q, want postgres", user)
	}
	if addr != "10.0.0.5:2222" {
		t.Errorf("addr = %q, want 10.0.0.5:2222", addr)
	}
	if id != filepath.Join(home, ".ssh", "id_db") {
		t.Errorf("identity = %q", id)
	}

	// An explicit port on the command line wins over the config.
	_, addr, _ = parseHost("bob@db:2200")
	if addr != "10.0.0.5:2200" {
		t.Errorf("addr = %q, want 10.0.0.5:2200", addr)
	}

	// An explicit user on the command line wins over User in the config.
	// Regression: the config used to overwrite it unconditionally, so
	// "root@db" silently ran as postgres.
	if user, _, _ = parseHost("bob@db"); user != "bob" {
		t.Errorf("explicit user = %q, want bob", user)
	}
	if user, _, _ = parseHost("bob@db:2200"); user != "bob" {
		t.Errorf("explicit user with port = %q, want bob", user)
	}
	// Without an explicit user the config still applies.
	if user, _, _ = parseHost("db"); user != "postgres" {
		t.Errorf("config user = %q, want postgres", user)
	}
}

// TestServeStreamFlushesOnEOF guards the bug where closing stdin made the
// server exit before goroutine-served requests could write their response:
// `ssh-mcp < requests.jsonl` produced zero output.
func TestServeStreamFlushesOnEOF(t *testing.T) {
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	input := strings.NewReader(init + "\n" + list + "\n")

	old := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = pw
	serveStream(input)
	pw.Close()
	os.Stdout = old

	out, err := io.ReadAll(pr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"id":2`) {
		t.Fatalf("response for id=2 missing after EOF; got %d bytes: %.200s", len(out), out)
	}
	if strings.Count(string(out), `"jsonrpc"`) != 2 {
		t.Fatalf("want 2 responses, got: %s", out)
	}
}

func TestIntArgClamp(t *testing.T) {
	p := &CallToolParams{Arguments: map[string]any{"n": float64(999999)}}
	if got := intArg(p, "n", 10, 100); got != 100 {
		t.Errorf("clamp high = %d, want 100", got)
	}
	if got := intArg(p, "missing", 10, 100); got != 10 {
		t.Errorf("default = %d, want 10", got)
	}
	if got := durArg(p, "n", time.Second, 30*time.Second); got != 30*time.Second {
		t.Errorf("durArg = %s, want 30s", got)
	}
}

func TestShq(t *testing.T) {
	if got := shq("/tmp/a b"); got != "'/tmp/a b'" {
		t.Errorf("got %q", got)
	}
	if got := shq("it's"); got != `'it'\''s'` {
		t.Errorf("got %q", got)
	}
}

func TestNotificationsAreNotAnswered(t *testing.T) {
	r := &Request{JSONRPC: "2.0", Method: "notifications/initialized"}
	if resp := handleRequest(r); resp != nil {
		t.Fatalf("notification answered: %+v", resp)
	}
	r = &Request{JSONRPC: "2.0", Method: "something/unknown"}
	if resp := handleRequest(r); resp != nil {
		t.Fatalf("unknown notification answered: %+v", resp)
	}
}
