<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/banner-dark.png">
    <img src="docs/assets/banner-light.png" width="100%" alt="Vigie : chaque appel d’outil de vos agents IA, vu, épinglé, filtré">
  </picture>
</p>

<p align="center">
  <b>Un serveur MCP peut réécrire la description de ses outils après votre feu vert, recevoir une clé d’API glissée dans un argument ou exposer un outil que personne n’a validé.<br>
  Vigie se place entre l’agent et ses serveurs, trace chaque appel en OpenTelemetry et arrête ce qui n’a rien à faire là.</b>
</p>

<p align="center">
  <a href="https://github.com/OlivMartins/vigie/releases/latest"><img src="https://img.shields.io/badge/version-0.1.0-F2C230?style=flat-square&labelColor=1B2A33" alt="Version 0.1.0"></a>
  <a href="https://github.com/OlivMartins/vigie/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/OlivMartins/vigie/ci.yml?branch=main&style=flat-square&label=tests&labelColor=1B2A33" alt="Tests"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/licence-Apache%202.0-1E7A68?style=flat-square&labelColor=1B2A33" alt="Licence Apache 2.0"></a>
  <img src="https://img.shields.io/badge/MCP-2025--11--25%20%C2%B7%202026--07--28-53636B?style=flat-square&labelColor=1B2A33" alt="MCP 2025-11-25 et 2026-07-28">
  <img src="https://img.shields.io/badge/OpenTelemetry-conventions%20MCP-53636B?style=flat-square&labelColor=1B2A33" alt="OpenTelemetry, conventions MCP">
  <img src="https://img.shields.io/badge/Go-1.26%2B-53636B?style=flat-square&labelColor=1B2A33" alt="Go 1.26 ou plus récent">
</p>

<p align="center">
  <a href="#démarrer"><b>Démarrer</b></a>
  &nbsp;&nbsp;|&nbsp;&nbsp;
  <a href="#brancher-votre-client-mcp"><b>Brancher votre client</b></a>
  &nbsp;&nbsp;|&nbsp;&nbsp;
  <a href="#sous-le-capot"><b>Sous le capot</b></a>
  &nbsp;&nbsp;|&nbsp;&nbsp;
  <a href="docs/ARCHITECTURE.md"><b>Architecture</b></a>
  &nbsp;&nbsp;|&nbsp;&nbsp;
  <a href="ROADMAP.md"><b>Feuille de route</b></a>
</p>

<br>

## En 30 secondes

