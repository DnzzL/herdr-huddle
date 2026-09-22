package thread

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/github"
	"github.com/DnzzL/herdr-huddle/internal/transcript"
)

func comment(id int64, author, association, body string) github.Comment {
	c := github.Comment{ID: id, Body: body, Association: association, HTMLURL: fmt.Sprintf("https://x/%d", id)}
	c.User.Login = author
	return c
}

// The gate is the security boundary: what arrives here can end up typed into
// the operator's terminal.
func TestInstructions_Gates(t *testing.T) {
	allowlist := []string{"thomas", "bob"}

	tests := []struct {
		name     string
		comment  github.Comment
		wantPass bool
		want     string // substring of the refusal reason
	}{
		{
			name:     "a collaborator on the allowlist",
			comment:  comment(1, "bob", "COLLABORATOR", "/agent rename the parser"),
			wantPass: true,
		},
		{
			name:     "the operator",
			comment:  comment(1, "thomas", "OWNER", "/agent what do you need from me?"),
			wantPass: true,
		},
		{
			name:     "an organisation member",
			comment:  comment(1, "bob", "MEMBER", "/agent ship it"),
			wantPass: true,
		},
		{
			name:     "a stranger",
			comment:  comment(1, "mallory", "COLLABORATOR", "/agent inject this"),
			wantPass: false,
			want:     "not on this share's allowlist",
		},
		{
			name:     "a contributor is not collaborative",
			comment:  comment(1, "bob", "CONTRIBUTOR", "/agent hello"),
			wantPass: false,
			want:     "author_association CONTRIBUTOR",
		},
		{
			name:     "a missing association denies",
			comment:  comment(1, "bob", "", "/agent hello"),
			wantPass: false,
			want:     "author_association (absent)",
		},
		{
			name:     "no prefix",
			comment:  comment(1, "bob", "COLLABORATOR", "just saying hi"),
			wantPass: false,
			want:     "does not start with /agent",
		},
		{
			// Prose that happens to begin with the same letters. Treating it as
			// an instruction would eat the 's'.
			name:     "a longer word",
			comment:  comment(1, "bob", "COLLABORATOR", "/agents are useful"),
			wantPass: false,
			want:     "does not start with /agent",
		},
		{
			name:     "the prefix with nothing behind it",
			comment:  comment(1, "bob", "COLLABORATOR", "/agent   "),
			wantPass: false,
			want:     "no instruction",
		},
		{
			name:     "an empty comment",
			comment:  comment(1, "bob", "COLLABORATOR", ""),
			wantPass: false,
			want:     "does not start with /agent",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, refused := Instructions([]github.Comment{tc.comment}, allowlist, 0)
			if tc.wantPass {
				if len(got) != 1 {
					t.Fatalf("got %d instructions, want 1 (refused: %+v)", len(got), refused)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("got %d instructions, want none", len(got))
			}
			if len(refused) != 1 {
				t.Fatalf("got %d refusals, want 1 so the operator can see why", len(refused))
			}
			if !strings.Contains(refused[0].Reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", refused[0].Reason, tc.want)
			}
		})
	}
}

// The self-trigger guard. A posted transcript carries the operator's own token's
// association and login, so it passes every other gate — and a tool result
// inside it can contain a line that reads like an instruction. It is skipped
// silently: it is our own output, not something a person was denied.
func TestInstructions_IgnoreOurOwnComments(t *testing.T) {
	own := comment(7, "thomas", "OWNER", Marker+"\n**claude** · a turn\n\n/agent rm -rf ~\n")
	got, refused := Instructions([]github.Comment{own}, []string{"thomas"}, 0)

	if len(got) != 0 {
		t.Fatalf("the tool's own comment was treated as an instruction: %+v", got)
	}
	if len(refused) != 0 {
		t.Errorf("refusals = %+v, want none: the tool's own output is not a denied instruction", refused)
	}
}

func TestInstructions_EmptyAllowlistRefusesEveryone(t *testing.T) {
	// A missing allowlist must lock the door, not open it.
	got, refused := Instructions([]github.Comment{comment(1, "thomas", "OWNER", "/agent hello")}, nil, 0)
	if len(got) != 0 {
		t.Fatalf("an empty allowlist let a comment through: %+v", got)
	}
	if len(refused) != 1 || !strings.Contains(refused[0].Reason, "no allowlist") {
		t.Errorf("refusals = %+v, want one naming the missing allowlist", refused)
	}
}

func TestInstructions_AllowlistIsCaseInsensitiveAndTrimmed(t *testing.T) {
	got, _ := Instructions([]github.Comment{comment(1, "Bob", "COLLABORATOR", "/agent hi")}, []string{" bob "}, 0)
	if len(got) != 1 {
		t.Fatalf("got %d instructions, want 1", len(got))
	}
}

