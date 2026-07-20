# graphify-service — Kubernetes deploy (`az-eastus-pltf-prd-prd`)

Deploys the graphify-service stack to the Vionix platform cluster at
**https://graphify.vionix.viasat.io** (internal / VPN-only). This overlay mirrors
the local `docker-compose.yaml` stack so Vionix developers build/debug locally with
compose and we run the same topology in Kubernetes for production.

> **Phase 1 = experiment.** Goal: stand up the async pipeline on k8s, compare it
> against compose, and learn the gaps (§ "compose ↔ k8s gaps"). Neo4j + Graphiti
> (the temporal-graph phase) are **not** deployed yet — see § "Next steps".

---

## What's deployed

| Compose service | K8s resource | Notes |
|-----------------|--------------|-------|
| `nats` | `StatefulSet nats` + headless `Service` (+ PVC) | JetStream, `nats://nats:4222` |
| `graphify-api` | `Deployment` + `Service graphify-api:8080` | the only externally-exposed service |
| `graphify-cloner` | `Deployment` + `Service` | NATS worker: `clone.requested → cloned` |
| `graphify-worker` | `Deployment` + `Service` | NATS worker: `cloned → graph.ready` (`graphify extract`) |
| shared `./data/repos` bind mount | `PersistentVolumeClaim graphify-repos` (RWX) | see § "compose ↔ k8s gaps" |
| — | Istio `Certificate`+`Gateway`+`VirtualService` | host + TLS for `graphify.vionix.viasat.io` |
| `graphify-mcp`, `graphify`, `graphify-service` (compose) | not deployed | dev/CLI-only surfaces |
| `neo4j` (compose, opt-in) | not deployed | temporal-graph phase — § "Next steps" |

Layout (one component dir per concern, aggregated by the top-level `kustomization.yaml`):

```
deploy/az-eastus-pltf-prd-prd/
├── kustomization.yaml     # aggregator + Artifactory image proxy/tag transformer
├── namespace/             # ns graphify (privileged PSA + istio-injection) + unrestricted-psp RB
├── storage/               # graphify-repos PVC (ReadWriteMany)
├── nats/                  # JetStream StatefulSet + headless Service
├── api/                   # graphify-api Deployment + Service
├── cloner/                # graphify-cloner Deployment + Service
├── worker/                # graphify-worker Deployment + Service
└── istio/                 # Certificate (istio-system) + Gateway + VirtualService (graphify)
```

---

## Operational model

- **Kustomize, applied manually.** ArgoCD is the platform's intended GitOps engine
  but is **not running** on this cluster yet, so we render + apply by hand. Per the
  **GitOps guardrail**, no operator/automation writes to the cluster — a human runs
  the apply, and this git tree is the source of truth to reconcile against later.
- **Images via Artifactory proxies, pinned.** Node egress to public registries is
  blocked. The top-level `kustomization.yaml` `images:` transformer rewrites:
  `ghcr.io/* → remote-ghcr.docker.artifactory.viasat.com/*` and
  `nats → dockerhub-oci.docker.artifactory.viasat.com/nats`. Bump `newTag` on merge.
- **TLS** issues automatically via the shared `ClusterIssuer vionix-viasat-io`
  (ACME + VICE DNS-01). The `Certificate` lives in `istio-system` (the ingress
  gateway resolves `credentialName` there); its `secretName` == the Gateway's
  `credentialName`.
- **DNS** is manual (no Istio→DNS automation). The CNAME is already created:
  `graphify.vionix.viasat.io → az-eastus-vionix-prod-istio.rancher.az.viasat.com.`
  (VICE, stripe `vionix`, internal view, TTL 900).

### Apply

```sh
# Render (validate first):
kustomize build deploy/az-eastus-pltf-prd-prd

# Apply (server-side; re-run once to clear the first-pass CRD/Certificate race):
kustomize build deploy/az-eastus-pltf-prd-prd | \
  kubectl apply --server-side --field-manager=kustomize -f -
```

Apply/dependency order (encoded by the `resources:` order): `namespace → storage →
nats → api → cloner → worker → istio`.

### Verify

