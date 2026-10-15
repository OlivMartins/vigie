# Déployer la passerelle

`vigie gateway` se déploie comme n’importe quel service HTTP sans état : une image, un fichier de configuration, des secrets dans l’environnement, des sondes de santé. Ce guide part d’un poste de travail et va jusqu’à plusieurs répliques derrière un répartiteur de charge.

## Sur un poste

```bash
vigie gateway --config vigie.yaml
```

La passerelle écoute `127.0.0.1:8787` et sert chaque serveur qui a une `url` sous `/mcp/<nom>`. Sur la boucle locale, aucun jeton n’est exigé : seules les requêtes de la machine arrivent, et les pages web d’autres sites sont refusées.

## L’image

À chaque version, la CI publie `ghcr.io/olivmartins/vigie` pour `linux/amd64` et `linux/arm64` : le binaire statique sur une base distroless, utilisateur non root, avec SBOM et attestation de provenance. L’image est signée sans clé par le workflow de publication ; vérifiez-la avant de la déployer :

```bash
cosign verify ghcr.io/olivmartins/vigie:0.2.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/OlivMartins/vigie/\.github/workflows/release\.yml@'
```

Par défaut, l’image lance `vigie gateway --config /etc/vigie/vigie.yaml --listen 0.0.0.0:8787`. Comme elle écoute alors toutes les interfaces du conteneur, la configuration doit donner `gateway.tokens`, ou `gateway.allow_anonymous: true` si un proxy en amont authentifie les clients : sinon la passerelle refuse de démarrer.

```bash
docker run --rm -p 8787:8787 \
  -v "$PWD/vigie.yaml:/etc/vigie/vigie.yaml:ro" \
  -v vigie-pins:/var/lib/vigie/pins \
  -e VIGIE_TOKEN_DATA -e BILLING_SERVICE_TOKEN \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318 \
  ghcr.io/olivmartins/vigie:0.2.0
```

Avec la configuration de la recette « passerelle d’équipe » de la [référence](configuration.md#recettes) (`pins_dir: /var/lib/vigie/pins`). Le volume des empreintes appartient à l’utilisateur `65532` de l’image.

## Ce qui est partagé, ce qui ne l’est pas

Chaque échange HTTP se suffit à lui-même ([ADR 0007](adr/0007-passerelle-http-sans-etat.md)) : un répartiteur de charge peut envoyer chaque requête à n’importe quelle réplique, sans affinité de session. Deux choses demandent attention :

- **Les fichiers d’empreintes** sont l’état commun : un outil approuvé, ou une définition modifiée en attente, doit l’être pour toutes les répliques. Montez `pins_dir` sur un volume partagé en lecture-écriture (`ReadWriteMany` : NFSv4, EFS, Filestore) ; Vigie y prend un verrou de fichier pour chaque écriture et remplace les fichiers de façon atomique. À défaut, une seule réplique avec un volume `ReadWriteOnce`.
- **La reprise des flux** des révisions 2025 (`GET` avec `Last-Event-ID`) n’est contrôlée que sur la réplique qui a vu la requête interrompue. Sans affinité, la réponse rejouée ailleurs est écartée et le client relance sa requête : rien ne passe sans contrôle, une requête est simplement refaite. Une affinité par `Mcp-Session-Id` l’évite, sans être nécessaire.

## Kubernetes

Un exemple complet : deux répliques, la configuration dans une ConfigMap, les secrets dans un Secret, les empreintes sur un volume partagé.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: vigie
data:
  vigie.yaml: |
    version: 1
    pins_dir: /var/lib/vigie/pins
    gateway:
      tokens:
        equipe-data: ${VIGIE_TOKEN_DATA}
    servers:
      billing:
        url: https://billing.internal.example/mcp
        headers:
          Authorization: Bearer ${BILLING_SERVICE_TOKEN}
---
apiVersion: v1
kind: Secret
metadata:
  name: vigie
stringData:
  # Vides tant qu’ils ne sont pas remplis : Vigie refuse alors de démarrer.
  VIGIE_TOKEN_DATA: ""        # openssl rand -hex 32
  BILLING_SERVICE_TOKEN: ""
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vigie-pins
spec:
  accessModes: ["ReadWriteMany"]
  resources:
    requests:
      storage: 100Mi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vigie
spec:
  replicas: 2
  selector:
    matchLabels: { app: vigie }
  template:
    metadata:
      labels: { app: vigie }
    spec:
      # Plus long que limits.stop_timeout (15 s) : les requêtes en cours finissent.
      terminationGracePeriodSeconds: 30
      securityContext:
        runAsNonRoot: true
        fsGroup: 65532
      containers:
        - name: vigie
          image: ghcr.io/olivmartins/vigie:0.2.0
          args: ["gateway", "--config", "/etc/vigie/vigie.yaml", "--listen", "0.0.0.0:8787", "--log-format", "json"]
          ports:
            - containerPort: 8787
          envFrom:
            - secretRef: { name: vigie }
          env:
            - name: OTEL_EXPORTER_OTLP_ENDPOINT
              value: http://otel-collector:4318
          readinessProbe:
            httpGet: { path: /readyz, port: 8787 }
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /healthz, port: 8787 }
            periodSeconds: 10
          resources:
            requests: { cpu: 100m, memory: 64Mi }
            limits: { memory: 256Mi }
          securityContext:
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities: { drop: ["ALL"] }
          volumeMounts:
            - { name: config, mountPath: /etc/vigie, readOnly: true }
            - { name: pins, mountPath: /var/lib/vigie/pins }
      volumes:
        - name: config
          configMap: { name: vigie }
        - name: pins
          persistentVolumeClaim: { claimName: vigie-pins }
