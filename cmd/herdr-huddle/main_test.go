package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// splitRepo feeds an API path, so a malformed slug must be rejected rather
// than escaped and hoped for.
func TestSplitRepo(t *testing.T) {
	valid := map[string][2]string{
		"acme/demo":          {"acme", "demo"},
		" acme/demo ":        {"acme", "demo"},
		"DnzzL/herdr-huddle": {"DnzzL", "herdr-huddle"},
	}
	for in, want := range valid {
		owner, repo, err := splitRepo(in)
		if err != nil {
			t.Errorf("splitRepo(%q) errored: %v", in, err)
			continue
		}
		if owner != want[0] || repo != want[1] {
			t.Errorf("splitRepo(%q) = (%q, %q), want (%q, %q)", in, owner, repo, want[0], want[1])
		}
	}

	for _, in := range []string{"", "demo", "acme/", "/demo", "a/b/c", "acme/demo/x"} {
		if _, _, err := splitRepo(in); err == nil {
			t.Errorf("splitRepo(%q) must fail", in)
		}
	}
}

// The plugin's hooks and the CLI must agree on where the token lives, or a
// login would be invisible to the poller.
func TestConfigDirPrefersHerdrPluginConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", dir)
	if got := configDir(); got != dir {
		t.Errorf("configDir() = %q, want %q", got, dir)
	}
}

func TestConfigDirFallsBackToUserConfigDir(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	if got := configDir(); !strings.HasSuffix(got, serviceName) {
		t.Errorf("configDir() = %q, want it to end with %q", got, serviceName)
	}
}

// The client id is build configuration; the environment override is what makes
// the binary usable in development without a rebuild.
func TestResolveClientID(t *testing.T) {
	t.Setenv("HERDR_HUDDLE_CLIENT_ID", "  Iv1.fromenv  ")
	if got := resolveClientID(); got != "Iv1.fromenv" {
		t.Errorf("resolveClientID() = %q, want the trimmed env value", got)
	}

	t.Setenv("HERDR_HUDDLE_CLIENT_ID", "")
	saved := clientID
	clientID = " Iv1.baked "
	t.Cleanup(func() { clientID = saved })
	if got := resolveClientID(); got != "Iv1.baked" {
		t.Errorf("resolveClientID() = %q, want the trimmed baked-in value", got)
	}
	clientID = ""
	if got := resolveClientID(); got != "" {
		t.Errorf("resolveClientID() = %q, want empty when nothing is configured", got)
	}
}

// An unknown command must be a usage error, not a silent success.
func TestRunRejectsUnknownCommand(t *testing.T) {
	if err := run([]string{"nonsense"}); err == nil {
		t.Fatal("run must fail for an unknown command")
	}
	if err := run(nil); err == nil {
		t.Fatal("run must fail with no arguments")
	}
	if err := run([]string{"help"}); err != nil {
		t.Fatalf("run(help) = %v, want nil", err)
	}
}

// The token file must land in the config directory the plugin also uses.
func TestDefaultStoreUsesConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", dir)
	store := defaultStore()
	if store.FallbackDir != dir {
		t.Errorf("FallbackDir = %q, want %q", store.FallbackDir, dir)
	}
	if store.Service != serviceName {
		t.Errorf("Service = %q, want %q", store.Service, serviceName)
	}
	if _, err := os.Stat(filepath.Join(dir, "token")); err == nil {
		t.Error("constructing the store must not create a token file")
	}
}
