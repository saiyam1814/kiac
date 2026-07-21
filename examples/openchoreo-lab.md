# OpenChoreo on kiac

[OpenChoreo](https://openchoreo.dev) is an open-source internal developer
platform for Kubernetes (CNCF Sandbox). This lab installs it on a kiac cluster
with one script, deploys a sample service, and reaches it through kiac's
built-in LoadBalancer.

Every OpenChoreo plane runs on VM-isolated kiac nodes, so it behaves like a real
cluster: real node IPs, a working `type: LoadBalancer`, and per-node kernels. No
Docker Desktop, no cloud.

## 1. Install

```sh
curl -fsSL https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/openchoreo.sh | bash -s -- --version 1.1.2
```

This creates a kiac cluster (`kiac-openchoreo`), installs the OpenChoreo control
plane and data plane, and wires reachability. First run takes ~10-15 minutes
(image pulls). At the end it prints the LoadBalancer IP, an `/etc/hosts` line,
and a sample deploy command.

Already have a cluster? Add `--use-existing` to install into it instead of
creating one. Pass `--cni cilium` if you want NetworkPolicies enforced.

## 2. Deploy a component

```sh
kubectl --context kiac-openchoreo apply \
  -f https://raw.githubusercontent.com/openchoreo/openchoreo/v1.1.2/samples/from-image/go-greeter-service/greeter-service.yaml
```

You wrote a `Component` and a `Workload`. OpenChoreo turns that into a real
workload — its own namespace, a Deployment, a Service, and an HTTPRoute:

```sh
kubectl --context kiac-openchoreo get deploy,svc,httproute -A -l openchoreo.dev/component=greeter-service
```

## 3. Reach it

kiac-lb gave the data-plane gateway a routable IP, so you can curl the endpoint
through it (no `/etc/hosts` needed):

```sh
LB=$(kubectl --context kiac-openchoreo get svc gateway-default -n openchoreo-data-plane \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl -H 'Host: development-default.openchoreoapis.localhost' \
  "http://$LB:19080/greeter-service-http/greeter/greet?name=you"
# -> Hello, you!
```

## 4. Open the portal (optional)

Add the printed `/etc/hosts` line, then open http://openchoreo.localhost:8080
and sign in with `admin@openchoreo.dev` / `Admin@123`.

Portal login also needs *in-cluster* resolution of `*.openchoreo.localhost` (the
backend validates tokens against the identity provider). On the default kubeadm
distro, add a CoreDNS entry mapping those hosts to the LoadBalancer IP.

## Clean up

```sh
kiac delete cluster --name openchoreo
```

## Notes

- kgateway watches the Gateway API `TLSRoute` type, so the script installs the
  experimental Gateway API channel (a superset of standard).
- The workflow (build) and observability planes are not wired yet.
