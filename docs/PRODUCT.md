# Vigie : cadrage produit

Ce document fixe ce que Vigie doit résoudre, pour qui, et comment on saura que c’est réussi. Il sert de boussole aux choix techniques ([décisions d’architecture](adr/)) et à la [feuille de route](../ROADMAP.md).

## En une phrase

Vigie est le poste de contrôle des outils d’un agent IA : il se place entre l’agent et ses serveurs MCP, trace chaque appel en OpenTelemetry, épingle les définitions d’outils validées et bloque ce que la politique n’autorise pas.

## Le problème

Un agent IA agit par ses outils. Avec le Model Context Protocol, ces outils viennent de serveurs tiers, installés en une ligne (`npx`, `uvx`) et exécutés avec les droits de l’utilisateur. Trois faits rendent la situation inconfortable.

**L’agent obéit à la description des outils.** Le modèle lit la description et le schéma d’un outil pour décider comment l’appeler. Une description piégée suffit donc à le détourner : Invariant Labs a nommé cette attaque *tool poisoning* en avril 2025 et montré qu’elle exfiltre des dépôts privés sans interaction de l’utilisateur. Sur le banc MCPTox, ce type d’attaque dépasse 60 % de réussite.

**Une définition validée peut changer après coup.** C’est le *rug pull* : en juillet 2025, Check Point Research a montré que Cursor ne revalidait pas les outils après l’approbation initiale (CVE-2025-54136). En septembre 2025, `postmark-mcp` est devenu le premier serveur MCP malveillant confirmé sur un registre public : il mettait discrètement un attaquant en copie de chaque e-mail. L’OWASP classe ces attaques ensemble (MCP03:2025).

**Personne ne voit ce qui passe.** La révision 2026-07-28 du protocole a déprécié la journalisation au niveau du protocole au profit d’OpenTelemetry, et les conventions sémantiques MCP d’OpenTelemetry existent. Mais la plupart des outils d’observabilité s’arrêtent à l’appel d’outil, côté agent. Ce qui part réellement vers le serveur (arguments, clés glissées par une injection de prompt) et ce qui revient au modèle reste hors champ.

Les mesures recommandées sont connues : empreinte des définitions au moment de l’approbation, liste d’outils autorisés, passerelle entre l’agent et ses serveurs, journalisation des chargements de définitions et des appels (note de recherche de la Cloud Security Alliance). Il manque un outil ouvert, léger et standard qui les applique sur le poste du développeur comme en production.

