package thread

import (
	"fmt"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
	"github.com/DnzzL/herdr-huddle/internal/transcript"
)

// reader is one agent kind's transcript format: how to decode its records, how
// to render them, and where its cursor sits.
//
// Each kind implements its own because the on-disk shapes are different enough
// that one parser with branches inside it would be two parsers wearing a coat
// (ADR-004). Keeping them behind one interface is what lets the rest of the
// program not care which agent it is talking about.
type reader interface {
	// render is the Markdown for everything after the cursor, the cursor
	// itself, and whether the cursor was found in the records at all.
	render(data []byte, after string, opts transcript.Options) (transcript.Result, error)
	// endCursor is the position at the end of the records, used to prime a
	// share so its first comment starts where the conversation is now.
	endCursor(data []byte) (string, error)
}

// readerFor returns the reader for an agent kind.
//
// An empty kind means Claude Code: a share recorded before there was a second
// adapter has no kind stored, and Claude Code was the only one there was.
func readerFor(kind string) (reader, error) {
	switch kind {
	case herdr.KindPi:
		return piReader{}, nil
	case "", herdr.KindClaude:
		return claudeReader{}, nil
	}
	return nil, fmt.Errorf("thread: no transcript adapter for agent kind %q", kind)
}

// claudeReader reads a Claude Code session file: `type: user|assistant`
// records, blocks named text/tool_use/tool_result, and a uuid per record.
type claudeReader struct{}

func (claudeReader) render(data []byte, after string, opts transcript.Options) (transcript.Result, error) {
	records, err := transcript.Parse(data)
	if err != nil {
		return transcript.Result{}, err
	}
	return transcript.Render(records, after, opts), nil
}

func (claudeReader) endCursor(data []byte) (string, error) {
	records, err := transcript.Parse(data)
	if err != nil {
		return "", err
	}
	return transcript.LastID(records), nil
}

// piReader reads a pi session file: `type: message` records whose role decides
// how they render, a separate toolResult record per call, and an id per record.
type piReader struct{}

func (piReader) render(data []byte, after string, opts transcript.Options) (transcript.Result, error) {
	records, err := transcript.ParsePi(data)
	if err != nil {
		return transcript.Result{}, err
	}
	return transcript.RenderPi(records, after, opts), nil
}

func (piReader) endCursor(data []byte) (string, error) {
	records, err := transcript.ParsePi(data)
	if err != nil {
		return "", err
	}
	return transcript.LastPiID(records), nil
}
