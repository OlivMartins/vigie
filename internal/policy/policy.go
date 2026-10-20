// Package policy decides what happens to tool lists, tool calls and tool
// results: allow, warn, block, or, in audit mode, record what would have been
// blocked.
package policy

import (
	"encoding/json"
	"fmt"
	"hash/maphash"
	"slices"
	"sync"

	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/detect"
	"github.com/OlivMartins/vigie/internal/mcp"
	"github.com/OlivMartins/vigie/internal/pins"
	"github.com/OlivMartins/vigie/internal/secrets"
)

// Decision is the outcome of a check.
type Decision string

// Decisions, from least to most severe.
const (
	Allow Decision = "allow"
	Warn  Decision = "warn"
	// Audit means a rule would have blocked, but the policy runs in audit mode.
	Audit Decision = "audit"
	Block Decision = "block"
)

// Rule identifiers, recorded in telemetry and logs.
const (
	RuleToolInvalid    = "tools.invalid"
	RuleToolDenied     = "tools.deny"
	RuleToolNotAllowed = "tools.allow"
	RulePinChanged     = "pins.changed"
	RulePinPending     = "pins.pending"
	RulePinUnavailable = "pins.unavailable"
	RuleSecretArgument = "secrets.arguments"
	RuleSecretResult   = "secrets.results"
	// RuleDetections prefixes the rules of the heuristics on tool
	// definitions: detections.hidden_instructions, detections.invisible_text...
	RuleDetections = "detections."
)

// Finding is one rule that matched. Reason is a single sentence meant for
// people and models alike; it never contains a credential.
type Finding struct {
	Rule       string
	Action     config.Action
	Tool       string
	Location   string
	SecretType string
	Reason     string
}

// Verdict is the decision with the findings behind it.
type Verdict struct {
	Decision Decision
	Findings []Finding
	// PinStatus is the pin status of the tool a call targets, empty when
	// pinning is disabled.
	PinStatus pins.Status
}

// Primary returns the finding that drove the decision: the first blocking
// one, else the first warning, else nil.
func (v Verdict) Primary() *Finding {
	for i := range v.Findings {
		if v.Findings[i].Action == config.ActionBlock {
			return &v.Findings[i]
		}
	}
	if len(v.Findings) > 0 {
		return &v.Findings[0]
	}
	return nil
}

// Engine applies the policy of one upstream server.
type Engine struct {
	server  string
	policy  config.Policy
	pins    *pins.Store
	scanner *secrets.Scanner

	// scans memoizes the heuristics: a tools/call must not pay for them again.
	seed  maphash.Seed
	mu    sync.Mutex
	scans map[scanKey][]detect.Finding
}

type scanKey struct {
	definition, tools uint64
}

// maxScans bounds the memo; it starts over when full.
const maxScans = 1024

// NewEngine builds the engine of a server. store may be nil when pinning is
// disabled.
func NewEngine(server string, p config.Policy, store *pins.Store, scanner *secrets.Scanner) *Engine {
	if !p.Pins.Enabled {
		store = nil
	}
	return &Engine{server: server, policy: p, pins: store, scanner: scanner, seed: maphash.MakeSeed(), scans: map[scanKey][]detect.Finding{}}
}

// Policy returns the rules in force.
func (e *Engine) Policy() config.Policy { return e.policy }

// Server returns the upstream server name.
func (e *Engine) Server() string { return e.server }

// DisplayName returns a tool name fit for logs, telemetry and messages: a
// name that holds a credential is replaced by [REDACTED].
func (e *Engine) DisplayName(tool string) string {
	if e.scanner.Contains(tool) {
		return "[REDACTED]"
	}
	return tool
}

