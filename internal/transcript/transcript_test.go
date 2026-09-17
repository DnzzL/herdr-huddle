package transcript

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -update rewrites the golden file. Regenerate deliberately and review the
// diff by eye: the golden file is the only artefact that shows whether the
// transcript is actually readable, which substring assertions cannot judge.
var update = flag.Bool("update", false, "rewrite golden files")

// Regression: assistant text was rendered once per content block AND once via
// the bare-string path, so every prose reply appeared twice. Substring
// assertions could not see this; counting can.
func TestAssistantTextRenderedExactlyOnce(t *testing.T) {
	records := loadFixture(t)
	out := render(t, records, "", Options{})

	for _, text := range []string{
		"Reading the current status command first.",
		"Adding the flag.",
		"Added `--json` to the status command.",
	} {
		if got := strings.Count(out, text); got != 1 {
			t.Errorf("agent text %q rendered %d times, want exactly 1", text, got)
		}
	}

	// The bullet list must not run into the preceding paragraph.
	mustContain(t, out, "Reading the current status command first.\n\n- 🔧 **Read**")
}

// The seven properties P0-B must prove are named in /tmp/FIXTURE-NOTES.md;
// each has a test named after its number so a failure says which one broke.

const fixturePath = "testdata/fixture-session.jsonl"

// render is the string form used by most tests; property 7 needs the full
// Result and calls Render directly.
func render(t *testing.T, records []Record, after string, opts Options) string {
	t.Helper()
	return Render(records, after, opts).Markdown
}

func loadFixture(t *testing.T) []Record {
	t.Helper()
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	records, err := Parse(data)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(records) != 17 {
		t.Fatalf("fixture should have 17 records, got %d", len(records))
	}
	return records
}

func parseString(t *testing.T, md string) []Record {
	t.Helper()
	records, err := Parse([]byte(md))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return records
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected output to contain %q\n--- got ---\n%s", needle, haystack)
	}
}

func mustNotContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("expected output NOT to contain %q\n--- got ---\n%s", needle, haystack)
	}
}

// Property 1: human turns and assistant turns are distinguishable and in order.
func TestProperty1_HumanAndAssistantTurnsDistinguishableAndOrdered(t *testing.T) {
	records := loadFixture(t)
	out := render(t, records, "", Options{})

	human := strings.Index(out, "**🧑 Human**")
	agent := strings.Index(out, "**🤖 Agent**")
	if human < 0 {
		t.Fatalf("no human turn marker in output:\n%s", out)
	}
	if agent < 0 {
		t.Fatalf("no agent turn marker in output:\n%s", out)
	}
	if human > agent {
		t.Errorf("first human turn should precede first agent turn (human@%d agent@%d)", human, agent)
	}

	// The human's opening prompt.
	mustContain(t, out, "Add a --json flag to the status command")

	// Exactly one human turn: tool-result records are also type=user and
	// must not be mistaken for human speech.
	if got := strings.Count(out, "**🧑 Human**"); got != 1 {
		t.Errorf("expected exactly 1 human turn, got %d", got)
	}
	if got := strings.Count(out, "**🤖 Agent**"); got != 4 {
		t.Errorf("expected 4 agent turns (one per assistant record), got %d", got)
	}
}

// Property 2: tool_use renders as a readable line (tool name + the one
// argument that matters), not a JSON dump.
func TestProperty2_ToolUseRendersReadableLine(t *testing.T) {
	records := loadFixture(t)
	out := render(t, records, "", Options{})

	mustContain(t, out, "- 🔧 **Read** — `/home/dev/Projects/demo-app/cmd/status.go`")
	mustContain(t, out, "- 🔧 **Edit** — `/home/dev/Projects/demo-app/cmd/status.go`")
	mustContain(t, out, "- 🔧 **Bash** — `go test ./...`")

	// No raw JSON of the tool arguments anywhere.
	mustNotContain(t, out, `"file_path"`)
	mustNotContain(t, out, `"old_string"`)
	mustNotContain(t, out, `{"`)
}

// Property 3: a tool_result with is_error true is visibly a failure.
func TestProperty3_ErrorToolResultIsVisibleFailure(t *testing.T) {
	records := loadFixture(t)
	out := render(t, records, "", Options{})

	mustContain(t, out, "**❌ Tool result (error)**")
	mustContain(t, out, "undefined: jsonOut")

	// The successful results stay marked as successes.
	mustContain(t, out, "**📄 Tool result**")
}

