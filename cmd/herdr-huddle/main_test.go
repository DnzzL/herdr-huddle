package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/github"
	"github.com/DnzzL/herdr-huddle/internal/poll"
	"github.com/DnzzL/herdr-huddle/internal/repo"
	"github.com/DnzzL/herdr-huddle/internal/share"
	"github.com/DnzzL/herdr-huddle/internal/thread"
)

// captureStdout collects what a command prints, which is how an operator reads
// it and therefore what a test has to look at.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	saved := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()

	fn()

	w.Close()
	os.Stdout = saved
	return <-done
}

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

// The poller only knows what to drive from what is on disk, and this is the only
// place the share's origin is written. It is tested through the store because
// the parts each had a test while the wiring between them did not: `share`
// recorded the repository and the pull request and dropped the agent, so every
// share opened on this machine was retired on the poller's first pass and the
// thread never received a transcript.
func TestRecordShareKeepsTheAgentItIsBoundTo(t *testing.T) {
	store := share.Store{Path: filepath.Join(t.TempDir(), "shares.json")}
	result := share.Result{
		Repo:        repo.Slug{Host: "github.com", Owner: "DnzzL", Name: "molkky"},
		Branch:      "herdr/molkky",
		Base:        "master",
		PullRequest: github.PullRequest{Number: 13, HTMLURL: "https://github.com/DnzzL/molkky/pull/13"},
		Allowlist:   []string{"DnzzL"},
	}
	origin := share.Origin{
		Agent: "pi", PaneID: "wQ:p1", CWD: "/home/thomas/Projects/molkky",
		Kind: "pi", Session: "/home/thomas/.pi/agent/sessions/s.jsonl", SessionID: "01a0c4d1",
	}

	now := time.Date(2026, 9, 21, 18, 33, 42, 0, time.UTC)
	if _, _, err := recordShare(store, result, origin, now); err != nil {
		t.Fatalf("recordShare: %v", err)
	}

	states, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("recorded %d shares, want 1", len(states))
	}
	got := states[0]
	if got.Origin != origin {
		t.Errorf("Origin = %+v, want %+v", got.Origin, origin)
	}
	if !got.Active() {
		t.Errorf("the share is not active: %+v", got)
	}
}

// Re-running `share` from the pane is how a retired share is picked back up: the
// record keeps where the sync got to and stops being retired. Without this, a
// share that was opened from outside a pane — or before the fix above — could
// never be repaired without deleting its state by hand.
func TestRecordShareRevivesARetiredShare(t *testing.T) {
	store := share.Store{Path: filepath.Join(t.TempDir(), "shares.json")}
	result := share.Result{
		Repo:        repo.Slug{Host: "github.com", Owner: "DnzzL", Name: "molkky"},
		Branch:      "herdr/molkky",
		Base:        "master",
		PullRequest: github.PullRequest{Number: 13},
	}
	origin := share.Origin{Agent: "pi", PaneID: "wQ:p1", Kind: "pi"}
	now := time.Date(2026, 9, 21, 18, 33, 42, 0, time.UTC)

	retired := time.Date(2026, 9, 21, 18, 38, 26, 0, time.UTC)
	dead := share.State{
		Repo: "DnzzL/molkky", Branch: "herdr/molkky", Base: "master", Number: 13,
		Cursors:   share.Cursors{Comment: 7, ETag: `W/"abc"`},
		RetiredAt: &retired, CreatedAt: now, UpdatedAt: retired,
	}
	if err := store.Put(dead); err != nil {
		t.Fatal(err)
	}

	state, resumed, err := recordShare(store, result, origin, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("recordShare: %v", err)
	}
	if !resumed {
		t.Error("resumed = false, want the existing thread picked up rather than created")
	}
	if !state.Active() {
		t.Errorf("the share is still retired: %+v", state)
	}
	// What was already delivered must not be delivered again, and what was
	// already posted must not be posted again.
	if state.Cursors.Comment != 7 || state.Cursors.ETag != `W/"abc"` {
		t.Errorf("cursors = %+v, want the delivered ones kept", state.Cursors)
	}
	states, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("recorded %d shares, want 1", len(states))
	}
	if states[0].Origin.PaneID != "wQ:p1" {
		t.Errorf("Origin = %+v, want the pane it was re-opened from", states[0].Origin)
	}
}

