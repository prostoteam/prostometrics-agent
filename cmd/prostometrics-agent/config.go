package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
	execcollector "github.com/prostoteam/prostometrics-agent/internal/collectors/exec"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/mongo"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/mysql"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/nginx"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/postgres"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/prom"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/rabbitmq"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/redis"
	"github.com/prostoteam/prostometrics-agent/internal/integrations"
)

const (
	configEnvVar      = "PROSTOMETRICS_CONFIG"
	defaultConfigName = "agent.yaml"

	// integrationRetryInterval is how long a failing instance is left alone before
	// the next attempt, so a database that is down does not turn into a
	// connection attempt every collection round.
	integrationRetryInterval = time.Minute
)

type fileConfig struct {
	EnvFiles     []string           `yaml:"env_files"`
	Agent        agentConfig        `yaml:"agent"`
	Integrations integrationsConfig `yaml:"integrations"`
}

type agentConfig struct {
	Workload *string `yaml:"workload"`
}

type integrationsConfig struct {
	Mongo      uriListConfig    `yaml:"mongo"`
	Nginx      nginxConfig      `yaml:"nginx"`
	Postgres   uriListConfig    `yaml:"postgres"`
	MySQL      mysqlConfig      `yaml:"mysql"`
	Redis      uriListConfig    `yaml:"redis"`
	RabbitMQ   rabbitmqConfig   `yaml:"rabbitmq"`
	Prometheus prometheusConfig `yaml:"prometheus"`
	Exec       execConfig       `yaml:"exec"`
}

// uriListConfig is the shape shared by every integration that connects to one or
// more instances by URI. An explicit label overrides the host derived from it,
// which matters when two instances live behind the same address.
type uriListConfig struct {
	Enabled   *bool               `yaml:"enabled"`
	Instances []uriInstanceConfig `yaml:"instances"`
}

type uriInstanceConfig struct {
	URI   string `yaml:"uri"`
	Label string `yaml:"label"`
}

type mysqlConfig struct {
	Enabled   *bool                 `yaml:"enabled"`
	Instances []mysqlInstanceConfig `yaml:"instances"`
}

type mysqlInstanceConfig struct {
	// DSN is the go-sql-driver form, "user:password@tcp(host:3306)/".
	DSN   string `yaml:"dsn"`
	Label string `yaml:"label"`
}

type rabbitmqConfig struct {
	Enabled   *bool                    `yaml:"enabled"`
	Instances []rabbitmqInstanceConfig `yaml:"instances"`
}

type rabbitmqInstanceConfig struct {
	// URL points at the management API, "http://user:password@127.0.0.1:15672".
	URL   string `yaml:"url"`
	Label string `yaml:"label"`
}

type prometheusConfig struct {
	Enabled *bool                    `yaml:"enabled"`
	Every   string                   `yaml:"every"`
	Targets []prometheusTargetConfig `yaml:"targets"`
}

type prometheusTargetConfig struct {
	Name      string   `yaml:"name"`
	URL       string   `yaml:"url"`
	Metrics   []string `yaml:"metrics"`
	Labels    []string `yaml:"labels"`
	MaxSeries int      `yaml:"max_series"`
}

type execConfig struct {
	Enabled  *bool               `yaml:"enabled"`
	Every    string              `yaml:"every"`
	Commands []execCommandConfig `yaml:"commands"`
}

type execCommandConfig struct {
	Metric  string            `yaml:"metric"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Kind    string            `yaml:"kind"`
	Labels  map[string]string `yaml:"labels"`
	Timeout string            `yaml:"timeout"`
}

type nginxConfig struct {
	Enabled  *bool  `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
	// AccessLog turns request outcomes and response times on. The status page
	// nginx exposes cannot report either, so this is the only way to see errors.
	AccessLog        string `yaml:"access_log"`
	AccessLogEnabled *bool  `yaml:"access_log_enabled"`
	// TimeSamplesPerTick bounds how many response times are reported each round,
	// which is what keeps a busy site's cost flat instead of proportional to traffic.
	TimeSamplesPerTick int `yaml:"time_samples_per_tick"`
}

