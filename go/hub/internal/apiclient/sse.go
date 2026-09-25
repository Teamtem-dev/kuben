package apiclient

import (
	"strings"
	"unicode/utf8"
)

// SSEEvent is one server-sent event: its type (`message` when the event
// names none) and its data lines joined with newlines.
type SSEEvent struct {
	Event string
	Data  string
}

// SSEParser splits server-sent events out of the bytes as they come. The
// zero SSEParser is ready to use.
type SSEParser struct {
	buffer string
}

// Push returns the complete events in chunk and what came before it;
// comments (keep-alives) and events without data are skipped.
func (p *SSEParser) Push(chunk string) []SSEEvent {
	p.buffer += strings.ReplaceAll(chunk, "\r\n", "\n")
	var events []SSEEvent
	for {
		end := strings.Index(p.buffer, "\n\n")
		if end < 0 {
			return events
		}
		block := p.buffer[:end+2]
		p.buffer = p.buffer[end+2:]
		event, data := "message", []string(nil)
		for _, line := range lines(block) {
			field, value, found := strings.Cut(line, ":")
			if !found {
				field, value = line, ""
			}
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event = value
			case "data":
				data = append(data, value)
			}
		}
		if len(data) > 0 {
			events = append(events, SSEEvent{Event: event, Data: strings.Join(data, "\n")})
		}
	}
}

// lines splits text as Rust's str::lines does: at `\n`, a `\r` before it
// dropped, and no empty line after a final newline.
func lines(text string) []string {
	if text == "" {
		return nil
	}
	parts := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, part := range parts {
		parts[i] = strings.TrimSuffix(part, "\r")
	}
	return parts
}

// validUTF8Prefix is how many bytes at the start of b are whole UTF-8
// characters; the rest (a character cut by a chunk boundary) waits for the
// next chunk.
func validUTF8Prefix(b []byte) int {
	i := 0
	for i < len(b) {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			break
		}
		i += size
	}
	return i
}
