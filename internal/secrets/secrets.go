// Package secrets finds credentials in JSON values: tool arguments on their
// way to a server, tool results on their way to a model.
//
// Rules only match formats with a recognizable structure (prefixes, fixed
// lengths, PEM headers), to stay quiet on ordinary text. Go regular
// expressions run in linear time, so a crafted argument cannot stall the
// proxy.
package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Rule detects one kind of credential.
type Rule struct {
	// ID names the credential type in telemetry, such as aws_access_key_id.
	ID string
	// Label describes it in messages read by people and models.
	Label string
	// keywords are literals one of which every match contains: a fast
	// substring search rules out most text before the expression runs.
	keywords []string
	re       *regexp.Regexp
}

func (r *Rule) candidate(s string) bool {
	for _, k := range r.keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

var defaultRules = []Rule{
	{ID: "private_key", Label: "a private key", keywords: []string{"PRIVATE KEY"},
		re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{ID: "aws_access_key_id", Label: "an AWS access key", keywords: []string{"AKIA", "ASIA", "ABIA", "ACCA"},
		re: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`)},
	{ID: "github_token", Label: "a GitHub token", keywords: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"},
		re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})\b`)},
	{ID: "gitlab_token", Label: "a GitLab token", keywords: []string{"glpat-"},
		re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{ID: "slack_token", Label: "a Slack token", keywords: []string{"xox"},
		re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{ID: "stripe_secret_key", Label: "a Stripe secret key", keywords: []string{"_live_"},
		re: regexp.MustCompile(`\b[rs]k_live_[A-Za-z0-9]{20,}`)},
	{ID: "anthropic_api_key", Label: "an Anthropic API key", keywords: []string{"sk-ant-"},
		re: regexp.MustCompile(`\bsk-ant-[a-z]+[0-9]{2}-[A-Za-z0-9_-]{80,}`)},
	{ID: "openai_api_key", Label: "an OpenAI API key", keywords: []string{"T3BlbkFJ"},
		re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}T3BlbkFJ[A-Za-z0-9_-]{20,}`)},
	{ID: "google_api_key", Label: "a Google API key", keywords: []string{"AIza"},
		re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{ID: "jwt", Label: "a JSON Web Token", keywords: []string{"eyJ"},
		re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{16,}`)},
	{ID: "url_credentials", Label: "credentials embedded in a URL", keywords: []string{"://"},
		re: regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]{1,20}://[^\s/:@"]{1,128}:[^\s/@"]{1,256}@[^\s/"]`)},
}

// Finding is one credential found in a JSON value. It never holds the
// credential itself: findings end up in logs and telemetry.
type Finding struct {
	Rule     string
	Label    string
	Location string
}

// Scanner applies a set of rules.
type Scanner struct {
	rules []Rule
}

// NewScanner returns a scanner with the built-in rules.
func NewScanner() *Scanner { return &Scanner{rules: defaultRules} }

// Rules lists the rule IDs, for documentation and tests.
func (s *Scanner) Rules() []string {
	ids := make([]string, 0, len(s.rules))
	for _, r := range s.rules {
		ids = append(ids, r.ID)
	}
	return ids
}

// MaxFindings bounds the findings reported for one value: enough to explain a
// decision, not enough to flood a span.
const MaxFindings = 16

// ScanJSON walks every string in value, member names included, and reports
// the credentials it finds. root prefixes the reported locations, such as
// "arguments". The value is read token by token, so a member given twice is
// scanned twice: what reaches the peer is the raw text, not one decoding of
// it. Values that are not valid JSON are scanned as plain text.
func (s *Scanner) ScanJSON(root string, value json.RawMessage) []Finding {
	if len(value) == 0 || s.clearlyClean(value) {
		return nil
	}
	var out []Finding
	err := s.walkTokens(root, value, func(path, str string, isKey bool) bool {
		if isKey {
			path += " (member name)"
		}
		out = s.scanString(path, str, out)
		return len(out) < MaxFindings
	})
	if err != nil {
		return s.scanString(root, string(value), out)
	}
	return out
}

// Contains reports whether text holds a credential.
func (s *Scanner) Contains(text string) bool {
	return len(s.scanString("", text, nil)) > 0
}

// RedactText replaces every credential in text by [REDACTED:<rule>].
func (s *Scanner) RedactText(text string) string { return s.redactString(text) }

func (s *Scanner) scanString(path, str string, out []Finding) []Finding {
	for i := range s.rules {
		r := &s.rules[i]
		if len(out) >= MaxFindings {
			return out
		}
		if r.candidate(str) && r.re.MatchString(str) {
			out = append(out, Finding{Rule: r.ID, Label: r.Label, Location: path})
		}
	}
	return out
}

// Redact returns value with every credential replaced by [REDACTED:<rule>].
// It is applied before any content is attached to telemetry.
func (s *Scanner) Redact(value json.RawMessage) json.RawMessage {
	if len(value) == 0 || s.clearlyClean(value) {
		return value
	}
	v, err := decode(value)
	if err != nil {
		return json.RawMessage(strconv.Quote(s.redactString(string(value))))
	}
	out, err := marshal(redactValue(v, s.redactString))
	if err != nil {
		return nil
	}
	return out
}

// clearlyClean reports whether raw JSON cannot hold a credential: no rule
// keyword appears in it, and it has no escape sequence that could spell one
// in disguise ("AK\u0049A..."). Most tool traffic takes this path and is
// never decoded.
func (s *Scanner) clearlyClean(raw []byte) bool {
	if bytes.IndexByte(raw, '\\') >= 0 {
		return false
	}
	for i := range s.rules {
		for _, k := range s.rules[i].keywords {
			if bytes.Contains(raw, []byte(k)) {
				return false
			}
		}
	}
	return true
}

func (s *Scanner) redactString(str string) string {
	for i := range s.rules {
		if r := &s.rules[i]; r.candidate(str) {
			str = r.re.ReplaceAllLiteralString(str, "[REDACTED:"+r.ID+"]")
		}
	}
	return str
}

func decode(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

type frame struct {
	object  bool
	path    string
	index   int
	key     string
	wantKey bool
}

// walkTokens visits every string of raw, member names included, in document
// order, with its location. A member name that holds a credential is shown
// as [REDACTED] in the locations below it. visit returns false to stop.
func (s *Scanner) walkTokens(root string, raw json.RawMessage, visit func(path, str string, isKey bool) bool) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var stack []frame
	path := func() string {
		if len(stack) == 0 {
			return root
		}
		top := &stack[len(stack)-1]
		if top.object {
			if s.Contains(top.key) {
				return top.path + "[REDACTED]"
			}
			return child(top.path, top.key)
		}
		return top.path + "[" + strconv.Itoa(top.index) + "]"
	}
	done := func() {
		if len(stack) == 0 {
			return
		}
		if top := &stack[len(stack)-1]; top.object {
			top.wantKey = true
		} else {
			top.index++
		}
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].wantKey {
			if key, ok := tok.(string); ok {
				stack[n-1].key, stack[n-1].wantKey = key, false
				if !visit(stack[n-1].path, key, true) {
					return nil
				}
				continue
			}
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, frame{object: true, path: path(), wantKey: true})
			case '[':
				stack = append(stack, frame{path: path()})
			default: // '}' or ']'
				stack = stack[:len(stack)-1]
				done()
			}
		case string:
			if !visit(path(), t, false) {
				return nil
			}
			done()
		default:
			done()
		}
	}
}

func redactValue(v any, redact func(string) string) any {
	switch t := v.(type) {
	case string:
		return redact(t)
	case []any:
		for i := range t {
			t[i] = redactValue(t[i], redact)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[redact(k)] = redactValue(v, redact)
		}
		return out
	default:
		return v
	}
}

// child renders a location step: .name for plain keys, ["key"] otherwise.
func child(path, key string) string {
	if isIdentifier(key) {
		if path == "" {
			return key
		}
		return path + "." + key
	}
	return path + "[" + strconv.Quote(key) + "]"
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) < 0
}
