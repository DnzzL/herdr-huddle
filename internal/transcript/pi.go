package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// This file renders a pi session file. The shapes are the ones a real session
// writes (900 records, 3.0 MB): see ADR-004. Pi's format is Claude Code's
// cousin rather than a variant of it — records are `type: message` with a
// separate `toolResult` record per call — so it gets its own parser and walker
// and shares only what is genuinely format-independent: redaction, the
// truncation caps, the indented tool-result body and the human/agent labels.

// PiRecord is one line of a pi session file, reduced to the fields a transcript
// needs. Everything else in the line is dropped rather than carried around.
type PiRecord struct {
	Type      string     `json:"type"` // session, message, compaction, model_change, …
	ID        string     `json:"id"`
	Timestamp string     `json:"timestamp"`
	Message   *PiMessage `json:"message"`
	Summary   string     `json:"summary"` // compaction
}

// PiMessage is the message a record carries. A tool result is its own record
// with its own role, not a block inside the turn that asked for it.
type PiMessage struct {
	Role       string          `json:"role"` // user, assistant, toolResult
	Content    json.RawMessage `json:"content"`
	IsError    bool            `json:"isError"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
}

// piBlock is one entry in a message's content array.
type piBlock struct {
	Type      string          `json:"type"` // text, thinking, toolCall
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ParsePi decodes JSONL records from a pi session file. As with Parse, a line
// that is not valid JSON is skipped rather than failed on: the file is appended
// to by another process, so its last line is routinely half-written.
func ParsePi(data []byte) ([]PiRecord, error) {
	var records []PiRecord
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// A single tool result can be megabytes, so the default 64 KiB token
	// limit is far too small.
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var r PiRecord
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		records = append(records, r)
	}
	return records, nil
}

// LastPiID is the cursor at the end of a set of records: the id RenderPi would
// return after processing all of them. Used to prime a cursor before anything
// has been delivered, so a share starts where the conversation is now rather
// than publishing a session file that may predate it by hours.
func LastPiID(records []PiRecord) string {
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].ID != "" {
			return records[i].ID
		}
	}
	return ""
}

// RenderPi renders the records after afterID into Markdown. An afterID that is
// not in records means the file was replaced or trimmed under the caller, so
// the whole conversation is rendered and Truncated reports it: publishing that
// as an increment would repeat the thread.
func RenderPi(records []PiRecord, afterID string, opts Options) Result {
	start, rotated := piStart(records, afterID)
	var out strings.Builder
	for _, r := range records[start:] {
		switch r.Type {
		case "message":
			writePiMessage(&out, r, opts)
		case "compaction":
			writePiCompaction(&out, r)
		default:
			// session, model_change, thinking_level_change, and anything a
			// future pi adds. Rendering nothing is deliberate: an unknown
			// record is not a turn, and guessing at one risks publishing
			// fields that were never meant to leave the machine.
		}
	}
	return Result{
		Markdown:  Redact(strings.TrimLeft(out.String(), "\n")),
		LastID:    LastPiID(records),
		Truncated: rotated,
	}
}

// piStart is the index of the first record to render: the one after the last
// occurrence of afterID, or 0 when there is no cursor.
func piStart(records []PiRecord, afterID string) (int, bool) {
	if afterID == "" {
		return 0, false
	}
	// The last occurrence, not the first: a cursor is a position, and ids are
	// only unique within a session.
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].ID == afterID {
			return i + 1, false
		}
	}
	return 0, true
}

func writePiMessage(out *strings.Builder, r PiRecord, opts Options) {
	if r.Message == nil {
		return
	}
	switch r.Message.Role {
	case "user":
		writePiHumanTurn(out, r)
	case "assistant":
		writePiAssistantTurn(out, r, opts)
	case "toolResult":
		writePiToolResult(out, r)
	}
	// Other roles render as nothing: measured in a real session, `bashExecution`
	// (a !command the operator ran) and `system`. ADR-004's known gaps records
	// that as undecided rather than as a decision.
}

func writePiHumanTurn(out *strings.Builder, r PiRecord) {
	text := strings.TrimSpace(strings.Join(piContentTexts(r.Message), "\n\n"))
	if text == "" {
		return
	}
	fmt.Fprintf(out, "\n**🧑 Human**%s\n\n%s\n", stamp(r.Timestamp), text)
}

func writePiAssistantTurn(out *strings.Builder, r PiRecord, opts Options) {
	// Blocks are collected in order and joined with blank lines, so prose and
	// tool calls stay in the order the agent produced them and consecutive
	// tool calls form one list.
	var parts []string
	var bullets []string
	flushBullets := func() {
		if len(bullets) > 0 {
			parts = append(parts, strings.Join(bullets, "\n"))
			bullets = nil
		}
	}

	for _, b := range piContentBlocks(r.Message) {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				flushBullets()
				parts = append(parts, t)
			}
		case "thinking":
			if !opts.IncludeThinking {
				continue
			}
			if t := strings.TrimSpace(b.Thinking); t != "" {
				flushBullets()
				parts = append(parts, "<details><summary>💭 Thinking</summary>\n\n"+t+"\n\n</details>")
			}
		case "toolCall":
			if line := piToolCallLine(b); line != "" {
				bullets = append(bullets, line)
			}
		}
	}
	flushBullets()

	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return
	}
	fmt.Fprintf(out, "\n**🤖 Agent**%s\n\n%s\n", stamp(r.Timestamp), text)
}

func writePiToolResult(out *strings.Builder, r PiRecord) {
	m := r.Message
	label := "**📄 Tool result**"
	if m.IsError {
		label = "**❌ Tool result (error)**"
	}
	if m.ToolName != "" {
		label = fmt.Sprintf("%s `%s`", label, oneLine(m.ToolName))
	}
	body := trimBlankEdges(truncate(piContentText(m.Content)))
	fmt.Fprintf(out, "\n%s%s\n", label, stamp(r.Timestamp))
	if body == "" {
		return
	}
	writeIndentedBody(out, body)
}

// writePiCompaction marks where the agent's context was summarised. The thread
// must not go silent at that point: the collaborator reading it later has no
// other way to tell a gap from an omission. The summary is capped because the
// ones measured on a real session ran 14,716 to 44,259 characters, and one of
// those in full would dominate a planning thread that already contains the turns
// being summarised.
func writePiCompaction(out *strings.Builder, r PiRecord) {
	fmt.Fprintf(out, "\n**⟳ Context compacted**%s\n", stamp(r.Timestamp))

	if summary := trimBlankEdges(r.Summary); summary != "" {
		// Rendered as prose, not indented: this is text the agent wrote about
		// its own conversation, the same trust level as its turns.
		body, capped := capRunes(summary, maxCompactionRunes)
		out.WriteString("\n" + body + "\n")
		if capped {
			out.WriteString("\n… (excerpt)\n")
		}
	}
	out.WriteString("\nThe agent's context was summarised here, and earlier turns are no longer sent to the model. The session file holds the full summary.\n")
}

// piToolCallLine renders a tool call as name plus the one argument that
// identifies what it operated on.
func piToolCallLine(b piBlock) string {
	if b.Name == "" {
		return ""
	}
	if arg := piSalientArg(b); arg != "" {
		return fmt.Sprintf("- 🔧 **%s** — `%s`", b.Name, arg)
	}
	return fmt.Sprintf("- 🔧 **%s**", b.Name)
}

// piPreferredArgs is the one argument worth showing per tool. Pi's built-in
// tools are named in lower case, so Claude Code's table cannot be reused; the
// fallback below covers a tool that is not listed.
var piPreferredArgs = map[string][]string{
	"read":      {"path", "file_path", "filePath"},
	"write":     {"path", "file_path", "filePath"},
	"edit":      {"path", "file_path", "filePath"},
	"bash":      {"command", "cmd"},
	"grep":      {"pattern", "query"},
	"glob":      {"pattern"},
	"fetch":     {"url"},
	"webFetch":  {"url"},
	"webSearch": {"query"},
	"task":      {"description", "prompt"},
	"skill":     {"skill", "name"},
}

func piSalientArg(b piBlock) string {
	var args map[string]any
	if len(b.Arguments) == 0 {
		return ""
	}
	if err := json.Unmarshal(b.Arguments, &args); err != nil {
		return ""
	}
	for _, key := range piPreferredArgs[b.Name] {
		if v, ok := args[key].(string); ok && v != "" {
			return oneLine(v)
		}
	}
	// Unknown tool: first string argument in stable key order, so the same
	// call renders the same way every time.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return oneLine(v)
		}
	}
	return ""
}

// piContentBlocks returns the content array, if the content is an array.
func piContentBlocks(m *PiMessage) []piBlock {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	if _, ok := piContentString(m); ok {
		return nil
	}
	var blocks []piBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// piContentString returns the content when it is a bare JSON string.
func piContentString(m *PiMessage) (string, bool) {
	if m == nil || len(m.Content) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err != nil {
		return "", false
	}
	return s, true
}

// piContentTexts is the human-readable text of a message: its text blocks, or a
// bare string.
func piContentTexts(m *PiMessage) []string {
	if s, ok := piContentString(m); ok {
		return []string{s}
	}
	var texts []string
	for _, b := range piContentBlocks(m) {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return texts
}

// piContentText is the flattened text of a tool result's content blocks.
func piContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []piBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
