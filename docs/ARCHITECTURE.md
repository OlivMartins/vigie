# Architecture

Vigie est un proxy MCP, sous deux formes qui partagent le même cœur. En **relais stdio** (`vigie run`), le client (l’hôte de l’agent) le lance comme il lancerait le serveur, et Vigie lance le vrai serveur derrière lui. En **passerelle HTTP** (`vigie gateway`), le client se connecte à Vigie au lieu du serveur distant, et Vigie relaie vers son URL. Entre les deux, chaque message est lu, tracé, et parfois arrêté.

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

```text
             POST /mcp/billing                    POST https://billing.example/mcp
client MCP ─────────────────────▶ ┌──────────────────┐ ──────────────────────────▶ serveur MCP
              (JSON ou SSE)       │  vigie gateway   │       (JSON ou SSE)          distant
           ◀───────────────────── │ une conversation │ ◀──────────────────────────
                                  │ par échange HTTP │
                                  └────────┬─────────┘
                                           └──▶ OTLP, journal d’audit
```

## Composants

| Paquet | Rôle |
| --- | --- |
| `internal/jsonrpc` | Découpe le flux en messages, les lit sans perte (membres bruts, clés exactes), réécrit seulement ce qui change |
| `internal/mcp` | Ce que Vigie sait du protocole : méthodes, cible d’une requête, version, forme de `tools/list` et des résultats |
| `internal/policy` | Décide : `allow`, `warn`, `audit` ou `block`, avec les constats (*findings*) qui justifient la décision |
| `internal/pins` | Empreintes des définitions d’outils, fichier par serveur, diff de revue |
| `internal/secrets` | Détection de clés et jetons dans des valeurs JSON, masquage |
| `internal/telemetry` | OpenTelemetry : export, propagation par `params._meta` et par en-têtes HTTP, noms d’attributs, instruments |
| `internal/proxy` | La conversation, indépendante du transport (politique, spans, appariement des réponses), le relais stdio et la vie du processus serveur |
| `internal/gateway` | La passerelle HTTP : routes, validation des en-têtes, relais JSON et SSE, reprise des flux, santé, arrêt |
| `internal/sse` | Lecture et écriture des flux Server-Sent Events, fidèles aux identifiants et commentaires |
| `internal/audit` | Journal d’audit JSON Lines, rotation par taille, partagé entre processus |
| `internal/filelock` | Verrous de fichier partagés entre processus (empreintes, journal d’audit) |
| `internal/config` | Lecture stricte du YAML, fusion défauts → section `defaults` → section du serveur, variables d’environnement |
| `internal/cli` | Commandes `run`, `gateway`, `pins`, `check`, `version` |

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

## La passerelle HTTP

`vigie gateway` sert chaque serveur de la configuration qui a une `url` sous `/mcp/{serveur}`, plus `/healthz` (vivant) et `/readyz` (prêt, `503` pendant l’arrêt). Le transport est Streamable HTTP, dans ses deux époques : sessions, flux GET et reprise de 2025-03-26 à 2025-11-25 ; requêtes autonomes, en-têtes recopiés du corps et `subscriptions/listen` depuis 2026-07-28. Le transport HTTP+SSE de 2024-11-05, déprécié, n’est pas relayé.

Chaque échange HTTP est traité par sa propre **conversation** : la même logique que le relais stdio (politique, empreintes, spans, appariement), sans état partagé entre échanges ([ADR 0007](adr/0007-passerelle-http-sans-etat.md)). Plusieurs instances se partagent la charge sans affinité.

### Le trajet d’un POST