// CheckCall decides whether a tools/call request may reach the server. An
// empty tool stands for a name that is missing or not a string.
func (e *Engine) CheckCall(tool string, arguments json.RawMessage) Verdict {
	var findings []Finding
	var pinStatus pins.Status
	if tool == "" {
		findings = append(findings, Finding{
			Rule: RuleToolInvalid, Action: config.ActionBlock,
			Reason: "the request has no tool name, or a name that is not a string",
		})
		return e.verdict(findings)
	}
	if e.policy.Secrets.Arguments != config.ActionOff {
		for _, s := range e.scanner.ScanJSON("name", jsonString(tool)) {
			findings = append(findings, Finding{
				Rule: RuleSecretArgument, Action: e.policy.Secrets.Arguments, Tool: "[REDACTED]",
				Location: "name", SecretType: s.Rule,
				Reason: fmt.Sprintf("the tool name looks like %s", s.Label),
			})
		}
	}
	tool = e.DisplayName(tool)
	if f := e.checkName(tool); f != nil {
		findings = append(findings, *f)
	}
	if e.pins != nil {
		file, err := e.pins.Load()
		var entry *pins.Entry
		status := pins.StatusUnknown
		if err == nil {
			entry = file.Tools[tool]
			status = entry.Status()
		}
		pinStatus = status
		switch {
		case err != nil:
			findings = append(findings, e.pinUnavailable(tool, err))
		case status == pins.StatusChanged:
			findings = append(findings, e.pinChanged(tool))
		case status == pins.StatusPending:
			findings = append(findings, e.pinPending(tool))
		case status == pins.StatusUnknown && !e.pins.Approves(e.policy.Pins.OnNew):
			findings = append(findings, Finding{
				Rule: RulePinPending, Action: config.ActionBlock, Tool: tool,
				Reason: fmt.Sprintf("tool %q was never listed by server %q, so it was never approved", tool, e.server),
			})
		}
		// A client may call a tool from a list it kept: check the definition
		// the model can have seen, not only the lists that go through Vigie.
		// A person who approved it has accepted what it says.
		if snap := latest(entry); snap != nil && snap.ApprovedBy != pins.ApprovedByCommand {
			set := toolSet{has: func(n string) bool { return file.Tools[n] != nil }}
			for n := range file.Tools {
				set.sum += maphash.String(e.seed, n)
			}
			findings = append(findings, e.detections(tool, snap.Definition, set)...)
		}
	}
	if e.policy.Secrets.Arguments != config.ActionOff {
		for _, s := range e.scanner.ScanJSON("arguments", arguments) {
			findings = append(findings, Finding{
				Rule: RuleSecretArgument, Action: e.policy.Secrets.Arguments, Tool: tool,
				Location: s.Location, SecretType: s.Rule,
				Reason: fmt.Sprintf("%s looks like %s", s.Location, s.Label),
			})
		}
	}
	v := e.verdict(findings)
	v.PinStatus = pinStatus
	return v
}

// ListOutcome is the result of checking a tools/list result.
type ListOutcome struct {
	Verdict Verdict
	// Kept are the tools the client gets to see.
	Kept []mcp.Tool
	// Hidden counts the tools removed from the list.
	Hidden int
	// New, Changed and Pending count tools by pin status.
	New, Changed, Pending int
}

// CheckList filters a tools/list result: tools the policy does not allow,
// and tools whose definition is not approved, are hidden from the client.
// Every definition goes through the pin file on the way.
func (e *Engine) CheckList(tools []mcp.Tool) ListOutcome {
	var out ListOutcome
	perTool := make(map[int][]Finding)
	var toPin []pins.Observed
	var toPinIndex []int
	listed := make(map[string]bool, len(tools))
	set := toolSet{has: func(n string) bool { return listed[n] }}
	for _, t := range tools {
		listed[t.Name] = true
		set.sum += maphash.String(e.seed, t.Name)
	}
	for i, t := range tools {
		if t.Name == "" || e.scanner.Contains(t.Name) {
			// Without a readable name a definition cannot be pinned: clients
			// may still accept it, so it is hidden.
			perTool[i] = append(perTool[i], Finding{
				Rule: RuleToolInvalid, Action: config.ActionBlock, Tool: e.DisplayName(t.Name),
				Reason: "a tool definition without a valid name cannot be pinned",
			})
			continue
		}
		if f := e.checkName(t.Name); f != nil {
			perTool[i] = append(perTool[i], *f)
			continue // not exposed anyway: keep it out of the pin file
		}
		toPin = append(toPin, pins.Observed{Name: t.Name, Definition: t.Definition})
		toPinIndex = append(toPinIndex, i)
	}
	// A new tool that trips a detection is not approved on sight: it waits
	// for a person, who reviews it with vigie pins diff.
	detected := make([][]Finding, len(toPin))
	for k, i := range toPinIndex {
		detected[k] = e.detections(tools[i].Name, tools[i].Definition, set)
		toPin[k].Hold = e.policy.Mode == config.ModeEnforce && slices.ContainsFunc(detected[k], func(f Finding) bool {
			return f.Action == config.ActionBlock
		})
	}
	if e.pins != nil && len(toPin) > 0 {
		obs, err := e.pins.Observe(toPin, e.policy.Pins.OnNew)
		for k, i := range toPinIndex {
			name := tools[i].Name
			if err != nil {
				perTool[i] = append(perTool[i], e.pinUnavailable(name, err))
				continue
			}
			o := obs[k]
			if o.Reviewed {
				detected[k] = nil
			}
			if o.New {
				out.New++
			}
			switch o.Status {
			case pins.StatusChanged:
				out.Changed++
				perTool[i] = append(perTool[i], e.pinChanged(name))
			case pins.StatusPending:
				out.Pending++
				perTool[i] = append(perTool[i], e.pinPending(name))
			}
		}
	}
	for k, i := range toPinIndex {
		perTool[i] = append(perTool[i], detected[k]...)
	}
	var all []Finding
	for i, t := range tools {
		findings := perTool[i]
		all = append(all, findings...)
		if e.verdict(findings).Decision == Block {
			out.Hidden++
			continue
		}
		out.Kept = append(out.Kept, t)
	}
	out.Verdict = e.verdict(all)
	return out
}

