# 0002 · Un proxy au niveau JSON-RPC, sans SDK MCP

**Statut** : acceptée, octobre 2026

## Contexte

Le protocole évolue vite : la révision 2026-07-28 a supprimé la poignée de main `initialize`, ajouté `server/discover`, `resultType`, les requêtes en plusieurs allers-retours et les abonnements. Les clients en circulation parlent encore 2025-06-18 ou 2025-11-25. Un proxy doit relayer toutes ces versions en même temps, et ce que les versions futures ajouteront.

Un SDK MCP valide et reconstruit les messages selon une révision donnée : il refuserait ou perdrait ce qu’il ne connaît pas.

## Décision

Vigie travaille au niveau JSON-RPC. Chaque message est décodé en membres bruts ; Vigie ne lit que les membres dont il a besoin (`method`, `id`, `params.name`, `params.arguments`, `params._meta`, `result.tools`...) et relaie le reste à l’octet près. Un message n’est réencodé que s’il est modifié, et alors seuls les membres touchés changent.

Les clés sont lues exactement. Et comme les pairs ne décodent pas tous de la même façon (Go accepte `"NAME"` pour `"name"` et garde le dernier de deux membres homonymes, d’autres gardent le premier), Vigie refuse en mode enforce tout message qu’un pair pourrait lire autrement que lui : membre en double, variante de casse d’un membre inspecté, réponse dont l’`id` ne correspond exactement à aucune requête en attente, ligne illisible. Sans cela, un attaquant montrerait un nom d’outil ou une liste à Vigie et une autre au pair.

## Conséquences

- Compatible avec toutes les révisions, des messages inconnus aux membres d’extension.
- Vigie connaît explicitement les quelques formes qu’il inspecte ; les tests de compatibilité avec les SDK officiels (TypeScript v1 et v2, deux ères du protocole) vérifient qu’il ne casse rien, et les tests adverses qu’un serveur malveillant ne les trompe pas à travers lui.
- Pas de validation complète des messages : c’est le rôle des pairs. Vigie refuse seulement ce qu’il ne sait pas lire, en mode enforce.

## Alternatives écartées

- **SDK MCP officiel en Go** : couplage à une révision, réécriture systématique des messages.
- **Proxy aveugle avec inspection par expressions régulières** : fragile, contournable.