```sh
kubectl -n graphify get deploy,statefulset,svc,pvc
kubectl -n istio-system get certificate graphify-vionix-viasat-io   # READY=True
kubectl -n graphify get gateway,virtualservice
curl -skI https://graphify.vionix.viasat.io/healthz                  # 200 (VPN/internal)
```

---

## Security posture (Phase 1)

The service **refuses** to start in `production` on a non-loopback bind with
`AUTH_MODE=none` unless `GRAPHIFY_INSECURE_ALLOW_NO_AUTH=1` is set. This overlay
sets that flag **deliberately** for the experiment: the host is internal-only
(internal Azure LB + VICE DNS, VPN-reachable) and there is no auth layer yet.

**This is not a production-hardened posture.** Replacing it is the first item under
"Next steps": either `AUTH_MODE=static` with a token from a Secret, or front the
host with the platform `oauth2-proxy` (`AuthorizationPolicy` + ext_authz).

---

## compose ↔ k8s gaps (the point of Phase 1)

1. **Shared repos volume.** Compose shares one host dir across api/cloner/worker.
   K8s needs **ReadWriteMany**; `storage/pvc-repos.yaml` requests `azurefile-csi`.
   **Confirm the cluster's RWX StorageClass before apply** (`kubectl get storageclass`)
   and adjust if the name differs — a wrong class leaves pods `Pending`.
2. **Worker's graphify base.** The worker image is `FROM marcellodesales/graphify`.
   If CI built it FROM the stale Docker Hub `:latest`, `graphify extract` may be
   missing/old and the worker stage will fail. Fix: publish a fresh graphify base
   to GHCR and build the worker `FROM` it (pass `GRAPHIFY_IMAGE` at build). Until
   then the pipeline may clone fine but fail at extract — a known Phase-1 gap.
3. **Image visibility.** The Artifactory `remote-ghcr` proxy must be able to pull
   the images. Ensure the GHCR packages (`graphify-service/{api,cloner,worker}`)
   are **public**, or publish to a Viasat first-party Artifactory repo.
4. **Image tag drift.** `newTag` in the top-level kustomization is pinned to a
   `main-<sha>` build; bump it (or wire a CI patch) when new images publish.
5. **SSH deploy keys.** Compose mounts `./secrets/ssh`; the k8s cloner has no SSH
   secret yet (public HTTPS clones only). Add a Secret + volume for private repos.

---

## Parallelization model (for future compose → k8s conversions)

What has **no dependencies** and can be done first / in parallel by separate agents:

- **VICE DNS CNAME** — depends on nothing (the ingress target is fixed). Do it
  immediately. *(Done for this host.)*
- **Image publish + visibility / Artifactory proxy availability** — independent of
  the manifests.
- **Manifest authoring** — namespace, storage, nats, api, cloner, worker, istio are
  authored independently; only their *apply* is ordered.
- **TLS Certificate** — issues independently of the CNAME and of the workloads
  (start it early; it can take minutes).

Synchronize on **apply** (ordered) and **integration verification** (curl the host,
run a submit→poll→artifacts smoke test).

---

## Next steps (what we still need to build)

1. **Auth hardening** — replace `GRAPHIFY_INSECURE_ALLOW_NO_AUTH=1` with
   `AUTH_MODE=static` (token Secret) or platform `oauth2-proxy`.
2. **Neo4j + Graphiti (temporal graph)** — add a `neo4j/` component (StatefulSet +
   PVC, **5.26+**) and a `graphiti/` deployment (MCP server + our ingest worker),
   per [`../../docs/GRAPHITI-INTEGRATION.md`](../../docs/GRAPHITI-INTEGRATION.md).
   Neo4j creds via an `ExternalSecret` (cluster serves `devsecops-cluster-secret-store`).
3. **Worker graphify base fix** — build the worker `FROM` a fresh GHCR graphify base.
4. **Dev environment** — a second overlay (e.g. `deploy/az-eastus-pltf-ppd-dev/` at
   `dev.graphify.vionix.viasat.io`) once Phase 1 is validated.
5. **CI image-tag automation** — patch `newTag` on merge instead of manual bumps.
6. **ArgoCD** — when the platform enables it, add an `Application` pointing here
   (`spec.source.kustomize.enableHelm: true` if helm charts are later introduced).
