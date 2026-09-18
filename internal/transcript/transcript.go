// Package transcript renders a Claude Code session JSONL file into Markdown
// suitable for a GitHub draft PR body.
//
// The format is undocumented and can change without notice, so parsing here is
// deliberately defensive: unknown record types, unknown content blocks,
// malformed lines and half-written trailing lines are all skipped rather than
// treated as errors. Anything this package does not understand is lost, never
// fatal.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Record is one line of a session JSONL file. Only the fields this renderer
// understands are decoded; the real format has dozens more and the set is open.
type Record struct {
	Type        string   `json:"type"`
	UUID        string   `json:"uuid"`
	ParentUUID  string   `json:"parentUuid"`
	Timestamp   string   `json:"timestamp"`
	IsMeta      bool     `json:"isMeta"`
	IsSidechain bool     `json:"isSidechain"`
	Message     *Message `json:"message"`
}

// Message is the embedded conversation message. Content is either a bare
// string or an array of content blocks, so it stays raw until we know which.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// block is one element of a content array. Fields are shared across the
// variants we render; anything absent stays zero.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     map[string]any  `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// Options controls rendering. The zero value is the intended default:
// thinking omitted.
type Options struct {
	IncludeThinking bool
}

// Result is the outcome of a render.
type Result struct {
	// Markdown is the rendered transcript body.
	Markdown string
	// LastUUID is the cursor to pass as afterUUID on the next poll. It
	// advances past records that rendered nothing, so the poller never
	// re-scans the same tail.
	LastUUID string
	// Truncated reports that afterUUID was not found in the records, so
	// Markdown is a full re-render and the caller should replace the body
	// rather than append. A live file that has been rotated, truncated or
	// rewritten lands here.
	Truncated bool
}

const (
	maxToolResultLines = 20
	maxToolResultRunes = 4000
)

// Parse decodes JSONL records. Lines that are not valid JSON are skipped: a
// live session file is appended to, so its final line is routinely a partly
// written record. Parse therefore has no error to return — malformed input is
// data loss, not a failure.
func Parse(data []byte) ([]Record, error) {
	var records []Record
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// A single tool result can be megabytes, so the default 64 KiB token
	// limit is far too small.
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		records = append(records, r)
	}
	// A scanner error here means an oversized line; treat it as data loss
	// rather than failing the whole render.
	return records, nil
}

// Render turns records into Markdown, processing only records after afterUUID.
// An empty afterUUID renders everything. The result carries the cursor for the
// next call.
func Render(records []Record, afterUUID string, opts Options) Result {
	start := 0
	truncated := false
	if afterUUID != "" {
		start = -1
		// Take the last match: if a uuid repeats, resuming from the final
		// occurrence cannot re-emit records we already rendered.
		for i, r := range records {
			if r.UUID == afterUUID {
				start = i + 1
			}
		}
		if start < 0 {
			// Cursor is gone (rotated or rewritten file). Re-render in full
			// and let the caller know to replace the body.
			start = 0
			truncated = true
		}
	}

	var out strings.Builder
	for _, r := range records[start:] {
		if r.IsSidechain {
			// Subagent traffic is a separate conversation; it does not
			// belong in the shared thread.
			continue
		}
		writeRecord(&out, r, opts)
	}

	last := afterUUID
	for _, r := range records[start:] {
		if r.UUID != "" {
			last = r.UUID
		}
	}

	return Result{
		Markdown:  Redact(strings.TrimLeft(out.String(), "\n")),
		LastUUID:  last,
		Truncated: truncated,
	}
}