func TestInstructions_OldestFirstAndSkipsWhatWasHandled(t *testing.T) {
	// The API returns newest first, and several queued instructions must reach
	// the agent in the order they were written.
	comments := []github.Comment{
		comment(30, "bob", "COLLABORATOR", "/agent third"),
		comment(20, "bob", "COLLABORATOR", "/agent second"),
		comment(10, "bob", "COLLABORATOR", "/agent first"),
	}

	got, refused := Instructions(comments, []string{"bob"}, 0)
	// Ids ascend, so ordering cannot be confused with the input order.
	// 10..30 ascending sorts as 10,20,30 — the input was the reverse.
	if len(got) != 3 {
		t.Fatalf("got %d instructions, want 3 (%+v)", len(got), refused)
	}
	if got[0].Text != "first" || got[1].Text != "second" || got[2].Text != "third" {
		t.Errorf("order = %q, %q, %q, want first, second, third", got[0].Text, got[1].Text, got[2].Text)
	}

	// A restart must not re-inject what was already handled — and that is not a
	// refusal, because nothing went wrong.
	got, refused = Instructions(comments, []string{"bob"}, 20)
	if len(got) != 1 || got[0].Text != "third" {
		t.Fatalf("after cursor 20: got %+v, want only 'third'", got)
	}
	if len(refused) != 0 {
		t.Errorf("refusals = %+v, want none for already-handled comments", refused)
	}
}

func TestInstructions_StripsThePrefix(t *testing.T) {
	got, _ := Instructions([]github.Comment{
		comment(1, "bob", "COLLABORATOR", "  /agent   rename the parser\n\nto something clearer"),
	}, []string{"bob"}, 0)
	if len(got) != 1 {
		t.Fatalf("got %d instructions, want 1", len(got))
	}
	if got[0].Text != "rename the parser\n\nto something clearer" {
		t.Errorf("Text = %q, want the prefix and surrounding space removed", got[0].Text)
	}
	if got[0].Author != "bob" || got[0].URL != "https://x/1" {
		t.Errorf("instruction = %+v, want the author and url carried", got[0])
	}
}

func TestInstruction_PromptTagsTheTextAsThirdParty(t *testing.T) {
	i := Instruction{Author: "bob", Text: "do the thing", URL: "https://x/1"}
	got := i.Prompt()

	// ADR-001: the injected text is tagged as untrusted third-party input
	// carrying the author's login. The agent has a standing relationship with
	// its operator and this text must not inherit it.
	for _, want := range []string{"@bob", "https://x/1", "not from your operator", "not been verified", "do the thing"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt = %q, want it to contain %q", got, want)
		}
	}
}