- **Le problème.** Un agent IA agit par ses outils, et ses outils viennent de serveurs MCP tiers, installés en une ligne, exécutés avec vos droits. Leur description guide le modèle : la changer après coup suffit à le détourner. Et personne ne voit ce qui part réellement dans les arguments.
- **La réponse.** Un proxy, un binaire, placé devant la commande du serveur. Il trace selon les conventions MCP d’OpenTelemetry, épingle chaque définition d’outil et applique une politique écrite en YAML.
- **Ce qu’on obtient.** Les appels d’outils dans la même trace que l’agent, les définitions modifiées masquées jusqu’à votre revue, les clés et jetons arrêtés avant de quitter la machine, un mode audit pour mesurer avant d’imposer.
- **Ce qu’il faut.** Rien d’autre que le binaire. Sans configuration, les valeurs par défaut protègent déjà. Pour voir les traces, n’importe quel backend OTLP, par exemple [Sonde](https://github.com/OlivMartins/sonde).

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/demo-dark.png">
    <img src="docs/assets/demo-light.png" width="100%" alt="La démo de Vigie : un serveur MCP de facturation utilisé, puis mis à jour avec une description d’outil piégée. Vigie masque l’outil modifié, bloque l’appel, refuse une clé AWS glissée dans un argument et montre le diff de la définition">
  </picture>
</p>

## Les trois questions auxquelles Vigie répond

<table>
<tr>
<td width="33%" valign="top">

**Que font les outils de mon agent ?**

Chaque requête MCP devient deux spans conformes aux conventions d’OpenTelemetry, rattachés à la trace de l’agent par `params._meta`. Durée par outil, erreurs, révision du protocole, et le temps que Vigie y ajoute (≈ 65 µs).

</td>
<td width="33%" valign="top">

**Ont-ils changé depuis que je les ai validés ?**

Les outils de la première session avec un serveur sont épinglés. Une description ou un schéma modifié, un outil apparu ensuite : masqués au modèle et leurs appels refusés jusqu’à `vigie pins approve`, le diff exact sous les yeux.

</td>
<td width="33%" valign="top">

**Qu’est-ce qui sort par les arguments ?**

Onze formats de clés et jetons détectés : bloqués quand ils partent vers un serveur, signalés quand ils reviennent au modèle. Des outils autorisés ou interdits par serveur. Jamais une valeur sensible dans la télémétrie.

</td>
</tr>
</table>

## Démarrer

**La démo**, sans rien installer d’autre que Go : un serveur de facturation, utilisé puis mis à jour avec une description piégée.

```bash
git clone https://github.com/OlivMartins/vigie.git && cd vigie
make demo
```

**L’installation**, au choix :

```bash
go install github.com/OlivMartins/vigie/cmd/vigie@latest
```

ou un binaire pour Linux, macOS ou Windows (amd64, arm64) depuis les [releases](https://github.com/OlivMartins/vigie/releases/latest), avec ses sommes de contrôle.

**L’usage** : préfixez la commande de votre serveur MCP.

```bash
vigie run --server filesystem -- npx -y @modelcontextprotocol/server-filesystem ~/projets
```

`--server` nomme le serveur : il choisit sa section dans la politique et son fichier d’empreintes. Tout ce qui suit `--` est la commande d’origine, inchangée.

## Brancher votre client MCP

Dans un client configuré en JSON (Claude Desktop, Cursor, `.mcp.json` de Claude Code), remplacez la commande du serveur par Vigie :

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "vigie",
      "args": ["run", "--server", "filesystem", "--", "npx", "-y", "@modelcontextprotocol/server-filesystem", "/Users/moi/projets"],
      "env": {
        "OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318"
      }
    }
  }
}
```

Avec la ligne de commande de Claude Code :

```bash
claude mcp add filesystem -- vigie run --server filesystem -- npx -y @modelcontextprotocol/server-filesystem ~/projets
```

Le client ne voit aucune différence : mêmes outils, mêmes réponses. Seuls changent les outils masqués, les appels refusés (avec leur raison) et le contexte de trace ajouté aux requêtes.

## La politique

Un fichier YAML, des défauts et des exceptions par serveur. Tout est optionnel sauf la version :

```yaml
version: 1
defaults:
  mode: enforce              # audit : tout passe, ce qui aurait été bloqué est enregistré
servers:
  github:
    pins:
      on_new: block          # chaque outil attend une approbation explicite
    tools:
      deny: ["delete_*", "merge_pull_request"]
  filesystem:
    tools:
      deny: ["write_*", "move_*"]
```

`vigie check` valide le fichier et affiche la politique de chaque serveur. Référence complète : [docs/configuration.md](docs/configuration.md), exemple commenté : [examples/vigie.yaml](examples/vigie.yaml).

Quand un outil change, la revue tient en deux commandes :

```bash
vigie pins diff billing get_invoice
# ce qui a changé, caractères invisibles révélés, et la commande d’approbation :
vigie pins approve billing get_invoice --fingerprint sha256:0332449c77f2
```

L’empreinte lie l’approbation à ce que vous avez relu : si le serveur envoie entre-temps une autre définition, l’approbation échoue.

Le fichier d’empreintes de chaque serveur est un JSON lisible : versionnez-le avec votre projet et relisez ses changements en revue de code, comme un fichier de verrouillage de dépendances.

## La télémétrie

Indiquez un point de collecte OTLP, Vigie s’occupe du reste :

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
```

