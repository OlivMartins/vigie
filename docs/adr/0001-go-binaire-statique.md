# 0001 · Go, un binaire statique

**Statut** : acceptée, octobre 2026

## Contexte

Vigie s’intercale devant chaque serveur MCP. Il est lancé par l’hôte de l’agent (Claude Code, Cursor, VS Code) sur le poste du développeur, sous Linux, macOS ou Windows, et plus tard en passerelle sur un serveur. Il doit démarrer vite, ne rien exiger de l’environnement et ajouter un délai négligeable.

## Décision

Vigie est écrit en Go et livré comme un binaire unique, compilé statiquement pour chaque plateforme.

## Conséquences

- Installation sans environnement d’exécution : pas de Node.js, de Python ni de conteneur à faire coïncider avec celui du serveur relayé.
- Le SDK OpenTelemetry pour Go est stable pour les traces et les métriques ; l’OpenTelemetry Collector est lui-même écrit en Go, ce qui facilitera le mode passerelle.
- Deux goroutines par sens de relais, un processus enfant, des signaux : le modèle de concurrence de Go s’y prête sans dépendance.
- Les expressions régulières de Go s’exécutent en temps linéaire : la détection de secrets ne peut pas être bloquée par une entrée fabriquée.
- Dépendances directes limitées au SDK OpenTelemetry et à un analyseur YAML.

## Alternatives écartées

- **TypeScript**, langage des SDK MCP de référence : impose Node.js à côté de chaque serveur, et un démarrage plus lent.
- **Rust** : performances équivalentes pour ce besoin, écosystème OpenTelemetry moins mûr côté métriques, coût de contribution plus élevé.
