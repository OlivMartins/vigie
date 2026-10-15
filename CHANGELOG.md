# Journal des modifications

Toutes les évolutions notables de Vigie sont consignées ici. Le format s’inspire de [Keep a Changelog](https://keepachangelog.com/fr/1.1.0/) et les versions suivent le [versionnage sémantique](https://semver.org/lang/fr/).

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
