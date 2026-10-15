package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrAmbiguous reports an object that peers may read differently: a member
// given twice, or a member whose name differs from one Vigie inspects only by
// case. Go's encoding/json matches struct fields case-insensitively and keeps
// the last duplicate; other decoders keep the first, or reject the object.
// Vigie cannot know which reading the peer makes, so it refuses to guess.
var ErrAmbiguous = errors.New("ambiguous JSON object")

// CheckKeys returns an ErrAmbiguous error when the JSON object raw has a
// duplicate member, or a member whose name equals one of inspected under case
// folding without being equal to it. Values that are not objects pass.
func CheckKeys(raw json.RawMessage, inspected ...string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil
	}
	seen := make(map[string]bool)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		if seen[key] {
			return fmt.Errorf("%w: member %q appears twice", ErrAmbiguous, key)
		}
		seen[key] = true
		for _, name := range inspected {
			if key != name && strings.EqualFold(key, name) {
				return fmt.Errorf("%w: member %q differs from %q only by case", ErrAmbiguous, key, name)
			}
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	return nil
}

// Envelope lists the members of a JSON-RPC message.
var Envelope = []string{"jsonrpc", "id", "method", "params", "result", "error"}

// Original returns the bytes the message was read from, nil for a message
// built or modified by Vigie.
func (m *Message) Original() []byte { return m.raw }
