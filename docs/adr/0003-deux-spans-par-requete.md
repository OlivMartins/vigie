# 0003 · Deux spans par requête : réception et relais

**Statut** : acceptée, octobre 2026

## Contexte

Les conventions MCP d’OpenTelemetry décrivent un span CLIENT, émis par qui envoie une requête, et un span SERVER, émis par qui la traite. Vigie fait les deux : il reçoit la requête du client et la transmet au serveur. Il prend aussi des décisions qui ne concernent que la réception (refus, filtrage).

## Décision

Pour chaque requête relayée, Vigie émet :

- un span **SERVER**, enfant du contexte trouvé dans `params._meta`, qui couvre l’appel tel que l’agent le vit et porte les décisions (`vigie.*`) ;
- un span **CLIENT**, enfant du précédent, qui couvre le relais vers le serveur. Son contexte est écrit dans `params._meta` : le span SERVER du vrai serveur, s’il est instrumenté, en devient l’enfant.

Une requête bloquée n’a qu’un span SERVER.

## Conséquences

- La trace se lit naturellement : agent → Vigie (réception) → Vigie (relais) → serveur.
- La durée du SERVER moins celle du CLIENT est le temps passé dans Vigie, mesurable dans n’importe quel backend.
- Les deux métriques des conventions (`mcp.server.operation.duration` et `mcp.client.operation.duration`) ont chacune leur source.
- Deux fois plus de spans qu’un proxy à span unique. Le volume reste faible devant celui des appels au modèle, et l’échantillonnage du backend s’applique.
- Les deux spans portent `gen_ai.operation.name = execute_tool` sur `tools/call` : un backend qui compte les spans d’exécution d’outils doit filtrer sur `vigie.upstream.name` (voir l’[intégration Sonde](../sonde.md)).

## Alternatives écartées

- **Un seul span CLIENT** : perd la mesure du surcoût et place les décisions sur un span qui décrit l’envoi.
- **Un seul span SERVER, contexte relayé tel quel** : le span du serveur deviendrait frère de celui de Vigie au lieu d’enfant.
- **Enrichir le span de l’agent** : impossible depuis un autre processus.
