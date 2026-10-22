# Configuration

Vigie fonctionne sans fichier : les valeurs par défaut épinglent les outils de la première session, bloquent les définitions modifiées et les secrets dans les arguments. Le fichier sert à adapter ces règles, serveur par serveur, et à décrire les serveurs distants que sert la passerelle HTTP.

Un exemple commenté de chaque champ : [`examples/vigie.yaml`](../examples/vigie.yaml).

## Où Vigie cherche son fichier

1. `--config FICHIER`, s’il est donné (un fichier absent est alors une erreur) ;
2. sinon la variable `VIGIE_CONFIG` ;
3. sinon `vigie.yaml` dans le répertoire de configuration de l’utilisateur : `~/.config/vigie/` sous Linux, `~/Library/Application Support/vigie/` sous macOS, `%AppData%\vigie\` sous Windows ;
4. sinon les valeurs par défaut.

`vigie check` affiche le fichier retenu et la politique qui en résulte pour chaque serveur. La lecture est stricte : un champ inconnu, une clé en double ou une valeur hors liste arrête Vigie, avec la ligne ou le champ en cause.

## Champs

| Champ | Valeurs | Défaut | Effet |
| --- | --- | --- | --- |
| `version` | `1` | obligatoire | Version du format |
| `pins_dir` | chemin | `pins/` à côté du fichier par défaut | Répertoire des fichiers d’empreintes. Relatif au fichier de configuration ; `~` accepté |
| `telemetry.capture_content` | booléen | `false` | Joint arguments et résultats aux spans, secrets masqués, tronqués à 32 Kio |
| `telemetry.propagate_context` | booléen | `true` | Écrit le contexte de trace de Vigie dans `params._meta` des requêtes relayées (et, en HTTP, dans l’en-tête `traceparent`) |
| `audit.file` | chemin | aucun | Journal d’audit : une ligne JSON par décision. Relatif au fichier de configuration |
| `audit.max_size` | taille | `100MiB` | Taille à partir de laquelle le journal tourne |
| `audit.max_files` | entier | `5` | Anciens journaux conservés (`.1` à `.5`) |
| `limits.max_message_size` | taille | `64MiB` | Taille maximale d’un message JSON-RPC, dans les deux sens, stdio et HTTP (64KiB au moins) |
| `limits.stop_timeout` | durée | `5s` en stdio, `15s` en passerelle | Temps laissé pour un arrêt propre : au serveur stdio pour sortir une fois son entrée fermée, aux requêtes en cours de la passerelle |
| `gateway` | passerelle | voir ci-dessous | La passerelle HTTP (`vigie gateway`) |
| `defaults` | politique | voir ci-dessous | Règles de tous les serveurs |
| `servers.<nom>` | politique | – | Règles d’un serveur, champ par champ au-dessus de `defaults` |
| `servers.<nom>.url` | URL `http://` ou `https://` | – | Point d’accès MCP d’un serveur distant : la passerelle le sert sous `/mcp/<nom>` |
| `servers.<nom>.headers` | en-têtes | – | En-têtes ajoutés à chaque requête vers ce serveur, à la place de ceux du client : l’identifiant de la passerelle auprès du serveur, typiquement |

Les tailles s’écrivent `512KiB`, `4MiB`, `1GiB` ou en octets ; les durées `500ms`, `15s`, `2m`.

### Politique

| Champ | Valeurs | Défaut | Effet |
| --- | --- | --- | --- |
| `mode` | `enforce`, `audit` | `enforce` | `audit` laisse tout passer et enregistre ce qui aurait été bloqué |
| `tools.allow` | motifs | `["*"]` | Outils exposés ; les autres sont masqués et leurs appels refusés |
| `tools.deny` | motifs | `[]` | Outils toujours refusés, prioritaire sur `allow` |
| `pins.enabled` | booléen | `true` | Active les empreintes |
| `pins.on_new` | `first-session`, `pin`, `block` | `first-session` | Outil vu pour la première fois : approuvé pendant la première session avec le serveur puis masqué jusqu’à approbation (`first-session`), toujours approuvé (`pin`), ou toujours masqué jusqu’à approbation (`block`) |
| `pins.on_change` | `block`, `warn` | `block` | Définition modifiée : masquée et refusée, ou seulement signalée |
| `secrets.arguments` | `block`, `warn`, `off` | `block` | Clé ou jeton dans les arguments d’un appel |
| `secrets.results` | `warn`, `off` | `warn` | Clé ou jeton dans un résultat renvoyé au modèle |
| `detections.action` | `block`, `warn`, `off` | `block` | Instructions cachées dans une définition d’outil (voir ci-dessous) |
| `detections.exempt` | motif d’outil → règles | `{}` | Outils dispensés de certaines règles de détection, ou de toutes (`"*"`) |

