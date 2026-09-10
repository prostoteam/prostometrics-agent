package nginx

import (
	"hash/fnv"
	"strconv"
	"strings"
	"unicode/utf8"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

// The ranked lists built here -- which pages, which failing pages and which
// referring sites drew the most different visitors -- are the only thing the
// agent reports that depends on who made a request rather than on how many
// requests there were. Everything else a host measures has no visitor in it at
// all, which is why these come from the access log and nowhere else.
//
// Each list needs two things per request: an identity to tell visitors apart,
// and the thing that visitor touched. The access log carries both on every
// line, and neither leaves this file in the shape it was logged: the address is
// hashed, and the page and the referrer are reduced to the row they belong in.

const (
	// maxItemBytes is the ceiling the product puts on one row's name.
	maxItemBytes = 256

	// idPlaceholder replaces a path segment that names one record rather than
	// one page.
	idPlaceholder = ":id"

	// The three lists. Each ranks its rows by how many different visitors drew
	// them, which is what the `top` metric type counts.
	metricPages      = "nginx.pages"
	metricErrorPages = "nginx.error_pages"
	metricReferrers  = "nginx.referrers"
)

// rankedRows is what one request contributes to the three lists. An empty field
// means the request belongs in that list not at all.
type rankedRows struct {
	page     string
	failing  string
	referrer string
}

// rowsFor decides which rows a request belongs in.
//
// A failing page carries its status in the row's own name rather than beside
// it, because a page answering 404 to one visitor and 502 to another is two
// different problems and would otherwise read as one row.
//
// A referral is a person arriving at a page. The browser then fetches that
// page's stylesheet, script and images carrying the same referrer, so counting
// those would multiply one arrival by however many files the page is built
// from -- and on a site with no site_host set, would report every one of them
// as a referral from the site to itself.
func (c *AccessLogCollector) rowsFor(entry accessLogEntry) rankedRows {
	var rows rankedRows
	path := normalizePath(entry.target)
	asset := path != "" && isAssetPath(path)
	rows.failing = failingRow(entry.status, path, asset)
	if path == "" || asset {
		return rows
	}
	rows.page = path
	if site := referrerSite(entry.referrer); site != "" && site != c.siteHost {
		rows.referrer = site
	}
	return rows
}

// recordRankedRows reports one request into whichever lists it belongs in. A
// request from a format carrying no address counts nowhere: a list that cannot
// tell visitors apart would rank every row equally.
func (c *AccessLogCollector) recordRankedRows(entry accessLogEntry) {
	if entry.remoteAddr == "" {
		return
	}
	rows := c.rowsFor(entry)
	if rows == (rankedRows{}) {
		return
	}
	visitor := visitorToken(entry.remoteAddr)
	if rows.page != "" {
		prostometrics.CountTop(visitor, metricPages, rows.page)
	}
	if rows.failing != "" {
		prostometrics.CountTop(visitor, metricErrorPages, rows.failing)
	}
	if rows.referrer != "" {
		prostometrics.CountTop(visitor, metricReferrers, rows.referrer)
	}
}

// assetExtensions are the files a page loads rather than pages a person opened.
// They are left out of the page list because every visit fetches the same few of
// them, which would put a stylesheet above every article on the site and leave
// no room for the pages anyone actually asked for. They stay in the failing list:
// a script that 404s is worth seeing, and a broken one is not fetched by every
// visit anyway.
var assetExtensions = map[string]bool{
	"js": true, "mjs": true, "cjs": true, "css": true, "map": true,
	"png": true, "jpg": true, "jpeg": true, "gif": true, "svg": true,
	"webp": true, "avif": true, "ico": true, "bmp": true,
	"woff": true, "woff2": true, "ttf": true, "otf": true, "eot": true,
	"mp4": true, "webm": true, "mp3": true, "ogg": true, "wav": true,
	"m4a": true, "mov": true,
}

// assetPrefixes are the roots a framework serves its own files from, where the
// file often has no extension at all. A Next.js site fetches /_next/image?url=
// several times per page: strip the query and it is /_next/image, which has no
// dot to recognise it by, so it is drawn by every visitor and takes the top row
// of the pages list -- the exact outcome the extension test exists to prevent.
var assetPrefixes = []string{
	"/_next/",
	"/_nuxt/",
	"/_astro/",
	"/static/",
	"/assets/",
	"/build/",
	"/dist/",
}

// isAssetPath reports whether a path names a file a page loads rather than a
// page a person opened. An endpoint answering JSON or a page ending in .php is
// still a page.
func isAssetPath(path string) bool {
	for _, prefix := range assetPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	slash := strings.LastIndexByte(path, '/')
	name := path[slash+1:]
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return false
	}
	return assetExtensions[strings.ToLower(name[dot+1:])]
}

// routineStatuses answer a request without the site being broken, and every one
// of them arrives from many different addresses -- which is exactly what this
// list ranks by. Left in, they take the top rows and push the real breakages
// below the fold: scanners knocking on /wp-login.php and /.env produce 404s
// from thousands of distinct addresses, an auth-gated app answers 401 or 403 to
// every logged-out poll, and nginx logs 499 whenever a visitor closes a tab
// mid-load, which on mobile is routine.
var routineStatuses = map[int]bool{
	401: true, // not signed in
	403: true, // signed in, not allowed
	499: true, // nginx: the visitor went away first
}

