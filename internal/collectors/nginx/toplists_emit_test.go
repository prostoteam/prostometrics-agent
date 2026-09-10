package nginx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

// captureTransport keeps what a flush would have sent instead of sending it, so
// a test can read the events the product would receive.
type captureTransport struct {
	mu   sync.Mutex
	tops []prostometrics.TopEvent
	// order records the kind of every event in the order it was flushed, so a
	// test can ask what the client was offered first.
	order []string
}

func (t *captureTransport) Send(_ context.Context, payload *prostometrics.Payload) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tops = append(t.tops, payload.Tops...)
	for range payload.Counters {
		t.order = append(t.order, "counter")
	}
	for range payload.Values {
		t.order = append(t.order, "value")
	}
	for range payload.Tops {
		t.order = append(t.order, "top")
	}
	return nil
}

// sent counts the events that actually left, without de-duplicating anything.
// rows() below folds repeats away by visitor, which is the right shape for
// asking what a card will show and exactly the wrong shape for asking whether
// the client collapsed a repeat before sending it -- that assertion cannot fail
// against a client that stopped collapsing.
func (t *captureTransport) sent(metric string, item string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, event := range t.tops {
		if event.Metric == metric && event.Item == item {
			count++
		}
	}
	return count
}

func (t *captureTransport) rows(metric string) map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	visitors := map[string]map[string]bool{}
	for _, event := range t.tops {
		if event.Metric != metric {
			continue
		}
		if visitors[event.Item] == nil {
			visitors[event.Item] = map[string]bool{}
		}
		visitors[event.Item][event.UniqueID] = true
	}
	out := map[string]int{}
	for item, ids := range visitors {
		out[item] = len(ids)
	}
	return out
}

// The whole path, from a line in the log to the events the product ranks: the
// unit tests above cover each decision, and this covers that they are wired to
// the client at all and under the names the dashboards will show.
func TestAccessLogCollectorReportsTheThreeRankedLists(t *testing.T) {
	lines := []string{
		// Two visitors read the same guide; one of them reads it twice, which
		// must not make it look twice as widely read.
		`10.0.0.1 - - [08/Sep/2026:10:00:00 +0000] "GET /guides/metrics HTTP/1.1" 200 12 "https://news.example.com/item?id=1" "ua"`,
		`10.0.0.1 - - [08/Sep/2026:10:00:01 +0000] "GET /guides/metrics HTTP/1.1" 200 12 "https://news.example.com/item?id=1" "ua"`,
		`10.0.0.2 - - [08/Sep/2026:10:00:02 +0000] "GET /guides/metrics HTTP/1.1" 200 12 "https://www.news.example.com/other" "ua"`,
		// Two records behind one page, and one of them is broken.
		`10.0.0.3 - - [08/Sep/2026:10:00:03 +0000] "GET /orders/41 HTTP/1.1" 200 12 "-" "ua"`,
		`10.0.0.4 - - [08/Sep/2026:10:00:04 +0000] "GET /orders/9002 HTTP/1.1" 502 12 "-" "ua"`,
		// A visitor arriving from one of our own pages is not a site sending us
		// traffic.
		`10.0.0.5 - - [08/Sep/2026:10:00:05 +0000] "GET /pricing HTTP/1.1" 200 12 "https://example.com/guides/metrics" "ua"`,
		// The stylesheet every one of those visits also fetched, and a script
		// that is missing.
		`10.0.0.1 - - [08/Sep/2026:10:00:06 +0000] "GET /assets/main.css HTTP/1.1" 200 12 "-" "ua"`,
		`10.0.0.2 - - [08/Sep/2026:10:00:07 +0000] "GET /assets/main.css HTTP/1.1" 200 12 "-" "ua"`,
		`10.0.0.3 - - [08/Sep/2026:10:00:08 +0000] "GET /assets/main.css HTTP/1.1" 200 12 "-" "ua"`,
		`10.0.0.4 - - [08/Sep/2026:10:00:09 +0000] "GET /assets/app.js HTTP/1.1" 404 12 "-" "ua"`,
		// The font that page pulled in carries the same referrer the page did.
		// Counting it would report one arrival as two.
		`10.0.0.6 - - [08/Sep/2026:10:00:10 +0000] "GET /blog/post HTTP/1.1" 200 12 "https://forum.example.org/t/9" "ua"`,
		`10.0.0.6 - - [08/Sep/2026:10:00:11 +0000] "GET /fonts/inter.woff2 HTTP/1.1" 200 12 "https://forum.example.org/t/9" "ua"`,
	}

	path := filepath.Join(t.TempDir(), "access.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	transport := &captureTransport{}
	client, err := prostometrics.Init("test", prostometrics.Config{Transport: transport, Silent: true})
	if err != nil {
		t.Fatal(err)
	}

	collector := NewAccessLogCollector(path, time.Second, AccessLogOptions{
		TopLists: true,
		SiteHost: "example.com",
	})
	defer collector.Close(context.Background())

	// The first read only takes the collector to the end of the file, because a
	// restart must not replay history as if it had just happened.
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}

	pages := transport.rows(metricPages)
	if pages["/guides/metrics"] != 2 {
		t.Fatalf("%s rows = %v; the guide should show two visitors, not its three reads", metricPages, pages)
	}
	// One visitor read the guide twice, and the client drops a repeat of the
	// same visitor on the same row before it goes out. That collapsing is the
	// whole cost argument for the feature, so it is asserted on what was sent
	// rather than on the rows, which fold repeats away themselves.
	if sent := transport.sent(metricPages, "/guides/metrics"); sent != 2 {
		t.Fatalf("%d events were sent for the guide, want 2: one per visitor, with the repeat collapsed", sent)
	}
	if pages["/orders/:id"] != 2 {
		t.Fatalf("%s rows = %v; both orders belong in one row", metricPages, pages)
	}

	// The stylesheet was fetched by more visitors than any page was, which is
	// exactly why it must not be on this list.
	if _, asset := pages["/assets/main.css"]; asset {
		t.Fatalf("%s rows = %v; page furniture outranked the pages", metricPages, pages)
	}

	failing := transport.rows(metricErrorPages)
	if failing["502 /orders/:id"] != 1 {
		t.Fatalf("%s rows = %v", metricErrorPages, failing)
	}
	// A missing script is worth seeing even though a working one is not.
	if failing["404 /assets/app.js"] != 1 {
		t.Fatalf("%s rows = %v; a broken asset should still be reported", metricErrorPages, failing)
	}

	referrers := transport.rows(metricReferrers)
	if referrers["news.example.com"] != 2 {
		t.Fatalf("%s rows = %v; www. and the bare host are one site", metricReferrers, referrers)
	}
	if _, self := referrers["example.com"]; self {
		t.Fatalf("%s rows = %v; the site's own pages were ranked as a referrer", metricReferrers, referrers)
	}

	if referrers["forum.example.org"] != 1 {
		t.Fatalf("%s rows = %v", metricReferrers, referrers)
	}
}

