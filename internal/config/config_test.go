package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrecedenceFlagsThenEnvThenFile(t *testing.T) {
	t.Setenv(EnvHub, "")
	t.Setenv(EnvRegistry, "")
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
region = "from-file"
hub = "root@file-host"
registry = "file-registry"
`), 0o600)
	t.Setenv(EnvHome, home)
	t.Setenv(EnvRegion, "from-env")
	t.Setenv(EnvAPIKey, "key-from-env")

	c, err := Load(Flags{Region: "from-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Region != "from-flag" {
		t.Errorf("a flag must win: %s", c.Region)
	}
	if c.APIKey != "key-from-env" {
		t.Errorf("api key from env: %s", c.APIKey)
	}
	// Only the file supplies these, so the file is still consulted.
	if c.Hub != "root@file-host" || c.Registry != "file-registry" {
		t.Errorf("file values not read: %+v", c)
	}

	c2, err := Load(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if c2.Region != "from-env" {
		t.Errorf("env must beat file: %s", c2.Region)
	}
}

// Environment alone has to be sufficient, so the CLI needs no setup step.
func TestEnvironmentOnlyNeedsNoFile(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	t.Setenv(EnvHubEndpoint, "")
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvHub, "root@203.0.113.10")
	c, err := Load(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RequireAPIKey(); err != nil {
		t.Error(err)
	}
	if err := c.RequireHub(); err != nil {
		t.Error(err)
	}
	if c.Endpoint() != "203.0.113.10" {
		t.Errorf("endpoint should default to the SSH host, got %q", c.Endpoint())
	}
}

func TestMissingSettingsExplainTheFix(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	// Clear them explicitly: an operator's own environment would otherwise decide
	// whether this test passes.
	t.Setenv(EnvAPIKey, "")
	t.Setenv(EnvHub, "")
	c, _ := Load(Flags{})
	err := c.RequireAPIKey()
	if err == nil || !strings.Contains(err.Error(), EnvAPIKey) {
		t.Errorf("want the env var named: %v", err)
	}
	err = c.RequireHub()
	if err == nil || !strings.Contains(err.Error(), "hub add") {
		t.Errorf("want the remedy named: %v", err)
	}
}

func TestSSHTargetParsing(t *testing.T) {
	t.Setenv(EnvHubEndpoint, "")
	for _, tc := range []struct{ in, user, host, port string }{
		{"root@1.2.3.4", "root", "1.2.3.4", "22"},
		{"1.2.3.4", "root", "1.2.3.4", "22"},
		{"ubuntu@host.example:2222", "ubuntu", "host.example", "2222"},
	} {
		t.Setenv(EnvHome, t.TempDir())
		t.Setenv(EnvHub, tc.in)
		c, _ := Load(Flags{})
		u, h, p := c.SSHTarget()
		if u != tc.user || h != tc.host || p != tc.port {
			t.Errorf("%q -> %q %q %q, want %q %q %q", tc.in, u, h, p, tc.user, tc.host, tc.port)
		}
	}
}

func TestHubEndpointOverridesSSHHost(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	t.Setenv(EnvHub, "root@bastion.internal")
	t.Setenv(EnvHubEndpoint, "203.0.113.10")
	c, _ := Load(Flags{})
	if c.Endpoint() != "203.0.113.10" {
		t.Errorf("explicit endpoint should win, got %q", c.Endpoint())
	}
}

func TestSaveThenLoadNeedsNoEnvironment(t *testing.T) {
	t.Setenv(EnvHubEndpoint, "")
	t.Setenv(EnvRegistry, "")
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvHub, "root@1.2.3.4")
	c, _ := Load(Flags{Region: "aws-us-west-2"})
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv(EnvHub)
	t.Setenv(EnvHome, home)
	again, err := Load(Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Hub != "root@1.2.3.4" || again.Region != "aws-us-west-2" {
		t.Errorf("saved config not read back: %+v", again)
	}
}

func TestProfilesFromEnvList(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	t.Setenv(EnvProfiles, "local, debug ,")
	c, _ := Load(Flags{})
	if len(c.Profiles) != 2 || c.Profiles[0] != "local" || c.Profiles[1] != "debug" {
		t.Errorf("profiles: %q", c.Profiles)
	}
}