// Property 4: thinking blocks are excluded by default, included on request.
func TestProperty4_ThinkingExcludedByDefault(t *testing.T) {
	records := loadFixture(t)
	const secret = "The status command lives in cmd/status.go. I should read it before editing."

	def := render(t, records, "", Options{})
	mustNotContain(t, def, secret)
	mustNotContain(t, def, "💭")

	withThinking := render(t, records, "", Options{IncludeThinking: true})
	mustContain(t, withThinking, secret)
}

// Property 5: attachment, mode, atis-latch, queue-operation and friends
// produce no output at all.
func TestProperty5_NoiseRecordsProduceNoOutput(t *testing.T) {
	records := loadFixture(t)

	noiseTypes := map[string]bool{
		"attachment":            true,
		"mode":                  true,
		"permission-mode":       true,
		"atis-latch":            true,
		"queue-operation":       true,
		"file-history-snapshot": true,
		"file-history-delta":    true,
		"last-prompt":           true,
		"system":                true,
	}

	var noise []Record
	for _, r := range records {
		if noiseTypes[r.Type] {
			noise = append(noise, r)
		}
	}
	if len(noise) != 9 {
		t.Fatalf("expected 9 noise records in fixture, found %d", len(noise))
	}

	out := render(t, noise, "", Options{})
	if strings.TrimSpace(out) != "" {
		t.Errorf("noise records should render nothing, got:\n%s", out)
	}
}

// Property 6: an unknown record type is skipped without error. The fixture has
// none, so this uses types observed in real sessions but absent from it, plus
// a deliberately invented one.
func TestProperty6_UnknownRecordTypeSkippedWithoutError(t *testing.T) {
	const extra = `
{"type":"ai-title","uuid":"e1","timestamp":"2026-09-17T10:02:06Z","message":{"role":"assistant","content":"INVENTED TITLE"}}
{"type":"cost-state","uuid":"e2","timestamp":"2026-09-17T10:02:13Z","message":{"role":"assistant","content":"INVENTED COST"}}
{"type":"brand-new-thing","uuid":"e3","timestamp":"2026-09-17T10:02:20Z","message":{"role":"assistant","content":"INVENTED FUTURE"}}
`
	records := parseString(t, extra)
	if len(records) != 3 {
		t.Fatalf("expected 3 records, got %d", len(records))
	}

	out := render(t, records, "", Options{})
	if strings.TrimSpace(out) != "" {
		t.Errorf("unknown record types should render nothing, got:\n%s", out)
	}
	mustNotContain(t, out, "INVENTED")

	// A malformed line, and a record with no message at all, must not error.
	records = parseString(t, `{"type":"user","uuid":"e4"}`+"\n"+`{"type":"user","uuid":`)
	if out := render(t, records, "", Options{}); strings.TrimSpace(out) != "" {
		t.Errorf("message-less records should render nothing, got:\n%s", out)
	}
}

