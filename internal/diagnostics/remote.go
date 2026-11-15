package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxScript = 64 * 1024
const maxCachedScripts = 128

var errScriptUnavailable = errors.New("diagnostic script unavailable")

// Retry downloads before execution only. The reservation still prevents a
// process restart or duplicate ack from executing the script a second time.
func (c *Client) downloadScript(parent context.Context, task Task) ([]byte, error) {
	deadline := time.Now().Add(10 * time.Second)
	if task.ExpiresAt.Before(deadline) {
		deadline = task.ExpiresAt
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	var data []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		data, err = c.loadScript(ctx, task)
		if err == nil || !errors.Is(err, errScriptUnavailable) || ctx.Err() != nil {
			return data, err
		}
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, errScriptUnavailable
			case <-timer.C:
			}
		}
	}
	return data, err
}

func validIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func validDigest(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func matchesScript(data []byte, digest string) bool {
	hash := sha256.Sum256(data)
	return len(data) > 0 && len(data) <= maxScript && hex.EncodeToString(hash[:]) == digest
}
func readScript(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxScript+1))
}
func (c *Client) collectTask(ctx context.Context, task Task) Result {
	if task.ScriptVersion == "" && task.ScriptSHA256 == "" {
		return collect(ctx, task)
	}
	script, err := c.downloadScript(ctx, task)
	if err != nil {
		// A fallback must be the exact pinned content, never a different local version.
		if body, ok := scripts[task.Recipe]; ok && errors.Is(err, errScriptUnavailable) && matchesScript([]byte(scriptPreamble+body), task.ScriptSHA256) {
			script = []byte(scriptPreamble + body)
		} else {
			return Result{ID: task.ID, CollectedAt: time.Now().UTC(), ExitCode: -1, Output: "Diagnostic script could not be obtained or verified. No script was executed."}
		}
	}
	if !time.Now().Before(task.ExpiresAt) {
		return Result{ID: task.ID, CollectedAt: time.Now().UTC(), ExitCode: -1, Output: "Diagnostic task expired before execution. No script was executed."}
	}
	return collectScript(ctx, task, string(script))
}
func (c *Client) loadScript(parent context.Context, task Task) ([]byte, error) {
	if !validIdentifier(task.Recipe) || !validIdentifier(task.ScriptVersion) || !validDigest(task.ScriptSHA256) {
		return nil, errors.New("invalid script identity")
	}
	path := filepath.Join(c.dir, "scripts", task.ScriptSHA256+".sh")
	if data, err := readScript(path); err == nil && matchesScript(data, task.ScriptSHA256) {
		return data, nil
	}
	endpoint, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, err
	}
	endpoint.Path = "/api/agent/diagnostics/scripts/" + task.ID
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.key)
	req.Header.Set("X-PM-Workload", c.workload)
	req.Header.Set("X-PM-Agent-Id", c.agentID)
	client := &http.Client{Transport: c.base, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, errScriptUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusNotFound || response.StatusCode >= 500 {
			return nil, errScriptUnavailable
		}
		return nil, errors.New("script download refused")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxScript+1))
	if err != nil {
		return nil, errScriptUnavailable
	}
	if !matchesScript(data, task.ScriptSHA256) {
		return nil, errors.New("script checksum mismatch")
	}
	c.cacheScript(path, data)
	return data, nil
}
func (c *Client) cacheScript(path string, data []byte) {
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".download-")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || os.Rename(f.Name(), path) != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type cached struct {
		name     string
		modified time.Time
	}
	var files []cached
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sh") {
			if info, err := e.Info(); err == nil {
				files = append(files, cached{e.Name(), info.ModTime()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
	remaining := len(files)
	for _, old := range files {
		if remaining <= maxCachedScripts {
			break
		}
		if old.name == filepath.Base(path) {
			continue
		}
		if os.Remove(filepath.Join(dir, old.name)) == nil {
			remaining--
		}
	}
}
