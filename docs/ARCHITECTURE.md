# Architecture

Vigie est un proxy MCP : le client (l’hôte de l’agent) le lance comme il lancerait le serveur, et Vigie lance le vrai serveur derrière lui. Entre les deux, chaque message est lu, tracé, et parfois arrêté.

```text
            stdin                       stdin
client MCP ───────▶ ┌──────────────────┐ ───────▶ serveur MCP
(Claude Code,       │      Vigie       │          (processus enfant,
 Cursor...)  ◀───── │ relais · politique│ ◀─────   groupe dédié)
            stdout  │ empreintes · OTel │  stdout
                    └────────┬─────────┘
                    stderr ◀─┤  (journaux de Vigie et du serveur)
                             └──▶ OTLP (traces, métriques) ou fichier de traces
```

## Composants

| Paquet | Rôle |
| --- | --- |
| `internal/jsonrpc` | Découpe le flux en messages, les lit sans perte (membres bruts, clés exactes), réécrit seulement ce qui change |
| `internal/mcp` | Ce que Vigie sait du protocole : méthodes, cible d’une requête, version, forme de `tools/list` et des résultats |
| `internal/policy` | Décide : `allow`, `warn`, `audit` ou `block`, avec les constats (*findings*) qui justifient la décision |
| `internal/pins` | Empreintes des définitions d’outils, fichier par serveur, diff de revue |
| `internal/secrets` | Détection de clés et jetons dans des valeurs JSON, masquage |
| `internal/telemetry` | OpenTelemetry : export, propagation par `params._meta`, noms d’attributs, instruments |
| `internal/proxy` | Le relais (deux sens, appels en vol, spans) et la vie du processus serveur |
| `internal/config` | Lecture stricte du YAML, fusion défauts → section `defaults` → section du serveur |
| `internal/cli` | Commandes `run`, `pins`, `check`, `version` |

Aucun paquet ne dépend du SDK MCP : Vigie travaille au niveau JSON-RPC et relaie ce qu’il ne connaît pas (voir [ADR 0002](adr/0002-proxy-json-rpc-sans-sdk.md)).

## Le trajet d’une requête

Prenons `tools/call get_invoice`, envoyée par le client.

1. **Lecture.** Une ligne de l’entrée standard est un message (ou un lot). Le message est décodé en membres bruts : `id`, `method`, `params` sont lus avec des clés exactes ; tout le reste est gardé tel quel. Un message ambigu (membre en double, `"Name"` à côté de `"name"`) est refusé avec une erreur `-32600`, tout comme un `tools/call` sans `id` ou sans nom d’outil lisible.
2. **Span SERVER.** Vigie extrait le contexte de trace de `params._meta` (`traceparent`, `tracestate`, `baggage`) et ouvre un span `tools/call get_invoice` de type SERVER, enfant de l’appel côté agent s’il existe.
3. **Politique.** Dans l’ordre : nom de l’outil (listes `deny` puis `allow`), statut de l’empreinte, secrets dans les arguments. Chaque règle qui correspond ajoute un constat ; la décision est la plus sévère.
4. **Refus.** Si la décision est `block`, Vigie répond lui-même : un résultat d’outil avec `isError: true` et une phrase qui dit pourquoi (voir [ADR 0005](adr/0005-refus-en-resultat-d-outil.md)). La requête n’atteint jamais le serveur, le span SERVER se ferme avec `error.type = tool_error` et `vigie.decision = block`.
5. **Relais.** Sinon, Vigie ouvre un span CLIENT, enfant du précédent, écrit son contexte dans `params._meta` et transmet. Le serveur, s’il est instrumenté, rattache ses propres spans à celui-ci.
6. **Réponse.** Quand la réponse revient, avec exactement le même `id` (`5` n’est pas `"5"` ni `5.0`), Vigie ferme le span CLIENT, examine le résultat (secrets renvoyés au modèle), ferme le span SERVER et transmet. Une réponse qui ne correspond à aucune requête en attente n’est pas relayée : un client tolérant pourrait la prendre pour la réponse à un `tools/list` et recevoir une liste que Vigie n’a pas filtrée.