// Property 7 — the one that justifies the JSONL path: given the last rendered
// uuid, only newer records are processed.
func TestProperty7_IncrementalFromLastUUID(t *testing.T) {
	records := loadFixture(t)

	full := Render(records, "", Options{})
	if full.Truncated {
		t.Errorf("a full render must not report Truncated")
	}
	if want := records[len(records)-1].UUID; full.LastUUID != want {
		t.Errorf("LastUUID = %q, want %q", full.LastUUID, want)
	}

	// Render from the 6th record's uuid: the first five must not reappear.
	mid := records[5].UUID
	inc := Render(records, mid, Options{})
	if inc.Truncated {
		t.Errorf("a uuid present in the file must not report Truncated")
	}
	mustNotContain(t, inc.Markdown, "Add a --json flag to the status command")
	mustNotContain(t, inc.Markdown, "Reading the current status command first.")
	mustContain(t, inc.Markdown, "undefined: jsonOut")
	if want := records[len(records)-1].UUID; inc.LastUUID != want {
		t.Errorf("incremental LastUUID = %q, want %q", inc.LastUUID, want)
	}

	// The incremental tail must be the tail of the full render, so repeated
	// polls compose into the same document.
	marker := strings.Index(full.Markdown, "**📄 Tool result**")
	if marker < 0 {
		t.Fatalf("full render missing tool result marker")
	}
	fullTail := full.Markdown[marker:]
	if !strings.HasSuffix(strings.TrimSpace(fullTail), strings.TrimSpace(inc.Markdown)) {
		t.Errorf("incremental tail does not match full render suffix\n--- suffix ---\n%s\n--- incremental ---\n%s", fullTail, inc.Markdown)
	}

	// The newest record produces no output of its own (an atis-latch), but the
	// cursor must still advance — otherwise the poller re-renders forever.
	empty := Render(records, records[15].UUID, Options{})
	if strings.TrimSpace(empty.Markdown) != "" {
		t.Errorf("only a noise record follows, expected no new output, got:\n%s", empty.Markdown)
	}
	if want := records[16].UUID; empty.LastUUID != want {
		t.Errorf("cursor must advance past non-rendering records: LastUUID = %q, want %q", empty.LastUUID, want)
	}
	if empty.Truncated {
		t.Errorf("must not report Truncated when the uuid was found")
	}

	// An unknown cursor (rotated or truncated file) must fall back to a full
	// re-render rather than silently emitting nothing.
	lost := Render(records, "no-such-uuid", Options{})
	if !lost.Truncated {
		t.Errorf("an unknown cursor must report Truncated so the caller knows to replace the body")
	}
	if lost.Markdown != full.Markdown {
		t.Errorf("an unknown cursor must fall back to the full render")
	}
}

// Parse must tolerate a partial trailing line: Claude Code appends, so the
// last line of a live file can be a half-written record.
func TestParse_ToleratesPartialTrailingLine(t *testing.T) {
	records := loadFixture(t)
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	partial := append([]byte{}, data[:len(data)-40]...)
	got, err := Parse(partial)
	if err != nil {
		t.Fatalf("Parse must not fail on a partial trailing line: %v", err)
	}
	if len(got) != len(records)-1 {
		t.Errorf("expected the final partial record to be dropped: got %d records, want %d", len(got), len(records)-1)
	}
}

// Parse must not be limited by a 64 KiB line; a tool result can be a single
// very large JSONL record.
func TestParse_HandlesLargeSingleLineRecord(t *testing.T) {
	big := strings.Repeat("x", 2<<20) // 2 MiB
	line := `{"type":"assistant","uuid":"big-1","timestamp":"2026-09-17T10:00:00Z","message":{"role":"assistant","content":"` + big + `"}}` + "\n"

	records, err := Parse([]byte(line))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
}

// Tool output indentation is significant: Claude's Read format prefixes every
// line with a numeric gutter. Trimming the body as a whole strips the first
// line's gutter and misaligns it against the rest.
func TestToolResultPreservesLeadingIndentation(t *testing.T) {
	const src = `
{"type":"user","uuid":"u1","isMeta":true,"timestamp":"2026-09-17T10:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"     1\tpackage cmd\n     2\t\n     3\tfunc Status() string {\n"}]}}
`
	out := render(t, parseString(t, src), "", Options{})

	// 4 spaces of block indent + the 5-space gutter from the tool output.
	gutter := strings.Repeat(" ", 9)
	for _, line := range []string{
		gutter + "1\tpackage cmd",
		gutter + "2\t",
		gutter + "3\tfunc Status() string {",
	} {
		mustContain(t, out, line)
	}
}

// The guardrail from the fixture notes: rendered output is pushed to GitHub,
// so the renderer must scan its own output and redact secrets.
func TestGuardrail_SecretsRedactedFromRenderedOutput(t *testing.T) {
	const secrets = `
{"type":"assistant","uuid":"s1","timestamp":"2026-09-17T10:03:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cat .env"}}]}}
{"type":"user","uuid":"s2","parentUuid":"s1","timestamp":"2026-09-17T10:03:01Z","isMeta":true,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\nDATABASE_URL=postgres://admin:hunter2@db.internal:5432/prod\nGITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789\nANTHROPIC_API_KEY=sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFF\nSLACK=xoxb-1234567890-abcdefghijkl\nAuthorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijklmnop\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----"}]}}
`
	records := parseString(t, secrets)
	out := render(t, records, "", Options{})

	for _, leak := range []string{
		"AKIAIOSFODNN7EXAMPLE",
		"hunter2",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFF",
		"xoxb-1234567890-abcdefghijkl",
		"eyJhbGciOiJIUzI1NiJ9",
		"MIIEowIBAAKCAQEA1234",
	} {
		mustNotContain(t, out, leak)
	}
	mustContain(t, out, "[REDACTED")

	// The tool call itself is still readable — redaction must not blank the
	// transcript.
	mustContain(t, out, "- 🔧 **Bash** — `cat .env`")
}

