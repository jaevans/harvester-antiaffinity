// Command harvester-antiaffinity live-migrates Harvester VMs so that VMs from
// the same Rancher machine pool are spread evenly across Harvester nodes.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jaevans/harvester-antiaffinity/internal/config"
	"github.com/jaevans/harvester-antiaffinity/internal/harvester"
	"github.com/jaevans/harvester-antiaffinity/internal/inventory"
	"github.com/jaevans/harvester-antiaffinity/internal/runner"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Parse(os.Args[1:], os.LookupEnv)
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	log.Info("starting", "version", version, "source", cfg.Source, "dryRun", cfg.DryRun)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r, err := build(cfg, log)
	if err != nil {
		return err
	}

	if cfg.Interval == 0 {
		res, err := r.RunOnce(ctx)
		logResult(log, res)
		return err
	}
	for {
		res, err := r.RunOnce(ctx)
		logResult(log, res)
		if err != nil {
			log.Error("pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.Interval):
		}
	}
}

func build(cfg *config.Config, log *slog.Logger) (*runner.Runner, error) {
	hvConfig, err := harvesterRESTConfig(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("harvester client config: %w", err)
	}
	hvClient, err := client.New(hvConfig, client.Options{Scheme: harvester.NewScheme()})
	if err != nil {
		return nil, fmt.Errorf("harvester client: %w", err)
	}

	filter := inventory.Filter{Include: cfg.IncludeClusters, Exclude: cfg.ExcludeClusters}
	var source inventory.Source
	switch cfg.Source {
	case config.SourceRancher:
		rc, err := rancherRESTConfig(cfg)
		if err != nil {
			return nil, err
		}
		dyn, err := dynamic.NewForConfig(rc)
		if err != nil {
			return nil, fmt.Errorf("rancher client: %w", err)
		}
		source = &inventory.RancherSource{Client: dyn, Namespace: cfg.RancherNamespace, Filter: filter}
	case config.SourceLabels:
		dyn, err := dynamic.NewForConfig(hvConfig)
		if err != nil {
			return nil, fmt.Errorf("harvester dynamic client: %w", err)
		}
		source = &inventory.LabelSource{Client: dyn, Filter: filter}
	}

	return &runner.Runner{
		Source:        source,
		Collector:     &harvester.Collector{Client: hvClient},
		Migrator:      &harvester.Migrator{Client: hvClient, Timeout: cfg.MigrationTimeout, PollInterval: cfg.PollInterval},
		Log:           log,
		MaxMigrations: cfg.MaxMigrations,
		MaxFailures:   cfg.MaxFailures,
		DryRun:        cfg.DryRun,
	}, nil
}

func harvesterRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
}

func rancherRESTConfig(cfg *config.Config) (*rest.Config, error) {
	token, err := cfg.Token()
	if err != nil {
		return nil, err
	}
	rc := &rest.Config{
		Host:        strings.TrimRight(cfg.RancherURL, "/") + cfg.RancherClusterPath,
		BearerToken: token,
		Timeout:     60 * time.Second,
		UserAgent:   "harvester-antiaffinity/" + version,
	}
	rc.Insecure = cfg.RancherInsecureSkipVerify
	if cfg.RancherCAFile != "" {
		rc.CAFile = cfg.RancherCAFile
	}
	return rc, nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

func logResult(log *slog.Logger, res runner.Result) {
	log.Info("pass complete",
		"planned", len(res.Planned),
		"succeeded", len(res.Succeeded),
		"failed", len(res.Failed),
		"skipped", res.Skipped)
}
