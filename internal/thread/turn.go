package thread

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/github"
	"github.com/DnzzL/herdr-huddle/internal/transcript"
)

// Turn is one batch of the agent's output, ready to publish.
type Turn struct {
	Markdown string
	// Cursor is the transcript position this turn was read up to. Empty for a
	// terminal snapshot, which has no cursor at all.
	Cursor string
	// Partial marks a turn read from the terminal instead of a session file:
	// readable prose, but overlapping with the previous snapshot and missing
	// tool detail. ADR-001 keeps it as a labelled fallback.
	Partial bool
	// Rotated reports that the cursor was not found in the file, so Markdown is
	// a re-read of the whole session rather than what is new. Publishing it
	// would repeat the conversation, so the caller advances the cursor and
	// says so instead.
	Rotated bool
}

// Empty reports whether there is nothing worth publishing.
func (t Turn) Empty() bool { return strings.TrimSpace(t.Markdown) == "" }

// Digest identifies the turn's content, so a partial snapshot that has not
// changed since the last poll can be recognised. The terminal has no cursor, so
// the content is the only thing there is to compare.
func (t Turn) Digest() string {
	sum := sha256.Sum256([]byte(t.Markdown))
	return hex.EncodeToString(sum[:8])
}

// ReadTurn reads the session file and renders what the agent has said since
// afterUUID.
func ReadTurn(path, afterUUID string, opts transcript.Options) (Turn, error) {
	records, err := readSession(path)
	if err != nil {
		return Turn{}, err
	}
	res := transcript.Render(records, afterUUID, opts)
	return Turn{Markdown: res.Markdown, Cursor: res.LastUUID, Rotated: res.Truncated}, nil
}

func readSession(path string) ([]transcript.Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("thread: read session: %w", err)
	}
	records, err := transcript.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("thread: parse session: %w", err)
	}
	return records, nil
}

// EndCursor is the transcript position at the end of a session file, used to
// prime a share's cursor so its first comment starts where the conversation is
// now rather than replaying a session that began before the share existed.
func EndCursor(path string) (string, error) {
	records, err := readSession(path)
	if err != nil {
		return "", err
	}
	return transcript.LastUUID(records), nil
}

// SnapshotTurn is a terminal snapshot: the fallback for an agent whose
// transcript file cannot be found.
func SnapshotTurn(text string) Turn {
	return Turn{Markdown: text, Partial: true}
}

const truncationNote = "\n\n_[truncated: this turn was longer than GitHub's comment limit. " +
	"The full transcript is in the session file on the operator's machine.]_"

// Comment renders a turn as the comment that carries it.
//
// Every comment starts with Marker so the poller can tell its own output from a
// person's (see Marker). A turn longer than GitHub's limit is truncated and
// says so: the API rejects an over-long body outright, so failing the sync
// would lose the turn entirely, and a silent truncation would misrepresent it.
func Comment(turn Turn, label string, now time.Time) string {
	if label == "" {
		label = "agent"
	}
	// The label becomes part of a one-line header, so a newline in it would
	// break the comment's shape.
	label = strings.NewReplacer("\n", " ", "\r", " ").Replace(label)
	var header strings.Builder
	header.WriteString(Marker)
	header.WriteString("\n**")
	header.WriteString(label)
	header.WriteString("**")
	if !turn.Partial {
		header.WriteString(" · ")
		header.WriteString(now.UTC().Format("2006-01-02 15:04 UTC"))
	} else {
		header.WriteString(" · terminal snapshot, not the session transcript")
	}
	header.WriteString("\n\n")

	head := header.String()
	limit := github.MaxCommentRunes - len([]rune(head)) - len([]rune(truncationNote))

	body := []rune(turn.Markdown)
	truncated := false
	if len(body) > limit {
		body = body[:limit]
		truncated = true
	}

	out := head + string(body)
	if truncated {
		out += truncationNote
	}
	return out
}