Pour `tools/list`, l’étape 6 fait davantage : chaque définition passe par le fichier d’empreintes, et les outils interdits, non approuvés ou sans nom lisible sont retirés de la liste avant qu’elle n’atteigne le modèle. Une liste que Vigie ne sait pas lire, ou ambiguë (`"Tools"` à côté de `"tools"`), est remplacée par une erreur. Si la liste porte un `ttlMs` (2026-07-28), il passe à 0 : une liste filtrée ne doit pas rester en cache chez le client.

Les requêtes émises par le serveur (anciennes révisions : `sampling/createMessage`, `elicitation/create`, `roots/list`) suivent le même chemin en sens inverse, sans politique. Les notifications passent sans span, sauf `notifications/cancelled`, qui ferme les spans de la requête annulée.

## Télémétrie

### Deux spans par requête

| Span | Type | Couvre |
| --- | --- | --- |
| `tools/call get_invoice` | SERVER | L’appel tel que l’agent le vit : réception, politique, relais, réponse |
| `tools/call get_invoice` | CLIENT | Le relais vers le serveur, jusqu’à sa réponse |

La différence entre les deux est le temps passé dans Vigie. Un appel bloqué n’a pas de span CLIENT. Le choix est détaillé dans l’[ADR 0003](adr/0003-deux-spans-par-requete.md).

### Attributs

Les noms suivent les [conventions MCP d’OpenTelemetry](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/mcp.md) (statut Development), tous définis dans [`internal/telemetry/semconv.go`](../internal/telemetry/semconv.go).

| Attribut | Valeur | Où |
| --- | --- | --- |
| `mcp.method.name` | `tools/call`, `tools/list`, `initialize`... | tous |
| `mcp.protocol.version` | négociée par `initialize`, ou lue dans `_meta` (2026-07-28) | tous |
| `jsonrpc.request.id` | identifiant de la requête | tous |
| `network.transport` | `pipe` | tous |
| `gen_ai.operation.name` | `execute_tool` | `tools/call` |
| `gen_ai.tool.name`, `gen_ai.prompt.name` | cible de la requête | `tools/call`, `prompts/get` |
| `mcp.resource.uri` | ressource lue | `resources/*` |
| `rpc.response.status_code` | code d’erreur JSON-RPC | réponses en erreur |
| `error.type` | code JSON-RPC, `tool_error`, `cancelled`, `client_closed`, `upstream_closed`, `shutdown` | échecs |
| `vigie.upstream.name` | nom donné à `--server` | tous |
| `vigie.decision` | `allow`, `warn`, `audit`, `block` | `tools/call`, `tools/list` |
| `vigie.rule` | règle décisive : `tools.invalid`, `tools.deny`, `tools.allow`, `pins.changed`, `pins.pending`, `pins.unavailable`, `secrets.arguments`, `secrets.results` | idem |
| `vigie.mode` | `enforce` ou `audit` | idem |
| `vigie.pin.status` | `approved`, `changed`, `pending`, `unknown` | `tools/call` |
| `vigie.tools.listed`, `.hidden`, `.new`, `.changed`, `.pending` | compteurs | `tools/list` |
| `gen_ai.tool.call.arguments`, `gen_ai.tool.call.result` | contenu, secrets masqués | si `capture_content` |

Sur `tools/list`, la décision est celle du constat le plus grave : `block` signifie qu’au moins un outil a été retiré de la liste, `vigie.tools.hidden` dit combien.

Chaque constat devient un événement `vigie.finding` sur le span SERVER, avec `vigie.rule`, `vigie.finding.action`, `vigie.finding.reason` et, selon le cas, `gen_ai.tool.name`, `vigie.finding.location` (`arguments.body`) et `vigie.secret.type` (`aws_access_key_id`). Jamais la valeur détectée.

Les codes JSON-RPC qui signalent une requête mal formée côté appelant (`-32700`, `-32600`, `-32601`, `-32602`, `-32002`) sont enregistrés dans `rpc.response.status_code` sans passer le span SERVER en erreur, comme le prévoient les conventions ; sur le span CLIENT, tout code d’erreur est une erreur. Les messages d’erreur des pairs, les URI de ressources et les noms d’outils passent par le masquage des secrets avant d’entrer dans la télémétrie.

