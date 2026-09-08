package redis

import "testing"

func TestParseInfoSkipsSectionHeaders(t *testing.T) {
	info := parseInfo("# Memory\r\nused_memory:1048576\r\nmaxmemory:0\r\n\r\n# Keyspace\r\ndb0:keys=12,expires=1,avg_ttl=0\r\n")
	if info["used_memory"] != "1048576" {
		t.Fatalf("used_memory = %q", info["used_memory"])
	}
	if _, present := info["# Memory"]; present {
		t.Fatal("section headers must not become keys")
	}
	if got := countKeys(info); got != 12 {
		t.Fatalf("countKeys = %d, want 12", got)
	}
}

func TestCountKeysSumsEveryDatabase(t *testing.T) {
	info := map[string]string{
		"db0":  "keys=10,expires=0,avg_ttl=0",
		"db1":  "keys=5,expires=0,avg_ttl=0",
		"role": "master",
	}
	if got := countKeys(info); got != 15 {
		t.Fatalf("countKeys = %d, want 15", got)
	}
}

func TestParseURI(t *testing.T) {
	cases := []struct {
		name        string
		uri         string
		wantAddress string
		wantNetwork string
		wantTLS     bool
		wantPass    string
	}{
		{name: "bare host gains the default port", uri: "127.0.0.1", wantAddress: "127.0.0.1:6379", wantNetwork: "tcp"},
		{name: "host and port", uri: "redis://db:6380", wantAddress: "db:6380", wantNetwork: "tcp"},
		{name: "tls scheme", uri: "rediss://db:6380", wantAddress: "db:6380", wantNetwork: "tcp", wantTLS: true},
		{name: "password in user info", uri: "redis://:secret@db:6379", wantAddress: "db:6379", wantNetwork: "tcp", wantPass: "secret"},
		{name: "unix socket path", uri: "/var/run/redis.sock", wantAddress: "/var/run/redis.sock", wantNetwork: "unix"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := parseURI(tc.uri)
			if err != nil {
				t.Fatal(err)
			}
			if target.address != tc.wantAddress || target.network != tc.wantNetwork {
				t.Fatalf("target = %s/%s, want %s/%s", target.network, target.address, tc.wantNetwork, tc.wantAddress)
			}
			if target.useTLS != tc.wantTLS {
				t.Fatalf("useTLS = %v, want %v", target.useTLS, tc.wantTLS)
			}
			if target.password != tc.wantPass {
				t.Fatalf("password = %q, want %q", target.password, tc.wantPass)
			}
		})
	}

	if _, err := parseURI(""); err == nil {
		t.Fatal("expected an empty uri to be rejected")
	}
}

// parsed.Host keeps the brackets around an IPv6 literal, so re-joining it with a
// default port produced "[[::1]]:6379" and every dial failed to parse.
func TestParseURIIPv6(t *testing.T) {
	cases := map[string]string{
		"redis://[::1]":      "[::1]:6379",
		"redis://[::1]:6380": "[::1]:6380",
		"rediss://[fe80::1]": "[fe80::1]:6379",
	}
	for uri, want := range cases {
		target, err := parseURI(uri)
		if err != nil {
			t.Errorf("%q returned err=%v", uri, err)
			continue
		}
		if target.address != want {
			t.Errorf("%q address = %q, want %q", uri, target.address, want)
		}
	}
}