1. **Accès.** L’en-tête `Host` doit nommer la passerelle (boucle locale par défaut, contre le *DNS rebinding*), l’en-tête `Origin`, s’il est présent, une origine autorisée (`403` sinon). Avec `gateway.tokens`, un jeton porteur valide est exigé (`401`) ; il est comparé en temps constant et n’est pas transmis au serveur ([ADR 0008](adr/0008-pas-d-ecoute-anonyme-hors-boucle-locale.md)).
2. **Lecture.** Le corps est borné (`gateway.max_request_size`, 4 Mio par défaut, `413` au-delà), doit être `application/json` (`415`) et du JSON-RPC lisible (`400`, erreur `-32700`).
3. **En-têtes et corps.** Les en-têtes `Mcp-Method`, `Mcp-Name` (décodé s’il utilise l’encodage `=?base64?…?=`) et `Mcp-Protocol-Version` doivent dire la même chose que le corps, et être présents quand la révision 2026-07-28 les exige. Un en-tête `Mcp-*` donné deux fois ou qui contient des caractères invalides est refusé. Le refus est un `400` avec l’erreur `-32020` (`HeaderMismatch`), quelle que soit la révision : un répartiteur placé après Vigie peut se fier aux en-têtes.
4. **Conversation.** Chaque message passe par les étapes du relais stdio : span SERVER, politique, refus en résultat d’outil ou relais avec span CLIENT. Un appel bloqué reçoit sa réponse de Vigie (`200`, `application/json`) sans que le serveur soit contacté.
5. **Relais.** La requête part vers l’URL du serveur avec les en-têtes du client, sans ceux qui ne valent que pour une connexion, plus `X-Forwarded-For`, `X-Forwarded-Proto` et `X-Forwarded-Host`, et les en-têtes de `servers.*.headers`, qui l’emportent. Le contexte de trace part dans l’en-tête `traceparent` ; il est aussi écrit dans `params._meta` quand la requête en a déjà un (toutes celles de 2026-07-28) : certains serveurs prennent la présence de `_meta` pour la marque de la révision 2026-07-28.
6. **Réponse.** Selon ce que le serveur renvoie :
   - `application/json` : chaque message est contrôlé comme en stdio (appariement exact, filtrage de `tools/list`, examen des résultats), et le corps passe à l’octet près s’il n’a pas changé. Un corps fait de plusieurs valeurs JSON successives, comme en produit le serveur de référence de la suite de conformance, est relu valeur par valeur et relayé au fil de l’eau.
   - `text/event-stream` : le flux est relu événement par événement. Chaque message est contrôlé ; un événement écarté laisse son identifiant (`id: …`) pour que la reprise reste possible. Commentaires (maintien de connexion), événements d’amorce sans données et champs `retry` passent tels quels. `X-Accel-Buffering: no` est ajouté.
   - `202 Accepted`, réponse à `DELETE` : relayés tels quels.
   - Erreur HTTP (`4xx`, `5xx`) : relayée avec ses en-têtes (`WWW-Authenticate`, notamment), sauf si son corps porte un résultat JSON-RPC, qu’un client tolérant pourrait prendre pour la réponse.
   - Redirection : ni suivie ni relayée en mode enforce (`502`) ; suivie par le client, elle contournerait Vigie.
   - Serveur injoignable : `502`, avec une erreur JSON-RPC pour chaque requête du corps.

### Les deux époques du transport

| | 2025-03-26 à 2025-11-25 | 2026-07-28 |
| --- | --- | --- |
| Session | `Mcp-Session-Id` relayé dans les deux sens, haché dans la télémétrie | aucune |
| Flux du serveur | `GET` relayé, requêtes du serveur relayées sans span ; réponses écartées sauf reprise | `subscriptions/listen` : un POST dont la réponse est un flux durable |
| Reprise | `GET` avec `Last-Event-ID` relayé ; la réponse rejouée est contrôlée | aucune |
| Fin de session | `DELETE` relayé | – |
| Lots | contrôlés élément par élément (2025-03-26) | refusés (`400`) |
| En-têtes `Mcp-Method`, `Mcp-Name` | vérifiés s’ils sont présents | exigés et vérifiés |

