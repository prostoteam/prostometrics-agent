package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readRecord(t *testing.T, c *Client, task Task) record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.dir, task.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var r record
	if json.Unmarshal(data, &r) != nil {
		t.Fatal("invalid record")
	}
	return r
}
func TestTransientUploadRetainsReportWithoutRecollection(t *testing.T) {
	uploads, runs := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads++
		if uploads == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	c := &Client{base: http.DefaultTransport, endpoint: server.URL, dir: t.TempDir()}
	c.collect = func(_ context.Context, task Task) Result {
		runs++
		return Result{ID: task.ID, Output: "saved evidence", CollectedAt: time.Now()}
	}
	task := remoteTask("true")
	c.handle(context.Background(), task)
	if readRecord(t, c, task).Sent {
		t.Fatal("transient failure marked delivered")
	}
	c.handle(context.Background(), task)
	c.replay(context.Background())
	c.replay(context.Background())
	if runs != 1 || uploads != 2 || !readRecord(t, c, task).Sent {
		t.Fatalf("runs=%d uploads=%d", runs, uploads)
	}
}
func TestTransientDownloadRecoversBeforeSingleExecution(t *testing.T) {
	downloads, uploads := 0, 0
	body := "printf recovered"
	task := remoteTask(body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			downloads++
			if downloads < 3 {
				w.WriteHeader(503)
				return
			}
			w.Write([]byte(body))
			return
		}
		uploads++
		w.WriteHeader(204)
	}))
	defer server.Close()
	c := &Client{base: http.DefaultTransport, endpoint: server.URL, dir: t.TempDir()}
	c.collect = c.collectTask
	c.handle(context.Background(), task)
	c.handle(context.Background(), task)
	c.replay(context.Background())
	rec := readRecord(t, c, task)
	if downloads != 3 || uploads != 1 || rec.Result == nil || rec.Result.Output != "recovered" || rec.Result.ExitCode != 0 {
		t.Fatalf("downloads=%d uploads=%d result=%+v", downloads, uploads, rec.Result)
	}
}
func TestDownloadRetryBudgetAndCancellation(t *testing.T) {
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downloads++; w.WriteHeader(503) }))
	defer server.Close()
	c := &Client{base: http.DefaultTransport, endpoint: server.URL, dir: t.TempDir()}
	task := remoteTask("true")
	if _, err := c.downloadScript(context.Background(), task); !errors.Is(err, errScriptUnavailable) || downloads != 3 {
		t.Fatalf("downloads=%d err=%v", downloads, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := downloads
	started := time.Now()
	c.downloadScript(ctx, task)
	if downloads != before || time.Since(started) > time.Second {
		t.Fatal("cancelled download retried")
	}
}
func TestDirectorySyncFailurePreventsExecution(t *testing.T) {
	c, uploads := testClient(t)
	runs := 0
	c.collect = func(context.Context, Task) Result { runs++; return Result{} }
	c.syncDir = func(string) error { return errors.New("directory sync failed") }
	task := remoteTask("true")
	c.handle(context.Background(), task)
	if runs != 0 || uploads.Load() != 0 {
		t.Fatal("execution/upload before durable reservation")
	}
	// Even after directory sync recovers, the same task is never run. Replay
	// reports the interrupted reservation, preserving the at-most-once choice.
	c.syncDir = nil
	c.handle(context.Background(), task)
	c.replay(context.Background())
	if runs != 0 || uploads.Load() != 1 {
		t.Fatal("reservation reran after sync failure")
	}
}
func TestReservationAndRenamedResultsSyncDirectory(t *testing.T) {
	c, _ := testClient(t)
	calls := 0
	task := remoteTask("true")
	c.syncDir = func(dir string) error {
		if dir != c.dir {
			t.Error("wrong directory")
		}
		calls++
		rec := readRecord(t, c, task)
		if calls == 1 && rec.Result != nil {
			t.Error("reservation sync happened after execution")
		}
		return nil
	}
	c.collect = func(_ context.Context, task Task) Result {
		if calls != 1 {
			t.Error("execution before reservation directory sync")
		}
		return Result{ID: task.ID, Output: "evidence", CollectedAt: time.Now()}
	}
	c.handle(context.Background(), task)
	if calls != 3 || !readRecord(t, c, task).Sent {
		t.Fatalf("directory sync calls=%d", calls)
	}
}
func TestResultSyncFailureRetriesPersistenceWithoutExecution(t *testing.T) {
	c, uploads := testClient(t)
	calls, runs := 0, 0
	task := remoteTask("true")
	c.syncDir = func(string) error {
		calls++
		if calls == 2 {
			return errors.New("result rename sync failed")
		}
		return nil
	}
	c.collect = func(_ context.Context, task Task) Result {
		runs++
		return Result{ID: task.ID, Output: "saved evidence", CollectedAt: time.Now()}
	}
	c.handle(context.Background(), task)
	if uploads.Load() != 0 {
		t.Fatal("non-durable result uploaded")
	}
	c.replay(context.Background())
	c.handle(context.Background(), task)
	if runs != 1 || uploads.Load() != 1 || !readRecord(t, c, task).Sent {
		t.Fatal("result lost or recollected after persistence recovered")
	}
}

func TestFailedUploadRetryDoesNotExtendLocalRetention(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	c := &Client{base: http.DefaultTransport, endpoint: server.URL, dir: t.TempDir()}
	c.collect = func(_ context.Context, task Task) Result {
		return Result{ID: task.ID, Output: "evidence", CollectedAt: time.Now()}
	}
	task := remoteTask("true")
	c.handle(context.Background(), task)
	path := filepath.Join(c.dir, task.ID+".json")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	c.replay(context.Background())
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(old) {
		t.Fatal("upload retries reset retention age", err)
	}
}
func TestSentFlagSyncFailureDoesNotRepeatSuccessfulUpload(t *testing.T) {
	c, uploads := testClient(t)
	calls := 0
	task := remoteTask("true")
	c.syncDir = func(string) error {
		calls++
		if calls == 3 {
			return errors.New("ack flag directory sync failed")
		}
		return nil
	}
	c.collect = func(_ context.Context, task Task) Result {
		return Result{ID: task.ID, Output: "evidence", CollectedAt: time.Now()}
	}
	c.handle(context.Background(), task)
	c.replay(context.Background())
	if uploads.Load() != 1 || len(c.pending) != 0 {
		t.Fatal("successful upload repeated instead of persisting its acknowledgement")
	}
}

func TestRetriedUploadAcknowledgementSyncFailureDoesNotResend(t *testing.T) {
	uploads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads++
		if uploads == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	c := &Client{base: http.DefaultTransport, endpoint: server.URL, dir: t.TempDir()}
	syncs := 0
	c.syncDir = func(string) error {
		syncs++
		if syncs == 3 {
			return errors.New("ack flag sync failed")
		}
		return nil
	}
	c.collect = func(_ context.Context, task Task) Result {
		return Result{ID: task.ID, Output: "evidence", CollectedAt: time.Now()}
	}
	c.handle(context.Background(), remoteTask("true"))
	c.replay(context.Background())
	c.replay(context.Background())
	if uploads != 2 || len(c.pending) != 0 {
		t.Fatal("successful retry uploaded again after flag sync failure")
	}
}

func TestReservationSyncFailureAfterStartupRetriesInterruption(t *testing.T) {
	c, uploads := testClient(t)
	c.replay(context.Background()) // Production performs the disk scan before accepting tasks.
	c.collect = func(context.Context, Task) Result { t.Fatal("failed reservation executed"); return Result{} }
	c.syncDir = func(string) error { return errors.New("temporary directory sync failure") }
	task := remoteTask("true")
	c.handle(context.Background(), task)
	c.replay(context.Background())
	if uploads.Load() != 0 || len(c.pending) != 1 {
		t.Fatal("failed reservation was lost or uploaded before persistence recovered")
	}
	c.syncDir = nil
	c.handle(context.Background(), task)
	c.replay(context.Background())
	c.replay(context.Background())
	rec := readRecord(t, c, task)
	if uploads.Load() != 1 || len(c.pending) != 0 || !rec.Sent || rec.Result == nil || rec.Result.ExitCode != -1 {
		t.Fatal("interruption was not durably reported once after recovery")
	}
}
