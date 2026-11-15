package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func remoteTask(body string) Task {
	sum := sha256.Sum256([]byte(body))
	return Task{ID: strings.Repeat("a", 64), Recipe: "future_recipe", ScriptVersion: "1", ScriptSHA256: hex.EncodeToString(sum[:]), ExpiresAt: time.Now().Add(time.Minute)}
}
func TestRemoteScriptDownloadCacheAndAtMostOnceExecution(t *testing.T) {
	body := "printf 'server-only-script'"
	task := remoteTask(body)
	downloads, uploads := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-key" || r.Header.Get("X-PM-Workload") != "web" || r.Header.Get("X-PM-Agent-Id") != "test-agent" {
			t.Error("missing scope")
		}
		if r.Method == http.MethodGet {
			downloads++
			if r.URL.Path != "/api/agent/diagnostics/scripts/"+task.ID {
				t.Error("wrong path")
			}
			w.Write([]byte(body))
			return
		}
		uploads++
		w.WriteHeader(204)
	}))
	defer server.Close()
	c := &Client{base: http.DefaultTransport, endpoint: server.URL + "/api/agent/diagnostics", dir: t.TempDir(), key: "test-key", workload: "web", agentID: "test-agent"}
	c.collect = c.collectTask
	c.handle(context.Background(), task)
	c.handle(context.Background(), task)
	// A new task on a restarted agent can use the verified cache without a GET.
	restarted := *c
	task.ID = strings.Repeat("b", 64)
	restarted.handle(context.Background(), task)
	if downloads != 1 || uploads != 2 {
		t.Fatalf("downloads=%d uploads=%d", downloads, uploads)
	}
	raw, err := os.ReadFile(filepath.Join(c.dir, task.ID+".json"))
	if err != nil || !strings.Contains(string(raw), "server-only-script") || !strings.Contains(string(raw), task.ScriptSHA256) {
		t.Fatal("missing execution or identity", err)
	}
	// Tampering with the cache must cause a download, never execution of that file.
	os.WriteFile(filepath.Join(c.dir, "scripts", task.ScriptSHA256+".sh"), []byte("exit 99"), 0600)
	if _, err = c.loadScript(context.Background(), task); err != nil || downloads != 2 {
		t.Fatal("corrupt cache not replaced", err)
	}
}
func TestRemoteScriptRejectsChecksumOversizeAuthAndRedirect(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"checksum", "printf hacked", 200}, {"oversize", strings.Repeat("x", maxScript+1), 200}, {"auth", "", 401}, {"redirect", "", 307},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://127.0.0.1:1/steal")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := &Client{base: http.DefaultTransport, endpoint: server.URL, dir: t.TempDir()}
			result := c.collectTask(context.Background(), remoteTask("printf expected"))
			if result.ExitCode != -1 || !strings.Contains(result.Output, "No script was executed") {
				t.Fatal("unverified script ran")
			}
		})
	}
}
func TestFallbackRequiresExactPinnedContent(t *testing.T) {
	scripts["fallback_test"] = "printf 'bundled-fallback'"
	defer delete(scripts, "fallback_test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	c := &Client{base: http.DefaultTransport, endpoint: server.URL, dir: t.TempDir()}
	task := remoteTask(scriptPreamble + scripts["fallback_test"])
	task.Recipe = "fallback_test"
	got := c.collectTask(context.Background(), task)
	if got.ExitCode != 0 || !strings.Contains(got.Output, "bundled-fallback") {
		t.Fatal("exact fallback failed")
	}
	task.ScriptSHA256 = strings.Repeat("f", 64)
	got = c.collectTask(context.Background(), task)
	if got.ExitCode != -1 || !strings.Contains(got.Output, "No script was executed") {
		t.Fatal("wrong version executed")
	}
}
func TestRemoteTaskIdentityValidation(t *testing.T) {
	task := remoteTask("true")
	if !validTask(task, time.Now()) {
		t.Fatal("future recipe rejected")
	}
	for _, bad := range []Task{
		{ID: task.ID, Recipe: "../../script", ScriptVersion: task.ScriptVersion, ScriptSHA256: task.ScriptSHA256, ExpiresAt: task.ExpiresAt},
		{ID: task.ID, Recipe: task.Recipe, ScriptVersion: "../1", ScriptSHA256: task.ScriptSHA256, ExpiresAt: task.ExpiresAt},
		{ID: task.ID, Recipe: task.Recipe, ScriptVersion: "1", ScriptSHA256: "invalid", ExpiresAt: task.ExpiresAt},
	} {
		if validTask(bad, time.Now()) {
			t.Fatal("invalid remote identity accepted")
		}
	}
}

func TestScriptCacheRemainsBoundedAfterClockMovesBack(t *testing.T) {
	c := &Client{dir: t.TempDir()}
	dir := filepath.Join(c.dir, "scripts")
	os.MkdirAll(dir, 0700)
	for i := 0; i < maxCachedScripts; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%064d.sh", i))
		if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Hour)
		os.Chtimes(path, future, future)
	}
	current := filepath.Join(dir, "current.sh")
	c.cacheScript(current, []byte("verified"))
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != maxCachedScripts {
		t.Fatalf("cache size=%d err=%v", len(files), err)
	}
	if data, err := os.ReadFile(current); err != nil || string(data) != "verified" {
		t.Fatal("current script evicted")
	}
}