// CheckResult inspects a tools/call result before it reaches the model.
// Results are never blocked: findings are recorded as warnings.
func (e *Engine) CheckResult(tool string, result json.RawMessage) Verdict {
	if e.policy.Secrets.Results == config.ActionOff {
		return Verdict{Decision: Allow}
	}
	var findings []Finding
	for _, s := range e.scanner.ScanJSON("result", result) {
		findings = append(findings, Finding{
			Rule: RuleSecretResult, Action: config.ActionWarn, Tool: tool,
			Location: s.Location, SecretType: s.Rule,
			Reason: fmt.Sprintf("%s looks like %s", s.Location, s.Label),
		})
	}
	return e.verdict(findings)
}

// toolSet describes the tools of a server: membership, and a digest that
// tells two sets apart (the sum of the hashes of their names).
type toolSet struct {
	has func(name string) bool
	sum uint64
}

// latest returns the definition a client can see: the one waiting for
// approval if there is one, else the approved one.
func latest(e *pins.Entry) *pins.Snapshot {
	switch {
	case e == nil:
		return nil
	case e.Pending != nil:
		return e.Pending
	default:
		return e.Approved
	}
}

// detections runs the heuristics on a tool definition.
func (e *Engine) detections(tool string, definition json.RawMessage, tools toolSet) []Finding {
	action := e.policy.Detections.Action
	if action == config.ActionOff {
		return nil
	}
	found := e.scan(tool, definition, tools)
	var out []Finding
	for _, d := range found {
		if e.exempt(tool, d.Rule) {
			continue
		}
		out = append(out, Finding{
			Rule: RuleDetections + d.Rule, Action: action, Tool: tool, Location: d.Location,
			Reason: fmt.Sprintf("the definition of tool %q holds %s (review: vigie pins diff %s %s, or exempt it in detections.exempt)", tool, d.Reason, e.server, tool),
		})
	}
	return out
}

// scan memoizes detect.Scan by definition and by the set of tools around it,
// which tool shadowing depends on. The memo is keyed by a seeded hash: a
// server cannot craft a definition that collides with a clean one.
func (e *Engine) scan(tool string, definition json.RawMessage, tools toolSet) []detect.Finding {
	key := scanKey{maphash.Bytes(e.seed, definition), tools.sum}
	e.mu.Lock()
	found, ok := e.scans[key]
	e.mu.Unlock()
	if ok {
		return found
	}
	found = detect.Scan(definition, func(n string) bool { return n != tool && tools.has(n) })
	e.mu.Lock()
	if len(e.scans) >= maxScans {
		clear(e.scans)
	}
	e.scans[key] = found
	e.mu.Unlock()
	return found
}

func (e *Engine) exempt(tool, rule string) bool {
	for pattern, rules := range e.policy.Detections.Exempt {
		if Match(pattern, tool) && (slices.Contains(rules, "*") || slices.Contains(rules, rule)) {
			return true
		}
	}
	return false
}

func jsonString(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

func (e *Engine) checkName(tool string) *Finding {
	if p, ok := MatchAny(e.policy.Tools.Deny, tool); ok {
		return &Finding{
			Rule: RuleToolDenied, Action: config.ActionBlock, Tool: tool,
			Reason: fmt.Sprintf("tool %q is denied by the policy of server %q (pattern %q)", tool, e.server, p),
		}
	}
	if _, ok := MatchAny(e.policy.Tools.Allow, tool); !ok {
		return &Finding{
			Rule: RuleToolNotAllowed, Action: config.ActionBlock, Tool: tool,
			Reason: fmt.Sprintf("tool %q is not in the allow list of server %q", tool, e.server),
		}
	}
	return nil
}

func (e *Engine) pinChanged(tool string) Finding {
	return Finding{
		Rule: RulePinChanged, Action: e.policy.Pins.OnChange, Tool: tool,
		Reason: fmt.Sprintf("the definition of tool %q changed since it was approved (review: vigie pins diff %s %s)", tool, e.server, tool),
	}
}

func (e *Engine) pinPending(tool string) Finding {
	return Finding{
		Rule: RulePinPending, Action: config.ActionBlock, Tool: tool,
		Reason: fmt.Sprintf("tool %q is waiting for approval (review: vigie pins diff %s %s)", tool, e.server, tool),
	}
}

func (e *Engine) pinUnavailable(tool string, err error) Finding {
	return Finding{
		Rule: RulePinUnavailable, Action: config.ActionBlock, Tool: tool,
		Reason: fmt.Sprintf("tool %q cannot be verified: the pin file of server %q is unreadable (%v)", tool, e.server, err),
	}
}

func (e *Engine) verdict(findings []Finding) Verdict {
	d := Allow
	for _, f := range findings {
		switch f.Action {
		case config.ActionBlock:
			if e.policy.Mode == config.ModeAudit {
				d = maxDecision(d, Audit)
			} else {
				d = Block
			}
		case config.ActionWarn:
			d = maxDecision(d, Warn)
		}
	}
	return Verdict{Decision: d, Findings: findings}
}

func maxDecision(a, b Decision) Decision {
	if b.Severity() > a.Severity() {
		return b
	}
	return a
}

// Severity orders decisions: allow < warn < audit < block.
func (d Decision) Severity() int {
	switch d {
	case Warn:
		return 1
	case Audit:
		return 2
	case Block:
		return 3
	default:
		return 0
	}
}
