#!/usr/bin/env bash
# Shared k3d image import and verification.
#
# hack/e2e.sh and hack/k3d-up.sh both import local images into a k3d cluster.
# `k3d image import` can exit 0 while a node's containerd still lacks an
# image, and the run then fails later with ErrImageNeverPull. These helpers
# import, verify every requested image on every server and agent node, retry
# the images that are still missing, and fail with the missing image and node.
#
# This file only defines functions, so a test can source it without k3d,
# docker, or a cluster.

# Print the container names of the k3d server and agent nodes for a cluster.
#
# k3d sets these labels on every node container. The server load balancer
# (serverlb) and registries have another role, so the filters exclude them.
k3d_cluster_nodes() {
  local cluster=$1 role
  for role in server agent; do
    docker ps \
      --filter "label=k3d.cluster=$cluster" \
      --filter "label=k3d.role=$role" \
      --format '{{.Names}}'
  done
}

# Report whether one image is present in crictl JSON.
#
#   k3d_image_present <repo:tag> <crictl-images-json>
#
# Exit 0 when present, 1 when absent, and 2 when the JSON cannot be read.
#
# containerd normalizes a short name to `docker.io/library/<name>:<tag>`.
# Remove one leading `docker.io/` and one leading `library/` from both sides
# before the comparison, so `mayflyd:e2e` matches
# `docker.io/library/mayflyd:e2e`. Digests are ignored; the requested tag is
# the identity that matters.
k3d_image_present() {
  local expected=$1 json=${2-} out rc=0
  out="$(jq -e --arg want "$expected" '
      def canon:
        sub("^docker\\.io/"; "") | sub("^library/"; "");
      ($want | canon) as $target
      | any((.images // [])[]?;
            any((.repoTags // [])[]?; canon == $target))
    ' <<<"$json" 2>&1 >/dev/null)" || rc=$?
  if ((rc == 0)); then
    return 0
  fi
  if ((rc == 1)); then
    return 1
  fi
  printf 'k3d images: cannot read crictl JSON for %s: %s\n' "$expected" "$out" >&2
  return 2
}

# Print each node container name and its crictl image table. Best effort: a
# missing node or a failed exec is shown and does not abort the report.
k3d_node_image_report() {
  local cluster=$1 node
  printf 'k3d cluster %s image inventory:\n' "$cluster"
  while IFS= read -r node; do
    [[ -n "$node" ]] || continue
    printf 'node %s:\n' "$node"
    docker exec "$node" crictl images 2>&1 | sed 's/^/  /' || true
  done < <(k3d_cluster_nodes "$cluster")
}

# Import images into a k3d cluster and verify every node.
#
#   k3d_import_images <cluster> <repo:tag>...
#
# Attempt 1 imports every image. A later attempt imports only the images that
# are still missing on at least one node. There are three attempts in total,
# with 1s and 2s backoff. A successful import exit is not sufficient; the
# crictl inventory on every node decides. On the last failure, print
# `image X missing on node Y` for every missing pair and the per-node image
# table, then return nonzero.
k3d_import_images() {
  local cluster=$1
  shift
  local -a pending=("$@")
  local -a nodes=() missing=()
  local attempt backoff image node json rc tool
  local missing_seen missing_pairs

  if ((${#pending[@]} == 0)); then
    printf 'k3d images: no images requested\n' >&2
    return 1
  fi

  for tool in k3d docker jq; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      printf 'k3d images: %s is required\n' "$tool" >&2
      return 1
    fi
  done

  for ((attempt = 1; attempt <= 3; attempt++)); do
    printf 'k3d images: importing %d image(s) into cluster %s (attempt %d/3)\n' \
      "${#pending[@]}" "$cluster" "$attempt" >&2
    # `direct` streams each image into every node's containerd from this host.
    # It does not start the temporary tools node that `tools` mode needs, so
    # the import path does not depend on a k3d default or on a tools image.
    if ! k3d image import --cluster "$cluster" --mode direct "${pending[@]}"; then
      printf 'k3d images: k3d image import reported an error; verifying every node anyway\n' >&2
    fi

    nodes=()
    while IFS= read -r node; do
      [[ -n "$node" ]] || continue
      nodes+=("$node")
    done < <(k3d_cluster_nodes "$cluster")
    if ((${#nodes[@]} == 0)); then
      printf 'k3d images: no server or agent node found for cluster %s\n' "$cluster" >&2
      return 1
    fi

    missing=()
    missing_seen=" "
    missing_pairs=""
    for node in "${nodes[@]}"; do
      if ! json="$(docker exec "$node" crictl images -o json 2>&1)"; then
        printf 'k3d images: cannot list images on node %s: %s\n' "$node" "$json" >&2
        return 1
      fi
      for image in "${pending[@]}"; do
        rc=0
        k3d_image_present "$image" "$json" || rc=$?
        if ((rc == 2)); then
          return 1
        fi
        if ((rc == 0)); then
          continue
        fi
        missing_pairs+="$image|$node"$'\n'
        case "$missing_seen" in
          *" $image "*)
            ;;
          *)
            missing+=("$image")
            missing_seen+="$image "
            ;;
        esac
      done
    done

    if ((${#missing[@]} == 0)); then
      printf 'k3d images: all requested images present on %d node(s)\n' \
        "${#nodes[@]}" >&2
      return 0
    fi
    if ((attempt == 3)); then
      break
    fi
    printf 'k3d images: %d image(s) missing after attempt %d/3; retrying the missing images\n' \
      "${#missing[@]}" "$attempt" >&2
    pending=("${missing[@]}")
    backoff=$((1 << (attempt - 1)))
    printf 'k3d images: waiting %ds before the retry\n' "$backoff" >&2
    sleep "$backoff"
  done

  while IFS= read -r pair; do
    [[ -n "$pair" ]] || continue
    printf 'image %s missing on node %s\n' "${pair%%|*}" "${pair#*|}" >&2
  done <<<"$missing_pairs"
  k3d_node_image_report "$cluster" >&2 || true
  printf 'k3d images: import verification failed after 3 attempt(s)\n' >&2
  return 1
}
