package share

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/repo"
)

func testStore(t *testing.T) Store {
	t.Helper()
	return Store{Path: filepath.Join(t.TempDir(), "nested", "shares.json")}
}

func state(repo, branch string, number int) State {
	return State{
		Repo:      repo,
		Branch:    branch,
		Base:      "main",
		Number:    number,
		URL:       "https://github.com/" + repo + "/pull/" + itoa(number),
		Allowlist: []string{"bob"},
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Before the first share there is no file. That is not a failure, and reporting
// it as one would make every command that reads state check for it first.
func TestStore_LoadMissingFileIsEmpty(t *testing.T) {
	states, err := testStore(t).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("got %d states, want none", len(states))
	}
}

// Put creates the directory, which on a fresh machine does not exist yet.
func TestStore_PutCreatesTheDirectoryAndRoundTrips(t *testing.T) {
	store := testStore(t)
	want := state("acme/demo", "herdr/improve-pane", 42)
	if err := store.Put(want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d states, want 1", len(got))
	}
	if got[0].Key() != want.Key() || got[0].Number != 42 || got[0].Allowlist[0] != "bob" {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
	// The state holds PR numbers and logins rather than secrets, but it lives in
	// a config directory and there is no reason to let anyone else read it.
	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// Re-sharing is the normal case (ADR-002 makes it idempotent), so the record is
// replaced, not appended. Two records for one thread would mean the poller
// watching it twice.
func TestStore_PutReplacesRatherThanDuplicates(t *testing.T) {
	store := testStore(t)
	if err := store.Put(state("acme/demo", "herdr/x", 1)); err != nil {
		t.Fatal(err)
	}
	// A second share of the same branch: this time the pull request is #7, and
	// CreatedAt is preserved because the record was replaced rather than made.
	second := state("acme/demo", "herdr/x", 7)
	second.CreatedAt = time.Time{}
	if err := store.Put(second); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d states, want 1 after replacing", len(got))
	}
	if got[0].Number != 7 {
		t.Errorf("Number = %d, want the replacement (7)", got[0].Number)
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("CreatedAt was lost when the record was replaced")
	}
}

func TestStore_HoldsSeveralShares(t *testing.T) {
	store := testStore(t)
	for _, st := range []State{
		state("acme/demo", "herdr/b", 2),
		state("acme/demo", "herdr/a", 1),
		state("acme/other", "herdr/a", 3),
	} {
		if err := store.Put(st); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d states, want 3", len(got))
	}
	// Sorted, so nothing downstream depends on map order.
	keys := []string{got[0].Key(), got[1].Key(), got[2].Key()}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Errorf("keys are not sorted: %v", keys)
		}
	}
}