---
apiVersion: v1
kind: Service
metadata:
  name: vigie
spec:
  selector: { app: vigie }
  ports:
    - port: 8787
      targetPort: 8787
```

Les commandes d’approbation tournent dans le conteneur, sur le même volume :

```bash
kubectl exec deploy/vigie -- vigie pins diff billing get_invoice --config /etc/vigie/vigie.yaml
kubectl exec deploy/vigie -- vigie pins approve billing get_invoice --fingerprint sha256:0332449c77f2 --config /etc/vigie/vigie.yaml
```

## Derrière un proxy ou un Ingress

- **Pas de mise en tampon.** Les réponses en flux (SSE) doivent passer événement par événement. Vigie envoie `X-Accel-Buffering: no`, que nginx respecte ; pour d’autres proxys, désactivez la mise en tampon des réponses (`proxy_buffering off`).
- **Délais longs.** Un flux `GET` ou `subscriptions/listen` reste ouvert tant que le client écoute. Portez le délai d’inactivité du proxy (`proxy_read_timeout`, délai d’inactivité du répartiteur cloud) au-delà de l’intervalle de maintien de connexion du serveur, une minute au moins.
- **TLS.** Terminez TLS au proxy, ou servez HTTPS directement avec `gateway.tls`.
- **Authentification déléguée.** Si le proxy authentifie déjà les clients (oauth2-proxy, un Ingress avec authentification externe), `gateway.allow_anonymous: true` autorise l’écoute sans jeton. Gardez alors la passerelle injoignable autrement que par ce proxy (NetworkPolicy).
- **Adresse du client.** `client.address` est l’adresse de la connexion reçue, donc celle du proxy ; Vigie ajoute la sienne à `X-Forwarded-For` vers le serveur.

## Arrêt et mises à jour

Sur SIGTERM, la passerelle répond `503` à `/readyz` (le répartiteur cesse de lui envoyer des requêtes), ferme aussitôt les flux durables (les clients se reconnectent sur une autre réplique), laisse aux requêtes en cours `limits.stop_timeout` pour finir, puis s’arrête. Une mise à jour progressive (*rolling update*) ne coupe donc qu’un flux durable par client, qui se rouvre ailleurs.

## Surveiller la passerelle

- Les spans et métriques MCP partent vers le collector configuré par les variables `OTEL_*` ; avec [Sonde](sonde.md), les requêtes de détection s’appliquent telles quelles à la passerelle.
- `vigie.decisions` compte les décisions par serveur, outil, règle ; `mcp.server.operation.duration` et `mcp.client.operation.duration` donnent la latence vue par le client et celle du serveur, leur différence le coût de Vigie.
- Le journal d’audit (`audit.file`) garde chaque décision sur disque, avec le nom du client et l’identifiant de trace.