type runtimeConfig struct {
	Workload       string
	MongoEnabled   bool
	MongoInstances []mongo.Instance
	NginxEnabled   bool
	NginxEndpoint  string

	NginxAccessLogEnabled bool
	NginxAccessLogPath    string
	NginxTimeSamples      int

	PostgresEnabled   bool
	PostgresInstances []postgres.Instance

	MySQLEnabled   bool
	MySQLInstances []mysql.Instance

	RedisEnabled   bool
	RedisInstances []redis.Instance

	RabbitMQEnabled   bool
	RabbitMQInstances []rabbitmq.Instance

	PrometheusEnabled bool
	PrometheusEvery   time.Duration
	PrometheusTargets []prom.Target

	ExecEnabled  bool
	ExecEvery    time.Duration
	ExecCommands []execcollector.Command
}

type configSource struct {
	Path     string
	Explicit bool
}

func resolveConfigSource(flagPath string) (configSource, []string, error) {
	if path := strings.TrimSpace(flagPath); path != "" {
		path = expandHome(path)
		return configSource{Path: path, Explicit: true}, []string{path}, nil
	}
	if envPath := strings.TrimSpace(os.Getenv(configEnvVar)); envPath != "" {
		envPath = expandHome(envPath)
		return configSource{Path: envPath, Explicit: true}, []string{envPath}, nil
	}
	paths := defaultConfigPaths()
	for _, path := range paths {
		if fileExists(path) {
			return configSource{Path: path}, paths, nil
		}
	}
	return configSource{}, paths, nil
}

func defaultConfigPaths() []string {
	var paths []string
	userPath := userConfigPath()
	systemPath := filepath.Join("/etc", "prostometrics", defaultConfigName)
	if os.Geteuid() == 0 {
		if systemPath != "" {
			paths = append(paths, systemPath)
		}
		if userPath != "" {
			paths = append(paths, userPath)
		}
		return paths
	}
	if userPath != "" {
		paths = append(paths, userPath)
	}
	if systemPath != "" {
		paths = append(paths, systemPath)
	}
	return paths
}

func userConfigPath() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "prostometrics", defaultConfigName)
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".config", "prostometrics", defaultConfigName)
}

