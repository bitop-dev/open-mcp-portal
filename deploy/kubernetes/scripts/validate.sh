#!/usr/bin/env bash
# Renders the base and every overlay with kustomize and validates the output
# with kubeconform -strict (Gateway API schemas from the pinned
# datreeio/CRDs-catalog). Also checks that the generated ConfigMap carries a
# content hash that the Deployment references, so configuration changes roll
# the pods.
#
# Used by `make k8s-validate`. Tools are overridable:
#   KUSTOMIZE=kustomize KUBECONFORM=kubeconform deploy/kubernetes/scripts/validate.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
KUSTOMIZE="${KUSTOMIZE:-kustomize}"
KUBECONFORM="${KUBECONFORM:-kubeconform}"
K8S_SCHEMA_VERSION="${K8S_SCHEMA_VERSION:-1.34.0}"
CRDS_CATALOG_REF="${CRDS_CATALOG_REF:-ad3b08c5045129d7bb1eeffd8e61719b2c8dd1e2}"
crd_schemas="https://raw.githubusercontent.com/datreeio/CRDs-catalog/${CRDS_CATALOG_REF}/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"

# kustomize refuses absolute paths outside the kustomization root, so the
# base build happens in a scratch directory next to base/.
scratch="$(mktemp -d "${root}/.tmp.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT

validate() { # name, rendered file
  echo "== $1"
  "$KUBECONFORM" -strict -summary -kubernetes-version "$K8S_SCHEMA_VERSION" \
    -schema-location default -schema-location "$crd_schemas" "$2"
}

# The base, in a namespace (it sets none itself).
mkdir -p "${scratch}/base"
cat >"${scratch}/base/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: omp-validate
resources:
  - ../../base
EOF
"$KUSTOMIZE" build "${scratch}/base" >"${scratch}/base.yaml"
validate "base" "${scratch}/base.yaml"

