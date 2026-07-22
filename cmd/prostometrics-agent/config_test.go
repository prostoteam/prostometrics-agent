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
