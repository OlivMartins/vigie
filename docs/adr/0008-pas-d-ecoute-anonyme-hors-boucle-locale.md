# 0008 · Pas d’écoute anonyme au-delà de la boucle locale

**Statut** : acceptée, octobre 2026

## Contexte

Une passerelle MCP donne accès à des outils qui lisent des fichiers, envoient des e-mails, écrivent en base. Exposée sans authentification sur une interface réseau, elle offre ces outils à quiconque la joint. La spécification demande aux serveurs HTTP de valider l’en-tête `Origin`, de n’écouter que la boucle locale quand ils tournent en local, et d’authentifier leurs clients.

L’identité complète (OAuth, droits par personne) est le sujet de la version 0.4. Une équipe qui déploie la 0.2 a pourtant besoin d’une protection, ou d’assumer explicitement qu’un proxy en amont l’assure.

## Décision

- Par défaut, la passerelle écoute `127.0.0.1:8787`. Elle n’accepte alors que les noms d’hôte de la boucle locale dans l’en-tête `Host`, et que les pages servies depuis la machine dans l’en-tête `Origin` : une page web d’un autre site, même résolue vers 127.0.0.1, est refusée (*DNS rebinding*).
- `gateway.tokens` associe un nom à un jeton porteur (24 caractères au moins, lu de préférence dans une variable d’environnement). Un client sans jeton valide reçoit `401`. Les jetons sont comparés en temps constant, le nom du client va dans les spans (`vigie.client.name`) et le journal d’audit. Le jeton reste à la passerelle : il n’est pas transmis au serveur, qui reçoit ses propres identifiants par `servers.*.headers`. Sans jetons configurés, l’en-tête `Authorization` du client passe tel quel : Vigie est alors un intermédiaire transparent, et le jeton est celui que le client a obtenu pour le serveur.
- La passerelle refuse de démarrer sur une adresse joignable depuis le réseau sans jeton, sauf `gateway.allow_anonymous: true`, réservé aux déploiements où un proxy en amont authentifie déjà les clients.
- Une redirection du serveur n’est pas suivie ni relayée en mode enforce : suivie par le client, elle contournerait Vigie avec ses identifiants.

## Conséquences

- La configuration la plus courte est sûre ; l’exposition se décide, elle n’arrive pas par oubli.
- Des jetons statiques ne disent pas *qui* appelle, seulement *quel client* : suffisant pour une équipe et des traces attribuables, pas pour des droits par personne (0.4).
- La découverte OAuth à travers la passerelle (métadonnées de ressource protégée, en-tête `WWW-Authenticate`) n’est pas réécrite : un serveur protégé par OAuth se branche avec un identifiant de service dans `servers.*.headers`.

## Alternatives écartées

- **Écouter partout par défaut, comme beaucoup de serveurs d’exemple** : la première démonstration exposerait les outils de l’équipe.
- **Relayer au serveur le jeton qui ouvre la passerelle** : le même secret ouvrirait les deux ; les bonnes pratiques de sécurité MCP proscrivent de présenter un jeton à un autre destinataire que celui pour lequel il a été émis (*token passthrough*).
