package telemetry

import "go.opentelemetry.io/otel/attribute"

// Attribute names. MCP, JSON-RPC and GenAI names follow the OpenTelemetry
// semantic conventions for MCP
// (https://github.com/open-telemetry/semantic-conventions-genai, status
// Development). Keeping every name in one place makes the next rename a
// one-line change.
const (
	MCPMethodName          = attribute.Key("mcp.method.name")
	MCPProtocolVersion     = attribute.Key("mcp.protocol.version")
	MCPResourceURI         = attribute.Key("mcp.resource.uri")
	JSONRPCRequestID       = attribute.Key("jsonrpc.request.id")
	JSONRPCProtocolVersion = attribute.Key("jsonrpc.protocol.version")
	RPCResponseStatusCode  = attribute.Key("rpc.response.status_code")
	ErrorType              = attribute.Key("error.type")
	NetworkTransport       = attribute.Key("network.transport")
	GenAIOperationName     = attribute.Key("gen_ai.operation.name")
	GenAIToolName          = attribute.Key("gen_ai.tool.name")
	GenAIPromptName        = attribute.Key("gen_ai.prompt.name")
	GenAIToolCallArguments = attribute.Key("gen_ai.tool.call.arguments") // opt-in content
	GenAIToolCallResult    = attribute.Key("gen_ai.tool.call.result")    // opt-in content
)

// Vigie attributes: what the conventions do not cover (policy decisions, pin
// status, tool list filtering).
const (
	// VigieUpstream names the upstream server, as given to --server.
	VigieUpstream = attribute.Key("vigie.upstream.name")
	// VigieDecision is allow, warn, audit or block.
	VigieDecision = attribute.Key("vigie.decision")
	// VigieRule is the rule behind the decision, such as secrets.arguments.
	VigieRule = attribute.Key("vigie.rule")
	// VigieMode is enforce or audit.
	VigieMode = attribute.Key("vigie.mode")
	// VigiePinStatus is approved, changed, pending or unknown.
	VigiePinStatus = attribute.Key("vigie.pin.status")
	// VigieResultType is the resultType of a result that is not complete,
	// such as input_required (multi round-trip requests, 2026-07-28).
	VigieResultType = attribute.Key("vigie.result_type")

	VigieToolsListed  = attribute.Key("vigie.tools.listed")
	VigieToolsHidden  = attribute.Key("vigie.tools.hidden")
	VigieToolsNew     = attribute.Key("vigie.tools.new")
	VigieToolsChanged = attribute.Key("vigie.tools.changed")
	VigieToolsPending = attribute.Key("vigie.tools.pending")

	// Attributes of the vigie.finding span event.
	VigieFindingAction   = attribute.Key("vigie.finding.action")
	VigieFindingLocation = attribute.Key("vigie.finding.location")
	VigieFindingReason   = attribute.Key("vigie.finding.reason")
	VigieSecretType      = attribute.Key("vigie.secret.type")
)

// EventFinding is the span event recorded for each rule that matched.
const EventFinding = "vigie.finding"

// Well-known values.
const (
	OperationExecuteTool = "execute_tool"
	TransportPipe        = "pipe"
	// ErrorToolError is the error.type of a tool result with isError set.
	ErrorToolError = "tool_error"
	// Error types for calls that ended without a response.
	ErrorCancelled      = "cancelled"
	ErrorClientClosed   = "client_closed"
	ErrorUpstreamClosed = "upstream_closed"
	ErrorShutdown       = "shutdown"
	ErrorDuplicateID    = "duplicate_id"
)

// Metric names.
const (
	MetricServerDuration = "mcp.server.operation.duration"
	MetricClientDuration = "mcp.client.operation.duration"
	MetricDecisions      = "vigie.decisions"
)

// DurationBuckets are the bucket boundaries the MCP conventions recommend for
// operation durations, in seconds.
var DurationBuckets = []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}
