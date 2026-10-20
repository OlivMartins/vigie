// Package detect looks for instructions hidden in tool definitions.
//
// A model reads the whole definition of a tool, not just the part a person
// reviews: description, schema, parameter names. Tool poisoning exploits the
// difference. The checks here are deterministic heuristics, no model involved
// (see ADR 0009): each one targets a technique that legitimate descriptions
// have no reason to use, and the corpus under testdata measures both sides,
// attacks detected and false positives on real servers.
package detect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// Rule identifiers, recorded in telemetry as detections.<id>.
const (
	HiddenInstructions = "hidden_instructions"
	InvisibleText      = "invisible_text"
	SensitivePath      = "sensitive_path"
	ToolShadowing      = "tool_shadowing"
	Concealment        = "concealment"
)

// Rules lists every rule identifier.
var Rules = []string{HiddenInstructions, InvisibleText, SensitivePath, ToolShadowing, Concealment}

// MaxFindings bounds the findings reported for one definition, and
// maxLocation the length of a location: member names come from the server.
const (
	MaxFindings = 16
	maxLocation = 200
)

// Finding is one suspicious spot in a definition. Reason is a sentence for
// people and models alike; it never quotes the definition, so a report cannot
// replay the injection it describes.
type Finding struct {
	Rule     string
	Location string
	Reason   string
}

// Scan inspects every string of a tool definition, member names included,
// and reports where a rule matched, at most once per rule and location.
// other tells whether a name is that of another tool of the same server; it
// may be nil. _meta is skipped: it is not part of what a model reads.
func Scan(definition json.RawMessage, other func(name string) bool) []Finding {
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(definition))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return nil
	}
	delete(v, "_meta")
	var out []Finding
	walk("", v, func(loc, text string) bool {
		lower := strings.ToLower(text)
		for _, c := range checks {
			if len(out) >= MaxFindings {
				return false
			}
			if reason := c.run(text, lower, other); reason != "" {
				out = append(out, Finding{Rule: c.rule, Location: clip(loc), Reason: reason})
			}
		}
		return true
	})
	return out
}

func clip(loc string) string {
	if len(loc) <= maxLocation {
		return loc
	}
	return strings.ToValidUTF8(loc[:maxLocation], "") + "…"
}

// walk visits the strings of v in a stable order. visit returns false to stop.
func walk(path string, v any, visit func(loc, text string) bool) bool {
	switch t := v.(type) {
	case string:
		return visit(path, t)
	case []any:
		for i, e := range t {
			if !walk(fmt.Sprintf("%s[%d]", path, i), e, visit) {
				return false
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			loc := k
			if path != "" {
				loc = path + "." + k
			}
			if !visit(loc+" (member name)", k) || !walk(loc, t[k], visit) {
				return false
			}
		}
	}
	return true
}

type check struct {
	rule string
	// run gets the text and its lower-case form, which the expressions match.
	run func(text, lower string, other func(string) bool) string
}

// textCheck matches expressions on the lower-case text. A match always holds
// one of the keywords, so a substring search rules out most strings before an
// expression runs: this is on the path of every tools/list and tools/call.
func textCheck(rule, reason string, keywords []string, patterns ...string) check {
	res := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		res[i] = regexp.MustCompile(p)
	}
	return check{rule, func(_, lower string, _ func(string) bool) string {
		if !slices.ContainsFunc(keywords, func(k string) bool { return strings.Contains(lower, k) }) {
			return ""
		}
		for _, re := range res {
			if re.MatchString(lower) {
				return reason
			}
		}
		return ""
	}}
}

