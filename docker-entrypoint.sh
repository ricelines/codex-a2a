#!/bin/sh
set -eu

target_user="${TARGET_USER:-codex}"
target_group="${TARGET_GROUP:-codex}"
target_home="${TARGET_HOME:-/home/codex}"
source_codex_home="${CODEX_SOURCE_HOME:-${target_home}/.codex}"
runtime_codex_home="${CODEX_RUNTIME_HOME:-${target_home}/.codex-runtime}"

copy_if_present() {
    src="$1"
    dst="$2"

    if [ ! -f "${src}" ]; then
        return
    fi

    cp "${src}" "${dst}"
    chmod 600 "${dst}"
}

mkdir -p "${runtime_codex_home}"
chmod 700 "${runtime_codex_home}"

rm -f \
    "${runtime_codex_home}/auth.json" \
    "${runtime_codex_home}/config.toml" \
    "${runtime_codex_home}/managed_config.toml" \
    "${runtime_codex_home}/.credentials.json"

copy_if_present "${source_codex_home}/auth.json" "${runtime_codex_home}/auth.json"
copy_if_present "${source_codex_home}/config.toml" "${runtime_codex_home}/config.toml"
copy_if_present "${source_codex_home}/managed_config.toml" "${runtime_codex_home}/managed_config.toml"
copy_if_present "${source_codex_home}/.credentials.json" "${runtime_codex_home}/.credentials.json"

chown -R "${target_user}:${target_group}" "${runtime_codex_home}"

export HOME="${target_home}"
export CODEX_HOME="${runtime_codex_home}"

if [ "$#" -eq 0 ]; then
    set -- codex-a2a
elif [ "${1#-}" != "${1}" ]; then
    set -- codex-a2a "$@"
fi

if command -v su-exec >/dev/null 2>&1; then
    exec su-exec "${target_user}:${target_group}" "$@"
fi

if command -v setpriv >/dev/null 2>&1; then
    exec setpriv --reuid="${target_user}" --regid="${target_group}" --init-groups -- "$@"
fi

echo "missing privilege drop helper (expected setpriv or su-exec)" >&2
exit 1
