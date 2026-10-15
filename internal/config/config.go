// Package config parses command-line flags, with environment variable
// fallbacks so the tool is easy to configure from a Kubernetes manifest.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	SourceRancher = "rancher"
	SourceLabels  = "labels"
)

// Config holds all runtime settings.
type Config struct {
	Source string

	RancherURL                string
	RancherToken              string
	RancherTokenFile          string
	RancherCAFile             string
	RancherInsecureSkipVerify bool
	// RancherClusterPath is appended to RancherURL to reach the management
	// cluster's Kubernetes API through Rancher's proxy.
	RancherClusterPath string
	RancherNamespace   string

	IncludeClusters []string
	ExcludeClusters []string

	Kubeconfig string

	DryRun           bool
	MaxMigrations    int
	MaxFailures      int
	MigrationTimeout time.Duration
	PollInterval     time.Duration
	// Interval > 0 runs forever, one pass per interval. 0 runs a single pass.
	Interval time.Duration

	LogLevel  string
	LogFormat string
}

// Parse reads flags from args, falling back to environment variables via
// lookupEnv for every flag (e.g. --max-migrations <- MAX_MIGRATIONS). A set but
// empty variable counts as set, so RANCHER_CLUSTER_PATH="" clears the default.
func Parse(args []string, lookupEnv func(string) (string, bool)) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("harvester-antiaffinity", flag.ContinueOnError)

	str := func(p *string, name, def, usage string) {
		if v, ok := lookupEnv(envName(name)); ok {
			def = v
		}
		fs.StringVar(p, name, def, usage+" (env "+envName(name)+")")
	}
	var errs []error
	boolean := func(p *bool, name string, def bool, usage string) {
		if v, ok := lookupEnv(envName(name)); ok && v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", envName(name), err))
			}
			def = b
		}
		fs.BoolVar(p, name, def, usage+" (env "+envName(name)+")")
	}
	integer := func(p *int, name string, def int, usage string) {
		if v, ok := lookupEnv(envName(name)); ok && v != "" {
			i, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", envName(name), err))
			}
			def = i
		}
		fs.IntVar(p, name, def, usage+" (env "+envName(name)+")")
	}
	duration := func(p *time.Duration, name string, def time.Duration, usage string) {
		if v, ok := lookupEnv(envName(name)); ok && v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", envName(name), err))
			}
			def = d
		}
		fs.DurationVar(p, name, def, usage+" (env "+envName(name)+")")
	}

	var include, exclude string
	str(&c.Source, "source", SourceRancher, "where group membership comes from: rancher or labels")
	str(&c.RancherURL, "rancher-url", "", "Rancher server URL, e.g. https://rancher.example.com")
	str(&c.RancherToken, "rancher-token", "", "Rancher API bearer token")
	str(&c.RancherTokenFile, "rancher-token-file", "", "file containing the Rancher API bearer token")
	str(&c.RancherCAFile, "rancher-ca-file", "", "PEM CA bundle for the Rancher server")
	boolean(&c.RancherInsecureSkipVerify, "rancher-insecure-skip-verify", false, "skip TLS verification for Rancher")
	str(&c.RancherClusterPath, "rancher-cluster-path", "/k8s/clusters/local", "path under the Rancher URL that proxies the management cluster API")
	str(&c.RancherNamespace, "rancher-namespace", "fleet-default", "namespace holding HarvesterMachine objects; empty for all")
	str(&include, "include-clusters", "", "comma-separated downstream clusters to balance (default all)")
	str(&exclude, "exclude-clusters", "", "comma-separated downstream clusters to ignore")
	str(&c.Kubeconfig, "kubeconfig", "", "kubeconfig for the Harvester cluster (default in-cluster)")
	boolean(&c.DryRun, "dry-run", false, "log the plan without migrating")
	integer(&c.MaxMigrations, "max-migrations", 10, "maximum migrations per pass, 0 for unlimited")
	integer(&c.MaxFailures, "max-failures", 3, "abort a pass after this many failed migrations, 0 for unlimited")
	duration(&c.MigrationTimeout, "migration-timeout", 30*time.Minute, "abort a single migration after this long")
	duration(&c.PollInterval, "poll-interval", 5*time.Second, "how often to poll migration status")
	duration(&c.Interval, "interval", 0, "run continuously with this interval between passes; 0 runs once and exits")
	str(&c.LogLevel, "log-level", "info", "debug, info, warn or error")
	str(&c.LogFormat, "log-format", "json", "json or text")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c.IncludeClusters = splitList(include)
	c.ExcludeClusters = splitList(exclude)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	switch c.Source {
	case SourceLabels:
	case SourceRancher:
		if c.RancherURL == "" {
			return errors.New("--rancher-url is required with --source=rancher")
		}
		if c.RancherToken == "" && c.RancherTokenFile == "" {
			return errors.New("--rancher-token or --rancher-token-file is required with --source=rancher")
		}
	default:
		return fmt.Errorf("unknown --source %q", c.Source)
	}
	if c.MigrationTimeout <= 0 {
		return errors.New("--migration-timeout must be positive")
	}
	if c.MaxMigrations < 0 || c.MaxFailures < 0 || c.Interval < 0 {
		return errors.New("--max-migrations, --max-failures and --interval must not be negative")
	}
	return nil
}

// Token returns the Rancher token, reading the token file if one is set.
func (c *Config) Token() (string, error) {
	if c.RancherTokenFile == "" {
		return c.RancherToken, nil
	}
	b, err := os.ReadFile(c.RancherTokenFile)
	if err != nil {
		return "", fmt.Errorf("reading rancher token file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func envName(flagName string) string {
	return strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
