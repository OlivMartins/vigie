#!/usr/bin/env python3
"""Check Vigie's spans against Sonde: allow-list coverage and SQL queries.

Replays the spans of the demo (written by `go run ./examples/demo -traces FILE`)
into an embedded ClickHouse (chDB) created from Sonde's own schema, the way
Sonde's collector writes them, then runs every SQL block of docs/sonde.md.

Usage:
    python3 scripts/sonde_check.py --sonde ../sonde --traces /tmp/traces.jsonl

Requirements: pip install chdb pyyaml
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import tempfile
from datetime import UTC, datetime
from pathlib import Path

import yaml
from chdb import session

ROOT = Path(__file__).resolve().parents[1]
KIND = {1: "Internal", 2: "Server", 3: "Client", 4: "Producer", 5: "Consumer"}
INVALID_SPAN = "0000000000000000"

# What each query must find in the demo's spans: a missing value means the
# query, the schema or Vigie's attributes drifted apart.
EXPECTED = [
    ("secrets.arguments", "block"),  # decisions per rule
    ("send_reminder", "secrets.arguments"),  # blocked calls
    ("get_invoice",),  # changed definitions
    ("secrets.arguments", "aws_access_key_id", "send_reminder", "arguments.body"),  # secrets
    ("get_invoice",),  # latency
    ("get_invoice",),  # server errors
    ("2026-07-28",),  # protocol versions
]


def sql_statements(path: Path) -> list[str]:
    text = "\n".join(line for line in path.read_text().splitlines() if not line.lstrip().startswith("--"))
    return [stmt.strip() for stmt in text.split(";") if stmt.strip()]


def as_string(value) -> str:
    """pcommon.Value.AsString(), what the ClickHouse exporter stores in maps."""
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (list, dict)):
        return json.dumps(value, separators=(",", ":"))
    return str(value)


def attributes(kvs) -> dict[str, str]:
    return {kv["Key"]: as_string(kv["Value"]["Value"]) for kv in kvs or []}


def timestamp(value: str) -> str:
    # 2026-10-15T10:01:30.123456789Z -> ClickHouse DateTime64(9) text
    main, _, rest = value.rstrip("Z").partition(".")
    nanos = (rest + "000000000")[:9]
    return main.replace("T", " ") + "." + nanos


def nanoseconds(value: str) -> int:
    main, _, rest = value.rstrip("Z").partition(".")
    dt = datetime.strptime(main, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=UTC)
    return int(dt.timestamp()) * 1_000_000_000 + int((rest + "000000000")[:9])


def to_row(span: dict, allowed: set[str]) -> dict:
    resource = attributes(span.get("Resource"))
    keep = lambda attrs: {k: v for k, v in attrs.items() if k in allowed}  # noqa: E731
    parent = span.get("Parent", {}).get("SpanID", INVALID_SPAN)
    events = span.get("Events") or []
    return {
        "Timestamp": timestamp(span["StartTime"]),
        "TraceId": span["SpanContext"]["TraceID"],
        "SpanId": span["SpanContext"]["SpanID"],
        "ParentSpanId": "" if parent == INVALID_SPAN else parent,
        "TraceState": "",
        "SpanName": span["Name"],
        "SpanKind": KIND[span["SpanKind"]],
        "ServiceName": resource.get("service.name", ""),
        "ResourceAttributes": keep(resource),
        "ScopeName": span["InstrumentationScope"]["Name"],
        "ScopeVersion": span["InstrumentationScope"].get("Version", ""),
        "SpanAttributes": keep(attributes(span.get("Attributes"))),
        "Duration": nanoseconds(span["EndTime"]) - nanoseconds(span["StartTime"]),
        "StatusCode": span["Status"]["Code"],
        "StatusMessage": span["Status"].get("Description", ""),
        "Events.Timestamp": [timestamp(e["Time"]) for e in events],
        "Events.Name": [e["Name"] for e in events],
        "Events.Attributes": [keep(attributes(e.get("Attributes"))) for e in events],
        "Links.TraceId": [],
        "Links.SpanId": [],
        "Links.TraceState": [],
        "Links.Attributes": [],
    }


def emitted_keys(spans: list[dict]) -> set[str]:
    keys: set[str] = set()
    for s in spans:
        keys |= {kv["Key"] for kv in s.get("Attributes") or []}
        keys |= {kv["Key"] for kv in s.get("Resource") or []}
        for e in s.get("Events") or []:
            keys |= {kv["Key"] for kv in e.get("Attributes") or []}
    return keys


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--sonde", type=Path, required=True, help="path to a Sonde checkout")
    parser.add_argument("--traces", type=Path, required=True, help="spans written by the demo")
    args = parser.parse_args()

    spans = [json.loads(line) for line in args.traces.read_text().splitlines() if line.strip()]
    collector = yaml.safe_load((args.sonde / "otel-collector" / "config.yaml").read_text())
    sonde_keys = set(collector["processors"]["redaction/genai"]["allowed_keys"])
    vigie_keys = set(yaml.safe_load((ROOT / "integrations" / "sonde" / "allowed-keys.yaml").read_text()))
    allowed = sonde_keys | vigie_keys

    # 1. Every key Vigie emits survives Sonde's allow-list once the snippet is added.
    dropped = sorted(k for k in emitted_keys(spans) if k not in allowed and not k.startswith(("host.", "os.", "process.")))
    if dropped:
        print(f"Keys emitted by Vigie but missing from the allow-list: {dropped}", file=sys.stderr)
        return 1
    redundant = sorted(vigie_keys & sonde_keys)
    if redundant:
        print(f"Keys already allowed by Sonde, remove them from allowed-keys.yaml: {redundant}", file=sys.stderr)
        return 1
    print(f"Allow-list: the {len(emitted_keys(spans))} keys emitted by Vigie are all kept by Sonde.")

    # 2. Sonde's schema, with the demo's spans written as the collector writes them.
    with tempfile.TemporaryDirectory() as tmp:
        sess = session.Session(tmp)
        for stmt in sql_statements(args.sonde / "clickhouse" / "initdb" / "01-otel-schema.sql"):
            sess.query(stmt)
        rows = "\n".join(json.dumps(to_row(s, allowed)) for s in spans)
        sess.query(f"INSERT INTO otel.otel_traces FORMAT JSONEachRow\n{rows}")
        print(f"Replayed {len(spans)} spans into Sonde's otel_traces.")

        # 3. Every SQL block of docs/sonde.md runs and finds what the demo did.
        doc = (ROOT / "docs" / "sonde.md").read_text()
        queries = re.findall(r"^### (.+?)\n.*?```sql\n(.*?)```", doc, re.S | re.M)
        if len(queries) != len(EXPECTED):
            print(f"docs/sonde.md has {len(queries)} queries, the check expects {len(EXPECTED)}", file=sys.stderr)
            return 1
        failed = False
        for (title, query), expected in zip(queries, EXPECTED, strict=True):
            result = str(sess.query(query, "TSVWithNames"))
            missing = [value for value in expected if value not in result]
            status = "ok" if not missing else f"MISSING {missing}"
            failed |= bool(missing)
            print(f"\n-- {status}: {title}\n{result.strip()}")
        return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