func TestStore_Delete(t *testing.T) {
	store := testStore(t)
	if err := store.Put(state("acme/demo", "herdr/a", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(state("acme/demo", "herdr/b", 2)); err != nil {
		t.Fatal(err)
	}
	found, err := store.Delete("acme/demo#herdr/a")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !found {
		t.Error("found = false for a share that was there")
	}
	got, _ := store.Load()
	if len(got) != 1 || got[0].Branch != "herdr/b" {
		t.Errorf("got %+v, want only herdr/b", got)
	}
	if found, err := store.Delete("acme/demo#herdr/a"); err != nil || found {
		t.Errorf("deleting twice: found = %v, err = %v, want false and nil", found, err)
	}
}

// A corrupt file must be loud. Treating it as empty would silently stop the
// poller for every share, with nothing for the operator to go on.
func TestStore_CorruptFileIsAnError(t *testing.T) {
	store := testStore(t)
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load succeeded on a corrupt file, want an error")
	}
	if err := store.Put(state("acme/demo", "herdr/a", 1)); err == nil {
		t.Fatal("Put overwrote a corrupt file, want an error so nothing is lost silently")
	}
}

// The write is atomic, so a reader never sees a half-written list and a failure
// leaves no debris in the config directory.
func TestStore_WriteLeavesNoTemporaryFiles(t *testing.T) {
	store := testStore(t)
	for i := 0; i < 5; i++ {
		if err := store.Put(state("acme/demo", "herdr/a", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(store.Path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "shares.json" {
		t.Errorf("directory contains %v, want only shares.json", names)
	}
}

func TestState_KeyIsUsableAsAnIdentifier(t *testing.T) {
	st := state("acme/demo", "herdr/improve-pane", 1)
	if strings.ContainsAny(st.Key(), "\n\t") {
		t.Errorf("Key() = %q contains whitespace", st.Key())
	}
	if st.Key() != "acme/demo#herdr/improve-pane" {
		t.Errorf("Key() = %q", st.Key())
	}
}

// The record is what the poller reads, so its field names are a compatibility
// surface. Renaming one silently stops an existing install from working.
func TestState_JSONFieldNamesAreStable(t *testing.T) {
	raw, err := json.Marshal(state("acme/demo", "herdr/x", 1))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"repo"`, `"branch"`, `"base"`, `"number"`, `"url"`, `"allowlist"`, `"created_at"`, `"updated_at"`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("encoded state is missing %s: %s", field, raw)
		}
	}
}

func TestRecord_NewShareStartsAtTheEndOfTheSession(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "shares.json")}
	now := time.Now()

	fresh := FromResult(Result{Repo: repo.Slug{Host: "github.com", Owner: "acme", Name: "demo"}, Branch: "herdr/x"}, Origin{}, now)
	got, resumed, err := store.Record(fresh, "uuid-at-the-end")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if resumed {
		t.Error("resumed = true, want false for a share that did not exist")
	}
	// ADR-003: a first share does not replay the conversation the operator
	// already had.
	if got.Cursors.Transcript != "uuid-at-the-end" {
		t.Errorf("Transcript cursor = %q, want the seeded end of the session", got.Cursors.Transcript)
	}
	if got.Cursors.Comment != 0 {
		t.Errorf("Comment cursor = %d, want 0 so existing instructions are read", got.Cursors.Comment)
	}
}

func TestRecord_ReSharingKeepsTheCursors(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "shares.json")}
	now := time.Now()

	base := FromResult(Result{Repo: repo.Slug{Host: "github.com", Owner: "acme", Name: "demo"}, Branch: "herdr/x"}, Origin{}, now)
	base.Origin = Origin{PaneID: "wQ:p1", Session: "/home/t/.claude/projects/-x/s.jsonl", Agent: "claude"}
	base.Cursors = Cursors{Transcript: "t9", Comment: 42, Snapshot: "abc", ETag: `W/"1"`, Blocked: true}
	if _, _, err := store.Record(base, "seed"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	stored, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("got %d records, want 1", len(stored))
	}

	// The operator re-runs `share` to pick the thread back up.
	again := FromResult(Result{Repo: repo.Slug{Host: "github.com", Owner: "acme", Name: "demo"}, Branch: "herdr/x"}, Origin{}, now.Add(time.Hour))
	again.Origin = base.Origin
	got, resumed, err := store.Record(again, "a-brand-new-seed")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !resumed {
		t.Error("resumed = false, want true for an existing share")
	}
	if got.Cursors != stored[0].Cursors {
		t.Errorf("Cursors = %+v, want the existing ones %+v", got.Cursors, stored[0].Cursors)
	}
	if got.Cursors.Transcript == "a-brand-new-seed" {
		t.Error("re-sharing reseeded the transcript cursor, so the thread would repeat turns")
	}
	states, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Errorf("re-sharing left %d records, want 1", len(states))
	}
}

func TestRecord_ANewSessionKeepsTheInstructionCursor(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "shares.json")}
	now := time.Now()

	base := FromResult(Result{Repo: repo.Slug{Host: "github.com", Owner: "acme", Name: "demo"}, Branch: "herdr/x"}, Origin{}, now)
	base.Origin = Origin{PaneID: "wQ:p1", Session: "/home/t/.claude/projects/-x/old.jsonl"}
	base.Cursors = Cursors{Transcript: "old-9", Comment: 42, Snapshot: "abc", ETag: `W/"1"`}
	if _, _, err := store.Record(base, "seed"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// The agent restarted, so the same share now points at a new session.
	again := FromResult(Result{Repo: repo.Slug{Host: "github.com", Owner: "acme", Name: "demo"}, Branch: "herdr/x"}, Origin{}, now.Add(time.Hour))
	again.Origin = Origin{PaneID: "wQ:p1", Session: "/home/t/.claude/projects/-x/new.jsonl"}
	got, _, err := store.Record(again, "new-seed")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	if got.Cursors.Transcript != "new-seed" {
		t.Errorf("Transcript cursor = %q, want the new session's end", got.Cursors.Transcript)
	}
	// Instructions already carried out are not carried out again.
	if got.Cursors.Comment != 42 || got.Cursors.ETag != `W/"1"` {
		t.Errorf("Cursors = %+v, want the instruction cursor and ETag kept", got.Cursors)
	}
	// Neither is terminal content that is already in the thread: identical
	// content is a duplicate whatever session is behind it.
	if got.Cursors.Snapshot != "abc" {
		t.Errorf("Snapshot = %q, want the posted snapshot kept", got.Cursors.Snapshot)
	}
}

func TestRecord_RefusesAnEmptyKey(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "shares.json")}
	if _, _, err := store.Record(State{}, ""); err == nil {
		t.Fatal("want an error rather than a record no share can ever match")
	}
}

func TestRecord_RevivesARetiredShare(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "shares.json")}
	now := time.Now()
	retired := now.Add(-time.Hour)

	base := FromResult(Result{Repo: repo.Slug{Host: "github.com", Owner: "acme", Name: "demo"}, Branch: "herdr/x"}, Origin{}, now)
	base.RetiredAt = &retired
	if err := store.Put(base); err != nil {
		t.Fatal(err)
	}

	again := FromResult(Result{Repo: repo.Slug{Host: "github.com", Owner: "acme", Name: "demo"}, Branch: "herdr/x"}, Origin{}, now)
	got, _, err := store.Record(again, "seed")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !got.Active() {
		t.Error("the record is still retired, so the poller would never serve it again")
	}
}