func expandHome(path string) string {
	if path == "" {
		return path
	}
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func loadFileConfig(source configSource) (*fileConfig, error) {
	if strings.TrimSpace(source.Path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(source.Path)
	if err != nil {
		if os.IsNotExist(err) && !source.Explicit {
			return nil, nil
		}
		return nil, fmt.Errorf("read config %s: %w", source.Path, err)
	}
	cfg := &fileConfig{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", source.Path, err)
	}
	envMap, err := loadEnvFiles(cfg.EnvFiles)
	if err != nil {
		return nil, err
	}
	expandEnvStringsWithMap(cfg, envMap)
	return cfg, nil
}

func resolveRuntimeConfig(cfg *fileConfig, workloadFlag string, workloadFlagSet bool) (runtimeConfig, error) {
	workload, err := resolveWorkload(cfg, workloadFlag, workloadFlagSet)
	if err != nil {
		return runtimeConfig{}, err
	}
	out := runtimeConfig{Workload: workload}
	if cfg == nil {
		out.NginxEnabled = true
		out.NginxEndpoint = ""
		out.NginxAccessLogEnabled = true
		out.NginxAccessLogPath = nginx.DefaultAccessLogPath
		return out, nil
	}
	nginxEnabled := true
	if cfg.Integrations.Nginx.Enabled != nil && !*cfg.Integrations.Nginx.Enabled {
		nginxEnabled = false
	}
	if nginxEnabled {
		endpoint := cfg.Integrations.Nginx.Endpoint
		if strings.TrimSpace(endpoint) != "" {
			endpoint = nginx.NormalizeEndpoint(endpoint)
			if err := nginx.ValidateEndpoint(endpoint); err != nil {
				return runtimeConfig{}, fmt.Errorf("nginx endpoint: %w", err)
			}
		}
		out.NginxEnabled = true
		out.NginxEndpoint = endpoint

		// Access-log reading follows the nginx integration by default; the probe
		// skips it silently when the file is not readable, which is the normal
		// case for a user install.
		accessLogEnabled := true
		if cfg.Integrations.Nginx.AccessLogEnabled != nil {
			accessLogEnabled = *cfg.Integrations.Nginx.AccessLogEnabled
		}
		if accessLogEnabled {
			path := strings.TrimSpace(cfg.Integrations.Nginx.AccessLog)
			if path == "" {
				path = nginx.DefaultAccessLogPath
			}
			out.NginxAccessLogEnabled = true
			out.NginxAccessLogPath = path
			out.NginxTimeSamples = cfg.Integrations.Nginx.TimeSamplesPerTick
		}
	}

	if err := resolveInstanceIntegrations(cfg, &out); err != nil {
		return runtimeConfig{}, err
	}
	if err := resolveGenericIntegrations(cfg, &out); err != nil {
		return runtimeConfig{}, err
	}

	return out, nil
}

// resolveInstanceIntegrations handles everything configured as a list of
// instances. They share a shape: absent means off, present means on, and an
// instance without an explicit label is named after whatever it connects to.
func resolveInstanceIntegrations(cfg *fileConfig, out *runtimeConfig) error {
	if err := resolveInstances("mongo", cfg.Integrations.Mongo, uriConnection, uriLabel,
		func(uri string, label string) mongo.Instance { return mongo.Instance{URI: uri, Label: label} },
		&out.MongoEnabled, &out.MongoInstances); err != nil {
		return err
	}
	if err := resolveInstances("postgres", cfg.Integrations.Postgres, uriConnection, uriLabel,
		func(uri string, label string) postgres.Instance { return postgres.Instance{URI: uri, Label: label} },
		&out.PostgresEnabled, &out.PostgresInstances); err != nil {
		return err
	}
	if err := resolveInstances("redis", cfg.Integrations.Redis, uriConnection, uriLabel,
		func(uri string, label string) redis.Instance { return redis.Instance{URI: uri, Label: label} },
		&out.RedisEnabled, &out.RedisInstances); err != nil {
		return err
	}
	if err := resolveInstances("rabbitmq", cfg.Integrations.RabbitMQ, rabbitConnection, uriLabel,
		func(url string, label string) rabbitmq.Instance { return rabbitmq.Instance{URL: url, Label: label} },
		&out.RabbitMQEnabled, &out.RabbitMQInstances); err != nil {
		return err
	}
	// MySQL is the one exception: its connection string is a driver DSN rather
	// than a URL, so the host has to be read out of it differently.
	return resolveInstances("mysql", cfg.Integrations.MySQL, mysqlConnection, mysqlLabel,
		func(dsn string, label string) mysql.Instance { return mysql.Instance{DSN: dsn, Label: label} },
		&out.MySQLEnabled, &out.MySQLInstances)
}

// instanceList is the configuration shape every instance integration shares.
type instanceList[C any] interface {
	enabledFlag() *bool
	entries() []C
}

func (c uriListConfig) enabledFlag() *bool           { return c.Enabled }
func (c uriListConfig) entries() []uriInstanceConfig { return c.Instances }

func (c rabbitmqConfig) enabledFlag() *bool                { return c.Enabled }
func (c rabbitmqConfig) entries() []rabbitmqInstanceConfig { return c.Instances }

func (c mysqlConfig) enabledFlag() *bool             { return c.Enabled }
func (c mysqlConfig) entries() []mysqlInstanceConfig { return c.Instances }

func uriConnection(entry uriInstanceConfig) (string, string) { return entry.URI, entry.Label }

func rabbitConnection(entry rabbitmqInstanceConfig) (string, string) { return entry.URL, entry.Label }

func mysqlConnection(entry mysqlInstanceConfig) (string, string) { return entry.DSN, entry.Label }

func uriLabel(connection string) (string, error) {
	return integrations.InstanceLabelFromURI(connection)
}

func mysqlLabel(dsn string) (string, error) { return mysqlLabelFromDSN(dsn), nil }

// resolveInstances applies the shared rules once: a blank connection string is
// an error, an instance with no explicit label is named after what it connects
// to, and the integration is enabled only when it has something to connect to.
func resolveInstances[L instanceList[C], C any, T any](
	name string,
	cfg L,
	connection func(C) (string, string),
	derive func(string) (string, error),
	build func(string, string) T,
	enabled *bool,
	instances *[]T,
) error {
	entries := cfg.entries()
	on, err := integrationEnabled(name, cfg.enabledFlag(), len(entries))
	if err != nil || !on {
		return err
	}

	resolved := make([]T, 0, len(entries))
	for i, entry := range entries {
		raw, explicitLabel := connection(entry)
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return fmt.Errorf("%s.instances[%d] has no connection string", name, i)
		}
		label := strings.TrimSpace(explicitLabel)
		if label == "" {
			label, err = derive(trimmed)
			if err != nil {
				return fmt.Errorf("%s.instances[%d]: %w", name, i, err)
			}
		}
		resolved = append(resolved, build(trimmed, label))
	}

	*enabled = true
	*instances = resolved
	return nil
}

// resolveGenericIntegrations handles the two collectors that report whatever they
// are pointed at rather than a known service.
func resolveGenericIntegrations(cfg *fileConfig, out *runtimeConfig) error {
	promEnabled, err := integrationEnabled("prometheus", cfg.Integrations.Prometheus.Enabled, len(cfg.Integrations.Prometheus.Targets))
	if err != nil {
		return err
	}
	if promEnabled {
		every, err := parseEvery(cfg.Integrations.Prometheus.Every, agent.PrometheusEvery)
		if err != nil {
			return fmt.Errorf("prometheus.every: %w", err)
		}
		seen := make(map[string]struct{}, len(cfg.Integrations.Prometheus.Targets))
		targets := make([]prom.Target, 0, len(cfg.Integrations.Prometheus.Targets))
		for i, target := range cfg.Integrations.Prometheus.Targets {
			endpoint := strings.TrimSpace(target.URL)
			if endpoint == "" {
				return fmt.Errorf("prometheus.targets[%d].url is empty", i)
			}
			name := strings.TrimSpace(target.Name)
			if name == "" {
				derived, err := integrations.InstanceLabelFromURI(endpoint)
				if err != nil {
					return fmt.Errorf("prometheus.targets[%d]: %w", i, err)
				}
				name = derived
			}
			// The name becomes a label value that separates one target's series
			// from another's, so a duplicate would silently merge them.
			if _, exists := seen[name]; exists {
				return fmt.Errorf("prometheus.targets[%d]: duplicate target name %q", i, name)
			}
			seen[name] = struct{}{}
			targets = append(targets, prom.Target{
				Name:      name,
				URL:       endpoint,
				Metrics:   target.Metrics,
				Labels:    target.Labels,
				MaxSeries: target.MaxSeries,
			})
		}
		out.PrometheusEnabled = true
		out.PrometheusEvery = every
		out.PrometheusTargets = targets
	}

	execEnabled, err := integrationEnabled("exec", cfg.Integrations.Exec.Enabled, len(cfg.Integrations.Exec.Commands))
	if err != nil {
		return err
	}
	if execEnabled {
		every, err := parseEvery(cfg.Integrations.Exec.Every, agent.ExecEvery)
		if err != nil {
			return fmt.Errorf("exec.every: %w", err)
		}
		commands := make([]execcollector.Command, 0, len(cfg.Integrations.Exec.Commands))
		for i, command := range cfg.Integrations.Exec.Commands {
			metric := strings.TrimSpace(command.Metric)
			if metric == "" {
				return fmt.Errorf("exec.commands[%d].metric is empty", i)
			}
			path := strings.TrimSpace(command.Command)
			if path == "" {
				return fmt.Errorf("exec.commands[%d].command is empty", i)
			}
			kind := strings.ToLower(strings.TrimSpace(command.Kind))
			switch kind {
			case "", "value":
				kind = "value"
			case "counter":
			default:
				return fmt.Errorf("exec.commands[%d].kind %q is not value or counter", i, command.Kind)
			}
			timeout, err := parseEvery(command.Timeout, 0)
			if err != nil {
				return fmt.Errorf("exec.commands[%d].timeout: %w", i, err)
			}
			commands = append(commands, execcollector.Command{
				Metric:  metric,
				Path:    path,
				Args:    command.Args,
				Kind:    kind,
				Labels:  labelPairs(command.Labels),
				Timeout: timeout,
			})
		}
		out.ExecEnabled = true
		out.ExecEvery = every
		out.ExecCommands = commands
	}

	return nil
}

// integrationEnabled applies the rule every optional integration follows: it is
// off without entries, on with them, and asking for it explicitly while
// configuring nothing is a mistake worth reporting rather than a silent no-op.
func integrationEnabled(name string, enabled *bool, entries int) (bool, error) {
	if enabled != nil && !*enabled {
		return false, nil
	}
	if entries == 0 {
		if enabled != nil && *enabled {
			return false, fmt.Errorf("%s integration enabled but nothing configured", name)
		}
		return false, nil
	}
	return true, nil
}

// mysqlLabelFromDSN reads the address out of the go-sql-driver form, which is not
// a URL: "user:password@tcp(127.0.0.1:3306)/db".
func mysqlLabelFromDSN(dsn string) string {
	open := strings.IndexByte(dsn, '(')
	close := strings.IndexByte(dsn, ')')
	if open >= 0 && close > open {
		address := strings.TrimSpace(dsn[open+1 : close])
		if host, _, err := net.SplitHostPort(address); err == nil && host != "" {
			return host
		}
		if address != "" {
			return address
		}
	}
	return "mysql"
}

func parseEvery(raw string, fallback time.Duration) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, err
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%q is not a positive duration", raw)
	}
	return parsed, nil
}

