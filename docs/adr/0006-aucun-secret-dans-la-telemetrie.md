# 0006 · Aucune valeur sensible dans la télémétrie

**Statut** : acceptée, octobre 2026

## Contexte

Vigie voit passer les arguments et les résultats des outils : identifiants, documents, données personnelles, et parfois les clés qu’il est chargé d’arrêter. La télémétrie part vers un collector, puis un stockage plus largement accessible que la machine de l’agent.

## Décision

- Une décision décrit le problème par sa règle, son type et son emplacement (`secrets.arguments`, `aws_access_key_id`, `arguments.body`), jamais par sa valeur.
- La capture du contenu (`gen_ai.tool.call.arguments`, `gen_ai.tool.call.result`) est désactivée par défaut, comme le prévoient les conventions (attributs opt-in). Activée, elle passe d’abord par le masquage des secrets et est tronquée à 32 Kio.
- Les noms d’outils, les URI de ressources et les messages d’erreur des pairs passent par le même masquage avant d’entrer dans un span ; les journaux suivent la même règle.
- Des tests parcourent les noms, attributs, événements et statuts des spans produits (contenu capturé, noms de membres, URI et erreurs compris) et échouent si une clé y apparaît.

## Conséquences

- La télémétrie peut être partagée avec les équipes d’exploitation sans exposer ce qu’elle protège.
- Pour enquêter sur un refus, il faut le contexte de l’agent ou rejouer l’appel : la trace dit où chercher, pas quoi.
- Le masquage ne reconnaît que les formats connus : la capture du contenu reste une décision à prendre en connaissance de cause, documentée comme telle.
