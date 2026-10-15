# 0004 · Empreintes de la première session, changements soumis à approbation

**Statut** : acceptée, octobre 2026

## Contexte

Le modèle choisit et appelle les outils d’après leur définition. Une définition modifiée après validation (*rug pull*, CVE-2025-54136) suffit à détourner un agent. Les recommandations publiées (CSA, OWASP MCP03:2025) convergent : enregistrer l’empreinte des définitions au moment de l’approbation et soumettre tout changement à une nouvelle revue.

Exiger une approbation explicite de chaque outil dès le départ rendrait l’outil pénible à adopter.

## Décision

- Chaque définition reçue dans un `tools/list` est empreinte (SHA-256 d’une forme canonique, `_meta` exclu).
- Par défaut (`on_new: first-session`), les définitions vues pendant la première session avec un serveur, celle qui commence avec un fichier d’empreintes vide, sont approuvées d’emblée. Un outil qui apparaît lors d’une session suivante attend une approbation. `on_new: pin` approuve tout nouvel outil à sa première apparition ; `on_new: block` exige une approbation pour chacun.
- Une définition différente de celle approuvée est enregistrée en attente, masquée au modèle et ses appels refusés (`on_change: block`), jusqu’à `vigie pins approve`.
- Les définitions sont stockées en entier pour que `vigie pins diff` montre exactement ce qui a changé, caractères invisibles révélés. L’approbation peut citer l’empreinte relue (`--fingerprint`) : si le serveur a envoyé une autre définition entre-temps, elle échoue.
- Un fichier JSON par serveur, lisible, versionnable et relu en revue comme un fichier de verrouillage, partagé entre processus sous verrou.

## Conséquences

- Protection immédiate contre le changement silencieux, sans configuration.
- Un outil ajouté ou renommé par une mise à jour (`get_invoice2` à côté de `get_invoice`) n’échappe pas à la revue : c’est une capacité nouvelle.
- Limite assumée : un serveur malveillant dès sa première version est approuvé. Documentée, avec `on_new: block` pour les serveurs tiers.
- La forme canonique devient un contrat : la modifier changerait toutes les empreintes. Toute évolution passera par une nouvelle version du format de fichier et une migration.
- Les mises à jour légitimes demandent une action ; le diff et la commande d’approbation sont donnés dans le refus lui-même.

## Alternatives écartées

- **Empreinte du seul texte de description** : manque les changements de schéma, tout aussi lus par le modèle.
- **Base de réputation distante** : dépendance réseau et confiance dans un tiers ; envisageable plus tard en complément.
- **Confiance à chaque première apparition** (`on_new: pin` comme défaut) : une mise à jour qui ajoute un outil piégé sous un nouveau nom passerait sans revue. Gardée en option pour les serveurs internes qui ajoutent souvent des outils.