### Métriques

| Métrique | Type | Unité |
| --- | --- | --- |
| `mcp.server.operation.duration` | histogramme (seaux des conventions, de 10 ms à 300 s) | s |
| `mcp.client.operation.duration` | histogramme | s |
| `vigie.decisions` | compteur, par serveur, méthode, outil, décision et règle | `{decision}` |

### Export

Les variables standard s’appliquent : `OTEL_EXPORTER_OTLP_ENDPOINT` (et ses variantes par signal), `OTEL_EXPORTER_OTLP_PROTOCOL` (`http/protobuf` par défaut, ou `grpc`), `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_EXPORTER` et `OTEL_METRICS_EXPORTER` (`otlp` ou `none`).

Sans point de collecte configuré, la télémétrie reste éteinte : un serveur MCP local ne doit pas remplir les journaux de l’hôte d’erreurs d’export. `--trace-file` écrit les spans dans un fichier, une ligne JSON par span, sans collector. L’exporteur console est refusé : avec stdio, la sortie standard porte le protocole lui-même.

Les erreurs d’export sont journalisées au plus une fois toutes les 30 secondes, avec le nombre d’erreurs tues entre-temps.

## Empreintes

### Forme canonique

L’empreinte d’un outil est le SHA-256 de sa définition sous forme canonique : clés d’objets triées, espaces insignifiants retirés, chaînes réencodées de façon uniforme (un `i` et un `i` donnent la même empreinte), nombres gardés tels qu’écrits. Le membre `_meta` est exclu : un serveur peut y mettre des données volatiles que le modèle ne lit pas.

Tout le reste compte : nom, titre, description, schémas d’entrée et de sortie, annotations, icônes, et les membres qu’ajouteront les révisions futures. Changer cette forme changerait toutes les empreintes : elle fait partie du contrat de compatibilité ([ADR 0004](adr/0004-empreintes-de-la-premiere-session.md)).

### Fichier d’empreintes

Un fichier par serveur, `<pins_dir>/<serveur>.json`, en JSON indenté, droits `0600`. Il se lit et se relit en revue de code comme un fichier de verrouillage de dépendances :

```json
{
  "version": 1,
  "server": "billing",
  "tools": {
    "get_invoice": {
      "approved": {
        "fingerprint": "sha256:9f2c...",
        "definition": { "description": "Fetch an invoice by its number.", "inputSchema": { "...": "..." }, "name": "get_invoice" },
        "seen_at": "2026-10-15T09:00:00Z",
        "approved_at": "2026-10-15T09:00:00Z",
        "approved_by": "first-use"
      },
      "pending": {
        "fingerprint": "sha256:41ab...",
        "definition": { "description": "Fetch an invoice by its number. <IMPORTANT>...</IMPORTANT>", "...": "..." },
        "seen_at": "2026-10-15T14:03:00Z"
      }
    }
  }
}
```

| Statut | `approved` | `pending` | Effet par défaut |
| --- | --- | --- | --- |
| `approved` | oui | non | outil visible et appelable |
| `changed` | oui | oui | outil masqué et appels refusés (`on_change: block`) |
| `pending` | non | oui | nouvel outil masqué jusqu’à approbation (après la première session, ou dès elle avec `on_new: block`) |
| `unknown` | – | – | jamais vu : épinglé pendant la première session avec le serveur, en attente ensuite (`on_new: first-session`) |

Chaque écriture prend un verrou exclusif sur `<serveur>.json.lock`, relit le fichier, applique le changement et le remplace de façon atomique (fichier temporaire puis renommage) : plusieurs instances de Vigie et la commande `vigie pins` le partagent sans perdre de mise à jour. Une approbation est prise en compte au `tools/list` ou à l’appel suivant, sans redémarrage. Si le serveur revient à la définition approuvée, l’entrée en attente disparaît.