Sources : [CSA, MCP Tool Poisoning](https://labs.cloudsecurityalliance.org/research/csa-research-note-mcp-tool-poisoning-ai-agent-exfiltration-2/), [conventions MCP d’OpenTelemetry](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/mcp.md), [changelog MCP 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/changelog).

## Pour qui

| Profil | Situation | Ce qu’il attend de Vigie |
| --- | --- | --- |
| **Le développeur outillé** | Branche des serveurs MCP tiers dans Claude Code, Cursor ou VS Code. Ne relit pas chaque mise à jour. | Être prévenu quand un outil change, voir ce que font ses outils, sans rien changer à son client MCP. |
| **L’équipe plateforme** | Opère des agents en production, avec un backend d’observabilité existant (Sonde, Langfuse, Datadog, Grafana). | Des traces et métriques MCP standard, reliées aux traces de l’agent ; un point unique où appliquer des règles. |
| **Le responsable sécurité ou conformité** | Doit écrire une politique d’usage des agents, la faire appliquer et prouver qu’elle l’est. | Une politique versionnée, des décisions tracées et datées, un mode audit pour mesurer avant d’imposer. |

### Ce qu’ils cherchent à accomplir

- *Quand j’ajoute un serveur MCP tiers*, je veux qu’un changement de ses outils me soit soumis avant d’atteindre mon agent, pour lui faire confiance sans relire chaque version.
- *Quand un agent se comporte bizarrement*, je veux retrouver chaque appel d’outil dans la même trace que l’agent, pour comprendre sans reproduire.
- *Quand je déploie une politique*, je veux mesurer ce qu’elle aurait bloqué avant de l’appliquer, pour ne pas casser le travail des équipes.

## La réponse

Un binaire unique, placé devant la commande du serveur MCP :

```text
agent ── stdio ──▶ vigie run --server billing -- npx billing-mcp ── stdio ──▶ serveur MCP
                     │
                     └── OTLP ──▶ collector ──▶ Sonde, Langfuse, Grafana...
```

| Promesse | Ce que fait Vigie |
| --- | --- |
| **Voir** | Chaque requête MCP devient deux spans conformes aux conventions MCP d’OpenTelemetry (réception et relais), rattachés à la trace de l’agent par `params._meta`. Durées, erreurs et décisions en métriques. |
| **Épingler** | Les définitions d’outils de la première session avec un serveur sont empreintes. Toute modification, tout outil apparu ensuite, est masqué à l’agent et bloqué jusqu’à `vigie pins approve`, avec le diff exact sous les yeux. |
| **Filtrer** | Listes d’outils autorisés et interdits, clés et jetons détectés dans les arguments (bloqués) et dans les résultats (signalés), mode audit pour observer avant d’appliquer. |

### Principes produit

1. **Transparent par défaut.** Ce que Vigie ne modifie pas passe à l’octet près. Une modification (contexte de trace, liste filtrée, refus) est documentée et testée.
2. **Aucun secret dans la télémétrie.** Une décision dit où et quoi (`arguments.body`, `aws_access_key_id`), jamais la valeur. La capture du contenu est opt-in et masquée.
3. **Standards d’abord.** Conventions OpenTelemetry, variables `OTEL_*`, OTLP. Les attributs propres à Vigie (`vigie.*`) ne couvrent que ce que les conventions ignorent.
4. **Expliquer chaque refus.** Le modèle reçoit une raison lisible et l’humain la commande pour réagir.
5. **Échouer fermé là où c’est sûr.** Un fichier d’empreintes illisible bloque ; en mode enforce, un message que Vigie ne sait pas lire sans ambiguïté n’est relayé dans aucun sens.
6. **Zéro configuration pour commencer.** Sans fichier, les valeurs par défaut protègent déjà : outils de la première session épinglés, modifications et nouveaux outils bloqués, secrets bloqués.

## Périmètre de la v0.1

Inclus : proxy stdio, toutes révisions du protocole (de 2024-11-05 à 2026-07-28), traces et métriques MCP, politique par serveur en YAML validé, empreintes et commandes `vigie pins`, détection de secrets, modes enforce et audit, démo scriptée, intégration documentée avec Sonde.

Exclu, et pourquoi :

| Hors périmètre | Raison | Quand |
| --- | --- | --- |
| Transport Streamable HTTP | Les serveurs les plus exposés tournent en local, en stdio, avec les droits de l’utilisateur. Le mode passerelle HTTP mérite son propre cycle. | v0.2 |
| Modifier les arguments en vol (masquage) | Changer silencieusement une requête trompe l’agent ; bloquer avec une raison claire est plus sûr. | À évaluer, v0.3 |
| Détection des injections par LLM | Coûteuse, non déterministe, lente sur le chemin critique. Les heuristiques déterministes viennent d’abord. | v0.3 (heuristiques) |
| Identité et autorisation par utilisateur | Suppose le transport HTTP et OAuth. | v0.4 |
| Interface graphique | Le backend d’observabilité existant est l’interface ; Sonde fournit les vues. | Non prévu |

## Comment on saura que c’est réussi

| Indicateur | Cible v0.1 | Comment on le mesure |
| --- | --- | --- |
| Transparence | 0 message altéré hors cas documentés | Tests de relais octet par octet, tests avec les SDK MCP officiels |
| Surcoût | < 1 ms par appel d’outil aller-retour | Benchmarks Go dans la CI (≈ 65 µs mesurés) |
| Fuite de secrets | 0 valeur sensible dans la télémétrie | Test dédié, contenu capturé compris |
| Délai de mise en route | < 5 minutes jusqu’à la première trace | Démarrage rapide du README, démo `make demo` |
| Explicabilité | 100 % des refus portent règle, raison et emplacement | Tests des spans et du résultat renvoyé au modèle |

Après publication, on suivra l’adoption (étoiles, téléchargements des releases, issues ouvertes par des utilisateurs réels) et les retours sur les faux positifs de la détection de secrets.

## Paysage et positionnement

- **Passerelles MCP d’entreprise** (offres SaaS ou plateformes de gouvernance) : complètes, centralisées, souvent propriétaires. Vigie vise d’abord le poste et le serveur local, sans dépendance à un service.
- **Scanners MCP** comme mcp-scan d’Invariant Labs, cité par la CSA : analysent les configurations et épinglent les outils. Vigie applique la même idée en continu, dans le flux, et l’expose en télémétrie standard.
- **Plateformes d’observabilité IA** (Langfuse, Arize Phoenix, LangSmith) : tracent l’agent de l’intérieur. Vigie trace la frontière entre l’agent et ses outils et leur envoie ses spans.

Ce qui distingue Vigie : natif OpenTelemetry selon les conventions MCP officielles, application en ligne plutôt qu’audit après coup, un binaire statique sans service à opérer, une politique en YAML versionnable, compatible avec la révision sans état 2026-07-28 comme avec les précédentes.

## Risques

| Risque | Parade |
| --- | --- |
| Faux positifs de la détection de secrets qui bloquent un usage légitime | Règles à structure reconnaissable uniquement, mode `warn` par serveur, mode audit global, emplacement précis dans chaque refus |
| Mises à jour légitimes perçues comme une gêne | Diff lisible, approbation en une commande, `on_change: warn` pour les serveurs internes |
| Conventions OpenTelemetry encore en statut Development | Noms d’attributs centralisés dans un seul fichier ; changements signalés dans le journal des modifications |
| Le protocole MCP évolue vite | Proxy au niveau JSON-RPC qui relaie ce qu’il ne connaît pas ; tests sur plusieurs révisions |
| Confiance à la première session | Documentée comme limite ; les outils apparus ensuite attendent une approbation ; `on_new: block` pour les serveurs tiers |
| Divergences d’analyse entre Vigie et les pairs | Messages ambigus refusés, réponses appariées à l’octet près, tests adverses face aux SDK officiels |
