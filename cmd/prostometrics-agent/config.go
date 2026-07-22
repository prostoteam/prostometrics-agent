package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/prostoteam/prostometrics-agent/internal/collectors/mongo"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/nginx"
	"github.com/prostoteam/prostometrics-agent/internal/integrations"
)

const (
	configEnvVar       = "PROSTOMETRICS_CONFIG"
	defaultConfigName  = "agent.yaml"
	mongoRetryInterval = time.Minute
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
	Mongo mongoConfig `yaml:"mongo"`
	Nginx nginxConfig `yaml:"nginx"`
}

type mongoConfig struct {
	Enabled   *bool                 `yaml:"enabled"`
	Instances []mongoInstanceConfig `yaml:"instances"`
}

type mongoInstanceConfig struct {
	URI string `yaml:"uri"`
}

type nginxConfig struct {
	Enabled  *bool  `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

type runtimeConfig struct {
	Workload       string
	MongoEnabled   bool
	MongoInstances []mongo.Instance
	NginxEnabled   bool
	NginxEndpoint  string
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
		return out, nil
	}
	mongoEnabled := true
	if cfg.Integrations.Mongo.Enabled != nil && !*cfg.Integrations.Mongo.Enabled {
		mongoEnabled = false
	}
	if mongoEnabled && len(cfg.Integrations.Mongo.Instances) == 0 {
		if cfg.Integrations.Mongo.Enabled != nil && *cfg.Integrations.Mongo.Enabled {
			return runtimeConfig{}, errors.New("mongo integration enabled but no instances configured")
		}
		mongoEnabled = false
	}
	if mongoEnabled {
		instances := make([]mongo.Instance, 0, len(cfg.Integrations.Mongo.Instances))
		for i, inst := range cfg.Integrations.Mongo.Instances {
			uri := strings.TrimSpace(inst.URI)
			if uri == "" {
				return runtimeConfig{}, fmt.Errorf("mongo.instances[%d].uri is empty", i)
			}
			label, err := integrations.InstanceLabelFromURI(uri)
			if err != nil {
				return runtimeConfig{}, fmt.Errorf("mongo.instances[%d]: %w", i, err)
			}
			instances = append(instances, mongo.Instance{
				URI:   uri,
				Label: label,
			})
		}
		out.MongoEnabled = true
		out.MongoInstances = instances
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
	}
	return out, nil
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
