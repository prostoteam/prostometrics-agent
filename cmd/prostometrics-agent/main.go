package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prostoteam/prostometrics-agent/internal/agent"
	"github.com/prostoteam/prostometrics-agent/internal/agent/catalog"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/mongo"
	"github.com/prostoteam/prostometrics-agent/internal/collectors/nginx"
	prostometrics "github.com/prostoteam/prostometrics-go"
)

const prostometricsIngestHost = "prostometrics.ru"

type stringFlag struct {
	value string
	set   bool
}

func (f *stringFlag) String() string { return f.value }

func (f *stringFlag) Set(v string) error {
	f.value = v
	f.set = true
	return nil
}

func main() {
	var verbose bool
	var workloadFlag stringFlag
	var configPath string
	flag.Var(&workloadFlag, "workload", "workload scope sent with every ingest request")
	flag.Var(&workloadFlag, "w", "shorthand for --workload")
	flag.StringVar(&configPath, "config", "", "path to YAML config (optional)")
	flag.StringVar(&configPath, "c", "", "shorthand for --config")
	flag.BoolVar(&verbose, "verbose", false, "enable verbose logging")
	flag.BoolVar(&verbose, "v", false, "shorthand for --verbose")
	flag.Parse()

	endpointHost := prostometricsIngestHost
	if envEndpoint := strings.TrimSpace(os.Getenv("PROSTOMETRICS_ENDPOINT")); envEndpoint != "" {
		endpointHost = envEndpoint
	} else if envHost := strings.TrimSpace(os.Getenv("PROSTOMETRICS_HOST")); envHost != "" {
		endpointHost = envHost
	}

	endpoint := prostometrics.EndpointFromHost(endpointHost)
	apiKey := os.Getenv("PROSTOMETRICS_API_KEY")

	configSource, configPaths, err := resolveConfigSource(configPath)
	if err != nil {
		log.Fatalf("prostometrics: config resolution failed: %v", err)
	}
	logConfigPaths(configPaths)
	fileCfg, err := loadFileConfig(configSource)
	if err != nil {
		log.Fatalf("prostometrics: config load failed: %v", err)
	}
	runtimeCfg, err := resolveRuntimeConfig(fileCfg, workloadFlag.value, workloadFlag.set)
	if err != nil {
		log.Fatalf("prostometrics: config invalid: %v", err)
	}
	if configSource.Path != "" {
		log.Printf("prostometrics: config loaded from %s", configSource.Path)
	} else {
		log.Printf("prostometrics: no config found, using defaults")
	}
	if err := initClient(runtimeCfg.Workload, endpoint, apiKey, verbose); err != nil {
		log.Fatalf("prostometrics: init failed: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	core := catalog.CoreCollectors()
	probes := catalog.IntegrationProbes()
	if runtimeCfg.MongoEnabled {
		probes = append(probes, mongo.NewProbe(runtimeCfg.MongoInstances, agent.MongoEvery, mongoRetryInterval))
	}
	if runtimeCfg.NginxEnabled {
		probes = append(probes, nginx.NewProbe(runtimeCfg.NginxEndpoint, agent.NginxEvery))
	}
	agent.Run(ctx, core, probes)
	flushAndClose()
}

func initClient(workload string, endpoint string, apiKey string, verbose bool) error {
	if apiKey == "" {
		return errors.New("PROSTOMETRICS_API_KEY is required")
	}
	_, err := prostometrics.Init(workload, prostometrics.Config{
		Endpoint: endpoint,
		APIKey:   apiKey,
		Verbose:  verbose,
	})
	if errors.Is(err, prostometrics.ErrMissingAPIKey) {
		return errors.New("PROSTOMETRICS_API_KEY is required")
	}
	return err
}

func flushAndClose() {
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	if client := prostometrics.Default(); client != nil {
		_ = client.Close(flushCtx)
	}
}

func logConfigPaths(paths []string) {
	if len(paths) == 0 {
		log.Printf("prostometrics: config search paths: (none)")
		return
	}
	log.Printf("prostometrics: config search paths: %s", strings.Join(paths, ", "))
}