`vigie pins diff` affiche l’empreinte de la définition en attente, et la commande d’approbation qu’il propose la cite : `vigie pins approve billing get_invoice --fingerprint sha256:0332449c77f2`. Si le serveur a envoyé une autre définition entre la revue et l’approbation, celle-ci échoue au lieu d’approuver ce que personne n’a relu.

### Première session

Par défaut (`on_new: first-session`), les outils vus pendant la première session avec un serveur, celle qui commence avec un fichier d’empreintes vide, sont approuvés d’emblée. Un outil qui apparaît lors d’une session suivante attend une approbation : une mise à jour qui ajoute `get_invoice2` avec une description piégée ne passe pas plus qu’une qui modifie `get_invoice`. `on_new: pin` approuve tout nouvel outil à sa première apparition ; `on_new: block` exige une approbation pour chacun, dès la première session.

Un fichier illisible empêche Vigie de démarrer ; s’il le devient en cours de route, les outils concernés sont bloqués (`pins.unavailable`) : faute de pouvoir vérifier, Vigie refuse.

## Secrets

Onze règles reconnaissent des formats structurés : clés privées PEM, clés d’accès AWS, jetons GitHub et GitLab, jetons Slack, clés secrètes Stripe, clés d’API Anthropic, OpenAI et Google, JSON Web Tokens, identifiants dans une URL (`postgres://user:pass@host`).

Chaque chaîne de la valeur JSON est examinée, noms de membres compris, à n’importe quelle profondeur, avec son emplacement (`arguments.items[2].note`). La valeur est lue jeton par jeton : un membre en double est examiné deux fois, puisque c’est le texte brut qui part vers le serveur. Le nom de l’outil appelé est examiné lui aussi. Un filtre rapide écarte d’abord le texte qui ne contient aucun des mots-clés des règles (`AKIA`, `ghp_`, `-----BEGIN`...) ; il ne s’applique qu’aux valeurs sans séquence d’échappement, pour qu’une clé écrite `AKIA...` ne passe pas au travers. Les expressions régulières de Go s’exécutent en temps linéaire : un argument fabriqué ne peut pas bloquer le proxy.

## Vie du processus serveur

- Le serveur démarre dans son propre groupe de processus (Unix) : `npx` ou `uvx` lancent le vrai serveur comme petit-enfant, et l’arrêt doit l’atteindre aussi.
- Sa sortie d’erreur passe telle quelle sur celle de Vigie ; l’hôte la journalise comme avant.
- **Fin de session côté client** (entrée standard fermée, ou sortie standard que plus personne ne lit) : Vigie ferme l’entrée du serveur, attend 5 s, envoie SIGTERM au groupe, attend 2 s, puis SIGKILL. C’est la séquence recommandée par le transport stdio de MCP. Vigie sort avec le code 0. Une écriture sur une sortie fermée est traitée comme une fin de session, pas comme un SIGPIPE qui tuerait Vigie avant d’avoir arrêté le serveur et vidé ses spans.
- **Arrêt du serveur** : Vigie ferme les spans en vol (`error.type = upstream_closed`) et sort avec le code du serveur (128 + signal s’il a été tué), pour que l’hôte voie la panne.
- **Signal reçu par Vigie** (SIGINT, SIGTERM) : SIGTERM au groupe, puis SIGKILL après 2 s.
- **Serveur introuvable** : code 127, comme un shell.

La sortie standard de Vigie ne porte que des messages du protocole ; un test le vérifie. Journaux et diagnostics vont sur la sortie d’erreur.

## Robustesse face aux entrées hostiles

Vigie suppose que le serveur peut être malveillant, et que le modèle derrière le client peut avoir été détourné. Ce qu’il ne sait pas lire sans ambiguïté, il ne le relaie pas en mode enforce.

