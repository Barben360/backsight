# shellcheck shell=bash
# The model cache of a CI stack, sourced by scripts/fresh-install.sh and
# scripts/sandbox.sh. Hindsight downloads its reranker into the project's
# hf_cache volume at its first start (the image ships other models only),
# so that start depends on huggingface.co. BACKSIGHT_HF_CACHE names a tar
# of that volume's content, which CI keeps with actions/cache keyed on the
# pinned image: hfc_seed fills a new volume from it before the start, and
# hfc_save writes it once the server is up. BACKSIGHT_HF_CACHE_DROP=1 (CI,
# whose restored tar is never saved again) deletes the tar once read, to
# free the disk. Unset, both do nothing. A
# failure here is a warning, never the run's: a missing or broken cache
# only means the server downloads its models.
#
# Both run tar in the pinned Hindsight image (pulled for the stack anyway),
# as root so the files keep their owner, with no network. The caller sets
# root (the repository).

# hfc_image: the image the source pins (internal/server/render.go).
hfc_image() { sed -n 's/^const pinnedHindsight = "\(.*\)"$/\1/p' "$root/internal/server/render.go"; }

# hfc_seed <compose project>: before the stack's first start, the volume
# Compose will use (created with Compose's labels, as Compose would),
# filled from the tar when there is one. hfc_read records that this run
# read the tar: hfc_save then leaves it alone, even once dropped.
hfc_read=
hfc_seed() {
  local tar=${BACKSIGHT_HF_CACHE:-} vol=$1_hf_cache image
  [ -n "$tar" ] || return 0
  if [ ! -s "$tar" ]; then
    echo "hf cache: no $tar yet, the server downloads its models" >&2
    return 0
  fi
  if docker volume inspect "$vol" >/dev/null 2>&1; then
    echo "hf cache: $vol already exists, left as it is" >&2
    return 0
  fi
  image=$(hfc_image)
  hfc_read=1
  if docker volume create --label "com.docker.compose.project=$1" --label com.docker.compose.volume=hf_cache "$vol" >/dev/null &&
    docker run --rm -i --network none --user 0 --entrypoint tar -v "$vol:/cache" "$image" -xpf - -C /cache < "$tar"; then
    echo "hf cache: $vol seeded from $tar" >&2
  else
    echo "::warning::hf cache: $vol could not be seeded from $tar; the server downloads its models" >&2
    docker volume rm -f "$vol" >/dev/null 2>&1 || true
  fi
  [ "${BACKSIGHT_HF_CACHE_DROP:-}" != 1 ] || rm -f "$tar"
  return 0
}

# hfc_save <compose project>: once the server is up, the volume's content
# into the tar, unless the tar is already there or this run read it
# (restored by CI). A tar
# without Hugging Face models is not kept: it would seed an empty cache.
hfc_save() {
  local tar=${BACKSIGHT_HF_CACHE:-} vol=$1_hf_cache image
  [ -n "$tar" ] && [ ! -s "$tar" ] && [ -z "$hfc_read" ] || return 0
  if ! docker volume inspect "$vol" >/dev/null 2>&1; then
    echo "::warning::hf cache: no volume $vol to save" >&2
    return 0
  fi
  image=$(hfc_image)
  if mkdir -p "$(dirname "$tar")" &&
    docker run --rm --network none --user 0 --entrypoint tar -v "$vol:/cache:ro" "$image" -cf - -C /cache . > "$tar.part" &&
    tar -tf "$tar.part" | grep '^\./huggingface/hub/models--' >/dev/null &&
    mv "$tar.part" "$tar"; then
    echo "hf cache: $vol saved to $tar" >&2
  else
    echo "::warning::hf cache: $vol could not be saved to $tar" >&2
    rm -f "$tar.part"
  fi
  return 0
}
