// Package policy decides what happens to tool lists, tool calls and tool
// results: allow, warn, block, or, in audit mode, record what would have been
// blocked.
package policy

import (
	"encoding/json"
	"fmt"

	"github.com/OlivMartins/vigie/internal/config"
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
}

// NewEngine builds the engine of a server. store may be nil when pinning is
// disabled.
func NewEngine(server string, p config.Policy, store *pins.Store, scanner *secrets.Scanner) *Engine {
	if !p.Pins.Enabled {
		store = nil
	}
	return &Engine{server: server, policy: p, pins: store, scanner: scanner}
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
		status, err := e.pins.Status(tool)
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
	if e.pins != nil && len(toPin) > 0 {
		obs, err := e.pins.Observe(toPin, e.policy.Pins.OnNew)
		for k, i := range toPinIndex {
			name := tools[i].Name
			if err != nil {
				perTool[i] = append(perTool[i], e.pinUnavailable(name, err))
				continue
			}
			o := obs[k]
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
