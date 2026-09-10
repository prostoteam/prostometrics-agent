package nginx

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNormalizePathCollapsesWhatNamesARecord(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"a numbered record", "/orders/41", "/orders/:id"},
		{"the query string is not part of the page", "/search?q=metrics&page=2", "/search"},
		{"a dashed uuid", "/users/9f1c2b3a-4d5e-6f70-8192-a3b4c5d6e7f8/edit", "/users/:id/edit"},
		{"an object id", "/files/507f1f77bcf86cd799439011", "/files/:id"},
		{"a date in the path", "/2026/09/launch", "/:id/:id/launch"},
		{"a trailing slash names the same page", "/about/", "/about"},
		{"the root keeps its slash", "/", "/"},
		{"a fragment never reaches the server but is logged when it does", "/pricing#plans", "/pricing"},

		// The whole point of the list is which pages people read, so a name has
		// to survive even when it is long or has digits in it.
		{"a slug is a page", "/guides/how-to-instrument-a-service", "/guides/how-to-instrument-a-service"},
		{"a version segment is a page", "/v2/status", "/v2/status"},
		{"a slug ending in a year is still a slug", "/posts/metrics-in-2026", "/posts/metrics-in-2026"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizePath(tc.target); got != tc.want {
				t.Fatalf("normalizePath(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

// A proxied request names the whole URL, and the rest of what can appear in a
// request line names no page at all.
func TestNormalizePathReadsAbsoluteFormAndRejectsTheRest(t *testing.T) {
	if got := normalizePath("http://example.com/orders/7"); got != "/orders/:id" {
		t.Fatalf("absolute form gave %q", got)
	}
	if got := normalizePath("https://example.com"); got != "" {
		t.Fatalf("a URL with no path gave %q", got)
	}
	for _, target := range []string{"", "example.com:443", "*", "garbage"} {
		if got := normalizePath(target); got != "" {
			t.Fatalf("normalizePath(%q) = %q, want nothing", target, got)
		}
	}
}

// The product refuses an item over 256 bytes, so a long path has to be cut here
// rather than dropped there -- and cut where a character ends, or what is left
// is not text.
func TestNormalizePathKeepsAnItemInsideTheCeiling(t *testing.T) {
	long := "/" + strings.Repeat("страница-", 60)
	got := normalizePath(long)
	if len(got) == 0 || len(got) > maxItemBytes {
		t.Fatalf("length = %d, want between 1 and %d", len(got), maxItemBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("the cut left invalid UTF-8: %q", got)
	}
}

func TestNormalizePathDropsALineWithControlBytes(t *testing.T) {
	if got := normalizePath("/orders\x01/41"); got != "" {
		t.Fatalf("got %q, want nothing", got)
	}
}

func TestReferrerSiteKeepsOnlyTheSite(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://news.ycombinator.com/item?id=42", "news.ycombinator.com"},
		{"http://www.google.com/", "google.com"},
		{"https://Example.COM:8443/path", "example.com"},
		{"//cdn.example.org/x", "cdn.example.org"},
		{"https://user:pass@example.net/page", "example.net"},
		{"http://[2001:db8::1]:8080/x", "[2001:db8::1]"},
		{"", ""},
		// What nginx writes when a request carried no referrer at all.
		{"-", ""},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := referrerSite(tc.raw); got != tc.want {
				t.Fatalf("referrerSite(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// The configured site host is put through the same reduction as a referrer, so
// that a customer writing it any reasonable way still matches.
func TestReferrerSiteMatchesAConfiguredHostHoweverItIsWritten(t *testing.T) {
	want := referrerSite("https://blog.example.com/post/1")
	for _, written := range []string{"blog.example.com", "https://blog.example.com", "www.blog.example.com", "BLOG.example.com/"} {
		if got := referrerSite(written); got != want {
			t.Fatalf("referrerSite(%q) = %q, want %q", written, got, want)
		}
	}
}

func TestRowsForPicksTheListsARequestBelongsIn(t *testing.T) {
	collector := NewAccessLogCollector("/dev/null", time.Second, AccessLogOptions{SiteHost: "example.com"})
	defer collector.Close(context.Background())

	entry := func(target, referrer string, status int) accessLogEntry {
		return accessLogEntry{target: target, referrer: referrer, status: status}
	}

	cases := []struct {
		name  string
		entry accessLogEntry
		want  rankedRows
	}{
		{
			name:  "a page someone arrived at from elsewhere",
			entry: entry("/blog/post", "https://forum.example.org/t/9", 200),
			want:  rankedRows{page: "/blog/post", referrer: "forum.example.org"},
		},
		{
			// The browser fetches the page's own files next, each carrying the
			// referrer the page had. One arrival must not count as several.
			name:  "the font that page pulled in",
			entry: entry("/fonts/inter.woff2", "https://forum.example.org/t/9", 200),
			want:  rankedRows{},
		},
		{
			name:  "a broken file is still worth seeing",
			entry: entry("/assets/app.js", "-", 404),
			want:  rankedRows{failing: "404 /assets/app.js"},
		},
		{
			name:  "a page that failed belongs in both lists",
			entry: entry("/orders/41", "-", 502),
			want:  rankedRows{page: "/orders/:id", failing: "502 /orders/:id"},
		},
		{
			name:  "arriving from one of our own pages is not a referral",
			entry: entry("/pricing", "https://example.com/blog/post", 200),
			want:  rankedRows{page: "/pricing"},
		},
		{
			name:  "a request that names no page at all",
			entry: entry("garbage", "https://forum.example.org/t/9", 200),
			want:  rankedRows{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := collector.rowsFor(tc.entry); got != tc.want {
				t.Fatalf("rowsFor() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestIsAssetPathSeparatesFurnitureFromPages(t *testing.T) {
	// Every visit fetches these, so leaving them in would rank a stylesheet
	// above every article on the site.
	for _, path := range []string{
		"/static/app.a3f9c2.js", "/assets/main.css", "/favicon.ico",
		"/img/hero.webp", "/fonts/inter.woff2", "/static/app.js.map",
		"/video/tour.MP4",
	} {
		if !isAssetPath(path) {
			t.Fatalf("%q should be page furniture", path)
		}
	}

	// These are pages and endpoints people actually asked for.
	for _, path := range []string{
		"/", "/guides/metrics", "/api/orders/:id", "/report.php",
		"/api/v2/status.json", "/downloads/handbook.pdf", "/robots.txt",
		"/careers/senior.engineer",
	} {
		if isAssetPath(path) {
			t.Fatalf("%q should count as a page", path)
		}
	}
}

func TestFailingRowNamesTheStatusBesideThePage(t *testing.T) {
	if got := failingRow(200, "/orders/:id", false); got != "" {
		t.Fatalf("a request that worked produced %q", got)
	}
	if got := failingRow(500, "/guides/missing", false); got != "500 /guides/missing" {
		t.Fatalf("got %q", got)
	}
	// The same page failing two ways is two problems and so two rows.
	if failingRow(422, "/orders/:id", false) == failingRow(502, "/orders/:id", false) {
		t.Fatal("a 404 and a 502 on one page collapsed into the same row")
	}
	if got := failingRow(500, "", false); got != "" {
		t.Fatalf("a failure with no page produced %q", got)
	}
}

// This list ranks by how many different visitors drew a row, which is exactly
// what internet background noise maximises: every scanner arrives from its own
// address. Left in, they take the top rows and the real breakages sit below
// the fold.
func TestFailingRowLeavesOutTheRoutineFailures(t *testing.T) {
	for _, status := range []int{401, 403, 404, 499} {
		if got := failingRow(status, "/wp-login.php", false); got != "" {
			t.Fatalf("status %d produced %q, which scanners and logged-out polls will top the list with", status, got)
		}
	}
	// What is left is the site actually failing.
	for _, status := range []int{400, 422, 429, 500, 502, 503, 504} {
		if got := failingRow(status, "/orders/:id", false); got == "" {
			t.Fatalf("status %d produced nothing, but it is the site failing", status)
		}
	}
}

// A framework serves its own files from a root of its own, and they often have
// no extension. Every visitor's browser fetches them several times a page, so
// one of them takes the top row of a list meant to say what people read.
func TestIsAssetPathCatchesFrameworkRootsWithoutAnExtension(t *testing.T) {
	for _, path := range []string{
		"/_next/image", "/_next/static/chunks/main", "/_nuxt/entry",
		"/_astro/hoisted", "/static/js/2", "/assets/index", "/build/app", "/dist/main",
	} {
		if !isAssetPath(path) {
			t.Fatalf("%q should be page furniture", path)
		}
	}
	// A page whose name merely resembles one of those roots is still a page.
	for _, path := range []string{"/statics", "/assetsmith", "/blog/_next-steps", "/buildings/:id"} {
		if isAssetPath(path) {
			t.Fatalf("%q should count as a page", path)
		}
	}
}

// An internationalised domain begins with a byte no ASCII test accepts, and a
// Russian-language product is exactly where those referrals come from.
func TestReferrerSiteKeepsANonLatinDomain(t *testing.T) {
	for raw, want := range map[string]string{
		"https://пример.рф/статьи":       "пример.рф",
		"https://münchen.de/x":           "münchen.de",
		"https://xn--e1afmkfd.xn--p1ai/": "xn--e1afmkfd.xn--p1ai",
	} {
		if got := referrerSite(raw); got != want {
			t.Fatalf("referrerSite(%q) = %q, want %q", raw, got, want)
		}
	}
	// Still not a site.
	if got := referrerSite("-"); got != "" {
		t.Fatalf("referrerSite(\"-\") = %q", got)
	}
}

func TestVisitorTokenTellsAddressesApartAndIsStable(t *testing.T) {
	first := visitorToken("203.0.113.7")
	if first != visitorToken("203.0.113.7") {
		t.Fatal("the same visitor got two identities, so one person would count twice")
	}
	if first == visitorToken("203.0.113.8") {
		t.Fatal("two visitors share an identity, so a page's reach would be understated")
	}
	if visitorToken("2001:db8::1") == visitorToken("2001:db8::2") {
		t.Fatal("two IPv6 visitors share an identity")
	}
}