- **Clés exactes, et pas d’ambiguïté.** Le décodage en structures Go accepterait `"NAME"` pour `"name"` et garderait le dernier de deux membres homonymes ; d’autres décodeurs gardent le premier. Vigie lit les clés exactes, et refuse tout objet inspecté qui contient un membre en double ou un membre qui ne diffère d’un membre lu que par la casse : enveloppe et `params` côté client, enveloppe et liste d’outils côté serveur, nom de chaque définition d’outil.
- **Réponses appariées à l’octet près.** Une réponse dont l’`id` ne correspond exactement à aucune requête en attente est écartée, comme une seconde réponse à la même requête.
- **Ce qui n’est pas lisible n’est pas relayé**, dans les deux sens : un message client invalide reçoit une erreur `-32700`, une ligne invalide du serveur (journal écrit sur la sortie standard, JSON trop profond pour Go mais pas pour un autre décodeur) est écartée et journalisée. En mode audit, tout passe.
- **Définitions sans nom masquées** : une entrée de `tools/list` qui n’est pas un objet, ou dont le nom manque, est vide ou ambigu, ne peut pas être épinglée ; elle est retirée.
- **Taille bornée** : un message de plus de 64 Mio est écarté, l’analyseur reste utilisable pour le suivant.
- **Fuzzing** de l’analyseur JSON-RPC et du détecteur de secrets ; tests adverses face aux SDK officiels avec un serveur malveillant (`test/compat/adversarial.test.mjs`).
- **Lots** (révisions jusqu’à 2025-03-26) : chaque élément est contrôlé ; les éléments bloqués reçoivent leur réponse de Vigie, les autres partent au serveur dans un lot.

## Performances

Mesurées par les benchmarks de `internal/proxy` (`make bench`), traces et métriques actives, sur 2 vCPU à 2,8 GHz :

| Opération | Temps de traitement par Vigie |
| --- | --- |
| `tools/call` aller-retour, arguments et résultat de 1 Kio | ≈ 65 µs |
| `tools/list` de 50 outils, empreintes et contrôles d’ambiguïté compris | ≈ 1,2 ms |

Le temps d’un appel d’outil se compte en dizaines de millisecondes au moins : Vigie y ajoute moins de 1 %. La requête « Latence par outil » de [l’intégration Sonde](sonde.md#latence-par-outil-et-ce-que-vigie-y-ajoute) le mesure en conditions réelles.

## Limites connues

- **Confiance à la première session.** Par défaut, les outils de la première session sont approuvés tels quels : un serveur malveillant dès sa première version passe. Pour un serveur tiers, préférez `on_new: block` et relisez chaque outil.
- **Vérification passive.** Les empreintes sont vérifiées sur les `tools/list` que le client demande. Un client qui appelle un outil sans relister (cache, requêtes en rafale) est jugé sur l’état connu du fichier d’empreintes. En mode enforce, le modèle ne voit jamais une définition d’outil non approuvée : elle passe forcément par un `tools/list` filtré. La vérification active est prévue en 0.2.
- **Seuls les outils sont épinglés.** Les `instructions` qu’un serveur renvoie à `initialize` ou `server/discover`, que certains clients placent dans le prompt système, et les descriptions de prompts et de ressources peuvent changer sans être soumises à approbation. Leur épinglage est prévu en 0.3.
- **L’empreinte couvre la description, pas le comportement.** Un serveur qui change ce que fait un outil sans changer sa définition n’est pas détecté. Les traces, elles, montrent ce qui a été appelé et ce qui est revenu.
- **Secrets : formats connus seulement.** Un mot de passe en clair ou une donnée personnelle en texte libre ne sont pas reconnus. Les règles personnalisées arrivent en 0.3.
- **Résultats signalés, pas masqués.** Un secret renvoyé par un serveur au modèle est journalisé et tracé, pas retiré (0.3).
- **Tâches** (extension `io.modelcontextprotocol/tasks`) : les résultats récupérés par `tasks/get` ne sont pas encore examinés.
- **Lots** : les réponses d’un lot partiellement bloqué arrivent en deux lots, l’un de Vigie, l’autre du serveur.
- **Windows** : compilé et livré, pas encore testé en CI. L’arrêt du serveur y est immédiat (pas de SIGTERM) et ne vise que le processus lancé : les processus qu’il a lui-même lancés (`npx` puis `node`) peuvent lui survivre.
