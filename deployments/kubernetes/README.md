# Kubernetes

```sh
# build and push the four images (api, scheduler, worker, web), then set them in kustomization.yaml
docker build -f deployments/docker/Dockerfile.service --build-arg APP=api -t REGISTRY/synapse-api:TAG .
docker build -f deployments/docker/Dockerfile.service --build-arg APP=scheduler -t REGISTRY/synapse-scheduler:TAG .
docker build -f deployments/docker/Dockerfile.service --build-arg APP=worker -t REGISTRY/synapse-worker:TAG .
docker build -f deployments/docker/Dockerfile.web -t REGISTRY/synapse-web:TAG .

# edit config.yaml (public URL, master key, DB password), then
kubectl apply -k deployments/kubernetes
```

- `postgres.yaml` is a single-node StatefulSet for evaluation only; use managed Postgres in
  production and point `SYNAPSE_DATABASE_URL` at it.
- The API applies migrations on boot (advisory-locked), so rolling out several replicas is safe.
- Workers scale horizontally on CPU (HPA); a killed worker's leases expire and its tasks are re-queued.
- Pods carry `prometheus.io/*` annotations. Set `SYNAPSE_METRICS_TOKEN` and configure the scrape
  job with a bearer token if `/metrics` must not be open inside the cluster.
- The manifests have not been applied to a live cluster in this repository's test runs; they
  are validated for YAML well-formedness only.