Les motifs comparent le nom exact d’un outil : `*` vaut n’importe quelle suite de caractères, `?` un caractère. `delete_*` couvre `delete_invoice` mais pas `undelete`.

**Détections.** Vigie lit chaque définition d’outil comme le modèle la lit, schéma et noms de paramètres compris, et cherche ce qu’une description honnête n’a aucune raison de contenir. Les règles, détaillées dans l’[architecture](ARCHITECTURE.md#détections) : `hidden_instructions`, `invisible_text`, `sensitive_path`, `tool_shadowing`, `concealment`. En mode `enforce`, un outil détecté est masqué et ses appels refusés ; un nouvel outil détecté attend une approbation même pendant la première session. `vigie pins diff` montre la définition, `vigie pins approve --fingerprint …` l’accepte : une approbation humaine vaut acceptation de cette définition, pas de la suivante. Pour dispenser un outil sans passer par les empreintes, ou quand `pins.enabled` est `false`, `detections.exempt` associe un motif d’outil aux règles à ignorer :

```yaml
detections:
  exempt:
    "read_*": [sensitive_path]   # un serveur de documentation qui parle de fichiers .env
    legacy_tool: ["*"]
```

Une liste donnée dans une section de serveur remplace celle de `defaults` ; `deny: []` efface donc les interdictions par défaut pour ce serveur. Un serveur sans section suit `defaults`.

### Passerelle

| Champ | Valeurs | Défaut | Effet |
| --- | --- | --- | --- |
| `gateway.listen` | `hôte:port` | `127.0.0.1:8787` | Adresse d’écoute |
| `gateway.tokens` | `nom: jeton` | aucun | Clients autorisés et leur jeton porteur (`Authorization: Bearer …`), 24 caractères au moins. Le nom va dans les spans (`vigie.client.name`) et le journal d’audit |
| `gateway.allow_anonymous` | booléen | `false` | Autorise l’écoute au-delà de la boucle locale sans jeton, quand un proxy en amont authentifie les clients |
| `gateway.allowed_origins` | origines | pages de la machine si l’écoute est locale, aucune sinon | Pages web autorisées à appeler la passerelle (`https://app.example.com`) ; une requête qui porte une autre origine est refusée |
| `gateway.allowed_hosts` | noms d’hôte | noms de la boucle locale si l’écoute est locale, tous sinon | Valeurs acceptées dans l’en-tête `Host` |
| `gateway.tls.cert_file`, `gateway.tls.key_file` | chemins | aucun | Sert en HTTPS (HTTP/2 compris) |
| `gateway.max_request_size` | taille | `4MiB` | Corps maximal d’une requête d’un client |

La passerelle refuse de démarrer sur une adresse joignable depuis le réseau sans `gateway.tokens` ni `gateway.allow_anonymous` ([ADR 0008](adr/0008-pas-d-ecoute-anonyme-hors-boucle-locale.md)). Avec des jetons, l’en-tête `Authorization` du client reste à la passerelle ; sans jetons, il est relayé tel quel au serveur.

**Variables d’environnement.** Dans `gateway.tokens` et `servers.<nom>.headers`, `${NOM}` est remplacé par la variable d’environnement `NOM` : les secrets restent hors du fichier. Une variable absente ou vide arrête Vigie, plutôt qu’un identifiant vide. `vigie check` affiche les noms des en-têtes ajoutés, jamais leurs valeurs.

Les en-têtes réservés au protocole ou à HTTP (`Host`, `Content-Type`, `Mcp-Session-Id`, `Mcp-Protocol-Version`, `Mcp-Method`, `Mcp-Name`, `Mcp-Param-*`, `traceparent`...) ne peuvent pas être fixés par `headers`.

## Ligne de commande

```text
vigie run --server NOM [options] -- COMMANDE [ARGUMENTS...]
vigie gateway [options]
```

| Option | Effet |
| --- | --- |
| `--server NOM` | Obligatoire. Choisit la section `servers.NOM` et le fichier d’empreintes `NOM.json`. Lettres, chiffres, `.`, `_`, `-` |
| `--config FICHIER` | Fichier de configuration |
| `--mode enforce\|audit` | Remplace le mode de la politique |
| `--trace-file FICHIER` | Écrit aussi chaque span dans ce fichier, une ligne JSON par span |
| `--log-level debug\|info\|warn\|error` | Niveau des journaux, sur la sortie d’erreur (défaut `info`) |
| `--log-format text\|json` | Format des journaux (défaut `text`) |

Tout ce qui suit `--` est la commande du serveur, lancée avec l’environnement de Vigie.

`vigie gateway` prend `--config`, `--mode`, `--trace-file`, `--log-level` et `--log-format`, plus `--listen hôte:port`, qui remplace `gateway.listen`. Elle sert chaque serveur qui a une `url`, et s’arrête proprement sur SIGINT ou SIGTERM.

| Commande | Effet |
| --- | --- |
| `vigie pins list [SERVEUR]` | Outils épinglés et leur statut |
| `vigie pins diff SERVEUR OUTIL` | Ce qui a changé depuis l’approbation |
| `vigie pins approve SERVEUR OUTIL --fingerprint EMPREINTE` | Approuve la définition relue, et seulement elle : la commande échoue si le serveur en a envoyé une autre depuis |
| `vigie pins approve SERVEUR OUTIL...` | Approuve les définitions en attente, quelles qu’elles soient (`--all` pour toutes) |
| `vigie pins forget SERVEUR OUTIL...` | Retire des outils : ils seront traités comme nouveaux |
| `vigie check` | Valide la configuration et affiche la politique de chaque serveur |
| `vigie version` | Version, révision, version de Go et plateforme |

Codes de sortie de `vigie run` : celui du serveur s’il s’arrête de lui-même, 0 si le client ferme la session, 1 si le fichier d’empreintes est illisible, 2 pour une erreur d’usage ou de configuration, 127 si le serveur ne démarre pas. De `vigie gateway` : 0 après un arrêt sur signal, 1 si l’adresse d’écoute est indisponible, 2 pour une erreur d’usage ou de configuration.

## Environnement

| Variable | Effet |
| --- | --- |
| `VIGIE_CONFIG` | Fichier de configuration |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Active l’export OTLP vers ce point de collecte (aussi `..._TRACES_ENDPOINT`, `..._METRICS_ENDPOINT`) |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` (défaut) ou `grpc` |
| `OTEL_EXPORTER_OTLP_HEADERS` | En-têtes d’authentification du collector |
| `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER` | `otlp` ou `none` |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Identité de Vigie dans le backend (défaut `vigie`) |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Échantillonnage |
| `NO_COLOR` | Désactive les couleurs de `vigie pins diff` |
| `HTTPS_PROXY`, `NO_PROXY` | Mandataire sortant de la passerelle vers les serveurs |
| `SSL_CERT_FILE`, `SSL_CERT_DIR` | Autorités de certification des serveurs, pour une PKI interne |

Le serveur relayé en stdio hérite de cet environnement : s’il est lui-même instrumenté, il exporte vers le même collector et ses spans rejoignent ceux de Vigie dans la même trace.

## Recettes

**Un serveur tiers, rien sans revue.** Chaque outil attend une approbation, les outils destructeurs sont exclus d’office :

```yaml
version: 1
servers:
  github:
    pins:
      on_new: block
    tools:
      deny: ["delete_*", "merge_pull_request"]
```

**Un serveur interne qui change souvent.** Les changements sont tracés sans bloquer :

```yaml
version: 1
servers:
  internal-kb:
    pins:
      on_change: warn
```

**Mesurer avant d’imposer.** Tout passe, chaque décision est enregistrée avec `vigie.decision = audit` :

```yaml
version: 1
defaults:
  mode: audit
```

**Une passerelle d’équipe.** Deux serveurs distants, un jeton par client, l’identifiant de service du serveur de facturation lu dans l’environnement, un journal d’audit :

```yaml
version: 1
pins_dir: /var/lib/vigie/pins
audit:
  file: /var/log/vigie/audit.jsonl
gateway:
  listen: 0.0.0.0:8787
  tokens:
    equipe-data: ${VIGIE_TOKEN_DATA}
    ci: ${VIGIE_TOKEN_CI}
servers:
  billing:
    url: https://billing.internal.example/mcp
    headers:
      Authorization: Bearer ${BILLING_SERVICE_TOKEN}
    pins:
      on_new: block
  docs:
    url: https://docs-mcp.example.com/mcp
```

Le déploiement (conteneur, Kubernetes, répartiteur de charge) : [deploiement.md](deploiement.md).

**Approbations versionnées avec le projet.** Un `vigie.yaml` à la racine du dépôt, `pins_dir: ./.vigie/pins`, et les fichiers d’empreintes passent en revue de code avec le reste. L’hôte lance Vigie avec `--config ./vigie.yaml` ou `VIGIE_CONFIG`.
