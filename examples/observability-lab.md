# Observability lab

This lab starts from a fresh kiac cluster and proves the full path:
Grafana gets a real LoadBalancer IP, Prometheus scrapes an annotated
app, and the sample counter moves when you send traffic.

## 1. Create a cluster with observability

```sh
kiac create cluster --name obs-lab --workers 1 --observability
```

At the end, kiac prints a Grafana URL:

```text
Grafana: http://<NODE-IP>:3000 (anonymous admin, local-only)
```

You can recover it later:

```sh
GRAFANA_IP=$(kubectl get svc grafana -n kiac-observability -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
open "http://$GRAFANA_IP:3000"
```

Grafana is intentionally anonymous admin for local dev clusters. Open
the **Cluster Overview** or **Nodes** dashboard first; both are
provisioned automatically.

## 2. Deploy an app with Prometheus annotations

The companion manifest runs nginx plus the official nginx Prometheus
exporter sidecar. The only observability integration is on the pod
template:

```yaml
prometheus.io/scrape: "true"
prometheus.io/port: "9113"
```

Apply it:

```sh
kubectl apply -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/observability-scrape.yaml
kubectl rollout status deployment/scrapeme --timeout=180s
```

Generate some traffic:

```sh
kubectl run traffic --image=docker.io/nginx:1.29-alpine --restart=Never --rm -it -- \
  sh -c 'for i in $(seq 50); do wget -q -O /dev/null http://scrapeme; done; echo done'
```

## 3. Query Prometheus directly

Prometheus is ClusterIP-only, so use a port-forward when you want the
raw UI or API from your Mac:

```sh
kubectl port-forward -n kiac-observability svc/prometheus 9090:9090
```

In another terminal:

```sh
curl -s 'http://localhost:9090/api/v1/query?query=nginx_http_requests_total%7Bpod%3D~%22scrapeme.*%22%7D'
```

Within one or two 30-second scrape intervals, the response should
contain a non-empty `result` array.

## 4. Query it from Grafana

Open Grafana Explore:

```sh
open "http://$GRAFANA_IP:3000/explore"
```

Select the bundled Prometheus datasource and run:

```promql
rate(nginx_http_requests_total{pod=~"scrapeme.*"}[2m])
```

You should see one series for the `scrapeme` pod. The labels include
`namespace` and `pod`, so app metrics can be correlated with the
cluster dashboards.

## 5. Optional: run Gateway API on the same cluster

Observability and Gateway API are designed to coexist. Create them
together:

```sh
kiac create cluster --name full-lab --workers 1 --observability --gateway
kubectl apply -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/httproute.yaml

GW=$(kubectl get svc traefik -n kiac-gateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl -H 'Host: echo.local' "http://$GW/"
```

Grafana uses port 3000 and Traefik uses 80/443, so the built-in
LoadBalancer can share one node IP for both when ports do not collide.

## Clean up

```sh
kubectl delete -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/observability-scrape.yaml
kiac delete cluster --name obs-lab
```