// Nothing is reported unless the host asked for it, because these are the only
// events whose number grows with the site's traffic.
func TestAccessLogCollectorSendsNoRankedListsUnlessAskedTo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	transport := &captureTransport{}
	client, err := prostometrics.Init("test", prostometrics.Config{Transport: transport, Silent: true})
	if err != nil {
		t.Fatal(err)
	}

	collector := NewAccessLogCollector(path, time.Second, AccessLogOptions{})
	defer collector.Close(context.Background())
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	line := `10.0.0.1 - - [08/Sep/2026:10:00:00 +0000] "GET /guides/metrics HTTP/1.1" 200 12 "-" "ua"` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}

	if got := len(transport.tops); got != 0 {
		t.Fatalf("sent %d ranked-list events with the lists switched off", got)
	}
}

// A host that switches the ranked lists on must not lose the counts and times
// it already had. The client's queue drops what it cannot hold, so whatever is
// offered last is what goes -- and a catch-up tick can offer tens of thousands
// of ranked-list events. The counts and times therefore have to be enqueued
// first, and the ranked rows have to be bounded.
func TestAlwaysOnMetricsAreReportedBeforeTheRankedLists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	transport := &captureTransport{}
	client, err := prostometrics.Init("test", prostometrics.Config{Transport: transport, Silent: true})
	if err != nil {
		t.Fatal(err)
	}

	collector := NewAccessLogCollector(path, time.Second, AccessLogOptions{TopLists: true})
	defer collector.Close(context.Background())
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}

	var body strings.Builder
	for i := 0; i < maxRankedRowsPerTick+500; i++ {
		fmt.Fprintf(&body, `10.0.%d.%d - - [08/Sep/2026:10:00:00 +0000] "GET /guides/p%d HTTP/1.1" 200 12 "-" "ua" 0.5`+"\n",
			i/256%256, i%256, i)
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}

	firstTop := -1
	lastAlwaysOn := -1
	for i, kind := range transport.order {
		if kind == "top" && firstTop < 0 {
			firstTop = i
		}
		if kind != "top" {
			lastAlwaysOn = i
		}
	}
	if firstTop < 0 || lastAlwaysOn < 0 {
		t.Fatalf("expected both kinds, saw %d events", len(transport.order))
	}
	if lastAlwaysOn > firstTop {
		t.Fatal("a ranked-list event was offered before the request counts and times, so a full queue would drop those instead")
	}
	if len(transport.tops) > maxRankedRowsPerTick {
		t.Fatalf("%d ranked-list events were offered in one tick, past the %d cap", len(transport.tops), maxRankedRowsPerTick)
	}
}
