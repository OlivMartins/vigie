# Journal des modifications

Toutes les évolutions notables de Vigie sont consignées ici. Le format s’inspire de [Keep a Changelog](https://keepachangelog.com/fr/1.1.0/) et les versions suivent le [versionnage sémantique](https://semver.org/lang/fr/).

## [0.2.0] - 2026-10-20

La passerelle HTTP : Vigie devant des serveurs MCP distants, partagé par une équipe, avec la même politique, les mêmes empreintes et les mêmes traces qu’en stdio.

### Ajouté

- `vigie gateway` : passerelle Streamable HTTP, un chemin par serveur (`/mcp/{serveur}`), vers l’`url` de chaque section `servers`. Révisions 2025-03-26 à 2025-11-25 (sessions, flux GET, `DELETE`, lots, reprise des flux interrompus) et 2026-07-28 (requêtes autonomes, `subscriptions/listen`, réponses en plusieurs allers-retours).
- Réponses JSON et SSE contrôlées message par message ; identifiants d’événement, commentaires et amorces conservés ; corps faits de plusieurs valeurs JSON relayés au fil de l’eau.
- En-têtes `Mcp-Method`, `Mcp-Name` (encodage Base64 compris) et `Mcp-Protocol-Version` vérifiés contre le corps, quelle que soit la révision : refus `400` avec l’erreur `-32020`.
- Sécurité de la passerelle : écoute locale par défaut, en-têtes `Host` et `Origin` vérifiés (*DNS rebinding*), jetons porteurs par client (`gateway.tokens`), refus d’écouter sur le réseau sans authentification sauf `gateway.allow_anonymous`, TLS optionnel, redirections et erreurs HTTP porteuses d’un résultat non relayées.
- `servers.*.headers` : identifiants de la passerelle auprès des serveurs ; `${VARIABLE}` lu dans l’environnement pour les jetons et en-têtes.
- Télémétrie HTTP : `network.transport=tcp`, `network.protocol.*`, `server.*` et `client.*`, `mcp.session.id` haché, `vigie.client.name`, `http.response.status_code` ; contexte de trace aussi propagé par l’en-tête `traceparent`.
- Journal d’audit (`audit.file`) : une ligne JSON par décision, rotation par taille, partagé entre processus, en stdio comme en HTTP.
- Limites configurables : `limits.max_message_size`, `limits.stop_timeout`, `gateway.max_request_size`.
- Sondes `/healthz` et `/readyz`, arrêt progressif (flux durables fermés, requêtes en cours terminées).
- Image conteneur distroless `ghcr.io/olivmartins/vigie`, amd64 et arm64, signée avec cosign, avec SBOM et provenance, publiée à chaque release ; guide de déploiement ([docs/deploiement.md](docs/deploiement.md)).
- `make demo-gateway`, `make conformance` (suite de conformance officielle à travers la passerelle, en CI), `make loadtest` ; tests de compatibilité des SDK officiels en HTTP et dix attaques d’un serveur HTTP malveillant ; fuzzing du lecteur SSE.
- ADR 0007 (passerelle sans état) et 0008 (pas d’écoute anonyme au-delà de la boucle locale).

### Modifié

- Le cœur du relais (politique, spans, appariement des réponses) est extrait dans une conversation indépendante du transport, partagée par `vigie run` et `vigie gateway`.
- `vigie check` décrit la passerelle : adresse, clients, chemins servis et en-têtes ajoutés (sans leurs valeurs).
- Les résultats d’outil signalés portent le nom d’outil masqué si ce nom contient un secret.

## [0.1.0] - 2026-10-15

Première version : voir, épingler, filtrer les appels d’outils d’un serveur MCP en stdio. Le pourquoi est dans le [cadrage produit](docs/PRODUCT.md), la suite dans la [feuille de route](ROADMAP.md).

### Ajouté

- `vigie run --server NOM -- COMMANDE` : proxy stdio transparent devant un serveur MCP, compatible avec la poignée de main `initialize` (2024-11-05 à 2025-11-25) et la révision sans état 2026-07-28.
- Traces selon les conventions MCP d’OpenTelemetry : un span SERVER et un span CLIENT par requête, contexte propagé par `params._meta`, attributs `mcp.*`, `gen_ai.*`, `jsonrpc.*` et `vigie.*`.
- Métriques `mcp.server.operation.duration`, `mcp.client.operation.duration` et `vigie.decisions`, export OTLP HTTP ou gRPC par les variables `OTEL_*`, fichier de traces local avec `--trace-file`.
- Empreintes des définitions d’outils : outils de la première session épinglés, définitions modifiées et outils apparus ensuite masqués et bloqués jusqu’à approbation, fichier par serveur lisible, versionnable et partagé sous verrou.
- `vigie pins list`, `diff`, `approve` et `forget` ; caractères invisibles révélés dans les diffs ; approbation liée à l’empreinte relue (`--fingerprint`).
- Listes d’outils autorisés et interdits par serveur.
- Détection de 11 types de clés et jetons : blocage dans les arguments (noms de membres et nom d’outil compris), signalement dans les résultats ; masquage dans la télémétrie.
- Lecture sans ambiguïté, dans les deux sens : membres en double et variantes de casse refusés, réponses appariées à l’octet près, lignes illisibles écartées en mode enforce.
- Modes enforce et audit, refus expliqués au modèle par un résultat d’outil en erreur.
- Configuration YAML stricte, `vigie check`.
- Démo scriptée (`make demo`), tests de compatibilité avec les SDK MCP officiels en TypeScript et tests adverses face à un serveur malveillant (`make compat`), intégration Sonde et requêtes vérifiées dans ClickHouse (`make sonde-check`).
- Binaires pour Linux, macOS et Windows, en amd64 et arm64.
