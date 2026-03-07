#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <series-file> <image-ref>" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required" >&2
  exit 1
fi

if ! docker buildx version >/dev/null 2>&1; then
  echo "docker buildx is required" >&2
  exit 1
fi

series_file=$1
image_ref=$2
series=$(tr -d '[:space:]' <"${series_file}")

if [[ -z "${series}" ]]; then
  echo "version series file is empty: ${series_file}" >&2
  exit 1
fi

floating_tags=()

if [[ "${series}" =~ ^([0-9]+)\.([0-9]+)\.x$ ]]; then
  major=${BASH_REMATCH[1]}
  minor=${BASH_REMATCH[2]}
  version_prefix="v${major}.${minor}."
  floating_tags=("v${major}.${minor}")
  if (( major > 0 )); then
    floating_tags+=("v${major}")
  fi
elif [[ "${series}" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)-([0-9A-Za-z.-]+)\.x$ ]]; then
  major=${BASH_REMATCH[1]}
  minor=${BASH_REMATCH[2]}
  patch=${BASH_REMATCH[3]}
  prerelease=${BASH_REMATCH[4]}
  version_prefix="v${major}.${minor}.${patch}-${prerelease}."
  floating_tags=("v${major}.${minor}.${patch}.${prerelease}")
else
  echo "unsupported version series: ${series}" >&2
  echo "expected MAJOR.MINOR.x or MAJOR.MINOR.PATCH-PRERELEASE.x" >&2
  exit 1
fi

next_index=0
while docker buildx imagetools inspect "${image_ref}:${version_prefix}${next_index}" >/dev/null 2>&1; do
  next_index=$((next_index + 1))
done

version_tag="${version_prefix}${next_index}"
version="${version_tag#v}"
image_tags=("${image_ref}:${version_tag}")

for floating_tag in "${floating_tags[@]}"; do
  image_tags+=("${image_ref}:${floating_tag}")
done

printf 'version=%s\n' "${version}"
printf 'version_tag=%s\n' "${version_tag}"
printf 'floating_tags=%s\n' "$(IFS=,; echo "${floating_tags[*]}")"
printf 'image_tags<<__IMAGE_TAGS__\n'
printf '%s\n' "${image_tags[@]}"
printf '__IMAGE_TAGS__\n'
