# Feuille de route

Chaque version répond à une question de ses utilisateurs, livre un ensemble cohérent et ne sort que si ses critères de sortie sont tenus. Le pourquoi de chaque choix est dans le [cadrage produit](docs/PRODUCT.md).

| Version | Thème | Question à laquelle elle répond |
| --- | --- | --- |
| 0.1 | Voir, épingler, filtrer | « Que font les outils de mon agent, et ont-ils changé depuis que je les ai validés ? » |
| **0.2** | Passerelle HTTP | « Puis-je mettre Vigie devant mes serveurs MCP distants, pour toute une équipe ? » |
| 0.3 | Détections | « Une définition d’outil cache-t-elle des instructions, avant même qu’on l’approuve ? » |
| 0.4 | Identité | « Qui a appelé quoi, et avec quels droits ? » |
| 1.0 | Production | « Puis-je en dépendre, et le mettre à jour sans crainte ? » |

## 0.1 · Voir, épingler, filtrer

Le socle : un proxy stdio sûr, observable et utile dès la première minute, sans configuration.

**Relais**
- [x] Proxy stdio transparent : ce qui n’est pas modifié passe à l’octet près, membres inconnus conservés
- [x] Toutes révisions MCP : poignée de main `initialize` (2024-11-05 à 2025-11-25) et révision sans état 2026-07-28 (`_meta` par requête, `resultType`, `ttlMs`)
- [x] Lecture stricte des messages contre les attaques par divergence d’analyse : clés exactes, membres en double et variantes de casse refusés, réponses appariées à l’octet près, dans les deux sens
- [x] Lots JSON-RPC, annulations, messages trop volumineux, sortie standard réservée au protocole
- [x] Cycle de vie du serveur : groupe de processus, arrêt gradué (fermeture de l’entrée, SIGTERM, SIGKILL), code de sortie propagé

**Voir**
- [x] Spans SERVER et CLIENT par requête selon les conventions MCP d’OpenTelemetry
- [x] Propagation du contexte de trace par `params._meta` (`traceparent`, `tracestate`, `baggage`)
- [x] Métriques `mcp.server.operation.duration`, `mcp.client.operation.duration`, `vigie.decisions`
- [x] Export OTLP HTTP et gRPC configuré par les variables `OTEL_*` standard, fichier de traces local
- [x] Capture du contenu opt-in, secrets masqués

**Épingler**
- [x] Empreinte canonique de chaque définition d’outil, fichier d’empreintes par serveur, lisible et versionnable
- [x] Confiance à la première session avec un serveur, à la première apparition, ou approbation obligatoire (`on_new`)
- [x] Définition modifiée masquée et bloquée (`on_change`), approbation prise en compte sans redémarrage
- [x] `vigie pins list | diff | approve | forget`, caractères invisibles révélés, approbation liée à l’empreinte relue
- [x] Fichier partagé entre processus sous verrou, écriture atomique

**Filtrer**
- [x] Listes d’outils autorisés et interdits par serveur
- [x] Détection de 11 types de clés et jetons dans les arguments, noms de membres et nom d’outil compris (blocage), et dans les résultats (signalement)
- [x] Modes enforce et audit, surchargeables par serveur et en ligne de commande
- [x] Refus expliqués au modèle et journalisés

**Qualité**
- [x] Configuration YAML stricte (champs inconnus et doublons refusés, erreurs localisées), `vigie check`
- [x] Tests unitaires et de bout en bout, détecteur de concurrence, fuzzing de l’analyseur, benchmarks
- [x] Tests de compatibilité avec les SDK MCP officiels (TypeScript, v1 et v2), et tests adverses : un serveur malveillant face à ces SDK
- [x] Démo scriptée (`make demo`), intégration Sonde documentée et requêtes de détection vérifiées dans ClickHouse

**Critères de sortie** : tous les tests verts en CI sur Linux et macOS, surcoût mesuré sous la milliseconde, aucune valeur sensible dans la télémétrie (test dédié), démarrage rapide suivi de bout en bout.

## 0.2 · Passerelle HTTP

Mettre Vigie devant des serveurs MCP distants et le partager au sein d’une équipe.

**Passerelle**
- [x] `vigie gateway` : Streamable HTTP, plusieurs serveurs distants dans une instance, routés par chemin (`/mcp/{serveur}`)
- [x] Révisions 2025-03-26 à 2025-11-25 : sessions, flux GET, `DELETE`, lots, reprise des flux interrompus contrôlée
- [x] Révision 2026-07-28 : en-têtes `Mcp-Method` et `Mcp-Name` vérifiés contre le corps (`-32020`), `subscriptions/listen`, réponses en plusieurs allers-retours
- [x] Réponses JSON et SSE contrôlées message par message ; redirections, erreurs HTTP porteuses d’un résultat et types inattendus écartés
- [x] Sans état : une conversation par échange HTTP, instances multiples sans affinité de session
- [x] Fermée par défaut : écoute locale, `Origin` et `Host` vérifiés, jetons par client, refus d’écouter sur le réseau sans authentification
- [x] Identifiants vers les serveurs (`servers.*.headers`), secrets lus dans l’environnement

