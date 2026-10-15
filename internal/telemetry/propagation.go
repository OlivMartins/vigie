package telemetry

import (
	"context"
	"encoding/json"
	"sort"

	"go.opentelemetry.io/otel/propagation"

	"github.com/OlivMartins/vigie/internal/jsonrpc"
)

// Propagator carries W3C trace context and baggage. MCP puts both in
// params._meta, under the unprefixed keys traceparent, tracestate and
// baggage (SEP-414).
var Propagator propagation.TextMapPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{}, propagation.Baggage{},
)

// metaCarrier adapts a params._meta object to the propagation API.
type metaCarrier map[string]json.RawMessage

func (c metaCarrier) Get(key string) string {
	v, ok := c[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

func (c metaCarrier) Set(key, value string) { c[key] = jsonrpc.Quote(value) }

func (c metaCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Extract returns ctx with the remote span context found in params._meta, if
// any.
func Extract(ctx context.Context, params map[string]json.RawMessage) context.Context {
	meta := metaOf(params)
	if meta == nil {
		return ctx
	}
	return Propagator.Extract(ctx, meta)
}

// Inject writes the span context of ctx into params._meta, replacing the
// context the request came with, and returns the updated params and whether
// they changed. A nil params map is created. A _meta member that is not an
// object is left alone: the peer will reject it, Vigie does not repair it.
func Inject(ctx context.Context, params map[string]json.RawMessage) (map[string]json.RawMessage, bool, error) {
	if params == nil {
		params = map[string]json.RawMessage{}
	}
	meta := metaCarrier{}
	if _, present := params["_meta"]; present {
		if meta = metaOf(params); meta == nil {
			return params, false, nil
		}
	}
	for _, f := range Propagator.Fields() {
		delete(meta, f)
	}
	Propagator.Inject(ctx, meta)
	raw, err := jsonrpc.Marshal(map[string]json.RawMessage(meta))
	if err != nil {
		return nil, false, err
	}
	params["_meta"] = raw
	return params, true, nil
}

func metaOf(params map[string]json.RawMessage) metaCarrier {
	v, ok := params["_meta"]
	if !ok {
		return nil
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(v, &meta) != nil || meta == nil {
		return nil
	}
	return meta
}