for overlay in "${root}"/overlays/*/; do
  name="overlays/$(basename "$overlay")"
  out="${scratch}/$(basename "$overlay").yaml"
  "$KUSTOMIZE" build "$overlay" >"$out"
  validate "$name" "$out"
done

# The cluster-level part of the ingress smoke overlay (GatewayClass, EnvoyProxy):
# applied by scripts/smoke/ingress.sh, so not one of the overlays above.
"$KUSTOMIZE" build "${root}/overlays/smoke-ingress/controller" >"${scratch}/smoke-ingress-controller.yaml"
validate "overlays/smoke-ingress/controller" "${scratch}/smoke-ingress-controller.yaml"

# Files kept beside an overlay as alternatives (not in its resources).
validate "overlays/example/httproute.yaml" "${root}/overlays/example/httproute.yaml"
validate "overlays/example/servicemonitor.yaml" "${root}/overlays/example/servicemonitor.yaml"
validate "overlays/example/prometheusrule.yaml" "${root}/overlays/example/prometheusrule.yaml"
validate "overlays/example/networkpolicy-otlp.yaml" "${root}/overlays/example/networkpolicy-otlp.yaml"

# open-mcp-portal-config must be hashed and referenced by both containers.
check_config_refs() { # name, rendered file
  local cm refs
  cm="$(sed -n 's/^  name: \(open-mcp-portal-config-[a-z0-9]\{10\}\)$/\1/p' "$2" | head -n 1)"
  [ -n "$cm" ] || { echo "$1: open-mcp-portal-config has no content hash" >&2; exit 1; }
  # the ConfigMap itself, plus envFrom in the migrate and portal containers
  refs="$(grep -c "name: ${cm}\$" "$2" || true)"
  if [ "$refs" -lt 3 ] || grep -q "name: open-mcp-portal-config\$" "$2"; then
    echo "$1: the Deployment does not reference ${cm} in every container" >&2
    exit 1
  fi
  echo "== $1: config is ${cm}"
}
check_config_refs "base" "${scratch}/base.yaml"
check_config_refs "overlays/example" "${scratch}/example.yaml"
check_config_refs "overlays/smoke" "${scratch}/smoke.yaml"
check_config_refs "overlays/smoke-ingress" "${scratch}/smoke-ingress.yaml"
# The ingress overlay signs in through OIDC on an https APP_URL: dev sign-in off,
# the ingress policy allows the gateway's namespace, no image is left unpinned.
if ! { grep -q "^  DEV_AUTH: \"false\"\$" "${scratch}/smoke-ingress.yaml" && grep -q "^  APP_URL: https://omp.smoke.test\$" "${scratch}/smoke-ingress.yaml"; }; then
  echo "overlays/smoke-ingress: DEV_AUTH is not false on an https APP_URL" >&2
  exit 1
fi
grep -q "kubernetes.io/metadata.name: envoy-gateway-system" "${scratch}/smoke-ingress.yaml" ||
  { echo "overlays/smoke-ingress: the ingress policy does not allow the gateway's namespace" >&2; exit 1; }
if grep -E "^\s+image:" "${scratch}/smoke-ingress.yaml" "${scratch}/smoke-ingress-controller.yaml" | grep -v "omp-smoke/" | grep -v "@sha256:"; then
  echo "overlays/smoke-ingress: an image is not pinned by digest" >&2
  exit 1
fi
grep -q "^  APP_URL: https://mcp.example.org\$" "${scratch}/example.yaml" ||
  { echo "overlays/example: APP_URL not merged into the config" >&2; exit 1; }
if ! { grep -q "^  METRICS_ADDR: :9090\$" "${scratch}/example.yaml" && grep -q "containerPort: 9090" "${scratch}/example.yaml"; }; then
  echo "overlays/example: the metrics port is not wired" >&2
  exit 1
fi

# The overlays' images: entries still match the base's image name (the
# kustomize match key; an entry with another name silently changes nothing, so
# the example's digest pin would be lost): both portal containers of the
# example render pinned by digest, and the smoke overlays render the local image.
base_image="$(sed -n 's/^ *image: \(ghcr\.io\/[^:@]*\)$/\1/p' "${root}/base/deployment.yaml" | sort -u)"
if [ "$(printf '%s\n' "$base_image" | wc -l)" -ne 1 ] || ! grep -q "^  - name: ${base_image}\$" "${root}/base/kustomization.yaml"; then
  echo "base: deployment.yaml and kustomization.yaml do not name one portal image ('${base_image}')" >&2
  exit 1
fi
for overlay in example smoke; do
  grep -q "^  - name: ${base_image}\$" "${root}/overlays/${overlay}/kustomization.yaml" ||
    { echo "overlays/${overlay}: no images: entry for the base's image ${base_image}" >&2; exit 1; }
done
if [ "$(grep -cE '^ +image: ghcr\.io/[a-z0-9-]+/open-mcp-portal(:[^@ ]+)?@sha256:[0-9a-f]{64}$' "${scratch}/example.yaml")" -ne 2 ]; then
  echo "overlays/example: the portal containers are not pinned by digest" >&2
  exit 1
fi
if grep -E '^ +image: .*open-mcp-portal' "${scratch}/smoke.yaml" "${scratch}/smoke-ingress.yaml"; then
  echo "overlays/smoke: a portal container does not use the local image omp-smoke/portal" >&2
  exit 1
fi

# No secret values in the manifests: the runtime secret is referenced only
# (the smoke overlay's secrets are created by scripts/k8s-smoke.sh).
if grep -q "^kind: Secret\$" "${scratch}/base.yaml" "${scratch}"/example.yaml "${scratch}"/smoke.yaml "${scratch}"/smoke-ingress.yaml; then
  echo "a rendered manifest contains a Secret; secrets must be created outside the base" >&2
  exit 1
fi
echo "all manifests valid"
