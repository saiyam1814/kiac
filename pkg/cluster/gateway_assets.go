package cluster

import _ "embed"

// gatewayCRDsManifest is the upstream Gateway API EXPERIMENTAL channel
// install (release v1.5.1, experimental-install.yaml from
// github.com/kubernetes-sigs/gateway-api). Experimental rather than
// standard because Traefik v3.7's kubernetesgateway provider
// unconditionally starts informers for TLSRoute (v1) and
// BackendTLSPolicy; without those CRDs its sync wedges and the
// GatewayClass never goes Accepted. Applied with --server-side: the
// HTTPRoute CRD alone exceeds the client-side 256KB annotation cap.
//
//go:embed assets/gateway/gateway-api-crds.yaml
var gatewayCRDsManifest string

// gatewayTraefikManifest runs Traefik v3 (kubernetesgateway provider,
// stable since v3.2) in namespace kiac-gateway behind a type
// LoadBalancer Service on 80/443. The provider watches all namespaces
// and publishes the Service's LB address into Gateway status.
//
//go:embed assets/gateway/traefik.yaml
var gatewayTraefikManifest string

// gatewayDefaultManifest is the GatewayClass "traefik" plus the default
// Gateway kiac-gateway/kiac with an HTTP listener on 80 open to routes
// from every namespace.
//
//go:embed assets/gateway/default-gateway.yaml
var gatewayDefaultManifest string
