package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRun stands in for exec: it records the command line and answers from a
// script, so no test needs a real keychain.
type fakeRun struct {
	calls   [][]string
	replies map[string]string // joined argv -> stdout
	err     error
}

func (f *fakeRun) run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.err != nil {
		return nil, f.err
	}
	for k, v := range f.replies {
		if strings.Contains(key, k) {
			return []byte(v), nil
		}
	}
	return nil, fmt.Errorf("no scripted reply for %q", key)
}

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestTokenStore_EnvWinsOverEverything(t *testing.T) {
	dir := t.TempDir()
	run := &fakeRun{replies: map[string]string{"find-generic-password": "from-keychain\n"}}
	store := &TokenStore{
		Service: "herdr-huddle", Account: "github.com", FallbackDir: dir,
		GOOS: "darwin", Run: run.run,
		Getenv: envFrom(map[string]string{"GH_TOKEN": "from-env"}),
	}
	got, backend, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "from-env" || backend != "env" {
		t.Errorf("got (%q, %q), want (from-env, env)", got, backend)
	}
	if len(run.calls) != 0 {
		t.Errorf("env token should short-circuit the keychain, but ran %v", run.calls)
	}
}

func TestTokenStore_GitHubTokenEnvAlsoAccepted(t *testing.T) {
	store := &TokenStore{
		Service: "s", Account: "a", FallbackDir: t.TempDir(), GOOS: "linux",
		Getenv: envFrom(map[string]string{"GITHUB_TOKEN": "from-github-env"}),
		Run:    (&fakeRun{}).run,
	}
	got, backend, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "from-github-env" || backend != "env" {
		t.Errorf("got (%q, %q)", got, backend)
	}
}

func TestTokenStore_MacOSKeychain(t *testing.T) {
	run := &fakeRun{replies: map[string]string{"security find-generic-password": "gho_from_keychain\n"}}
	store := &TokenStore{
		Service: "herdr-huddle", Account: "github.com", FallbackDir: t.TempDir(),
		GOOS: "darwin", Run: run.run, Getenv: envFrom(nil),
	}
	got, backend, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "gho_from_keychain" || backend != "keychain" {
		t.Errorf("got (%q, %q), want (gho_from_keychain, keychain)", got, backend)
	}
	want := []string{"security", "find-generic-password", "-s", "herdr-huddle", "-a", "github.com", "-w"}
	if strings.Join(run.calls[0], " ") != strings.Join(want, " ") {
		t.Errorf("ran %v, want %v", run.calls[0], want)
	}
}

func TestTokenStore_LinuxSecretService(t *testing.T) {
	run := &fakeRun{replies: map[string]string{"secret-tool lookup": "gho_linux\n"}}
	store := &TokenStore{
		Service: "herdr-huddle", Account: "github.com", FallbackDir: t.TempDir(),
		GOOS: "linux", Run: run.run, Getenv: envFrom(nil),
	}
	got, backend, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "gho_linux" || backend != "keychain" {
		t.Errorf("got (%q, %q)", got, backend)
	}
	want := []string{"secret-tool", "lookup", "service", "herdr-huddle", "account", "github.com"}
	if strings.Join(run.calls[0], " ") != strings.Join(want, " ") {
		t.Errorf("ran %v, want %v", run.calls[0], want)
	}
}

// This machine has no Secret Service at all, so the fallback is not
// hypothetical: it is the path that will actually run on the devbox.
func TestTokenStore_FallsBackToFileWhenKeychainUnavailable(t *testing.T) {
	dir := t.TempDir()
	run := &fakeRun{err: errors.New("exec: \"secret-tool\": executable file not found in $PATH")}
	store := &TokenStore{
		Service: "herdr-huddle", Account: "github.com", FallbackDir: dir,
		GOOS: "linux", Run: run.run, Getenv: envFrom(nil),
	}

	backend, err := store.Save(context.Background(), "gho_file_token")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if backend != "file" {
		t.Errorf("Save backend = %q, want file", backend)
	}

	info, err := os.Stat(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatalf("token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}

	got, backend, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "gho_file_token" || backend != "file" {
		t.Errorf("got (%q, %q), want (gho_file_token, file)", got, backend)
	}

	if err := store.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "token")); !os.IsNotExist(err) {
		t.Errorf("token file survived Delete: %v", err)
	}
}