Sans point de collecte, la télémétrie reste éteinte ; `--trace-file spans.jsonl` écrit les spans dans un fichier. Les attributs suivent les [conventions MCP d’OpenTelemetry](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/mcp.md) (`mcp.method.name`, `gen_ai.tool.name`, `jsonrpc.request.id`, `mcp.protocol.version`...) ; les décisions s’ajoutent sous `vigie.*`. La liste complète est dans [l’architecture](docs/ARCHITECTURE.md#télémétrie).

**Avec Sonde**, les décisions de Vigie se lisent en SQL à côté des coûts et de la qualité des agents : décisions par règle, définitions modifiées, secrets par type et par outil, latence par outil et surcoût de Vigie. Branchement et requêtes : [docs/sonde.md](docs/sonde.md).

## Sous le capot

Quelques choix qui font la différence entre une démo et un outil sur lequel compter :

- **Transparent à l’octet près.** Vigie travaille au niveau JSON-RPC, sans SDK MCP : ce qu’il ne modifie pas sort tel quel, membres inconnus compris. Il relaie la poignée de main `initialize` des révisions 2024-11-05 à 2025-11-25 comme la révision sans état 2026-07-28 (`_meta` par requête, `resultType`, `ttlMs`).
- **Testé avec les SDK officiels, et contre eux.** La CI fait passer les SDK MCP en TypeScript, v1 et v2, client et serveur, à travers Vigie, dans les deux ères du protocole. Puis un serveur malveillant tente de leur faire parvenir un outil interdit ou modifié par six chemins détournés.
- **Rien d’ambigu ne passe.** Un message qu’un pair pourrait lire autrement que Vigie (membre en double, `"Name"` à côté de `"name"`), une réponse qui ne correspond exactement à aucune requête en attente, une ligne illisible : en mode enforce, rien de cela n’est relayé, dans un sens comme dans l’autre. Une divergence d’analyse suffirait sinon à montrer une chose à Vigie et une autre au client ou au serveur.
- **Deux spans par requête.** Un span SERVER pour la réception, un span CLIENT pour le relais : la trace se lit d’un bloc jusqu’au serveur instrumenté, et leur différence mesure ce que coûte Vigie.
- **Des refus qui s’expliquent.** Un appel bloqué revient au modèle comme un résultat d’outil en erreur, avec la règle et l’emplacement : l’agent comprend, prévient l’utilisateur et ne réessaie pas en boucle.
- **Aucun secret dans la télémétrie.** Une décision dit `arguments.body` et `aws_access_key_id`, jamais la valeur. Les tests le vérifient sur les spans produits, contenu capturé, noms de membres, URI de ressources et messages d’erreur compris.
- **Un arrêt propre.** Sous Linux et macOS, le serveur tourne dans son propre groupe de processus et s’arrête selon la séquence du transport stdio (fermeture de l’entrée, SIGTERM, SIGKILL). S’il s’arrête de lui-même, son code de sortie remonte à l’hôte.

Les détails, les choix écartés et les limites connues : [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) et les [décisions d’architecture](docs/adr/).

## Tests

```bash
make test && make lint
```

- Tests unitaires et de bout en bout, avec le détecteur de concurrence, sous Linux et macOS : relais, spans et attributs, empreintes à travers deux sessions, refus, annulations, lots, processus serveur réel.
- Tests adverses : messages ambigus, identifiants de réponse détournés, définitions sans nom, secrets cachés dans des noms de membres, côté client comme côté serveur.
- `make compat` fait dialoguer les SDK MCP officiels à travers Vigie, puis les confronte à un serveur malveillant ; `make sonde-check` rejoue les spans de la démo dans le schéma ClickHouse de Sonde et exécute chaque requête de la documentation.
- `make fuzz` éprouve l’analyseur JSON-RPC et le détecteur de secrets, `make bench` mesure le temps de traitement d’un appel.

## D’où vient Vigie

En construisant [Sonde](https://github.com/OlivMartins/sonde), une question revenait : l’agent trace ses appels d’outils, mais que reçoit vraiment le serveur, et que fait-on quand la description d’un outil change sous nos pieds ? La révision 2026-07-28 du protocole MCP a confié la journalisation à OpenTelemetry, les attaques par description d’outil sont documentées depuis 2025, et les parades recommandées sont connues : empreinte des définitions, liste d’outils autorisés, passerelle entre l’agent et ses serveurs. Vigie les rassemble dans un binaire ouvert, sur les standards existants.

Le cadrage du produit, ses cibles et ce qu’il ne fait pas exprès : [docs/PRODUCT.md](docs/PRODUCT.md).

## Auteur

Olivier, tech lead sur des plateformes de logs à grande échelle et des backends d’observabilité pour agents IA. OpenTelemetry, ClickHouse, Kafka, Elasticsearch, sécurité.

Une question sur Vigie, un projet d’observabilité ou de sécurité des agents ? Ouvrez une issue ou écrivez-moi sur [LinkedIn](https://www.linkedin.com/in/olivf).

Voir aussi [Sonde](https://github.com/OlivMartins/sonde), l’observabilité des agents IA en briques ouvertes, et [Passerelle](https://github.com/OlivMartins/passerelle), la migration d’Elasticsearch vers ClickHouse.

## Licence

Apache 2.0. Voir [LICENSE](LICENSE).
