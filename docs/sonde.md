# Vigie avec Sonde

[Sonde](https://github.com/OlivMartins/sonde) reçoit de l’OTLP, le normalise, le filtre et l’écrit dans ClickHouse. Vigie y ajoute ce que l’agent ne voit pas : ce qui part réellement vers les serveurs MCP et ce que la politique en a fait.

```text
agent ── OTLP ─────────────────────────────┐
  │                                         ▼
  └── stdio ──▶ vigie ── stdio ──▶ serveur  collector Sonde ──▶ ClickHouse ──▶ Grafana
                  └──── OTLP ───────────────▲
```

Quand l’agent est instrumenté et transmet son contexte dans `params._meta`, les spans de Vigie s’insèrent dans la même trace que les appels au modèle : une exécution d’agent se lit d’un bloc, de la question à l’appel d’outil refusé.

## Brancher

1. Démarrez le socle de Sonde, sans l’agent de démonstration :

   ```bash
   cd sonde && make infra
   ```

2. Ajoutez les clés de Vigie à la liste blanche du collector. Sonde supprime tout attribut qu’il ne connaît pas : sans cette étape, les spans arrivent vides. Copiez le contenu de [`integrations/sonde/allowed-keys.yaml`](../integrations/sonde/allowed-keys.yaml) à la fin de `allowed_keys`, dans `otel-collector/config.yaml`, puis redémarrez le collector :

   ```bash
   docker compose restart otel-collector
   ```

3. Lancez Vigie avec l’adresse du collector. Le collector de Sonde ne reçoit pas de métriques : coupez-les pour éviter des erreurs d’export.

   ```bash
   export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
   export OTEL_METRICS_EXPORTER=none
   vigie run --server billing -- npx -y billing-mcp
   ```

   Dans la configuration d’un client MCP, ces variables vont dans le bloc `env` du serveur.

4. Ouvrez un shell ClickHouse (`make sql` dans Sonde) et essayez les requêtes ci-dessous.

## Ce qu’il faut savoir

- **Appels d’outils comptés deux fois.** Comme le demandent les conventions, les spans de Vigie portent `gen_ai.operation.name = execute_tool`. La vue `genai_tool_calls` de Sonde 0.3 les compte donc comme des appels d’outils, sans agent, en plus de ceux que l’agent trace lui-même. En attendant que Sonde les traite à part, ajoutez `AND SpanAttributes['vigie.upstream.name'] = ''` à la clause `WHERE` de `genai_tool_calls_mv` (`clickhouse/initdb/02-genai-model.sql`), et lisez les appels MCP avec les requêtes de cette page.
- **Masquage.** Le collector de Sonde masque les e-mails et les jetons dans les valeurs qu’il garde, y compris dans les événements. Les raisons des décisions de Vigie ne contiennent jamais de secret ; si vous activez `capture_content`, Vigie masque déjà les secrets qu’il reconnaît avant l’envoi, et Sonde ajoute sa propre couche.
- **Échantillonnage.** Avec le profil d’échantillonnage en queue de Sonde, gardez les traces où `vigie.decision` vaut `block` ou `audit` : ce sont celles qu’on voudra relire.

## Requêtes

Chaque requête de cette page est exécutée par `scripts/sonde_check.py` sur le schéma de Sonde, avec les spans réels de la démo de Vigie.

### Décisions par serveur et par règle

La vue d’ensemble : ce que la politique laisse passer, signale et bloque.

```sql
SELECT
    SpanAttributes['vigie.upstream.name'] AS server,
    SpanAttributes['mcp.method.name'] AS method,
    SpanAttributes['vigie.decision'] AS decision,
    SpanAttributes['vigie.rule'] AS rule,
    count() AS requests
FROM otel.otel_traces
WHERE ServiceName = 'vigie' AND SpanKind = 'Server'
  AND SpanAttributes['vigie.decision'] != ''
  AND Timestamp >= now() - INTERVAL 1 DAY
GROUP BY server, method, decision, rule
ORDER BY requests DESC;
```

### Les appels bloqués, avec leur trace

```sql
SELECT
    Timestamp,
    SpanAttributes['vigie.upstream.name'] AS server,
    SpanAttributes['gen_ai.tool.name'] AS tool,
    SpanAttributes['vigie.rule'] AS rule,
    TraceId
FROM otel.otel_traces
WHERE ServiceName = 'vigie' AND SpanKind = 'Server'
  AND SpanAttributes['mcp.method.name'] = 'tools/call'
  AND SpanAttributes['vigie.decision'] IN ('block', 'audit')
  AND Timestamp >= now() - INTERVAL 1 DAY
ORDER BY Timestamp DESC
LIMIT 50;
```

### Définitions d’outils modifiées depuis leur approbation

Chaque ligne est un *rug pull* possible : un outil dont la description ou le schéma a changé. Relisez-le avec `vigie pins diff SERVER TOOL`.

```sql
SELECT
    SpanAttributes['vigie.upstream.name'] AS server,
    finding['gen_ai.tool.name'] AS tool,
    min(Timestamp) AS first_seen,
    max(Timestamp) AS last_seen,
    count() AS lists_filtered
FROM otel.otel_traces
ARRAY JOIN Events.Name AS event, Events.Attributes AS finding
WHERE ServiceName = 'vigie' AND event = 'vigie.finding'
  AND SpanAttributes['mcp.method.name'] = 'tools/list'
  AND finding['vigie.rule'] = 'pins.changed'
  AND Timestamp >= now() - INTERVAL 7 DAY
GROUP BY server, tool
ORDER BY first_seen DESC;
```

### Secrets détectés, par type et par outil

`secrets.arguments` : ce qui partait vers un serveur. `secrets.results` : ce qu’un serveur renvoyait au modèle.

```sql
SELECT
    finding['vigie.rule'] AS direction,
    finding['vigie.secret.type'] AS secret,
    finding['gen_ai.tool.name'] AS tool,
    finding['vigie.finding.location'] AS location,
    count() AS findings
FROM otel.otel_traces
ARRAY JOIN Events.Name AS event, Events.Attributes AS finding
WHERE ServiceName = 'vigie' AND event = 'vigie.finding'
  AND finding['vigie.secret.type'] != ''
  AND Timestamp >= now() - INTERVAL 7 DAY
GROUP BY direction, secret, tool, location
ORDER BY findings DESC;
```

### Latence par outil, et ce que Vigie y ajoute

Le span SERVER couvre l’appel vu par l’agent, le span CLIENT son relais vers le serveur. Leur différence est le temps passé dans Vigie.

```sql
SELECT
    s.SpanAttributes['vigie.upstream.name'] AS server,
    s.SpanAttributes['gen_ai.tool.name'] AS tool,
    count() AS calls,
    round(quantile(0.5)(s.Duration) / 1e6, 2) AS p50_ms,
    round(quantile(0.95)(s.Duration) / 1e6, 2) AS p95_ms,
    round(quantile(0.95)(if(s.Duration > c.Duration, s.Duration - c.Duration, 0)) / 1e3, 1) AS vigie_p95_us
FROM otel.otel_traces AS s
INNER JOIN otel.otel_traces AS c ON c.TraceId = s.TraceId AND c.ParentSpanId = s.SpanId
WHERE s.ServiceName = 'vigie' AND s.SpanKind = 'Server' AND c.SpanKind = 'Client'
  AND s.SpanAttributes['mcp.method.name'] = 'tools/call'
  AND s.Timestamp >= now() - INTERVAL 1 DAY
GROUP BY server, tool
ORDER BY p95_ms DESC;
```

### Erreurs des serveurs, par outil

Seuls les appels relayés comptent : un refus de Vigie n’est pas une panne du serveur.

```sql
SELECT
    SpanAttributes['vigie.upstream.name'] AS server,
    SpanAttributes['gen_ai.tool.name'] AS tool,
    count() AS calls,
    countIf(SpanAttributes['error.type'] = 'tool_error') AS tool_errors,
    countIf(SpanAttributes['rpc.response.status_code'] != '') AS protocol_errors,
    round(countIf(StatusCode = 'Error') / count(), 3) AS error_rate
FROM otel.otel_traces
WHERE ServiceName = 'vigie' AND SpanKind = 'Client'
  AND SpanAttributes['mcp.method.name'] = 'tools/call'
  AND Timestamp >= now() - INTERVAL 1 DAY
GROUP BY server, tool
ORDER BY error_rate DESC, calls DESC;
```

### Révisions du protocole MCP en usage

Utile avant de migrer un parc de clients et de serveurs vers la révision sans état 2026-07-28.

```sql
SELECT
    SpanAttributes['vigie.upstream.name'] AS server,
    SpanAttributes['mcp.protocol.version'] AS version,
    count() AS requests
FROM otel.otel_traces
WHERE ServiceName = 'vigie' AND SpanKind = 'Server'
  AND Timestamp >= now() - INTERVAL 1 DAY
GROUP BY server, version
ORDER BY server, requests DESC;
```