func writeRecord(out *strings.Builder, r Record, opts Options) {
	switch r.Type {
	case "user":
		if r.IsMeta {
			writeToolResults(out, r)
			return
		}
		writeHumanTurn(out, r)
	case "assistant":
		writeAssistantTurn(out, r, opts)
	default:
		// Noise, and every type not yet invented: no output.
		//
		// This default is a security boundary, not just tidiness. A real
		// session's attachment records carry the entire system prompt, every
		// skill description, and the user's own ~/.claude/CLAUDE.md, wrapped in
		// <system-reminder> blocks. Rendering them would push all of that into a
		// pull request. Confirmed against a real session file: environment,
		// model, deferred_tools_delta, agent_listing_delta, skill_listing,
		// total_tokens_reminder, instructions, date, remote_session_change and
		// prompt_snapshot all arrive as type "attachment" and all render to
		// nothing.
		//
		// Do not add a case for an unknown type without checking what it
		// contains on a real session first. Also skipped, and safe today for the
		// same reason: mode, permission-mode, atis-latch, queue-operation,
		// file-history-snapshot, file-history-delta, last-prompt, system.
		return
	}
}

func writeHumanTurn(out *strings.Builder, r Record) {
	texts := contentTexts(r.Message)
	text := strings.TrimSpace(strings.Join(texts, "\n\n"))
	if text == "" {
		return
	}
	fmt.Fprintf(out, "\n**🧑 Human**%s\n\n%s\n", stamp(r.Timestamp), text)
}

func writeAssistantTurn(out *strings.Builder, r Record, opts Options) {
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

	for _, b := range contentBlocks(r.Message) {
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
		case "tool_use":
			if line := toolUseLine(b); line != "" {
				bullets = append(bullets, line)
			}
		}
	}
	flushBullets()

	// A bare string is also a valid assistant content shape. This is the
	// only place bare content is consumed, so text is never emitted twice.
	if s, ok := contentString(r.Message); ok {
		if t := strings.TrimSpace(s); t != "" {
			parts = append(parts, t)
		}
	}

	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return
	}
	fmt.Fprintf(out, "\n**🤖 Agent**%s\n\n%s\n", stamp(r.Timestamp), text)
}

// toolUseLine renders a tool call as name plus the single argument that
// identifies what it operated on.
func toolUseLine(b block) string {
	if b.Name == "" {
		return ""
	}
	if arg := salientArg(b); arg != "" {
		return fmt.Sprintf("- 🔧 **%s** — `%s`", b.Name, arg)
	}
	return fmt.Sprintf("- 🔧 **%s**", b.Name)
}

// salientArgument is the one argument worth showing per tool. Unknown tools
// fall back to a deterministic pick so a new tool still renders usefully.
func salientArg(b block) string {
	preferred := map[string][]string{
		"Read":         {"file_path"},
		"Write":        {"file_path"},
		"Edit":         {"file_path"},
		"MultiEdit":    {"file_path"},
		"NotebookEdit": {"notebook_path", "file_path"},
		"Bash":         {"command"},
		"Grep":         {"pattern"},
		"Glob":         {"pattern"},
		"WebFetch":     {"url"},
		"WebSearch":    {"query"},
		"Task":         {"description"},
		"Skill":        {"skill"},
	}
	for _, key := range preferred[b.Name] {
		if v, ok := b.Input[key].(string); ok && v != "" {
			return oneLine(v)
		}
	}
	// Unknown tool: first string argument in stable key order.
	keys := make([]string, 0, len(b.Input))
	for k := range b.Input {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := b.Input[k].(string); ok && v != "" {
			return oneLine(v)
		}
	}
	return ""
}

func writeToolResults(out *strings.Builder, r Record) {
	for _, b := range contentBlocks(r.Message) {
		if b.Type != "tool_result" {
			continue
		}
		label := "**📄 Tool result**"
		if b.IsError {
			label = "**❌ Tool result (error)**"
		}
		body := trimBlankEdges(toolResultText(b.Content))
		fmt.Fprintf(out, "\n%s%s\n", label, stamp(r.Timestamp))
		if body == "" {
			continue
		}
		out.WriteString("\n")
		// Indented rather than fenced: tool output can contain ``` and
		// close a fence, which would let arbitrary content render as
		// Markdown in the PR body.
		for _, line := range indentBody(body) {
			out.WriteString("    " + line + "\n")
		}
	}
}

