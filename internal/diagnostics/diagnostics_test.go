package diagnostics

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T) (*Client, *atomic.Int32) {
	t.Helper()
	uploads := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-key" || r.Header.Get("X-PM-Workload") != "web" || r.Header.Get("X-PM-Agent-Id") == "" {
			t.Error("missing scope")
		}
		uploads.Add(1)
		w.WriteHeader(204)
	}))
	t.Cleanup(server.Close)
	return &Client{base: http.DefaultTransport, endpoint: server.URL, key: "test-key", workload: "web", agentID: strings.Repeat("a", 32), dir: t.TempDir(), queue: make(chan Task, 8)}, uploads
}
func TestDuplicateAndRestartDoNotRunScriptAgain(t *testing.T) {
	c, uploads := testClient(t)
	runs := 0
	c.collect = func(_ context.Context, t Task) Result {
		runs++
		return Result{ID: t.ID, Output: "evidence", CollectedAt: time.Now()}
	}
	task := Task{ID: strings.Repeat("b", 64), Recipe: "systemd", ExpiresAt: time.Now().Add(time.Minute)}
	c.handle(context.Background(), task)
	c.handle(context.Background(), task)
	c.replay(context.Background())
	if runs != 1 || uploads.Load() != 1 {
		t.Fatalf("runs=%d uploads=%d", runs, uploads.Load())
	}
}
func TestInterruptedReservationUploadsLimitationWithoutRerun(t *testing.T) {
	c, uploads := testClient(t)
	task := Task{ID: strings.Repeat("b", 64), Recipe: "cpu", ExpiresAt: time.Now().Add(time.Minute)}
	path := filepath.Join(c.dir, task.ID+".json")
	if err := c.save(path, record{Task: task}); err != nil {
		t.Fatal(err)
	}
	c.collect = func(context.Context, Task) Result { t.Fatal("interrupted script ran again"); return Result{} }
	c.replay(context.Background())
	c.replay(context.Background())
	if uploads.Load() != 1 {
		t.Fatalf("uploads=%d", uploads.Load())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "interrupted") {
		t.Fatal("missing interruption explanation")
	}
}
func TestUnrecognizedAndExpiredScriptsAreNotQueued(t *testing.T) {
	c, _ := testClient(t)
	for _, task := range []Task{{ID: strings.Repeat("a", 64), Recipe: "arbitrary-shell", ExpiresAt: time.Now().Add(time.Minute)}, {ID: "../../etc/passwd", Recipe: "cpu", ExpiresAt: time.Now().Add(time.Minute)}, {ID: strings.Repeat("a", 64), Recipe: "cpu", ExpiresAt: time.Now().Add(-time.Minute)}} {
		raw, _ := json.Marshal(task)
		c.accept(base64.RawURLEncoding.EncodeToString(raw))
	}
	if len(c.queue) != 0 {
		t.Fatal("unsafe task queued")
	}
}
func TestOutputIsBoundedWithoutBlockingProducer(t *testing.T) {
	b := &boundedOutput{}
	input := strings.Repeat("x", maxOutput*2)
	n, err := b.Write([]byte(input))
	if err != nil || n != len(input) || b.buf.Len() != maxOutput || !b.truncated {
		t.Fatal("output cap failed")
	}
}
func TestScriptsAreValidBash(t *testing.T) {
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			command := exec.Command("bash", "-n")
			command.Stdin = strings.NewReader(scriptPreamble + script)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v", output, err)
			}
		})
	}
}
func TestRedactCredentialsBeforeUpload(t *testing.T) {
	got := Redact(`password=do-not-send mongodb://user:do-not-send@host {"token":"do-not-send"}`)
	if strings.Contains(got, "do-not-send") {
		t.Fatal("secret remains")
	}
}