func labelPairs(labels map[string]string) []string {
	if len(labels) == 0 {
		return nil
	}
	pairs := make([]string, 0, len(labels))
	for name, value := range labels {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || name == "workload" || value == "" {
			continue
		}
		pairs = append(pairs, name+"="+value)
	}
	// Sorted so the same configuration always produces the same series.
	sort.Strings(pairs)
	return pairs
}

func resolveWorkload(cfg *fileConfig, workloadFlag string, workloadFlagSet bool) (string, error) {
	if cfg != nil && cfg.Agent.Workload != nil {
		workload := strings.TrimSpace(*cfg.Agent.Workload)
		if workload == "" {
			return "", errors.New("prostometrics: workload is empty")
		}
		return workload, nil
	}
	workload := strings.TrimSpace(workloadFlag)
	if workloadFlagSet {
		if workload == "" {
			return "", errors.New("prostometrics: workload is empty")
		}
		return workload, nil
	}
	if workload != "" {
		return workload, nil
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("prostometrics: workload not set and hostname lookup failed: %w", err)
	}
	workload = strings.TrimSpace(host)
	if workload == "" {
		return "", errors.New("prostometrics: workload not set and hostname is empty")
	}
	return workload, nil
}

func expandEnvStringsWithMap(v any, env map[string]string) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return
	}
	expandEnvValueWithMap(rv.Elem(), env)
}