**Exploitation**
- [x] Image conteneur distroless, multi-architecture, signée (cosign), avec SBOM et provenance
- [x] Sondes `/healthz` et `/readyz`, arrêt progressif
- [x] Journal d’audit local des décisions (JSON Lines, rotation), partagé entre processus, en stdio comme en HTTP
- [x] Limites configurables : taille des messages et des requêtes, délais d’arrêt
- [x] Télémétrie HTTP : `network.*`, `server.*`, `client.*`, `mcp.session.id` haché, contexte de trace par en-têtes

**Qualité**
- [x] SDK officiels (v1, v2) à travers la passerelle, dans les deux époques ; dix attaques d’un serveur HTTP malveillant
- [x] Suite de conformance officielle passée à travers la passerelle, en CI (`make conformance`)
- [x] Test de charge publié (`make loadtest`), fuzzing du lecteur SSE
- [x] Démo HTTP (`make demo-gateway`), intégration Sonde vérifiée sur les spans HTTP

**Critères de sortie** : suite de conformance officielle MCP passée à travers la passerelle, test de charge publié, déploiement documenté derrière un répartiteur de charge sans affinité de session. Tenus : chaque scénario de conformance réussi en direct réussit à travers Vigie, pour 2025-11-25 et 2026-07-28 ; résultats de charge dans [l’architecture](docs/ARCHITECTURE.md#sous-charge) ; déploiement dans [docs/deploiement.md](docs/deploiement.md).

Reportés : la vérification active des empreintes (Vigie relit `tools/list` lui-même) passe en 0.3, le passif suffisant tant que le modèle ne voit que des listes filtrées ; la rangée « Outils MCP » du tableau de bord appartient au dépôt de Sonde, les requêtes de [docs/sonde.md](docs/sonde.md) en sont la base.

## 0.3 · Détections

Repérer une définition piégée dès sa première apparition, sans attendre qu’elle change.

- Vérification active des empreintes : Vigie relit `tools/list` lui-même avant le premier appel, pour les clients qui gardent la liste en cache
- Passerelle : en-têtes `Mcp-Param-*` vérifiés contre les arguments, d’après les annotations `x-mcp-header` de la définition épinglée
- Épingler aussi les `instructions` du serveur (`initialize`, `server/discover`) et les descriptions de prompts et de ressources
- Heuristiques déterministes sur les définitions : balises d’instructions (`<IMPORTANT>`), caractères invisibles et de contrôle bidirectionnel, références à des fichiers sensibles, mentions d’autres outils (*tool shadowing*), consignes de dissimulation
- Règles de secrets personnalisées dans la configuration, exemptions par outil
- Masquage des secrets dans les résultats, au lieu du seul signalement
- Règles sur les annotations (`destructiveHint`, `openWorldHint`) : approbation exigée pour les outils destructeurs
- Détection de boucles et limites de débit par outil
- Corpus de tests public d’attaques connues, mesure du taux de faux positifs

**Critères de sortie** : corpus d’attaques détecté à 100 % ; moins de 1 % de faux positifs sur les définitions des serveurs MCP de référence.

## 0.4 · Identité

Rattacher chaque appel à une personne et lui appliquer ses droits.

- Relais OAuth 2.1 et autorisation gérée par l’entreprise (extension EMA de juin 2026)
- Politiques par identité ou par groupe, attribut `enduser.id` sur les spans
- Approbations signées : qui a approuvé quelle empreinte, vérifiable dans le fichier
- Flux d’approbation en équipe : fichiers d’empreintes en revue de code, propriétaires par serveur

**Critères de sortie** : scénario multi-utilisateur de bout en bout documenté et testé avec un fournisseur d’identité réel.

## 1.0 · Production

Des garanties sur lesquelles construire.

- Stabilité : format de configuration v1 et noms d’attributs `vigie.*` gelés, toute évolution accompagnée d’une migration
- Budget de performance publié et vérifié en CI à chaque changement
- Fuzzing continu de l’analyseur et des politiques
- Chaîne de livraison : SBOM, provenance SLSA, binaires et images signés
- Rechargement à chaud de la politique, santé et métriques internes
- Matrice de compatibilité tenue à jour avec les clients majeurs (Claude Code, Cursor, VS Code) ; Windows testé en CI
- Guide d’exploitation : déploiement, mise à jour, réponse à incident

**Critères de sortie** : trois mois sans changement incompatible, audit de sécurité externe des composants exposés.