func TestEffectiveIntervalReportsTheDefaultItWillUse(t *testing.T) {
	// The banner must not claim "0s" when the default is doing the work.
	if got := effectiveInterval(0); got != poll.DefaultInterval {
		t.Errorf("effectiveInterval(0) = %s, want the default %s", got, poll.DefaultInterval)
	}
	if got := effectiveInterval(3 * time.Second); got != 3*time.Second {
		t.Errorf("effectiveInterval(3s) = %s, want 3s", got)
	}
}

func TestSeedTranscriptStartsAtTheEndOfTheSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	records := `{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"}}
{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}
`
	if err := os.WriteFile(path, []byte(records), 0o600); err != nil {
		t.Fatal(err)
	}

	// Opening a share must not dump the conversation the operator has already
	// had into the thread.
	if got := seedTranscript("claude", path); got != "a1" {
		t.Errorf("seedTranscript = %q, want the last record's uuid", got)
	}
	if got := seedTranscript("claude", ""); got != "" {
		t.Errorf("seedTranscript(\"\") = %q, want the empty cursor", got)
	}
	if got := seedTranscript("claude", filepath.Join(t.TempDir(), "gone.jsonl")); got != "" {
		t.Errorf("seedTranscript of a missing file = %q, want the empty cursor so the poller decides", got)
	}
}

func TestPrintPollResult(t *testing.T) {
	var buf bytes.Buffer
	printPollResult(&buf, poll.Result{
		Posted:   []string{"herdr/x: https://github.com/a/b/pull/1#issuecomment-2"},
		Injected: []string{"herdr/x: @bob"},
		Refused:  []thread.Refusal{{Author: "mallory", Reason: "not on this share's allowlist"}},
		Retired:  []string{"herdr/y"},
		Warnings: []string{"herdr/x: the thread could not be read"},
	})
	got := buf.String()
	for _, want := range []string{"posted", "delivered", "refused", "mallory", "retired", "warning"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q does not mention %q", got, want)
		}
	}

	buf.Reset()
	printPollResult(&buf, poll.Result{})
	if !strings.Contains(buf.String(), "nothing to do") {
		t.Errorf("empty result printed %q, want it to say so", buf.String())
	}
}

func TestLazyForgeNeedsNoTokenUntilItIsUsed(t *testing.T) {
	// A poller with nothing to do must not fail on a machine where nobody has
	// logged in, and a daemon that started before `auth login` must start
	// working after it rather than needing a restart.
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())

	forge := &lazyForge{store: defaultStore()}
	if _, err := forge.load(context.Background()); err == nil {
		t.Fatal("want an error when there is no token anywhere")
	}
	if forge.client != nil {
		t.Error("a failed load left a client behind, so a later login would not be picked up")
	}

	t.Setenv("GH_TOKEN", "ghp_from_the_environment")
	client, err := forge.load(context.Background())
	if err != nil {
		t.Fatalf("load after a token appeared: %v", err)
	}
	if client.Token != "ghp_from_the_environment" {
		t.Errorf("token = %q, want the one that appeared", client.Token)
	}
}

func TestLocalOriginWithoutAPane(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "")
	origin, warnings := localOrigin(context.Background())
	if origin.PaneID != "" {
		t.Errorf("origin = %+v, want none", origin)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not running inside a Herdr pane") {
		t.Errorf("warnings = %v, want one explaining that nothing will be synced", warnings)
	}
}

func TestLocalOriginReportsAPaneWithNoAgent(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "wQ:p999")
	origin, warnings := localOrigin(context.Background())
	if origin.PaneID != "" {
		t.Errorf("origin = %+v, want none for a pane that does not exist", origin)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "wQ:p999") {
		t.Errorf("warnings = %v, want one naming the pane", warnings)
	}
}

