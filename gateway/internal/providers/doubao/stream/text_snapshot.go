package stream

import (
	"encoding/json"
	"strings"
)

// TextSnapshot is private response content, never a log or audit record.
type TextSnapshot struct {
	Text         string
	Acknowledged bool
}

func (TextSnapshot) String() string   { return "doubao.stream.TextSnapshot[redacted]" }
func (TextSnapshot) GoString() string { return "doubao.stream.TextSnapshot[redacted]" }

func (s *State) textSnapshot(acknowledged bool) (TextSnapshot, error) {
	var output strings.Builder
	for _, id := range s.Order {
		message := s.Messages[id]
		if message.UserType != 2 {
			continue
		}
		for _, block := range message.Blocks {
			key, field := "", ""
			switch block.Type {
			case 10000:
				key, field = "text_block", "text"
			case 10008:
				key, field = "code_block", "code"
			default:
				continue
			}
			raw := block.Content[key]
			if raw == nil {
				continue
			}
			var value map[string]json.RawMessage
			if decode(raw, &value) != nil {
				return TextSnapshot{}, ErrMalformed
			}
			if value[field] == nil {
				continue
			}
			var text string
			if decode(value[field], &text) != nil {
				return TextSnapshot{}, ErrMalformed
			}
			if output.Len()+len(text) > s.limits.StateBytes {
				return TextSnapshot{}, ErrLimit
			}
			output.WriteString(text)
		}
	}
	return TextSnapshot{Text: output.String(), Acknowledged: acknowledged}, nil
}