// Redaction must not mangle ordinary transcript text.
func TestGuardrail_RedactionKeepsOrdinaryText(t *testing.T) {
	records := loadFixture(t)
	plain := render(t, records, "", Options{})
	withRedaction := Redact(plain)

	for _, keep := range []string{
		"Add a --json flag to the status command",
		"undefined: jsonOut",
		"`go test ./...`",
	} {
		mustContain(t, withRedaction, keep)
	}
}

func TestRedact_LeavesNoSecretAndIsIdempotent(t *testing.T) {
	cases := map[string]string{
		"key AKIAIOSFODNN7EXAMPLE here":                  "AKIAIOSFODNN7EXAMPLE",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789": "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"url postgres://u:p4ssw0rd@host:5432/db":         "p4ssw0rd",
		"PASSWORD=hunter2":                               "hunter2",
		"api sk-ant-api03-ZZZZYYYYXXXXWWWWVVVVUUUU":      "sk-ant-api03-ZZZZYYYYXXXXWWWWVVVVUUUU",
		"slack xoxp-1234567890-abcdefghijklmnop":         "xoxp-1234567890-abcdefghijklmnop",
	}
	for in, secret := range cases {
		got := Redact(in)
		if strings.Contains(got, secret) {
			t.Errorf("Redact(%q) leaked %q: %q", in, secret, got)
		}
		if again := Redact(got); again != got {
			t.Errorf("Redact is not idempotent:\nfirst:  %q\nsecond: %q", got, again)
		}
	}
}

func TestParse_ReadsFixtureFromDisk(t *testing.T) {
	// Guards the testdata copy itself.
	if _, err := os.Stat(filepath.Join("testdata", "fixture-session.jsonl")); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
}

// The fixture in testdata is synthetic: its structure comes from real sessions
// but every value is invented, so it cannot prove the renderer survives real
// input. This runs against any real Claude Code transcript on the machine and
// is skipped when there is none. It is the gate the fixture notes demand
// before the JSONL path is considered validated.
func TestRealSessionIfPresent(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	if len(matches) == 0 {
		t.Skip("no real Claude Code transcript on this machine: the synthetic fixture is still unvalidated")
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			records, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(records) == 0 {
				t.Fatalf("no records parsed from %s", path)
			}

			full := Render(records, "", Options{})
			if full.LastUUID == "" {
				t.Errorf("no cursor produced from %d records", len(records))
			}
			if again := Render(records, "", Options{}).Markdown; again != full.Markdown {
				t.Errorf("render is not deterministic")
			}

			// Incremental rendering must compose: the tail from a mid-file
			// cursor has to be a suffix of the full render, or a poller that
			// resumes will post something the full render never showed.
			mid := records[len(records)/2].UUID
			inc := Render(records, mid, Options{})
			if inc.Truncated {
				t.Errorf("cursor %q taken from the file reported Truncated", mid)
			}
			if !strings.HasSuffix(strings.TrimSpace(full.Markdown), strings.TrimSpace(inc.Markdown)) {
				t.Errorf("incremental render from mid-file is not a suffix of the full render")
			}

			t.Logf("%s: %d records -> %d bytes of Markdown", filepath.Base(path), len(records), len(full.Markdown))
		})
	}
}

// TestGoldenRender pins the full rendering of the fixture. The fixture values
// are synthetic, so this is not a claim that the output is correct — only that
// it has not changed silently. Review the file by eye after any change.
func TestGoldenRender(t *testing.T) {
	path := filepath.Join("testdata", "fixture-session.golden.md")
	got := render(t, loadFixture(t), "", Options{})

	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run: go test ./internal/transcript/ -update): %v", err)
	}
	if got != string(want) {
		t.Errorf("rendered transcript differs from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