// trimBlankEdges removes leading and trailing blank lines and trailing spaces
// while leaving indentation intact. Tool output indentation is significant —
// Claude's Read format prefixes every line with a numeric gutter — so trimming
// the body as a whole would strip the first line's gutter and misalign it
// against the rest.
func trimBlankEdges(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	return strings.TrimLeft(s, "\r\n")
}

func indentBody(s string) []string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxToolResultLines {
		return lines
	}
	kept := append([]string{}, lines[:maxToolResultLines]...)
	kept = append(kept, fmt.Sprintf("… (%d more lines)", len(lines)-maxToolResultLines))
	return kept
}

// toolResultText normalises the several shapes a tool_result body takes:
// a bare string, or an array of blocks.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return truncate(s)
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return truncate(strings.Join(parts, "\n"))
	}
	return ""
}

func truncate(s string) string {
	// Count runes, not bytes: cutting mid-rune would emit invalid UTF-8 into
	// the PR body.
	runes := []rune(s)
	if len(runes) <= maxToolResultRunes {
		return s
	}
	return string(runes[:maxToolResultRunes]) + "\n… (truncated)"
}

// contentBlocks returns the content array, if the content is an array.
func contentBlocks(m *Message) []block {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	// A bare string is valid content; there are no blocks then.
	if _, ok := contentString(m); ok {
		return nil
	}
	var blocks []block
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// contentString returns the content when it is a bare JSON string, which is a
// valid shape for both user and assistant messages.
func contentString(m *Message) (string, bool) {
	if m == nil || len(m.Content) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err != nil {
		return "", false
	}
	return s, true
}

// contentTexts returns the human-readable text of a message: either its bare
// string content, or the text of its text blocks.
func contentTexts(m *Message) []string {
	if s, ok := contentString(m); ok {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []string{s}
	}
	var texts []string
	for _, b := range contentBlocks(m) {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			texts = append(texts, b.Text)
		}
	}
	return texts
}

// stamp renders a record timestamp as a short UTC clock time, or nothing if
// the timestamp is absent or unparseable.
func stamp(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return " `" + t.UTC().Format("15:04:05Z") + "`"
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	// Backticks are stripped because the value is rendered inside backticks;
	// an embedded backtick would end the code span early.
	s = strings.ReplaceAll(s, "`", "'")
	runes := []rune(s)
	if len(runes) > 120 {
		s = string(runes[:120]) + "…"
	}
	return strings.TrimSpace(s)
}

// secretKeyWords are the substrings that make an identifier look like it holds a
// credential. Shared by the two keyed rules below, which differ only in how they
// reach the key: one from the start of a line (an env file or shell dump), one
// from a quote anywhere in a line (a JSON document, where the key of interest is
// nested and never starts a line).
const secretKeyWords = `PASSWORD|PASSWD|PASS|PWD|SECRET|TOKEN|APIKEY|API_KEY|ACCESS_KEY|PRIVATE_KEY|CREDENTIAL|CLIENT_SECRET|SIGNING_KEY|ENCRYPTION_KEY|MASTER_KEY|WEBHOOK_SECRET|SESSION_SECRET|REFRESH_TOKEN|BEARER|DSN|CONNECTION_STRING|DATABASE_URL`

// secretKey matches an identifier that looks like it holds a credential.
var secretKey = `[A-Z0-9_]*(?:` + secretKeyWords + `)[A-Z0-9_]*`

// secretPattern is one redaction rule.
//
// template is a regexp replacement template applied to the whole match; an empty
// template means the match is replaced by a labelled marker. Templates exist
// where the surrounding syntax carries meaning that redacting must not destroy:
// a URL's scheme, or an assignment's key. They also keep quoted values balanced,
// which matters because a JSON config dump is the likeliest place for a
// credential to appear and an unbalanced quote would corrupt it.
type secretPattern struct {
	name     string
	re       *regexp.Regexp
	template string
}

