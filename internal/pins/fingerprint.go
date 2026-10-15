// Package pins records the tool definitions a person has approved, and spots
// the ones that changed since.
//
// A tool definition (description, input and output schemas, annotations) is
// what the model reads to decide how to call a tool. Changing it after
// approval, the "rug pull", is enough to steer an agent. Every definition is
// therefore fingerprinted the first time it is seen, and any later change waits
// for an explicit approval.
package pins

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Fingerprint returns the SHA-256 of a tool definition in canonical form:
// object keys sorted, insignificant whitespace removed, strings re-encoded
// uniformly, number literals kept as written. The _meta member is left out:
// servers may put volatile data there, and the model does not read it.
func Fingerprint(definition json.RawMessage) (string, error) {
	canonical, err := Canonical(definition)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Canonical returns the canonical form of a tool definition (see Fingerprint).
func Canonical(definition json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(definition))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if obj, ok := v.(map[string]any); ok {
		delete(obj, "_meta")
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case string:
		return writeString(buf, t)
	case json.Number:
		buf.WriteString(t.String())
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case nil:
		buf.WriteString("null")
	}
	return nil
}

func writeString(buf *bytes.Buffer, s string) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1) // Encode appends a newline
	return nil
}
