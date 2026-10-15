package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestDefaultsWithLabelsSource(t *testing.T) {
	c, err := Parse([]string{"--source=labels"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxMigrations != 10 || c.MaxFailures != 3 || c.MigrationTimeout != 30*time.Minute || c.Interval != 0 || c.DryRun {
		t.Fatalf("unexpected defaults %+v", c)
	}
	if c.RancherClusterPath != "/k8s/clusters/local" || c.RancherNamespace != "fleet-default" {
		t.Fatalf("unexpected rancher defaults %+v", c)
	}
}

func TestRancherRequiresURLAndToken(t *testing.T) {
	if _, err := Parse(nil, env(nil)); err == nil || !strings.Contains(err.Error(), "rancher-url") {
		t.Fatalf("expected rancher-url error, got %v", err)
	}
	if _, err := Parse([]string{"--rancher-url=https://r"}, env(nil)); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected token error, got %v", err)
	}
	if _, err := Parse([]string{"--rancher-url=https://r", "--rancher-token=t"}, env(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestEnvFallbackAndFlagPrecedence(t *testing.T) {
	c, err := Parse([]string{"--max-migrations=2"}, env(map[string]string{
		"RANCHER_URL":      "https://rancher",
		"RANCHER_TOKEN":    "tok",
		"MAX_MIGRATIONS":   "7",
		"DRY_RUN":          "true",
		"INTERVAL":         "3h",
		"EXCLUDE_CLUSTERS": "a, b,,",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxMigrations != 2 {
		t.Fatalf("flag should override env, got %d", c.MaxMigrations)
	}
	if !c.DryRun || c.Interval != 3*time.Hour || c.RancherURL != "https://rancher" {
		t.Fatalf("env not applied: %+v", c)
	}
	if !reflect.DeepEqual(c.ExcludeClusters, []string{"a", "b"}) {
		t.Fatalf("exclude = %v", c.ExcludeClusters)
	}
}

func TestEmptyEnvClearsStringDefault(t *testing.T) {
	c, err := Parse([]string{"--source=labels"}, env(map[string]string{"RANCHER_CLUSTER_PATH": "", "MAX_MIGRATIONS": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.RancherClusterPath != "" {
		t.Fatalf("empty env should clear the path, got %q", c.RancherClusterPath)
	}
	if c.MaxMigrations != 10 {
		t.Fatalf("empty numeric env should keep the default, got %d", c.MaxMigrations)
	}
}

func TestEmptyDryRunIsAnError(t *testing.T) {
	if _, err := Parse([]string{"--source=labels"}, env(map[string]string{"DRY_RUN": ""})); err == nil {
		t.Fatalf("blank DRY_RUN must not silently disable dry run")
	}
}

func TestBadEnvValues(t *testing.T) {
	for k, v := range map[string]string{"DRY_RUN": "maybe", "MAX_MIGRATIONS": "x", "INTERVAL": "soon", "RANCHER_INSECURE_SKIP_VERIFY": ""} {
		if _, err := Parse([]string{"--source=labels"}, env(map[string]string{k: v})); err == nil {
			t.Errorf("%s=%s should fail", k, v)
		}
	}
}

func TestValidation(t *testing.T) {
	for _, args := range [][]string{
		{"--source=nope"},
		{"--source=labels", "--migration-timeout=0s"},
		{"--source=labels", "--max-migrations=-1"},
		{"--source=labels", "--rancher-cluster-path=k8s/clusters/local"},
		{"--source=labels", "--rancher-ca-file=/ca", "--rancher-insecure-skip-verify"},
		{"--source=labels", "--settle-time=-1s"},
	} {
		if _, err := Parse(args, env(nil)); err == nil {
			t.Errorf("%v should fail validation", args)
		}
	}
}

func TestTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Parse([]string{"--rancher-url=https://r", "--rancher-token-file=" + path}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.Token()
	if err != nil || tok != "secret" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	c.RancherTokenFile = filepath.Join(t.TempDir(), "missing")
	if _, err := c.Token(); err == nil {
		t.Fatalf("missing token file should error")
	}
}
