# 0005 · Un refus est un résultat d’outil en erreur, pas une erreur de protocole

**Statut** : acceptée, octobre 2026

## Contexte

Quand Vigie refuse un `tools/call`, il doit répondre au client. Deux formes sont possibles : une erreur JSON-RPC, ou un résultat d’outil marqué `isError: true`. La spécification réserve les erreurs de protocole aux cas où l’outil est introuvable ou la requête invalide, et demande que les échecs d’exécution soient rendus dans le résultat, pour que le modèle puisse les lire et se corriger.

## Décision

Un appel refusé reçoit un résultat d’outil avec `isError: true` et un texte qui dit pourquoi, sans secret : « Vigie blocked this call to tool "send_reminder": arguments.body looks like an AWS access key. The request was not sent to the server. » Pour un client en révision 2026-07-28, le résultat porte `resultType: "complete"`.

Le texte est en anglais : il est lu par le modèle et finit dans des journaux.

## Conséquences

- L’agent comprend qu’il a été arrêté, et pourquoi ; il peut prévenir l’utilisateur au lieu de réessayer en boucle.
- Côté télémétrie, le span porte `error.type = tool_error` comme tout échec d’outil, et `vigie.decision = block` pour le distinguer.
- Un modèle détourné lit aussi la raison du refus. Elle ne contient que le type et l’emplacement du problème, rien qui l’aide à contourner la règle.

## Alternatives écartées

- **Erreur JSON-RPC** : beaucoup de clients la remontent comme une panne du serveur, et le modèle ne voit pas toujours le message.
- **Réponse silencieuse ou vide** : le modèle conclut que l’outil a fonctionné.