// failingRow names the row a request belongs in on the failing-pages list, or
// nothing when the request did not fail in a way worth ranking. The status
// leads so that the same page failing two ways is two rows, which is what it
// is.
//
// A missing page and a missing file are not the same event. Nobody asks a
// server for a page that never existed except a scanner working through a list,
// so 404 on a page is noise; a browser only asks for a file the page it just
// loaded told it to, so 404 on one is a deployment that shipped half of itself.
func failingRow(status int, path string, asset bool) string {
	if status < 400 || path == "" || routineStatuses[status] {
		return ""
	}
	if status == 404 && !asset {
		return ""
	}
	return clampItem(strconv.Itoa(status) + " " + path)
}

// visitorToken turns a client address into the non-negative integer the client
// library takes as an identity. The address is hashed on the machine that read
// it rather than sent: the lists need to tell visitors apart, not to know who
// they are, and the address is the one piece of personal data in the log.
func visitorToken(addr string) uint64 {
	sum := fnv.New64a()
	_, _ = sum.Write([]byte(addr))
	return sum.Sum64()
}

// normalizePath reduces a request target to the page it names. The query string
// goes, and a segment that identifies one record becomes :id, so /orders/41 and
// /orders/9002 share a row instead of taking two of the few thousand a list
// holds. Without that, a site with a URL per record fills its list with rows
// nobody asked about and pushes out the pages that matter.
func normalizePath(target string) string {
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if !strings.HasPrefix(target, "/") {
		// A request may legally name the whole URL rather than the path, and a
		// proxied one usually does. Anything else -- a CONNECT target, or a
		// request line nginx could not parse and logged whole -- names no page.
		i := strings.Index(target, "://")
		if i < 0 {
			return ""
		}
		j := strings.IndexByte(target[i+3:], '/')
		if j < 0 {
			return ""
		}
		target = target[i+3+j:]
	}

	segments := strings.Split(target, "/")
	for i, segment := range segments {
		if isIdentifierSegment(segment) {
			segments[i] = idPlaceholder
		}
	}
	path := strings.Join(segments, "/")

	// A trailing slash names the same page, and keeping both spreads one page's
	// visitors across two rows.
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	return clampItem(path)
}

// referrerSite reduces a referrer to the site it names. The whole URL is not
// wanted: the question these rows answer is which sites send visitors, and the
// pages on them run to as many rows as those sites have. A leading www. goes so
// that one site is one row.
func referrerSite(raw string) string {
	value := raw
	if i := strings.Index(value, "://"); i >= 0 {
		value = value[i+3:]
	} else {
		value = strings.TrimPrefix(value, "//")
	}
	if i := strings.IndexAny(value, "/?#"); i >= 0 {
		value = value[:i]
	}
	if i := strings.LastIndexByte(value, '@'); i >= 0 {
		value = value[i+1:]
	}
	if strings.HasPrefix(value, "[") {
		// A bracketed IPv6 host, whose colons are part of the address.
		if i := strings.IndexByte(value, ']'); i >= 0 {
			value = value[:i+1]
		}
	} else if i := strings.LastIndexByte(value, ':'); i >= 0 {
		value = value[:i]
	}
	value = strings.ToLower(strings.TrimSuffix(value, "."))
	value = strings.TrimPrefix(value, "www.")

	// A host starts with a letter, a digit, or the bracket of an IPv6 address.
	// Nothing else is a site, and nginx's "-" for no referrer would otherwise
	// become a row of its own.
	if value == "" {
		return ""
	}
	if first := value[0]; !isHostStart(first) {
		return ""
	}
	return clampItem(value)
}

// isHostStart accepts anything a hostname may begin with: a letter, a digit,
// the bracket of an IPv6 address, or the lead byte of a non-Latin script. Only
// the last matters in practice -- an internationalised domain such as
// пример.рф starts with 0xD0, and testing for ASCII alone discarded every
// referral from one.
func isHostStart(b byte) bool {
	return b == '[' ||
		b >= 0x80 ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9')
}

// isIdentifierSegment reports whether a path segment names one record rather
// than one page. Three shapes cover nearly every scheme in use: a number, a
// dashed UUID, and the long run of hex that object ids and content hashes take,
// which also catches a UUID written without its dashes. A slug is deliberately
// left alone -- /guides/how-to-instrument-a-service is a page, and collapsing it
// would empty the list of everything worth reading.
func isIdentifierSegment(segment string) bool {
	switch {
	case segment == "":
		return false
	case isAllDigits(segment):
		return true
	case isDashedUUID(segment):
		return true
	case len(segment) >= 16 && isAllHex(segment):
		return true
	default:
		return false
	}
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func isAllHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

func isDashedUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHexDigit(s[i]) {
				return false
			}
		}
	}
	return true
}

// clampItem holds an item inside the product's ceiling, cutting on a rune
// boundary so what survives is still valid UTF-8. An item carrying a control
// byte is dropped rather than cleaned: the client refuses it anyway, and a log
// line holding one is not a request worth ranking.
func clampItem(item string) string {
	if item == "" {
		return ""
	}
	for i := 0; i < len(item); i++ {
		if b := item[i]; b < 0x20 || b == 0x7f {
			return ""
		}
	}
	if len(item) > maxItemBytes {
		cut := maxItemBytes
		for cut > 0 && !utf8.RuneStart(item[cut]) {
			cut--
		}
		item = item[:cut]
	}
	if !utf8.ValidString(item) {
		return ""
	}
	return item
}