func TestTokenStore_NoTokenAnywhere(t *testing.T) {
	store := &TokenStore{
		Service: "herdr-huddle", Account: "github.com", FallbackDir: t.TempDir(),
		GOOS: "linux", Run: (&fakeRun{err: errors.New("not found")}).run, Getenv: envFrom(nil),
	}
	if _, _, err := store.Load(context.Background()); !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
}

// An empty keychain reply (entry exists but no password) must not be mistaken
// for a token.
func TestTokenStore_EmptyKeychainValueIsNotAToken(t *testing.T) {
	run := &fakeRun{replies: map[string]string{"secret-tool lookup": "\n"}}
	store := &TokenStore{
		Service: "herdr-huddle", Account: "github.com", FallbackDir: t.TempDir(),
		GOOS: "linux", Run: run.run, Getenv: envFrom(nil),
	}
	if _, _, err := store.Load(context.Background()); !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
}

// A whitespace-only token saved by mistake must never come back as a usable
// credential.
func TestTokenStore_RejectsEmptyTokenOnSave(t *testing.T) {
	store := &TokenStore{
		Service: "s", Account: "a", FallbackDir: t.TempDir(), GOOS: "linux",
		Run: (&fakeRun{err: errors.New("missing")}).run, Getenv: envFrom(nil),
	}
	if _, err := store.Save(context.Background(), "   \n"); err == nil {
		t.Fatal("Save must reject a blank token")
	}
}

func TestScopesFor(t *testing.T) {
	tests := []struct {
		name    string
		private bool
		want    []string
	}{
		{"public repo asks for the narrow scope", false, []string{"public_repo"}},
		{"private repo needs the broad scope", true, []string{"repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScopesFor(tt.private)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("ScopesFor(%v) = %v, want %v", tt.private, got, tt.want)
			}
		})
	}
}

// A keychain that reports "not found" for delete, or whose delete fails, must
// not let the operator be told the token was removed while it is still there.
// `security` and `secret-tool` disagree on how to report "not found", so the
// exit code alone cannot be trusted.
func TestTokenStore_DeleteFailsLoudlyIfTokenSurvives(t *testing.T) {
	// Only "lookup" is scripted, so the delete command fails; lookup still
	// returns a token, so the token survived.
	run := &fakeRun{replies: map[string]string{"secret-tool lookup": "gho_still_here\n"}}
	store := &TokenStore{
		Service: "herdr-huddle", Account: "github.com", FallbackDir: t.TempDir(),
		GOOS: "linux", Run: run.run, Getenv: envFrom(nil),
	}
	err := store.Delete(context.Background())
	if err == nil {
		t.Fatal("Delete must not report success while a token is still retrievable")
	}
	if !strings.Contains(err.Error(), "still present") {
		t.Errorf("error should say the token survived, got: %v", err)
	}
}

// Saving to the keychain must remove a fallback copy, or a rotated token stays
// readable on disk.
func TestTokenStore_SaveToKeychainRemovesFallbackFile(t *testing.T) {
	dir := t.TempDir()

	// First save: no keychain, so it lands in the file.
	noKeychain := &fakeRun{err: errors.New("exec: secret-tool: not found")}
	fileStore := &TokenStore{
		Service: "herdr-huddle", Account: "github.com", FallbackDir: dir,
		GOOS: "linux", Run: noKeychain.run, Getenv: envFrom(nil),
	}
	if backend, err := fileStore.Save(context.Background(), "gho_old"); err != nil || backend != "file" {
		t.Fatalf("first Save = (%q, %v), want (file, nil)", backend, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "token")); err != nil {
		t.Fatalf("expected a fallback file: %v", err)
	}

	// Second save: the keychain is available now.
	withKeychain := &fakeRun{replies: map[string]string{
		"secret-tool store":  "",
		"secret-tool lookup": "gho_new\n",
	}}
	kcStore := &TokenStore{
		Service: "herdr-huddle", Account: "github.com", FallbackDir: dir,
		GOOS: "linux", Run: withKeychain.run, Getenv: envFrom(nil),
	}
	if backend, err := kcStore.Save(context.Background(), "gho_new"); err != nil || backend != "keychain" {
		t.Fatalf("second Save = (%q, %v), want (keychain, nil)", backend, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "token")); !os.IsNotExist(err) {
		t.Errorf("the superseded token file survived a keychain save: %v", err)
	}
	if got, backend, _ := kcStore.Load(context.Background()); got != "gho_new" || backend != "keychain" {
		t.Errorf("Load = (%q, %q), want (gho_new, keychain)", got, backend)
	}
}
