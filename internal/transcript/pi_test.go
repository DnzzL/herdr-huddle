package transcript

import (
	"strings"
	"testing"
)

// piRecord is one line of a pi session file. The shapes here are copied from a
// real session (900 records, 3.0 MB), not invented: see ADR-004.
func piRecord(body string) string { return body }

func piSession(records ...string) []byte {
	return []byte(strings.Join(records, "\n") + "\n")
}

// A human prompt is a message record whose content is a list of text blocks.
func piHuman(id, text string) string {
	return `{"type":"message","id":"` + id + `","parentId":"p","timestamp":"2026-09-20T10:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":` + quote(text) + `}]}}`
}

func piAssistant(id, text string) string {
	return `{"type":"message","id":"` + id + `","parentId":"p","timestamp":"2026-09-20T10:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":` + quote(text) + `}]}}`
}

func piThinking(id, text string) string {
	return `{"type":"message","id":"` + id + `","parentId":"p","timestamp":"2026-09-20T10:00:02.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":` + quote(text) + `,"thinkingSignature":"reasoning_content"}]}}`
}

func piToolCall(id, name, args string) string {
	return `{"type":"message","id":"` + id + `","parentId":"p","timestamp":"2026-09-20T10:00:03.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call_1","name":"` + name + `","arguments":` + args + `}]}}`
}

func piToolResult(id, name, text string, isError bool) string {
	return `{"type":"message","id":"` + id + `","parentId":"p","timestamp":"2026-09-20T10:00:04.000Z","message":{"role":"toolResult","toolCallId":"call_1","toolName":"` + name + `","isError":` + boolLit(isError) + `,"content":[{"type":"text","text":` + quote(text) + `}]}}`
}

func piCompaction(id, summary string) string {
	return `{"type":"compaction","id":"` + id + `","parentId":"p","timestamp":"2026-09-20T10:00:05.000Z","summary":` + quote(summary) + `,"firstKeptEntryId":"a1","tokensBefore":111771,"fromHook":false}`
}

func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// quote is a minimal JSON string encoder for the fixture text.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

const piSessionHeader = `{"type":"session","version":3,"id":"01a0ae5c-7228-70e3-9e63-2e82dacc2a2c","timestamp":"2026-09-20T09:59:00.000Z","cwd":"/work/alpha"}`

func TestParsePi_ReadsTheShapesArealSessionUses(t *testing.T) {
	data := piSession(
		piSessionHeader,
		piHuman("u1", "rename the parser"),
		piAssistant("a1", "Starting with the lexer."),
		piThinking("a2", "The lexer is the right entry point."),
		piToolCall("a3", "read", `{"path":"/work/alpha/lexer.go"}`),
		piToolResult("r1", "read", "package alpha\n", false),
		piCompaction("c1", "## Goal\nRename the parser."),
		`{"type":"model_change","id":"m1","parentId":"p","timestamp":"2026-09-20T10:00:06.000Z","modelId":"x","provider":"y"}`,
	)

	records, err := ParsePi(data)
	if err != nil {
		t.Fatalf("ParsePi: %v", err)
	}
	if len(records) != 8 {
		t.Fatalf("parsed %d records, want all 8", len(records))
	}
	if got := records[1].Message; got == nil || got.Role != "user" {
		t.Errorf("second record = %+v, want the human turn", records[1])
	}
	if got := records[5].Message; got == nil || got.Role != "toolResult" || got.ToolName != "read" {
		t.Errorf("tool result = %+v", records[5].Message)
	}
}

