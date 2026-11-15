// Package diagnostics collects bounded, read-only host reports requested in ingest acknowledgements.
package diagnostics

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxOutput = 48 * 1024
const taskHeader = "X-PM-Diagnostic-Task"

type Task struct {
	ScriptVersion string    `json:"scriptVersion,omitempty"`
	ScriptSHA256  string    `json:"scriptSha256,omitempty"`
	ID            string    `json:"id"`
	Recipe        string    `json:"recipe"`
	ExpiresAt     time.Time `json:"expiresAt"`
}
type Result struct {
	ScriptVersion string    `json:"scriptVersion,omitempty"`
	ScriptSHA256  string    `json:"scriptSha256,omitempty"`
	ID            string    `json:"id"`
	Output        string    `json:"output"`
	CollectedAt   time.Time `json:"collectedAt"`
	ExitCode      int       `json:"exitCode"`
	Truncated     bool      `json:"truncated"`
}
type record struct {
	Task   Task    `json:"task"`
	Result *Result `json:"result,omitempty"`
	Sent   bool    `json:"sent"`
	dirty  bool
}
type Client struct {
	pending     map[string]record
	loaded      bool
	nextCleanup time.Time
	base        http.RoundTripper
	endpoint    string
	ingestHost  string
	key         string
	workload    string
	agentID     string
	dir         string
	queue       chan Task
	cancel      context.CancelFunc
	done        chan struct{}
	collect     func(context.Context, Task) Result
	syncDir     func(string) error
}

func New(endpoint, key, workload, dir string) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("invalid diagnostics endpoint")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1")) {
		return nil, errors.New("diagnostics requires HTTPS")
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	idPath := filepath.Join(dir, "agent-id")
	id, err := os.ReadFile(idPath)
	if errors.Is(err, os.ErrNotExist) {
		random := make([]byte, 16)
		if _, err = rand.Read(random); err != nil {
			return nil, err
		}
		id = []byte(hex.EncodeToString(random))
		if err = os.WriteFile(idPath, id, 0600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(id) != 32 {
		return nil, errors.New("invalid stored agent identity")
	}
	parsed.Path = "/api/agent/diagnostics"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	c := &Client{base: http.DefaultTransport, endpoint: parsed.String(), ingestHost: parsed.Host, key: key, workload: workload, agentID: string(id), dir: dir, queue: make(chan Task, 8), done: make(chan struct{}), collect: collect}
	c.collect = c.collectTask
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go c.run(ctx)
	return c, nil
}
func (c *Client) Close() { c.cancel(); <-c.done }
func (c *Client) RoundTrip(original *http.Request) (*http.Response, error) {
	request := original.Clone(original.Context())
	request.Header = original.Header.Clone()
	eligible := request.URL.Host == c.ingestHost && request.URL.Path == "/api/i/batch"
	if eligible {
		request.Header.Set("X-PM-Diagnostics", "v2")
		request.Header.Set("X-PM-Agent-Id", c.agentID)
	}
	response, err := c.base.RoundTrip(request)
	if err == nil && eligible && response.StatusCode >= 200 && response.StatusCode < 300 {
		c.accept(response.Header.Get(taskHeader))
	}
	return response, err
}
func (c *Client) accept(encoded string) {
	if encoded == "" || len(encoded) > 2048 {
		return
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return
	}
	var task Task
	if json.Unmarshal(data, &task) != nil || !validTask(task, time.Now()) {
		return
	}
	select {
	case c.queue <- task:
	default:
	}
}
func validTask(t Task, now time.Time) bool {
	if len(t.ID) != 64 {
		return false
	}
	if _, err := hex.DecodeString(t.ID); err != nil {
		return false
	}
	_, ok := scripts[t.Recipe]
	if t.ScriptVersion != "" || t.ScriptSHA256 != "" {
		ok = validIdentifier(t.Recipe) && validIdentifier(t.ScriptVersion) && validDigest(t.ScriptSHA256)
	}
	return ok && now.Before(t.ExpiresAt) && t.ExpiresAt.Before(now.Add(5*time.Minute))
}
func (c *Client) run(ctx context.Context) {
	defer close(c.done)
	c.replay(ctx)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-c.queue:
			c.handle(ctx, task)
		case <-ticker.C:
			c.replay(ctx)
		}
	}
}
func (c *Client) handle(ctx context.Context, task Task) {
	if !validTask(task, time.Now()) {
		return
	}
	path := filepath.Join(c.dir, task.ID+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return
	}
	if err != nil {
		log.Printf("diagnostics: cannot reserve task")
		return
	}
	// Persist reservation before executing: a restart never runs the script again.
	err = json.NewEncoder(f).Encode(record{Task: task})
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		c.remember(path, interruptedRecord(task))
		return
	}
	if c.syncStateDir(filepath.Dir(path)) != nil {
		log.Printf("diagnostics: cannot durably reserve task")
		c.remember(path, interruptedRecord(task))
		return
	}
	result := c.collect(ctx, task)
	rec := record{Task: task, Result: &result}
	if c.save(path, rec) != nil {
		log.Printf("diagnostics: cannot persist report")
		rec.dirty = true
		c.remember(path, rec)
		return
	}
	if c.send(ctx, result) == nil {
		rec.Sent = true
		if c.save(path, rec) == nil {
			return
		}
		rec.dirty = true
	}
	c.remember(path, rec)
}