**Réponses sur un flux GET.** Le serveur n’y envoie de réponse que pour reprendre un flux interrompu. Une réponse quelconque y serait pourtant prise par un client pour celle d’une requête en cours, sans être passée par le filtrage : Vigie écarte en mode enforce toute réponse qu’il ne sait pas rattacher. Quand un flux POST se termine avant sa réponse, Vigie retient pendant 10 minutes, en mémoire, ce que demandaient les requêtes restées sans réponse ; la réponse rejouée sur le GET de reprise est alors contrôlée comme les autres. Sur une autre instance, elle n’est pas retrouvée et le client relance sa requête.

### Durée de vie

Les flux durables (`GET`, `subscriptions/listen`) ne sont limités que par le client et le serveur. À l’arrêt (SIGINT, SIGTERM), la passerelle répond `503` à `/readyz`, ferme aussitôt les flux durables (les clients se reconnectent ailleurs), laisse aux autres requêtes en cours `limits.stop_timeout` (15 s par défaut) pour se terminer, puis ferme les connexions restantes.

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
| `network.transport` | `pipe` en stdio, `tcp` en HTTP | tous |
| `gen_ai.operation.name` | `execute_tool` | `tools/call` |
| `gen_ai.tool.name`, `gen_ai.prompt.name` | cible de la requête | `tools/call`, `prompts/get` |
| `mcp.resource.uri` | ressource lue | `resources/*` |
| `rpc.response.status_code` | code d’erreur JSON-RPC | réponses en erreur |
| `error.type` | code JSON-RPC ou statut HTTP, `tool_error`, `cancelled`, `client_closed`, `upstream_closed`, `upstream_unavailable`, `shutdown` | échecs |
| `vigie.upstream.name` | nom donné à `--server`, ou serveur routé par la passerelle | tous |
| `network.protocol.name`, `network.protocol.version` | `http`, `1.1` ou `2` | passerelle |
| `client.address`, `client.port` | adresse du client | passerelle, span SERVER |
| `server.address`, `server.port` | adresse du serveur relayé | passerelle, span CLIENT |
| `mcp.session.id` | hachage (SHA-256 tronqué) de `Mcp-Session-Id` : l’identifiant lui-même peut suffire à détourner une session | passerelle, révisions 2025, span SERVER |
| `vigie.client.name` | client authentifié par `gateway.tokens` | passerelle, span SERVER |
| `http.response.status_code` | statut HTTP d’erreur du serveur | passerelle, span CLIENT |
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

## Journal d’audit

Pour qui n’a pas encore de backend d’observabilité, et pour une preuve qui survit à la rétention des traces, `audit.file` écrit chaque décision dans un fichier, un objet JSON par ligne : chaque `tools/call` (`tool.call`, autorisé ou non), et chaque liste ou résultat où une règle s’est appliquée (`tool.list`, `tool.result`).

```json
{"time":"2026-10-15T14:03:12Z","event":"tool.call","server":"billing","transport":"http","client":"equipe-data","method":"tools/call","tool":"send_reminder","decision":"block","mode":"enforce","rule":"secrets.arguments","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7","findings":[{"rule":"secrets.arguments","action":"block","tool":"send_reminder","location":"arguments.body","secret_type":"aws_access_key_id","reason":"arguments.body looks like an AWS access key"}]}
```

