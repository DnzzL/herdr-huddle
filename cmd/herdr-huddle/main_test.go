package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DnzzL/herdr-huddle/internal/github"
	"github.com/DnzzL/herdr-huddle/internal/repo"
	"github.com/DnzzL/herdr-huddle/internal/share"
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

// The share output is the whole of what the operator sees, so its wording is the
// interface. A "created" branch that was reused, or a warning that got dropped,
// changes what they do next.
func TestPrintShareResult(t *testing.T) {
	res := share.Result{
		Repo:          repo.Slug{Host: "github.com", Owner: "acme", Name: "demo"},
		Slug:          "improve-pane",
		Branch:        "herdr/improve-pane",
		Base:          "main",
		BaseRef:       "origin/main",
		BranchCreated: true,
		Pushed:        true,
		PullRequest:   github.PullRequest{Number: 42, HTMLURL: "https://github.com/acme/demo/pull/42", Draft: true},
		Allowlist:     []string{"operator", "bob"},
		Warnings:      []string{"could not invite carol: needs admin rights"},
	}
	var buf bytes.Buffer
	printShareResult(&buf, res)
	got := buf.String()

	for _, want := range []string{
		"acme/demo",
		"herdr/improve-pane (created)",
		"main",
		"origin/main",
		"#42",
		"https://github.com/acme/demo/pull/42",
		"draft",
		"created",
		"operator bob",
		"could not invite carol",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q:\n%s", want, got)
		}
	}

	// A reused share must not read as a new one: the difference is whether the
	// operator expects a second thread to exist.
	reused := res
	reused.BranchCreated = false
	reused.Reused = true
	buf.Reset()
	printShareResult(&buf, reused)
	if !strings.Contains(buf.String(), "(reused)") {
		t.Errorf("a reused share reads as new:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "(created)") {
		t.Errorf("a reused share claims to be created:\n%s", buf.String())
	}

	// A dry run reports no pull request, so the line must not print "#0" against
	// a URL that does not exist.
	dry := res
	dry.DryRun = true
	dry.PullRequest = github.PullRequest{}
	buf.Reset()
	printShareResult(&buf, dry)
	if strings.Contains(buf.String(), "#0") {
		t.Errorf("a dry run printed a pull request number:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "would be opened") {
		t.Errorf("a dry run does not say what would happen:\n%s", buf.String())
	}
}

// The invite flag is repeatable and also accepts a comma-separated list.
func TestStringListFlag(t *testing.T) {
	var l stringList
	for _, v := range []string{"bob", "@carol,dave", " ", "bob"} {
		if err := l.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	want := []string{"bob", "@carol", "dave", "bob"}
	if len(l) != len(want) {
		t.Fatalf("got %v, want %v", l, want)
	}
	for i := range want {
		if l[i] != want[i] {
			t.Errorf("l[%d] = %q, want %q", i, l[i], want[i])
		}
	}
	if l.String() != strings.Join(want, ",") {
		t.Errorf("String() = %q", l.String())
	}
}

// A dry run needs no token, so it must not be blocked by a missing one: it is the
// command an operator runs before authorizing anything.
func TestRunShareDryRunNeedsNoToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())

	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q", "-b", "work")
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "i")
	git("remote", "add", "origin", "git@github.com:acme/demo.git")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	if err := runShare([]string{"--dry-run"}); err != nil {
		t.Fatalf("a dry run must not require a token: %v", err)
	}
	// And it must not have recorded a share for the poller.
	store := share.Store{Path: filepath.Join(os.Getenv("HERDR_PLUGIN_CONFIG_DIR"), "shares.json")}
	if states, err := store.Load(); err != nil || len(states) != 0 {
		t.Errorf("a dry run recorded shares: %v, %v", states, err)
	}
}
