# 0007 · Une passerelle HTTP sans état, une conversation par échange

**Statut** : acceptée, octobre 2026

## Contexte

Les serveurs MCP les plus exposés tournent en local, en stdio ; Vigie 0.1 les couvrait. Mais une équipe branche aussi des serveurs distants, en Streamable HTTP, et veut un point unique où appliquer sa politique. Le transport HTTP vit deux époques à la fois :

- de 2025-03-26 à 2025-11-25 : sessions (`Mcp-Session-Id`), flux SSE ouvert par un GET, requêtes du serveur sur ces flux, reprise par `Last-Event-ID`, lots jusqu’à 2025-03-26 ;
- depuis 2026-07-28 : plus de session, chaque POST porte sa version et ses capacités, les en-têtes `Mcp-Method` et `Mcp-Name` recopient le corps pour que les intermédiaires puissent router, les notifications durables passent par `subscriptions/listen`.

Une passerelle d’équipe tourne à plusieurs instances derrière un répartiteur de charge. Ce que l’une apprend, l’autre l’ignore.

## Décision

- `vigie gateway` sert chaque serveur sous `/mcp/{serveur}` et relaie vers son URL. Le client pointe vers Vigie au lieu du serveur ; rien d’autre ne change.
- Chaque échange HTTP est traité par sa propre conversation : la logique du relais stdio (politique, empreintes, spans, appariement des réponses), extraite pour être partagée. Une réponse n’est relayée que si elle correspond exactement à une requête de l’échange qui la porte.
- Vigie ne garde aucun état de session : les identifiants de session circulent entre le client et le serveur, Vigie n’en enregistre qu’un hachage dans la télémétrie. L’état partagé se limite aux fichiers d’empreintes.
- Les en-têtes MCP et le corps doivent concorder, quelle que soit la révision : un en-tête `Mcp-Method`, `Mcp-Name` ou `Mcp-Protocol-Version` qui contredit le corps, ou absent quand 2026-07-28 l’exige, vaut un refus `400` avec l’erreur `-32020` (`HeaderMismatch`).
- Les réponses en flux sont relues événement par événement : chaque message est contrôlé, les identifiants d’événement et les commentaires sont conservés pour que la reprise fonctionne.
- Les requêtes émises par le serveur (révisions 2025) sont relayées sans span : leur réponse revient dans un autre échange HTTP, peut-être sur une autre instance.
- La reprise d’un flux interrompu (`GET` avec `Last-Event-ID`) est la seule exception à l’absence d’état : la requête interrompue est retenue dix minutes en mémoire, pour que la réponse rejouée soit contrôlée comme les autres. Sur une autre instance, elle n’est pas retrouvée et la réponse est écartée en mode enforce : le client relance sa requête.

## Conséquences

- Plusieurs instances derrière un répartiteur, sans affinité : un échange se suffit à lui-même. Seule la reprise de flux des révisions 2025 profite d’une affinité, sans en dépendre.
- Les intermédiaires placés après Vigie peuvent se fier aux en-têtes MCP : Vigie a vérifié qu’ils disent la même chose que le corps.
- Le contrôle porte sur les messages, pas sur les connexions : un flux SSE de plusieurs heures est contrôlé message par message, sans tampon.
- La « première session » de l’épinglage change de sens : en passerelle, c’est la première liste d’outils jamais enregistrée pour un serveur (et la minute qui suit, le temps qu’arrivent les pages d’une liste paginée), pas la première session d’un client.
- Le transport HTTP+SSE de 2024-11-05, déprécié, n’est pas relayé.

## Alternatives écartées

- **Une session Vigie par session MCP** : une table de sessions à répliquer entre instances, ou une affinité obligatoire ; et rien à mettre dedans pour la révision 2026-07-28, sans session.
- **Faire confiance aux en-têtes pour router et décider** : c’est précisément la divergence qu’un attaquant exploiterait, un répartiteur lisant un nom d’outil et le serveur un autre.
- **Mandataire inverse générique** (`httputil.ReverseProxy`) avec un filtre de corps : la relecture des flux SSE, l’appariement des réponses et les refus en résultat d’outil ne s’y prêtent pas.