// TestLocalOriginAgainstTheLiveAgent is the real thing: it is what the share
// record is populated from, so a wrong field here is a poller that drives
// nothing. It self-skips outside a Herdr pane.
func TestLocalOriginAgainstTheLiveAgent(t *testing.T) {
	pane := os.Getenv("HERDR_PANE_ID")
	if os.Getenv("HERDR_ENV") != "1" || pane == "" {
		t.Skip("not running inside a Herdr pane")
	}

	origin, warnings := localOrigin(context.Background())
	for _, warning := range warnings {
		t.Logf("warning: %s", warning)
	}
	if origin.PaneID != pane {
		t.Errorf("PaneID = %q, want the pane the share was opened in (%q)", origin.PaneID, pane)
	}
	if origin.Agent == "" {
		t.Error("Agent is empty, so the thread would not say which agent it is watching")
	}
	if origin.CWD == "" {
		t.Error("CWD is empty, so a new session could not be found by working directory")
	}
	t.Logf("origin: agent=%q pane=%q cwd=%q session=%q partial=%v", origin.Agent, origin.PaneID, origin.CWD, origin.Session, origin.Partial)
}

func TestRunPollOnceWithNothingToDo(t *testing.T) {
	// No shares and no token: a one-shot poll must say so rather than demand a
	// login for work that does not exist.
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	out := captureStdout(t, func() {
		if err := runPoll([]string{"--once"}); err != nil {
			t.Fatalf("runPoll: %v", err)
		}
	})
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("output = %q, want it to report that there was nothing to do", out)
	}
}

func TestRunPollRejectsPositionalArguments(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())
	err := runPoll([]string{"extra"})
	if !errors.Is(err, errUsage) {
		t.Errorf("err = %v, want a usage error", err)
	}
}

