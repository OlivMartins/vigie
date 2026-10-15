# Contribuer

Merci de votre intérêt pour Vigie. Les tickets et les pull requests sont bienvenus, en français ou en anglais.

## Avant de coder

Pour un changement de comportement, ouvrez d’abord un ticket : le [cadrage produit](docs/PRODUCT.md) et la [feuille de route](ROADMAP.md) disent ce que Vigie cherche à faire, et surtout ce qu’il ne fait pas exprès. Une décision structurante s’accompagne d’un [ADR](docs/adr/).

## Développer

Il faut Go 1.26 ou plus récent. Node.js 22 pour les tests de compatibilité et la suite de conformance, Python 3.12 et chDB pour la vérification de l’intégration Sonde, Docker pour l’image.

```bash
make build          # bin/vigie
make test           # tests unitaires et de bout en bout
make race           # les mêmes, avec le détecteur de concurrence
make lint           # golangci-lint
make demo           # la démo scriptée, en stdio
make demo-gateway   # la même, à travers la passerelle HTTP
make compat         # Vigie entre les SDK MCP officiels, en stdio et en HTTP
make conformance    # la suite de conformance MCP officielle, en direct puis à travers Vigie
make loadtest       # débit et latences de la passerelle sous charge
make help           # tout le reste
```

Un changement de la passerelle passe `make conformance` : tout scénario réussi en direct doit réussir à travers Vigie.

## Conventions

- **Langues** : documentation en français ; code, commentaires, messages du binaire et attributs de télémétrie en anglais. Les messages du binaire finissent dans des journaux et dans le contexte des modèles.
- **Transparence** : un message que Vigie ne modifie pas doit sortir à l’octet près, en stdio comme dans un corps HTTP. Toute nouvelle modification est documentée dans `docs/ARCHITECTURE.md` et testée.
- **Télémétrie** : les noms d’attributs vivent dans `internal/telemetry/semconv.go`. On suit les conventions OpenTelemetry quand elles existent ; un attribut `vigie.*` n’est créé que pour ce qu’elles ne couvrent pas. Aucune valeur sensible dans un attribut, un événement ou un journal.
- **Tests** : tout changement de comportement vient avec son test. Les clés d’exemple sont assemblées à l’exécution (`"AKIA" + "..."`) pour que le dépôt n’en contienne aucune.
- **Commits** : un sujet court qui dit ce qui change, un corps qui dit pourquoi.

## Publier une version

1. Mettre à jour `CHANGELOG.md`, le badge de version du README et `SECURITY.md`.
2. Créer le tag `vX.Y.Z` et publier la release sur GitHub : le workflow `Release` y joint les binaires des six plateformes et leurs sommes de contrôle, et publie l’image `ghcr.io/olivmartins/vigie` signée, avec SBOM et attestation de provenance.