var checks = []check{
	textCheck(HiddenInstructions, "markup that addresses the model instead of describing the tool",
		[]string{"<", "inst]", "ignore", "disregard", "forget"},
		`</?\s*(?:important|system|instructions?|secret|hidden|override|assistant)\b[^>]*>|\[/?inst\]|<<\s*/?sys\s*>>|<!--`,
		`\b(?:ignore|disregard|forget)\b[^.\n]{0,30}\b(?:previous|prior|above|earlier|all)\b[^.\n]{0,30}\b(?:instructions?|rules|prompts?)\b`),
	{InvisibleText, invisibleText},
	textCheck(SensitivePath, "a reference to a credential or configuration file",
		[]string{".env", ".ssh", "id_", ".aws/", "/etc/", ".npmrc", ".netrc", ".git-credentials", ".kube/", ".gnupg", ".docker/", "mcp.json", "claude_desktop_config", "_history"},
		`(?:^|[\s"'`+"`"+`/\\])\.env(?:\.\w+)?\b`,
		`\.ssh\b|(?:^|[^a-z0-9])id_(?:rsa|dsa|ecdsa|ed25519)(?:$|[^a-z0-9])|\.aws/(?:credentials|config)|/etc/(?:passwd|shadow)|\.npmrc\b|\.netrc\b|\.git-credentials|\.kube/config|\.gnupg\b|\.docker/config\.json|\b(?:mcp|claude_desktop_config)\.json\b|\.(?:bash|zsh)_history`),
	{ToolShadowing, toolShadowing},
	textCheck(Concealment, "an instruction to hide something from the user",
		[]string{"user", "human", "operator", "anyone", "secret", "confidential", "covert", "stealth"},
		`\b(?:do not|don't|never|without)\b[^.\n]{0,40}\b(?:tell|inform|mention|reveal|disclose|notify|alert|show)\b[^.\n]{0,80}\b(?:user|human|operator|anyone)\b`,
		`\b(?:user|human)\b[^.\n]{0,20}\b(?:must not|should not|shouldn't|cannot|can't)\b[^.\n]{0,20}\b(?:see|know|notice|be told|be aware)\b`,
		`\bthis (?:is|must remain|should remain|must stay) (?:a )?(?:secret|confidential)\b|\b(?:secretly|covertly|stealthily)\b`),
}

// invisibleText finds characters a reviewer cannot see but a model reads:
// control characters, bidirectional overrides, zero-width characters and
// Unicode tag characters (which encode ASCII invisibly). Zero-width joiners
// and variation selectors stay: emoji and many scripts need them.
func invisibleText(text, _ string, _ func(string) bool) string {
	for _, r := range text {
		if isInvisible(r) {
			return fmt.Sprintf("an invisible character (U+%04X)", r)
		}
	}
	return ""
}

func isInvisible(r rune) bool {
	switch {
	case r < 0xA0: // ASCII and C1 controls: no table lookup for ordinary text
		return r < ' ' && r != '\t' && r != '\n' && r != '\r' || r >= 0x7F
	case r == '\t' || r == '\n' || r == '\r':
		return false
	case r == '‌' || r == '‍' || r == '︎' || r == '️':
		return false
	case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r):
		return true
	case r >= 0xE0100 && r <= 0xE01EF, r >= 0xFE00 && r <= 0xFE0D: // variation selectors
		return true
	}
	return false
}

var (
	sentenceEnd = regexp.MustCompile(`[.!?]\s+|\n`)
	// A sentence that sets conditions on, and obligations around, another tool.
	shadowing = regexp.MustCompile(`\b(?:when(?:ever)?|each time|every time|before|after|if)\b.*\b(?:always|must|should|also|never|first|instead|make sure|ensure|do not|don't)\b`)
	overrides = regexp.MustCompile(`\b(?:overrid(?:e|es|ing)|supersed(?:e|es|ing)|takes? precedence over)\b[^.\n]{0,40}\b(?:tools?|functions?|instructions?)\b`)
)

// toolShadowing finds a description that dictates how the model must use
// another tool: the poisoned tool never runs, yet it changes what the others
// do. Mentioning a sibling in passing ("use list_files first") is fine; it is
// the condition plus the obligation in the same sentence that gives it away.
func toolShadowing(text, lower string, other func(string) bool) string {
	if strings.Contains(lower, "overrid") || strings.Contains(lower, "supersed") || strings.Contains(lower, "precedence") {
		if overrides.MatchString(lower) {
			return "a description that overrides other tools"
		}
	}
	if other == nil || !mentions(text, other) {
		return ""
	}
	for _, sentence := range sentenceEnd.Split(text, -1) {
		if shadowing.MatchString(strings.ToLower(sentence)) && mentions(sentence, other) {
			return "a description that sets rules for how another tool must be used"
		}
	}
	return ""
}

// mentions reports whether text names another tool.
func mentions(text string, other func(string) bool) bool {
	return slices.ContainsFunc(strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.'
	}), func(w string) bool { return other(strings.Trim(w, ".-")) })
}