// secretPatterns are matched against the finished Markdown. This runs on every
// render because the output is pushed to GitHub, and tool results routinely
// contain .env dumps, config files and connection strings.
//
// Order matters in two places. Whole-blob rules run first so that a private
// key's interior is not partially eaten by a narrower rule. The keyed rules run
// last, by which point any value they might have preserved has already gone.
//
// Over-redaction is the deliberate direction: a value that looks like a
// credential is redacted even when it is not one. `TOKENIZER=bert` loses its
// value. That is acceptable because the cost of a false positive is a
// slightly less readable transcript, while the cost of a false negative is a
// live secret in a public pull request.
var secretPatterns = []secretPattern{
	// Blob rules.
	{"private_key", regexp.MustCompile(`(?s)-----BEGIN[^-]*PRIVATE KEY-----.*?-----END[^-]*PRIVATE KEY-----`), ""},

	// Vendor-prefixed tokens, long enough that a false positive is implausible.
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), ""},
	{"github_token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b`), ""},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`), ""},
	{"openai_key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}`), ""},
	{"stripe_key", regexp.MustCompile(`\b(?:sk|rk)_live_[0-9a-zA-Z]{20,}`), ""},
	{"stripe_webhook_secret", regexp.MustCompile(`\bwhsec_[A-Za-z0-9]{20,}`), ""},
	// Covers the bot, user, refresh and the xoxc/xoxd session cookies, which are
	// as good as a token to anyone who has one.
	{"slack_token", regexp.MustCompile(`\bxox[a-z]-[A-Za-z0-9-]{10,}`), ""},
	{"slack_webhook", regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/_-]{20,}`), ""},
	{"gitlab_token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`), ""},
	{"npm_token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`), ""},
	{"pypi_token", regexp.MustCompile(`\bpypi-AgEIcHlwaS5vcmc[A-Za-z0-9_-]{20,}`), ""},
	{"huggingface_token", regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`), ""},
	{"digitalocean_token", regexp.MustCompile(`\bdop_v1_[a-f0-9]{64}\b`), ""},
	{"sendgrid_key", regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}\b`), ""},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), ""},
	{"google_oauth_secret", regexp.MustCompile(`\bGOCSPX-[A-Za-z0-9_-]{20,}`), ""},
	{"azure_account_key", regexp.MustCompile(`(?i)\bAccountKey=[A-Za-z0-9+/=]{40,}`), ""},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), ""},

	// Header forms. Anchored to the header name, because an unanchored
	// "basic <word>" matches ordinary prose such as "the basic implementation".
	{"bearer_token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`), ""},
	{"authorization_basic", regexp.MustCompile(`(?i)\bauthorization:\s*basic\s+[A-Za-z0-9+/=]+`), ""},

	// Keyed rules. These keep the key so the reader can still see which setting
	// held the secret, which is often the point of the tool result.
	{"url_credentials", regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]{2,}://[^:@/\s]+):([^@/\s]+)@`), "$1:[REDACTED:url_credentials]@"},
	//
	// The quoted-value alternative stops before the closing quote so that
	// redacting a JSON value leaves the document balanced. The bare alternative
	// stops before a comma or semicolon for the same reason.
	{"json_secret", regexp.MustCompile(
		`(?i)("` + secretKey + `"[ \t]*:[ \t]*)("[^"\n]*")`), "$1[REDACTED:json_secret]"},
	{"assigned_secret", regexp.MustCompile(
		`(?im)^([ \t]*"?` + secretKey + `"?[ \t]*[:=][ \t]*)("[^"\n]*"|'[^'\n]*'|[^\s,;]+)`), "$1[REDACTED:assigned_secret]"},
}

// Redact replaces likely secrets in rendered Markdown with a labelled marker.
// It is applied to the whole document so a secret cannot slip through because
// it travelled an unexpected path through the renderer.
func Redact(markdown string) string {
	out := markdown
	for _, p := range secretPatterns {
		tmpl := p.template
		if tmpl == "" {
			tmpl = "[REDACTED:" + p.name + "]"
		}
		out = p.re.ReplaceAllString(out, tmpl)
	}
	return out
}