// Failed reservations must remain queued even after the startup disk scan. Never
// execute them on recovery: persist and upload the interruption instead.
func interruptedRecord(task Task) record {
	return record{Task: task, Result: &Result{ID: task.ID, CollectedAt: time.Now().UTC(), ExitCode: -1, Output: "Diagnostics interrupted before a durable result was saved. The script was not run again."}, dirty: true}
}
func (c *Client) remember(path string, r record) {
	if c.pending == nil {
		c.pending = map[string]record{}
	}
	c.pending[path] = r
}
func (c *Client) syncStateDir(path string) error {
	if c.syncDir != nil {
		return c.syncDir(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func (c *Client) save(path string, r record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(path+".tmp", path); err != nil {
		return err
	}
	return c.syncStateDir(filepath.Dir(path))
}
func (c *Client) replay(ctx context.Context) {
	if c.pending == nil {
		c.pending = map[string]record{}
	}
	if !c.loaded || time.Now().After(c.nextCleanup) {
		entries, err := os.ReadDir(c.dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(c.dir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) > 30*24*time.Hour {
				_ = os.Remove(path)
				delete(c.pending, path)
				continue
			}
			if c.loaded {
				continue
			}
			if _, remembered := c.pending[path]; remembered {
				continue
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var r record
			if json.Unmarshal(raw, &r) != nil || r.Sent {
				continue
			}
			if r.Result == nil {
				r = interruptedRecord(r.Task)
			}
			c.pending[path] = r
		}
		c.loaded = true
		c.nextCleanup = time.Now().Add(time.Hour)
	}
	for path, r := range c.pending {
		if ctx.Err() != nil {
			return
		}
		// Durability failures retain the result in memory; retry persistence before
		// upload, never collection. A successfully sent result only needs its flag saved.
		if r.dirty {
			if c.save(path, r) != nil {
				continue
			}
			r.dirty = false
			c.pending[path] = r
		}
		if r.Sent {
			delete(c.pending, path)
			continue
		}
		if c.send(ctx, *r.Result) == nil {
			r.Sent = true
			if c.save(path, r) == nil {
				delete(c.pending, path)
			} else {
				r.dirty = true
				c.pending[path] = r
			}
		}
	}
}

func (c *Client) send(parent context.Context, result Result) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.key)
	req.Header.Set("X-PM-Workload", c.workload)
	req.Header.Set("X-PM-Agent-Id", c.agentID)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: c.base, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("report upload failed")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode == 204 || res.StatusCode == 409 || res.StatusCode == 401 || res.StatusCode == 403 {
		return nil
	}
	return fmt.Errorf("report upload HTTP %d", res.StatusCode)
}

type boundedOutput struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	room := maxOutput - b.buf.Len()
	if len(p) > room {
		p = p[:room]
		b.truncated = true
	}
	_, _ = b.buf.Write(p)
	return n, nil
}
func collect(parent context.Context, t Task) Result {
	return collectScript(parent, t, scriptPreamble+scripts[t.Recipe])
}
func collectScript(parent context.Context, t Task, script string) Result {
	now := time.Now().UTC()
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	// Also wrap timeout in older pinned scripts: its default separate process
	// group would otherwise survive cancellation of the outer shell.
	cmd := exec.CommandContext(ctx, "bash", "-c", timeoutPreamble+script)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=cat", "PAGER=cat")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	output := &boundedOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	// A timed-out command can leave grandchildren behind even when its shell
	// has already exited. The collection never owns lasting background work.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	code := 0
	if err != nil {
		code = -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
	}
	text := Redact(strings.ToValidUTF8(output.buf.String(), "�"))
	if strings.TrimSpace(text) == "" {
		text = "No diagnostic output was collected."
	}
	if ctx.Err() != nil {
		text += "\nDiagnostics reached its time limit."
	}
	if len(text) > maxOutput {
		text = strings.ToValidUTF8(text[:maxOutput], "")
		output.truncated = true
	}
	return Result{ID: t.ID, Output: text, CollectedAt: now, ExitCode: code, Truncated: output.truncated, ScriptVersion: t.ScriptVersion, ScriptSHA256: t.ScriptSHA256}
}