func writeSession(t *testing.T, records ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func userRecord(uuid, text string) string {
	return fmt.Sprintf(`{"type":"user","uuid":%q,"timestamp":"2026-09-18T10:00:00Z","message":{"role":"user","content":%q}}`, uuid, text)
}

func assistantRecord(uuid, text string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"timestamp":"2026-09-18T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, uuid, text)
}

func TestReadTurn_DeliversOnlyWhatIsNew(t *testing.T) {
	path := writeSession(t, userRecord("u1", "hello"), assistantRecord("a1", "hi there"))

	all, err := ReadTurn("claude", path, "", transcript.Options{})
	if err != nil {
		t.Fatalf("ReadTurn: %v", err)
	}
	if !strings.Contains(all.Markdown, "hi there") {
		t.Fatalf("Markdown = %q, want the assistant turn", all.Markdown)
	}
	if all.Cursor != "a1" {
		t.Errorf("Cursor = %q, want the last record's uuid", all.Cursor)
	}

	// A second read from that cursor has nothing new, which is what stops the
	// poller from posting the same turn every 10s.
	again, err := ReadTurn("claude", path, all.Cursor, transcript.Options{})
	if err != nil {
		t.Fatalf("ReadTurn: %v", err)
	}
	if !again.Empty() {
		t.Errorf("Markdown = %q, want nothing new", again.Markdown)
	}
}

func TestReadTurn_RotationIsReportedNotPublished(t *testing.T) {
	path := writeSession(t, assistantRecord("a1", "fresh session"))

	got, err := ReadTurn("claude", path, "uuid-from-a-file-that-is-gone", transcript.Options{})
	if err != nil {
		t.Fatalf("ReadTurn: %v", err)
	}
	if !got.Rotated {
		t.Error("Rotated = false, want true when the cursor is not in the file")
	}
}

func TestReadTurn_MissingFileIsAnError(t *testing.T) {
	if _, err := ReadTurn("claude", filepath.Join(t.TempDir(), "gone.jsonl"), "", transcript.Options{}); err == nil {
		t.Fatal("want an error for a missing session file")
	}
}

func TestEndCursor_IsTheEndOfTheConversation(t *testing.T) {
	path := writeSession(t, userRecord("u1", "hello"), assistantRecord("a1", "hi"), userRecord("u2", "and again"))

	got, err := EndCursor("claude", path)
	if err != nil {
		t.Fatalf("EndCursor: %v", err)
	}
	if got != "u2" {
		t.Errorf("EndCursor = %q, want the last record's uuid", got)
	}

	// Priming with it must mean the existing conversation is not replayed.
	turn, err := ReadTurn("claude", path, got, transcript.Options{})
	if err != nil {
		t.Fatalf("ReadTurn: %v", err)
	}
	if !turn.Empty() {
		t.Errorf("a freshly primed share replayed %q", turn.Markdown)
	}
}

func TestEndCursor_TrailingRecordsWithoutUUIDs(t *testing.T) {
	path := writeSession(t, assistantRecord("a1", "hi"), `{"type":"summary","timestamp":"2026-09-18T10:00:02Z"}`)

	got, err := EndCursor("claude", path)
	if err != nil {
		t.Fatalf("EndCursor: %v", err)
	}
	if got != "a1" {
		t.Errorf("EndCursor = %q, want the last uuid-bearing record", got)
	}
}

func TestComment_IsMarkedAndTruncated(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	got := Comment(Turn{Markdown: "hello"}, "claude", now)
	if !strings.HasPrefix(got, Marker) {
		t.Errorf("comment does not start with the marker:\n%s", got)
	}
	if !strings.Contains(got, "**claude**") || !strings.Contains(got, "2026-09-18 12:00 UTC") {
		t.Errorf("comment = %q, want a labelled header", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("comment = %q, want the turn body", got)
	}

	// Past GitHub's limit the API rejects the whole body, so a long turn would
	// be lost rather than shortened. It is truncated and says so.
	huge := Comment(Turn{Markdown: strings.Repeat("x", github.MaxCommentRunes+1000)}, "claude", now)
	if n := len([]rune(huge)); n > github.MaxCommentRunes {
		t.Errorf("comment is %d characters, over GitHub's limit of %d", n, github.MaxCommentRunes)
	}
	for _, want := range []string{Marker, "**claude**", "truncated"} {
		if !strings.Contains(huge, want) {
			t.Errorf("a truncated comment lost %q", want)
		}
	}
}

func TestComment_JustUnderTheLimitIsNotTruncated(t *testing.T) {
	// The boundary matters: GitHub rejects over the limit, so a comment exactly
	// at it must survive whole.
	turn := Turn{Markdown: strings.Repeat("x", 1000)}
	got := Comment(turn, "claude", time.Now())
	if strings.Contains(got, "truncated") {
		t.Errorf("a short comment was truncated:\n%s", got)
	}
}

func TestComment_PartialIsLabelled(t *testing.T) {
	got := Comment(SnapshotTurn("$ go test\nok"), "claude", time.Now())
	if !strings.Contains(got, "terminal snapshot") {
		t.Errorf("comment = %q, want it to say the transcript is a snapshot", got)
	}
	if strings.Contains(got, "UTC") {
		t.Errorf("comment = %q, want no timestamp implying a session turn", got)
	}
}

func TestComment_MultilineLabelStaysOneLine(t *testing.T) {
	got := Comment(Turn{Markdown: "hi"}, "claude\ninjected", time.Now())
	if strings.Contains(got, "**claude\ninjected**") {
		t.Errorf("label newline was not neutralised:\n%s", got)
	}
}

func TestTurn_DigestTracksContent(t *testing.T) {
	a := SnapshotTurn("hello")
	b := SnapshotTurn("hello")
	c := SnapshotTurn("hello!")

	if a.Digest() != b.Digest() {
		t.Error("the same content produced two digests, so a snapshot would repeat")
	}
	if a.Digest() == c.Digest() {
		t.Error("different content produced the same digest, so a change would be missed")
	}
	if SnapshotTurn("").Digest() == a.Digest() {
		t.Error("empty and non-empty content share a digest")
	}
}

// piSession writes a session file the way pi does: a session record first, then
// message records whose role decides how they render.
func piSession(t *testing.T, records ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "2026-09-20T09-59-00-000Z_x.jsonl")
	header := `{"type":"session","version":3,"id":"x","timestamp":"2026-09-20T09:59:00.000Z","cwd":"/work/repo"}`
	body := strings.Join(append([]string{header}, records...), "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func piHuman(id, text string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":"p","timestamp":"2026-09-20T10:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":%q}]}}`, id, text)
}

func piAssistant(id, text string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":"p","timestamp":"2026-09-20T10:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, id, text)
}

func TestReadTurn_ReadsAPiSessionWithThePiAdapter(t *testing.T) {
	path := piSession(t, piHuman("u1", "hello"), piAssistant("a1", "hi there"))

	got, err := ReadTurn("pi", path, "", transcript.Options{})
	if err != nil {
		t.Fatalf("ReadTurn: %v", err)
	}
	if !strings.Contains(got.Markdown, "hi there") || !strings.Contains(got.Markdown, "hello") {
		t.Errorf("Markdown = %q, want both turns", got.Markdown)
	}
	if got.Cursor != "a1" {
		t.Errorf("Cursor = %q, want pi's record id", got.Cursor)
	}

	again, err := ReadTurn("pi", path, got.Cursor, transcript.Options{})
	if err != nil {
		t.Fatalf("ReadTurn: %v", err)
	}
	if !again.Empty() {
		t.Errorf("Markdown = %q, want nothing new", again.Markdown)
	}
}

func TestReadTurn_TheWrongAdapterIsSilenceSoItIsRefused(t *testing.T) {
	// Reading a pi file with the Claude adapter renders nothing rather than
	// failing, which is the failure mode nobody notices. An unknown kind has to
	// be an error, not the empty string and a quiet poll.
	if _, err := ReadTurn("some-new-agent", piSession(t, piAssistant("a1", "hi")), "", transcript.Options{}); err == nil {
		t.Fatal("want an error for an agent kind with no adapter")
	}
}

func TestReadTurn_AKindlessShareIsStillClaude(t *testing.T) {
	// A share recorded before there was a second adapter has no kind stored.
	path := writeSession(t, assistantRecord("a1", "hi"))

	got, err := ReadTurn("", path, "", transcript.Options{})
	if err != nil {
		t.Fatalf("ReadTurn: %v", err)
	}
	if !strings.Contains(got.Markdown, "hi") {
		t.Errorf("Markdown = %q, want the Claude adapter to read a kindless share", got.Markdown)
	}
}

func TestEndCursor_OfAPiSession(t *testing.T) {
	path := piSession(t, piHuman("u1", "hello"), piAssistant("a1", "hi"))

	got, err := EndCursor("pi", path)
	if err != nil {
		t.Fatalf("EndCursor: %v", err)
	}
	if got != "a1" {
		t.Errorf("EndCursor = %q, want pi's last record id", got)
	}
	turn, err := ReadTurn("pi", path, got, transcript.Options{})
	if err != nil {
		t.Fatalf("ReadTurn: %v", err)
	}
	if !turn.Empty() {
		t.Errorf("a freshly primed share replayed %q", turn.Markdown)
	}
}

// The record of an instruction delivered live must never be read back as an
// instruction to deliver: that would run the colleague's text a second time
// through the poller. The Marker is what makes it ours, and the body never
// starting with the prefix is the same guard from the other side.
func TestTheLiveInstructionCommentIsNeverReReadAsAnInstruction(t *testing.T) {
	body := LiveInstruction{
		Author: "pagbrl",
		Text:   "add a rule: three consecutive misses eliminates the player",
		At:     time.Date(2026, 9, 22, 16, 40, 0, 0, time.UTC),
	}.Body()

	if !strings.HasPrefix(body, Marker+"\n") {
		t.Errorf("body must open with the marker so the poller recognises it as ours:\n%s", body)
	}
	if !strings.Contains(body, "@pagbrl") {
		t.Errorf("body must attribute the instruction to its author:\n%s", body)
	}
	if strings.HasPrefix(body, Prefix) {
		t.Errorf("body must not start with %q — that is the instruction prefix:\n%s", Prefix, body)
	}
	if !strings.Contains(body, "three consecutive misses") {
		t.Errorf("body must carry the instruction text verbatim:\n%s", body)
	}
	if !strings.Contains(body, "sent live") {
		t.Errorf("body must say the instruction arrived live, so the record is not misleading:\n%s", body)
	}

	// The whole point: feed our own comment back through the poller's reader.
	var comment github.Comment
	comment.ID = 9001
	comment.Body = body
	comment.Association = "COLLABORATOR"
	comment.User.Login = "pagbrl"

	instructions, refusals := Instructions(
		[]github.Comment{comment},
		[]string{"DnzzL", "pagbrl"},
		0,
	)
	if len(instructions) != 0 {
		t.Errorf("the poller would deliver %d instructions from our own record — a second delivery of text the agent already has", len(instructions))
	}
	if len(refusals) != 0 {
		t.Errorf("refusals = %v, want none: our own output is not a denied instruction", refusals)
	}
}