Comme les spans, une ligne dit où et quoi, jamais la valeur détectée ; la session est hachée. Le fichier tourne au-delà de `audit.max_size` (100 Mio par défaut) et garde `audit.max_files` anciens fichiers (5). Chaque écriture prend un verrou partagé avec les autres processus : plusieurs `vigie run` et une passerelle peuvent écrire le même fichier, chacun suivant les rotations des autres.

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
- **Fin de session côté client** (entrée standard fermée, ou sortie standard que plus personne ne lit) : Vigie ferme l’entrée du serveur, attend 5 s (`limits.stop_timeout`), envoie SIGTERM au groupe, attend 2 s, puis SIGKILL. C’est la séquence recommandée par le transport stdio de MCP. Vigie sort avec le code 0. Une écriture sur une sortie fermée est traitée comme une fin de session, pas comme un SIGPIPE qui tuerait Vigie avant d’avoir arrêté le serveur et vidé ses spans.
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
- **Taille bornée** : un message de plus de 64 Mio (`limits.max_message_size`) est écarté, l’analyseur reste utilisable pour le suivant ; en HTTP, le corps d’une requête est borné à 4 Mio (`gateway.max_request_size`) et chaque événement SSE à la taille d’un message.
- **En HTTP, en-têtes et corps concordent** (voir la passerelle) ; une réponse d’erreur HTTP qui porte un résultat, une redirection, un type de contenu inattendu ne sont pas relayés en mode enforce.
- **Fuzzing** de l’analyseur JSON-RPC, du lecteur SSE et du détecteur de secrets ; tests adverses face aux SDK officiels avec un serveur malveillant, en stdio (`test/compat/adversarial.test.mjs`, six attaques) et en HTTP (`test/compat/adversarial-http.test.mjs`, dix attaques, dont une réponse glissée sur le flux GET). Sans le mode enforce, sept des dix attaques HTTP trompent les SDK officiels.
- **Lots** (révisions jusqu’à 2025-03-26) : chaque élément est contrôlé ; les éléments bloqués reçoivent leur réponse de Vigie, les autres partent au serveur dans un lot.

## Performances

### Temps de traitement

Mesuré par les benchmarks (`make bench`), traces et métriques actives, sans réseau, sur 2 vCPU :

| Opération | Temps de traitement par Vigie |
| --- | --- |
| stdio : `tools/call` aller-retour, arguments et résultat de 1 Kio | ≈ 50 µs |
| stdio : `tools/list` de 50 outils, empreintes et contrôles d’ambiguïté compris | ≈ 0,8 ms |
| passerelle : échange `tools/call` 2026-07-28, réponse JSON | ≈ 100 µs |
| passerelle : échange `tools/call` 2026-07-28, réponse en flux SSE | ≈ 145 µs |

