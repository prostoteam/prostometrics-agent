package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRuntimeConfigDefaults(t *testing.T) {
	t.Setenv("HOSTNAME", "ignored")
	cfg, err := resolveRuntimeConfig(nil, "api-host", true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workload != "api-host" || !cfg.NginxEnabled || cfg.MongoEnabled {
		t.Fatalf("runtime config = %+v", cfg)
	}
}

func TestLoadFileConfigExpandsEnvFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "agent.env")
	configPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(envPath, []byte("MONGO_PASSWORD=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "env_files:\n  - " + envPath + "\nagent:\n  workload: host-a\nintegrations:\n  mongo:\n    instances:\n      - uri: mongodb://monitor:${MONGO_PASSWORD}@localhost/admin\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadFileConfig(configSource{Path: configPath, Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Integrations.Mongo.Instances[0].URI; got != "mongodb://monitor:secret@localhost/admin" {
		t.Fatalf("expanded URI = %q", got)
	}
}

// A unix socket is how these services are usually reached on the machine they
// run on, and the label derivation used to reject one — which failed the whole
// configuration and stopped the agent, taking every host metric with it.
func TestResolveRuntimeConfigAcceptsSocketInstances(t *testing.T) {
	cfg := &fileConfig{}
	cfg.Agent.Workload = strPtr("host-a")
	cfg.Integrations.Redis.Instances = []uriInstanceConfig{{URI: "/var/run/redis.sock"}}
	cfg.Integrations.Postgres.Instances = []uriInstanceConfig{{URI: "postgres:///app?host=/var/run/postgresql"}}

	out, err := resolveRuntimeConfig(cfg, "", false)
	if err != nil {
		t.Fatalf("resolveRuntimeConfig returned err=%v", err)
	}
	if !out.RedisEnabled || len(out.RedisInstances) != 1 || out.RedisInstances[0].Label != "redis" {
		t.Fatalf("redis instances = %+v", out.RedisInstances)
	}
	if !out.PostgresEnabled || len(out.PostgresInstances) != 1 || out.PostgresInstances[0].Label != "postgresql" {
		t.Fatalf("postgres instances = %+v", out.PostgresInstances)
	}
}

func TestResolveRuntimeConfigInstanceRules(t *testing.T) {
	t.Run("an explicit label wins over the derived one", func(t *testing.T) {
		cfg := &fileConfig{}
		cfg.Agent.Workload = strPtr("host-a")
		cfg.Integrations.Redis.Instances = []uriInstanceConfig{{URI: "redis://127.0.0.1:6379", Label: "sessions"}}

		out, err := resolveRuntimeConfig(cfg, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if out.RedisInstances[0].Label != "sessions" {
			t.Fatalf("label = %q, want sessions", out.RedisInstances[0].Label)
		}
	})

	t.Run("mysql is labelled from its driver DSN", func(t *testing.T) {
		cfg := &fileConfig{}
		cfg.Agent.Workload = strPtr("host-a")
		cfg.Integrations.MySQL.Instances = []mysqlInstanceConfig{{DSN: "monitor:pass@tcp(db.internal:3306)/"}}

		out, err := resolveRuntimeConfig(cfg, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if out.MySQLInstances[0].Label != "db.internal" {
			t.Fatalf("label = %q, want db.internal", out.MySQLInstances[0].Label)
		}
	})

	t.Run("asking for an integration while configuring nothing is an error", func(t *testing.T) {
		cfg := &fileConfig{}
		cfg.Agent.Workload = strPtr("host-a")
		enabled := true
		cfg.Integrations.Postgres.Enabled = &enabled

		if _, err := resolveRuntimeConfig(cfg, "", false); err == nil {
			t.Fatal("expected an error rather than a silent no-op")
		}
	})

	t.Run("an empty connection string is an error", func(t *testing.T) {
		cfg := &fileConfig{}
		cfg.Agent.Workload = strPtr("host-a")
		cfg.Integrations.Mongo.Instances = []uriInstanceConfig{{URI: "  "}}

		if _, err := resolveRuntimeConfig(cfg, "", false); err == nil {
			t.Fatal("expected an error for a blank uri")
		}
	})

	t.Run("disabling keeps configured instances off", func(t *testing.T) {
		cfg := &fileConfig{}
		cfg.Agent.Workload = strPtr("host-a")
		disabled := false
		cfg.Integrations.Mongo.Enabled = &disabled
		cfg.Integrations.Mongo.Instances = []uriInstanceConfig{{URI: "mongodb://localhost:27017/admin"}}

		out, err := resolveRuntimeConfig(cfg, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if out.MongoEnabled {
			t.Fatal("mongo should stay disabled")
		}
	})
}

func strPtr(v string) *string { return &v }