func TestParsePi_SkipsAMalformedLineRatherThanTheFile(t *testing.T) {
	// A session file is written by another process while this one reads it, so a
	// half-written last line is expected, not exceptional.
	data := piSession(piSessionHeader, piHuman("u1", "hello"), `{"type":"message","id":"a1","mess`)
	records, err := ParsePi(data)
	if err != nil {
		t.Fatalf("ParsePi: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("parsed %d records, want the two complete ones", len(records))
	}
}

func TestRenderPi_RendersTurnsInOrder(t *testing.T) {
	data := piSession(
		piSessionHeader,
		piHuman("u1", "rename the parser"),
		piAssistant("a1", "Starting with the lexer."),
		piToolCall("a2", "read", `{"path":"/work/alpha/lexer.go"}`),
		piToolResult("r1", "read", "package alpha", false),
	)
	records, err := ParsePi(data)
	if err != nil {
		t.Fatal(err)
	}

	res := RenderPi(records, "", Options{})
	for _, want := range []string{"🧑 Human", "rename the parser", "🤖 Agent", "Starting with the lexer.", "read", "/work/alpha/lexer.go", "📄 Tool result", "package alpha"} {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("markdown is missing %q:\n%s", want, res.Markdown)
		}
	}
	if i, j := strings.Index(res.Markdown, "rename the parser"), strings.Index(res.Markdown, "Starting with the lexer."); i > j {
		t.Error("the human turn was rendered after the agent's answer")
	}
	// The system/session header is not part of the conversation.
	if strings.Contains(res.Markdown, "/work/alpha\"") || strings.Contains(res.Markdown, "version") {
		t.Errorf("the session header leaked into the transcript:\n%s", res.Markdown)
	}
}

func TestRenderPi_ThinkingIsBehindTheOption(t *testing.T) {
	records := mustParsePi(t, piSession(piThinking("a1", "The lexer is the right entry point.")))

	hidden := RenderPi(records, "", Options{})
	if strings.Contains(hidden.Markdown, "lexer is the right") {
		t.Errorf("thinking rendered without the option:\n%s", hidden.Markdown)
	}
	shown := RenderPi(records, "", Options{IncludeThinking: true})
	if !strings.Contains(shown.Markdown, "💭 Thinking") || !strings.Contains(shown.Markdown, "<details>") {
		t.Errorf("thinking did not render as a collapsible block:\n%s", shown.Markdown)
	}
}

func TestRenderPi_AToolResultIsIndentedAndNeverFenced(t *testing.T) {
	// Indented rather than fenced: tool output can contain ``` and close a
	// fence, which would let arbitrary content render as Markdown in the PR.
	records := mustParsePi(t, piSession(piToolResult("r1", "read", "```\n# not a heading\n```", false)))

	res := RenderPi(records, "", Options{})
	if strings.Contains(res.Markdown, "```") && !strings.Contains(res.Markdown, "    ```") {
		t.Errorf("a fence survived unindented:\n%s", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "    # not a heading") {
		t.Errorf("the tool result is not indented:\n%s", res.Markdown)
	}
}

func TestRenderPi_MarksAnErrorToolResult(t *testing.T) {
	records := mustParsePi(t, piSession(piToolResult("r1", "bash", "exit status 1", true)))
	if !strings.Contains(RenderPi(records, "", Options{}).Markdown, "❌ Tool result (error)") {
		t.Error("an error tool result is not distinguishable from a successful one")
	}
}

func TestRenderPi_ALongToolResultIsCapped(t *testing.T) {
	long := strings.Repeat("x", maxToolResultRunes*2)
	records := mustParsePi(t, piSession(piToolResult("r1", "read", long, false)))

	got := RenderPi(records, "", Options{}).Markdown
	if !strings.Contains(got, "(truncated)") {
		t.Error("a huge tool result was not marked as truncated")
	}
	if len(got) > len(long) {
		t.Errorf("the render is %d bytes for a %d-byte result, so nothing was capped", len(got), len(long))
	}
}

func TestRenderPi_CompactionIsAVisibleMarker(t *testing.T) {
	// The thread must not go silent where the agent's context was summarised:
	// the collaborator has no other way to tell a gap from an omission.
	records := mustParsePi(t, piSession(
		piAssistant("a1", "before the compaction"),
		piCompaction("c1", "## Goal\nRename the parser."),
		piAssistant("a2", "after the compaction"),
	))

	got := RenderPi(records, "", Options{}).Markdown
	for _, want := range []string{"Context compacted", "## Goal", "Rename the parser.", "session file holds the full summary"} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown is missing %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "before the compaction"), strings.Index(got, "Context compacted"); i > j {
		t.Error("the marker was rendered before the turn it follows")
	}
	if i, j := strings.Index(got, "Context compacted"), strings.Index(got, "after the compaction"); i > j {
		t.Error("the marker was rendered after the turn that follows it")
	}
}