func expandEnvValueWithMap(v reflect.Value, env map[string]string) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(os.Expand(v.String(), func(key string) string {
				if env != nil {
					if val, ok := env[key]; ok {
						return val
					}
				}
				return os.Getenv(key)
			}))
		}
	case reflect.Ptr:
		if !v.IsNil() {
			expandEnvValueWithMap(v.Elem(), env)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if !field.CanSet() && field.Kind() == reflect.String {
				continue
			}
			expandEnvValueWithMap(field, env)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			expandEnvValueWithMap(v.Index(i), env)
		}
	case reflect.Map:
		// Map values are not addressable, so each one is expanded into a copy and
		// written back. Without this a "${VAR}" in a map field would reach the
		// collector unexpanded, unlike every other string in the file.
		if v.IsNil() || v.Type().Elem().Kind() != reflect.String {
			return
		}
		for _, key := range v.MapKeys() {
			expanded := reflect.New(v.Type().Elem()).Elem()
			expanded.Set(v.MapIndex(key))
			expandEnvValueWithMap(expanded, env)
			v.SetMapIndex(key, expanded)
		}
	}
}

func loadEnvFiles(paths []string) (map[string]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	env := make(map[string]string)
	for _, rawPath := range paths {
		path := expandHome(strings.TrimSpace(rawPath))
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read env file %s: %w", path, err)
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "export ") {
				line = strings.TrimSpace(line[len("export "):])
			} else if strings.HasPrefix(line, "export\t") {
				line = strings.TrimSpace(line[len("export\t"):])
			}
			eq := strings.IndexByte(line, '=')
			if eq <= 0 {
				return nil, fmt.Errorf("env file %s:%d: expected KEY=VALUE", path, i+1)
			}
			key := strings.TrimSpace(line[:eq])
			val := strings.TrimSpace(line[eq+1:])
			if key == "" {
				return nil, fmt.Errorf("env file %s:%d: empty key", path, i+1)
			}
			if len(val) >= 2 {
				if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
					val = val[1 : len(val)-1]
				}
			}
			env[key] = val
		}
	}
	return env, nil
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
