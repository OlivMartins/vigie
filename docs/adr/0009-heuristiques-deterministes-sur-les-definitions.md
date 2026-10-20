# 0009 · Des heuristiques déterministes sur les définitions d’outils, et une approbation qui vaut acceptation

**Statut** : acceptée, octobre 2026

## Contexte

Les empreintes ([ADR 0004](0004-empreintes-de-la-premiere-session.md)) disent qu’une définition a changé, pas ce qu’elle contient. Un serveur piégé dès sa première version passe, puisque la première session est approuvée d’emblée ; et la personne qui relit un diff lit moins que le modèle, qui lit tout : description, schéma, noms de paramètres, valeurs d’énumération. L’attaque (*tool poisoning*) tient dans cet écart.

Détecter ce que contient une définition peut se faire avec un modèle, ou avec des règles.

## Décision

Des règles déterministes, sans modèle, appliquées à chaque chaîne de la définition (`internal/detect`) : balises d’instructions, caractères invisibles, fichiers sensibles, consignes pour un autre outil, dissimulation. Chaque règle cible une technique qu’une description honnête n’a aucune raison d’employer.

- **Mesurées des deux côtés.** Un corpus d’attaques documentées doit être détecté en entier, un corpus de définitions de serveurs de référence ne doit rien lever. Les deux sont des tests : une règle qui ajoute un faux positif ne passe pas la CI, un faux positif signalé par un utilisateur entre au corpus avant d’être corrigé.
- **Bloquant par défaut, avec une issue.** Un outil détecté est masqué ; un *nouvel* outil détecté n’est pas approuvé d’emblée, même dans la première session. La revue est celle des empreintes (`vigie pins diff`, `vigie pins approve --fingerprint`), et l’approbation par une personne vaut acceptation de cette définition précise. Une approbation automatique (première session, `on_new: pin`) n’est pas une revue et ne vaut rien ici. `detections.exempt` couvre les cas sans empreintes.
- **Sur ce que le modèle peut voir.** À chaque `tools/call`, la définition relue dans le fichier d’empreintes est analysée aussi : un client qui garde une liste en cache ne contourne pas la détection.
- **Un constat ne cite pas la définition.** Il donne la règle et l’emplacement. La raison d’un refus est lue par le modèle ([ADR 0005](0005-refus-en-resultat-d-outil.md)) : elle ne doit pas rejouer l’injection qu’elle décrit.

## Conséquences

- Le surcoût est de quelques dizaines de microsecondes par définition, mémorisé ensuite ; il reste sans commune mesure avec un appel d’outil.
- Les résultats sont explicables et reproductibles : même définition, même verdict. Un audit peut les rejouer.
- Les heuristiques ne voient pas une instruction paraphrasée sans marque ni consigne de dissimulation. Elles se trompent aussi dans l’autre sens, sur un serveur de documentation qui cite `.env` : l’exemption par outil et par règle est faite pour cela.
- Les motifs de texte sont en anglais.

## Alternatives écartées

- **Un modèle juge chaque définition.** Coûteux, lent sur le chemin critique, non déterministe, et il lit lui-même l’injection qu’il doit juger. Reste possible hors ligne, pour constituer des corpus, pas pour décider.
- **Seulement signaler.** Un constat dans une trace arrive après que le modèle a lu la définition.
- **Une exemption propre aux détections, sans lien avec les empreintes.** Elle oblige à relire deux fichiers pour accepter un outil. L’approbation existe déjà et lie la décision à l’empreinte relue.
