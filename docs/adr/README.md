# Décisions d’architecture

Chaque décision structurante est consignée ici, avec son contexte et ce qu’elle coûte. Une décision remplacée n’est pas effacée : elle passe au statut « remplacée » et pointe vers celle qui lui succède.

| N° | Décision | Statut |
| --- | --- | --- |
| [0001](0001-go-binaire-statique.md) | Go, un binaire statique | Acceptée |
| [0002](0002-proxy-json-rpc-sans-sdk.md) | Un proxy au niveau JSON-RPC, sans SDK MCP | Acceptée |
| [0003](0003-deux-spans-par-requete.md) | Deux spans par requête : réception et relais | Acceptée |
| [0004](0004-empreintes-de-la-premiere-session.md) | Empreintes de la première session, changements soumis à approbation | Acceptée |
| [0005](0005-refus-en-resultat-d-outil.md) | Un refus est un résultat d’outil en erreur, pas une erreur de protocole | Acceptée |
| [0006](0006-aucun-secret-dans-la-telemetrie.md) | Aucune valeur sensible dans la télémétrie | Acceptée |
| [0007](0007-passerelle-http-sans-etat.md) | Une passerelle HTTP sans état, une conversation par échange | Acceptée |
| [0008](0008-pas-d-ecoute-anonyme-hors-boucle-locale.md) | Pas d’écoute anonyme au-delà de la boucle locale | Acceptée |
