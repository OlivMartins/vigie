# Politique de sécurité

Vigie est un composant de sécurité : une faille dans Vigie, c’est une politique qui ne s’applique pas. Les signalements sont traités en priorité.

## Versions maintenues

| Version | Correctifs de sécurité |
|---|---|
| 0.2.x | Oui |
| 0.1.x | Jusqu’à la publication de 0.3.0 |

## Signaler une vulnérabilité

Merci de ne pas ouvrir de ticket public pour une faille de sécurité.

Utilisez le signalement privé de GitHub : onglet **Security** du dépôt, puis **Report a vulnerability**. Le rapport n’est visible que des mainteneurs.

Pour un traitement rapide, précisez :

- la version de Vigie (`vigie version`) et la plateforme ;
- le client et le serveur MCP en jeu, et la révision du protocole ;
- la configuration utilisée, sans secret réel ;
- les étapes pour reproduire et l’impact constaté.

Accusé de réception sous 72 heures. Le correctif est publié dans une nouvelle version, accompagné d’un avis de sécurité. Le rapporteur y est crédité s’il le souhaite.

## Périmètre

Sont notamment concernés :

- un appel qui atteint le serveur alors que la politique le refuse, en mode enforce ;
- une définition d’outil modifiée ou non approuvée qui atteint le modèle ;
- une divergence d’analyse entre Vigie et un client ou un serveur MCP (un message lu différemment des deux côtés) ;
- une valeur sensible écrite dans la télémétrie ou les journaux ;
- un message qui fait planter Vigie ou le bloque ;
- pour la passerelle HTTP : une requête acceptée sans jeton valide quand des jetons sont configurés, une écoute hors boucle locale sans authentification, une page web d’une origine non autorisée qui atteint un serveur, un en-tête HTTP qui fait lire au serveur autre chose que ce que Vigie a contrôlé, une réponse rejouée ou glissée sur un flux qui échappe aux contrôles, un secret configuré dans `headers` qui fuit vers un client ou la télémétrie.

Ne sont pas des failles de Vigie : un serveur malveillant dès sa première version, dont les outils sont approuvés lors de la première session (limite documentée, `on_new: block` pour l’éviter), un secret dans un format que les règles ne reconnaissent pas, un comportement d’outil modifié sans changement de sa définition, une passerelle exposée avec `allow_anonymous: true` sans proxy authentifiant devant elle, les requêtes que le serveur envoie au client sur un flux (relayées, non contrôlées en v0.2). Ces limites sont décrites dans [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#limites-connues) ; les propositions pour les réduire sont bienvenues en ticket public.
