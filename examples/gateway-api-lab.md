# Gateway API lab

This lab starts from a fresh kiac cluster and walks through the Gateway
API objects kiac creates for you: `GatewayClass`, `Gateway`, Traefik's
LoadBalancer Service, and an application `HTTPRoute`.

## 1. Create a cluster with Gateway API

```sh
kiac create cluster --name gw-lab --workers 1 --gateway
```

At the end, kiac prints the Gateway address:

```text
Gateway API ready: http://<NODE-IP> (GatewayClass traefik, Gateway kiac-gateway/kiac)
```

You can recover it later:

```sh
GW=$(kubectl get svc traefik -n kiac-gateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
echo "$GW"
```

## 2. Inspect what kiac installed

`GatewayClass` is cluster-scoped. It says which controller owns Gateways
that use this class:

```sh
kubectl get gatewayclass
kubectl get gatewayclass traefik -o jsonpath='{.spec.controllerName}{"\n"}'
```

Expected controller:

```text
traefik.io/gateway-controller
```

`Gateway` is namespaced. kiac creates one default Gateway in
`kiac-gateway`, listening on HTTP port 80 and accepting routes from all
namespaces:

```sh
kubectl get gateway kiac -n kiac-gateway -o wide
kubectl get gateway kiac -n kiac-gateway -o yaml | sed -n '/listeners:/,/status:/p'
```

The Traefik implementation is a normal Deployment plus a `type:
LoadBalancer` Service:

```sh
kubectl get deploy,pods,svc -n kiac-gateway
```

The Service's `EXTERNAL-IP` is the address you curl from your Mac.

## 3. Apply an HTTPRoute

The companion manifest deploys a tiny echo app, a plain ClusterIP
Service, and an `HTTPRoute` attached to the default Gateway:

```sh
kubectl apply -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/httproute.yaml
kubectl rollout status deployment/echo --timeout=180s
```

The important route fields are:

```yaml
parentRefs:
  - name: kiac
    namespace: kiac-gateway
hostnames:
  - echo.local
rules:
  - backendRefs:
      - name: echo
        port: 80
```

`parentRefs` attaches the route to the pre-created Gateway. `hostnames`
means the request must include `Host: echo.local`. `backendRefs` points
at the app Service in the same namespace as the route.

## 4. Check attachment and status

```sh
kubectl get httproute echo -o wide
kubectl describe httproute echo
kubectl get gateway kiac -n kiac-gateway -o wide
```

Healthy signs:

- `kubectl get gateway` shows an `ADDRESS`.
- `kubectl get gateway` shows `PROGRAMMED=True`.
- `kubectl describe httproute` shows parent conditions such as
  `Accepted=True` and `ResolvedRefs=True`.

## 5. Send traffic through the Gateway

```sh
curl -fsS -H 'Host: echo.local' "http://$GW/" | head
```

The echo server returns JSON describing the request. You should see
`"hostname":"echo.local"` in the response.

Without the matching Host header, Traefik should return a 404 because no
route matches:

```sh
curl -i --max-time 10 "http://$GW/" | head
```

That 404 is useful: it proves the Gateway is reachable and route matching
is doing the filtering.

## 6. Try a path match

Add a second HTTPRoute rule that only matches `/api` for the same
hostname:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: echo-api
spec:
  parentRefs:
    - name: kiac
      namespace: kiac-gateway
  hostnames:
    - echo.local
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /api
      backendRefs:
        - name: echo
          port: 80
EOF
```

Probe it:

```sh
curl -fsS -H 'Host: echo.local' "http://$GW/api/version" | head
kubectl describe httproute echo-api
```

The response should show the original URL path, and the route status
should show that it attached to `kiac-gateway/kiac`.

## 7. Optional: run observability on the same cluster

Gateway API and observability are designed to coexist:

```sh
kiac create cluster --name full-lab --workers 1 --observability --gateway
```

Grafana uses port 3000 and Traefik uses 80/443, so the built-in
LoadBalancer can share one node IP for both when ports do not collide.
Use the [observability lab](observability-lab.md) to scrape an app and
then route traffic to it through the Gateway.

## Troubleshooting

If the route does not work:

```sh
kubectl get gatewayclass
kubectl get gateway -n kiac-gateway
kubectl describe httproute echo
kubectl get svc traefik -n kiac-gateway
kubectl logs -n kiac-gateway deploy/traefik
```

Common causes:

- `--gateway` was not enabled when the cluster was created.
- The request is missing the route's `Host` header.
- The `parentRefs.namespace` is not `kiac-gateway`.
- The backend Service name or port does not match the route.
- The cluster was created with `--no-lb`; `--gateway` requires the
  built-in LoadBalancer.

## Clean up

```sh
kubectl delete httproute echo-api --ignore-not-found
kubectl delete -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/httproute.yaml
kiac delete cluster --name gw-lab
```
