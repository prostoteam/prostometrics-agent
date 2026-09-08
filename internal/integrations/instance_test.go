package integrations

import "testing"

func TestInstanceLabelFromURIHosts(t *testing.T) {
	cases := map[string]string{
		"mongodb://user:pass@db1.internal:27017/admin": "db1.internal",
		"postgres://monitor@localhost:5432/postgres":   "localhost",
		"redis://127.0.0.1:6379":                       "127.0.0.1",
		"127.0.0.1:6379":                               "127.0.0.1",
		"http://monitor:pass@[::1]:15672":              "::1",
		"mongodb://a.internal:27017,b.internal:27017":  "a.internal",
	}
	for uri, want := range cases {
		got, err := InstanceLabelFromURI(uri)
		if err != nil {
			t.Errorf("%q returned err=%v", uri, err)
			continue
		}
		if got != want {
			t.Errorf("%q label = %q, want %q", uri, got, want)
		}
	}
}

// A service reached over a unix socket has no host. Refusing those URIs stopped
// the whole agent at startup rather than the one integration, because an
// unresolvable label is a fatal configuration error.
func TestInstanceLabelFromURISockets(t *testing.T) {
	cases := map[string]string{
		"/var/run/redis.sock":                         "redis",
		"unix:///var/run/redis.sock":                  "redis",
		"/var/run/postgresql":                         "postgresql",
		"/tmp/mysql.sock":                             "mysql",
		"postgres:///app?host=/var/run/postgresql":    "postgresql",
		"postgres://user@/app?host=/var/run/pgbounce": "pgbounce",
	}
	for uri, want := range cases {
		got, err := InstanceLabelFromURI(uri)
		if err != nil {
			t.Errorf("%q returned err=%v", uri, err)
			continue
		}
		if got != want {
			t.Errorf("%q label = %q, want %q", uri, got, want)
		}
	}
}

func TestInstanceLabelFromURIRejectsUnnameable(t *testing.T) {
	for _, uri := range []string{"", "   ", "redis://", "/"} {
		if got, err := InstanceLabelFromURI(uri); err == nil {
			t.Errorf("%q should be rejected, got label %q", uri, got)
		}
	}
}
