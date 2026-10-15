# Configuration

Vigie fonctionne sans fichier : les valeurs par défaut épinglent les outils à leur première apparition, bloquent les définitions modifiées et les secrets dans les arguments. Le fichier sert à adapter ces règles, serveur par serveur.

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
| `telemetry.propagate_context` | booléen | `true` | Écrit le contexte de trace de Vigie dans `params._meta` des requêtes relayées |
| `defaults` | politique | voir ci-dessous | Règles de tous les serveurs |
| `servers.<nom>` | politique | – | Règles d’un serveur, champ par champ au-dessus de `defaults` |

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

Les motifs comparent le nom exact d’un outil : `*` vaut n’importe quelle suite de caractères, `?` un caractère. `delete_*` couvre `delete_invoice` mais pas `undelete`.

Une liste donnée dans une section de serveur remplace celle de `defaults` ; `deny: []` efface donc les interdictions par défaut pour ce serveur. Un serveur sans section suit `defaults`.

## Ligne de commande

```text
vigie run --server NOM [options] -- COMMANDE [ARGUMENTS...]
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

| Commande | Effet |
| --- | --- |
| `vigie pins list [SERVEUR]` | Outils épinglés et leur statut |
| `vigie pins diff SERVEUR OUTIL` | Ce qui a changé depuis l’approbation |
| `vigie pins approve SERVEUR OUTIL --fingerprint EMPREINTE` | Approuve la définition relue, et seulement elle : la commande échoue si le serveur en a envoyé une autre depuis |
| `vigie pins approve SERVEUR OUTIL...` | Approuve les définitions en attente, quelles qu’elles soient (`--all` pour toutes) |
| `vigie pins forget SERVEUR OUTIL...` | Retire des outils : ils seront traités comme nouveaux |
| `vigie check` | Valide la configuration et affiche la politique de chaque serveur |
| `vigie version` | Version, révision, version de Go et plateforme |

Codes de sortie de `vigie run` : celui du serveur s’il s’arrête de lui-même, 0 si le client ferme la session, 1 si le fichier d’empreintes est illisible, 2 pour une erreur d’usage ou de configuration, 127 si le serveur ne démarre pas.

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

Le serveur relayé hérite de cet environnement : s’il est lui-même instrumenté, il exporte vers le même collector et ses spans rejoignent ceux de Vigie dans la même trace.

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

**Approbations versionnées avec le projet.** Un `vigie.yaml` à la racine du dépôt, `pins_dir: ./.vigie/pins`, et les fichiers d’empreintes passent en revue de code avec le reste. L’hôte lance Vigie avec `--config ./vigie.yaml` ou `VIGIE_CONFIG`.