func TestSeedTranscriptOfAPiSession(t *testing.T) {
	// The seed is where the share's first comment starts, so it has to be read
	// with the adapter that matches the file.
	path := filepath.Join(t.TempDir(), "2026-09-18T09-00-00-000Z_s1.jsonl")
	records := `{"type":"session","version":3,"id":"s1","cwd":"/work"}
{"type":"message","id":"a1","parentId":"p","message":{"role":"assistant","content":[{"type":"text","text":"already said"}]}}
`
	if err := os.WriteFile(path, []byte(records), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := seedTranscript("pi", path); got != "a1" {
		t.Errorf("seedTranscript = %q, want pi's last record id", got)
	}
	// The wrong adapter has no records it understands, so a seed taken with it
	// would silently be empty and the share would replay the conversation.
	if got := seedTranscript("claude", path); got != "s1" {
		t.Logf("claude adapter seeded %q (its last id-bearing record); the point is that pi's seed is not empty", got)
	}
}

// `serve` takes both its pane and its allowlist from the same share record:
// the allowlist is the gate for a public endpoint (ADR-006), so a stream with
// no share behind it has nothing to gate with and must not start.
func TestShareForServe(t *testing.T) {
	write := func(t *testing.T, states ...share.State) share.Store {
		t.Helper()
		store := share.Store{Path: filepath.Join(t.TempDir(), "shares.json")}
		for _, state := range states {
			if err := store.Put(state); err != nil {
				t.Fatal(err)
			}
		}
		return store
	}
	retired := func(s share.State) share.State {
		at := time.Now()
		s.RetiredAt = &at
		return s
	}

	live := share.State{
		Repo: "acme/demo", Branch: "herdr/demo",
		Origin:    share.Origin{Agent: "pi", PaneID: "w16:p1"},
		Allowlist: []string{"DnzzL", "pagbrl"},
	}
	other := share.State{
		Repo: "acme/other", Branch: "herdr/other",
		Origin:    share.Origin{Agent: "pi", PaneID: "wP:p1"},
		Allowlist: []string{"aymericdo"},
	}

	t.Run("one active share yields pane and allowlist", func(t *testing.T) {
		state, err := shareForServe(write(t, retired(share.State{
			Repo: "acme/old", Branch: "herdr/old", Origin: share.Origin{PaneID: "w1:p1"},
		}), live), "")
		if err != nil {
			t.Fatalf("shareForServe errored: %v", err)
		}
		if state.Origin.PaneID != "w16:p1" {
			t.Errorf("pane = %q, want w16:p1", state.Origin.PaneID)
		}
		if strings.Join(state.Allowlist, ",") != "DnzzL,pagbrl" {
			t.Errorf("allowlist = %v, want the share's", state.Allowlist)
		}
	})

	t.Run("no shares at all", func(t *testing.T) {
		_, err := shareForServe(share.Store{Path: filepath.Join(t.TempDir(), "none.json")}, "")
		if err == nil {
			t.Error("serving with nothing shared must fail, not stream an ungateable pane")
		}
	})

	t.Run("only retired shares", func(t *testing.T) {
		_, err := shareForServe(write(t, retired(live)), "")
		if err == nil {
			t.Error("a retired share must not be served: its agent is gone")
		}
	})

	t.Run("several active shares are ambiguous", func(t *testing.T) {
		_, err := shareForServe(write(t, live, other), "")
		if err == nil {
			t.Fatal("with two active shares the pane is ambiguous, so it must fail")
		}
		if !strings.Contains(err.Error(), "--pane") {
			t.Errorf("error = %v, want it to name the --pane escape hatch", err)
		}
	})

	t.Run("--pane picks the share", func(t *testing.T) {
		state, err := shareForServe(write(t, live, other), "wP:p1")
		if err != nil {
			t.Fatalf("shareForServe errored: %v", err)
		}
		if state.Origin.PaneID != "wP:p1" || strings.Join(state.Allowlist, ",") != "aymericdo" {
			t.Errorf("got pane %q allowlist %v, want the share bound to that pane", state.Origin.PaneID, state.Allowlist)
		}
	})

	t.Run("--pane matching no share", func(t *testing.T) {
		_, err := shareForServe(write(t, live), "w99:p9")
		if err == nil {
			t.Error("a pane no share is bound to has no allowlist, so it must fail")
		}
	})

	t.Run("an active share with no pane", func(t *testing.T) {
		_, err := shareForServe(write(t, share.State{Repo: "acme/demo", Branch: "herdr/demo"}), "")
		if err == nil {
			t.Error("a share opened outside a pane has nothing to stream")
		}
	})
}

// The door is the operator answering a question at their own terminal, so the
// question and the answer are the whole contract: anything but a plain yes is
// a no, and nobody to ask is an error rather than a yes.
func TestTerminalApproverReadsTheOperatorsAnswer(t *testing.T) {
	cases := []struct {
		name    string
		typed   string
		want    bool
		wantErr bool
	}{
		{"y", "y\n", true, false},
		{"yes", "yes\n", true, false},
		{"upper case", "Y\n", true, false},
		{"with whitespace", "  y  \n", true, false},
		{"n", "n\n", false, false},
		{"just enter is a no", "\n", false, false},
		{"anything else is a no", "maybe\n", false, false},
		{"an answer with no newline still counts", "y", true, false},
		{"nobody there is not a yes", "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var asked bytes.Buffer
			approver := &terminalApprover{in: bufio.NewReader(strings.NewReader(c.typed)), out: &asked}

			got, err := approver.Approve(context.Background(), "collaborator")
			if (err != nil) != c.wantErr {
				t.Fatalf("Approve error = %v, wantErr %v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("Approve = %v, want %v", got, c.want)
			}
			if !strings.Contains(asked.String(), "@collaborator") {
				t.Errorf("the question %q does not name who is asking", asked.String())
			}
		})
	}
}

// Which door `serve` opens is decided by two flags and nothing else, and the
// wrong default here would be an unattended public pane.
func TestDoorFor(t *testing.T) {
	state := share.State{Repo: "acme/demo", Branch: "herdr/a", Allowlist: []string{"operator"}}
	store := share.Store{Path: filepath.Join(t.TempDir(), "shares.json")}

	knock := doorFor(store, state, false, false, nil)
	if knock.Admit == nil || knock.Open {
		t.Error("the default door must knock")
	}
	open := doorFor(store, state, true, false, nil)
	if !open.Open || open.Admit != nil {
		t.Error("--open must admit without an approver")
	}
	closed := doorFor(store, state, false, true, nil)
	if closed.Open || closed.Admit != nil {
		t.Error("--closed must have no way to admit anyone new")
	}
}

// An admission has to reach the file the poller reads, or the collaborator is
// in the huddle and their comments are still refused.
func TestRememberJoinerWritesTheAllowlist(t *testing.T) {
	store := share.Store{Path: filepath.Join(t.TempDir(), "shares.json")}
	state := share.State{Repo: "acme/demo", Branch: "herdr/a", Allowlist: []string{"operator"}}
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}

	if err := rememberJoiner(store, state.Key())("collaborator"); err != nil {
		t.Fatalf("remembering an admitted joiner: %v", err)
	}
	states, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := states[0].Allowlist; len(got) != 2 || got[1] != "collaborator" {
		t.Errorf("allowlist = %v, want the operator and the admitted joiner", got)
	}
}