Le temps d’un appel d’outil se compte en dizaines de millisecondes au moins : Vigie y ajoute moins de 1 %. La requête « Latence par outil » de [l’intégration Sonde](sonde.md#latence-par-outil-et-ce-que-vigie-y-ajoute) le mesure en conditions réelles.

### Sous charge

`make loadtest` envoie des `tools/call` de 2026-07-28 (arguments et résultat d’environ 1 Kio) pendant 10 s par mesure, directement au serveur, à travers un mandataire inverse nu (`net/http/httputil`, le coût de n’importe quel saut HTTP), puis à travers `vigie gateway`. Le serveur répond en mémoire, sans travail : c’est le pire cas pour un intermédiaire, dont tout le coût se voit. Générateur, serveur et Vigie partagent la même machine, 2 vCPU Intel Xeon à 2,1 GHz ; télémétrie sans export.

| Clients | Direct | Mandataire inverse nu | Passerelle Vigie | Vigie, p99 | CPU de Vigie par appel |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 13 716 req/s, p50 0,05 ms | 3 012 req/s, p50 0,29 ms | 1 701 req/s, p50 0,53 ms | 1,75 ms | 428 µs |
| 8 | 22 366 req/s, p50 0,26 ms | 7 963 req/s, p50 0,74 ms | 4 204 req/s, p50 1,54 ms | 6,85 ms | 235 µs |
| 32 | 20 037 req/s, p50 1,29 ms | 7 331 req/s, p50 3,95 ms | 3 898 req/s, p50 7,66 ms | 20,57 ms | 275 µs |
| 128 | 23 371 req/s, p50 5,13 ms | 8 489 req/s, p50 14,18 ms | 4 629 req/s, p50 25,91 ms | 68,73 ms | 253 µs |

À faible concurrence, la passerelle ajoute un demi-milliseconde à un appel, dont la moitié tient au saut HTTP lui-même. Au-delà, ces deux vCPU partagés plafonnent à quelque 4 000 appels d’outils par seconde : chaque appel coûte à Vigie 250 µs de CPU environ, pour lire et relire deux fois du HTTP et du JSON, appliquer la politique et produire ses spans. Une équipe d’agents en produit rarement plus de quelques dizaines par seconde ; au-delà, la passerelle se réplique sans affinité.

## Limites connues

- **Confiance à la première session.** Par défaut, les outils de la première session sont approuvés tels quels : un serveur malveillant dès sa première version passe. Pour un serveur tiers, préférez `on_new: block` et relisez chaque outil.
- **Vérification passive.** Les empreintes sont vérifiées sur les `tools/list` que le client demande. Un client qui appelle un outil sans relister (cache, requêtes en rafale) est jugé sur l’état connu du fichier d’empreintes. En mode enforce, le modèle ne voit jamais une définition d’outil non approuvée : elle passe forcément par un `tools/list` filtré. La vérification active, où Vigie relit lui-même la liste, est reportée en 0.3.
- **Seuls les outils sont épinglés.** Les `instructions` qu’un serveur renvoie à `initialize` ou `server/discover`, que certains clients placent dans le prompt système, et les descriptions de prompts et de ressources peuvent changer sans être soumises à approbation. Leur épinglage est prévu en 0.3.
- **L’empreinte couvre la description, pas le comportement.** Un serveur qui change ce que fait un outil sans changer sa définition n’est pas détecté. Les traces, elles, montrent ce qui a été appelé et ce qui est revenu.
- **Secrets : formats connus seulement.** Un mot de passe en clair ou une donnée personnelle en texte libre ne sont pas reconnus. Les règles personnalisées arrivent en 0.3.
- **Résultats signalés, pas masqués.** Un secret renvoyé par un serveur au modèle est journalisé et tracé, pas retiré (0.3).
- **Tâches** (extension `io.modelcontextprotocol/tasks`) : les résultats récupérés par `tasks/get` ne sont pas encore examinés.
- **Lots** : les réponses d’un lot partiellement bloqué arrivent en deux lots, l’un de Vigie, l’autre du serveur.
- **Passerelle et OAuth.** Vigie relaie l’en-tête `Authorization` et les défis `WWW-Authenticate`, mais ne réécrit pas les métadonnées de ressource protégée : un client qui découvre l’OAuth d’un serveur à travers la passerelle obtient un jeton pour l’URL du serveur, pas pour celle de Vigie, et peut le refuser. En 0.2, un serveur protégé par OAuth se branche avec un identifiant de service dans `servers.*.headers` ; le relais OAuth arrive en 0.4.
- **Passerelle : reprise sur la même instance.** La réponse rejouée par la reprise d’un flux (révisions 2025) n’est contrôlée que sur l’instance qui a vu la requête ; ailleurs, elle est écartée et le client relance sa requête.
- **Passerelle : requêtes du serveur non tracées.** Les requêtes que le serveur envoie au client sur un flux (révisions 2025) sont relayées sans span : leur réponse revient dans un autre échange HTTP.
- **Passerelle : en-têtes `Mcp-Param-*`.** Vigie vérifie leurs caractères et leur encodage, pas leur concordance avec les arguments : il les relaie, et le serveur, qui connaît le schéma de l’outil, doit les valider comme la spécification l’exige.
- **Passerelle : pas de certificat client** vers les serveurs (mTLS) ; des autorités de certification internes se déclarent avec `SSL_CERT_FILE`.
- **Windows** : compilé et livré, pas encore testé en CI. L’arrêt du serveur y est immédiat (pas de SIGTERM) et ne vise que le processus lancé : les processus qu’il a lui-même lancés (`npx` puis `node`) peuvent lui survivre.