func TestRenderPi_ALongCompactionSummaryIsCappedAndSaysSo(t *testing.T) {
	// Measured on a real session: compaction summaries ran 14,716 to 44,259
	// characters. One of those in full would dominate the thread.
	records := mustParsePi(t, piSession(piCompaction("c1", strings.Repeat("Q", maxCompactionRunes*3))))

	got := RenderPi(records, "", Options{}).Markdown
	if !strings.Contains(got, "excerpt") {
		t.Errorf("a capped summary is not labelled as an excerpt:\n%s", got[:min(len(got), 400)])
	}
	if n := strings.Count(got, "Q"); n > maxCompactionRunes {
		t.Errorf("rendered %d characters of a capped summary, want at most %d", n, maxCompactionRunes)
	}
}

func TestRenderPi_RedactsSecretsInEveryShape(t *testing.T) {
	// Redaction runs inside each adapter, so every adapter has to prove it.
	records := mustParsePi(t, piSession(
		piAssistant("a1", "the token is ghp_0123456789abcdefghijklmnopqrstuvwxyz"),
		piToolResult("r1", "read", "PASSWORD=hunter2", false),
		piCompaction("c1", "API_KEY=abcdef123456"),
	))

	got := RenderPi(records, "", Options{}).Markdown
	for _, leaked := range []string{"ghp_0123456789abcdefghijklmnopqrstuvwxyz", "hunter2", "abcdef123456"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q survived redaction:\n%s", leaked, got)
		}
	}
}

func TestRenderPi_AfterAnIdIsWhatIsNew(t *testing.T) {
	records := mustParsePi(t, piSession(
		piAssistant("a1", "first"),
		piAssistant("a2", "second"),
		piAssistant("a3", "third"),
	))

	res := RenderPi(records, "a1", Options{})
	if strings.Contains(res.Markdown, "first") {
		t.Errorf("the cursor was ignored:\n%s", res.Markdown)
	}
	for _, want := range []string{"second", "third"} {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("markdown is missing %q:\n%s", want, res.Markdown)
		}
	}
	if res.LastID != "a3" {
		t.Errorf("LastID = %q, want the last record with an id", res.LastID)
	}
	if res.Truncated {
		t.Error("Truncated is set for a cursor that is present")
	}
}

func TestRenderPi_AnUnknownCursorReportsRotation(t *testing.T) {
	// The file no longer holds the position, so this render is the whole
	// conversation. Publishing it would repeat the thread; the caller has to be
	// able to tell.
	records := mustParsePi(t, piSession(piAssistant("a1", "everything")))

	res := RenderPi(records, "gone", Options{})
	if !res.Truncated {
		t.Error("a missing cursor was not reported")
	}
}

func TestRenderPi_AdvancesPastRecordsThatRenderNothing(t *testing.T) {
	records := mustParsePi(t, piSession(
		piAssistant("a1", "something"),
		`{"type":"thinking_level_change","id":"t1","parentId":"p","timestamp":"2026-09-20T10:00:07.000Z","thinkingLevel":"high"}`,
	))

	if res := RenderPi(records, "a1", Options{}); res.LastID != "t1" || res.Markdown != "" {
		t.Errorf("LastID = %q, markdown = %q, want the cursor to advance past a record that renders nothing", res.LastID, res.Markdown)
	}
}

func TestLastPiID_TheEndOfTheConversation(t *testing.T) {
	records := mustParsePi(t, piSession(piSessionHeader, piHuman("u1", "hi"), piAssistant("a3", "there")))
	if got := LastPiID(records); got != "a3" {
		t.Errorf("LastPiID = %q, want the last record with an id", got)
	}
	if got := LastPiID(nil); got != "" {
		t.Errorf("LastPiID(nil) = %q, want empty", got)
	}
}

func TestParsePi_IgnoresUnknownTypes(t *testing.T) {
	records := mustParsePi(t, piSession(
		piSessionHeader,
		`{"type":"something_new","id":"x1","payload":{"secret":"do not render"}}`,
		piAssistant("a1", "hello"),
	))
	got := RenderPi(records, "", Options{}).Markdown
	if strings.Contains(got, "do not render") {
		t.Errorf("an unknown record type rendered its contents:\n%s", got)
	}
}

func mustParsePi(t *testing.T, data []byte) []PiRecord {
	t.Helper()
	records, err := ParsePi(data)
	if err != nil {
		t.Fatalf("ParsePi: %v", err)
	}
	return records
}
