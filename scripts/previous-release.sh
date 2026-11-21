#!/usr/bin/env bash
# Names the release the upgrade test installs first, and the digest of its
# image, so the test can install it by digest the way docs/deploy.md tells
# operators to.
#
#   scripts/previous-release.sh               # v0.15.2 ghcr.io/ncecere/open-mcp-portal@sha256:...
#   scripts/previous-release.sh --tag-only    # v0.15.2
#   scripts/previous-release.sh --tag v0.7.0  # that release's digest (older start for a real schema change)
#
# "Previous release" is the newest final release tag (vX.Y.Z, no pre-release
# suffix) that is reachable from HEAD, was published (not in UNPUBLISHED_TAGS)
# and does not point at HEAD itself: on a
# release tag it is the release before that one, on main the latest release.
# The digest is read from the registry (the image index's Docker-Content-Digest,
# anonymous pull token). When the example overlay at HEAD pins that same tag
# with a digest, the two must agree, so a moved or re-pushed tag fails here
# instead of testing the wrong image; the overlay must also name the image
# repository that release is published under.
#
# The image repository depends on the release: the GitHub repository moved from
# ncecere/open-mcp-portal to bitop-dev/open-mcp-portal after v0.15.2, so
# releases up to and including v0.15.2 are ghcr.io/ncecere/open-mcp-portal and
# later ones ghcr.io/bitop-dev/open-mcp-portal (docs/deploy.md, "Image").
#
# Needs git, curl and jq. Settings (environment, mostly for tests):
#   OMP_IMAGE_REPO         bitop-dev/open-mcp-portal  (releases after OMP_LEGACY_IMAGE_LAST)
#   OMP_LEGACY_IMAGE_REPO  ncecere/open-mcp-portal    (releases up to OMP_LEGACY_IMAGE_LAST)
#   OMP_LEGACY_IMAGE_LAST  v0.15.2                    (the last release published under the legacy repository)
#   OMP_REGISTRY_URL       https://ghcr.io  (the registry API base)
#   OMP_TOKEN_URL          the token endpoint (default <registry>/token)
set -euo pipefail

CURRENT_REPO="${OMP_IMAGE_REPO:-bitop-dev/open-mcp-portal}"
LEGACY_REPO="${OMP_LEGACY_IMAGE_REPO:-ncecere/open-mcp-portal}"
LEGACY_LAST="${OMP_LEGACY_IMAGE_LAST:-v0.15.2}"
REGISTRY_URL="${OMP_REGISTRY_URL:-https://ghcr.io}"
REGISTRY_HOST="${REGISTRY_URL#*://}"
TOKEN_URL="${OMP_TOKEN_URL:-${REGISTRY_URL}/token}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() { echo "previous-release: $*" >&2; exit 1; }

# Release tags that exist but were never published (no image, no release), so
# nobody can be running them and there is nothing to upgrade from. Tags are
# immutable, so a failed publication stays a tag; list it here. Override with
# OMP_UNPUBLISHED_TAGS (space separated) in tests.
#   v0.15.0: image scan failed on CVE-2026-78661; superseded by v0.15.1.
UNPUBLISHED_TAGS="${OMP_UNPUBLISHED_TAGS-v0.15.0}"

# previous_tag: reads tags on stdin, one per line, and the tags on HEAD in the
# environment variable HEAD_TAGS (space separated); prints the newest final
# release tag that is not one of those.
previous_tag() {
  grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V -r | while read -r t; do
    case " ${HEAD_TAGS:-} " in *" $t "*) continue ;; esac
    case " ${UNPUBLISHED_TAGS:-} " in *" $t "*) continue ;; esac
    echo "$t"
    break
  done
}

# image_repo TAG: the image repository (without the registry host) the release
# TAG was published under.
image_repo() {
  local tag=$1
  if [ "$(printf '%s\n%s\n' "$tag" "$LEGACY_LAST" | sort -V | head -n 1)" = "$tag" ]; then
    echo "$LEGACY_REPO"
  else
    echo "$CURRENT_REPO"
  fi
}

# registry_digest TAG: the digest of the tag's manifest (index) on the registry,
# read from the repository in $REPO.
registry_digest() {
  local tag=$1 token digest
  token="$(curl -fsS --retry 3 --retry-delay 2 --max-time 30 "${TOKEN_URL}?service=${REGISTRY_HOST}&scope=repository:${REPO}:pull" | jq -er .token)" ||
    die "could not get a pull token from ${TOKEN_URL}"
  digest="$(curl -fsSI --retry 3 --retry-delay 2 --max-time 30 -H "Authorization: Bearer ${token}" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
    "${REGISTRY_URL}/v2/${REPO}/manifests/${tag}" | tr -d '\r' | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *//p' | head -n 1)" ||
    die "could not read the manifest of ${REPO}:${tag}"
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "the registry returned no usable digest for ${REPO}:${tag} (got '${digest}')"
  echo "$digest"
}

# pinned_image TAG: "IMAGE DIGEST" for the images: entry of the example overlay
# at HEAD that pins TAG, if any. IMAGE is the entry's newName, or its name when
# it has none (the image the overlay renders); DIGEST may be empty.
pinned_image() {
  local tag=$1 f="${ROOT}/deploy/kubernetes/overlays/example/kustomization.yaml"
  [ -f "$f" ] || return 0
  awk -v tag="$tag" '
    function flush() { if (t == tag && !done) { print (nn != "" ? nn : n), d; done = 1 } }
    /^[^ #]/ { flush(); n = nn = t = d = "" }
    $1 == "-" && $2 == "name:" { flush(); n = $3; nn = t = d = "" }
    $1 == "newName:" { nn = $2 }
    $1 == "newTag:" { t = $2 }
    $1 == "digest:" { d = $2 }
    END { flush() }' "$f" | tr -d '"'"'"
}

tag=""
tag_only=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --tag-only) tag_only=1 ;;
    --tag)
      shift
      [ "$#" -gt 0 ] || die "--tag needs a value"
      tag="$1"
      ;;
    *) die "usage: $0 [--tag-only] [--tag vX.Y.Z]" ;;
  esac
  shift
done

if [ -z "$tag" ]; then
  HEAD_TAGS="$(git -C "$ROOT" tag --points-at HEAD | tr '\n' ' ')"
  export HEAD_TAGS
  tag="$(git -C "$ROOT" tag --merged HEAD --list 'v*' | previous_tag || true)"
  [ -n "$tag" ] || die "no release tag (vX.Y.Z) is reachable from HEAD; fetch tags (git fetch --tags, and a full clone in CI)"
fi
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "'$tag' is not a release tag vX.Y.Z"
if [ "$tag_only" = 1 ]; then
  echo "$tag"
  exit 0
fi

REPO="$(image_repo "$tag")"
digest="$(registry_digest "$tag")"
read -r pinned_name pinned <<<"$(pinned_image "$tag")" || true
# The repository path is compared, not the host: the overlay names ghcr.io, and
# the tests use a stub registry on another host.
if [ -n "${pinned_name:-}" ] && [ "${pinned_name#*/}" != "$REPO" ]; then
  die "deploy/kubernetes/overlays/example/kustomization.yaml pins ${tag} as ${pinned_name}, but ${tag} is published as ${REPO}: set the entry's newName"
fi
if [ -n "${pinned:-}" ] && [ "$pinned" != "$digest" ]; then
  die "${REPO}:${tag} is ${digest} on the registry but ${pinned} in deploy/kubernetes/overlays/example/kustomization.yaml: the tag moved, or the pin is wrong"
fi
echo "${tag} ${REGISTRY_HOST}/${REPO}@${digest}"
