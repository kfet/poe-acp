#!/usr/bin/env bash
# regen-poe-schema.sh — dump JSON Schemas for Poe's parameter_controls
# and SettingsResponse from a pinned fastapi_poe release. Output goes
# to internal/poeproto/testdata/ and is committed; tests validate our
# emitted JSON against these files.
#
# Bump FASTAPI_POE_VERSION to track upstream. Do not hand-edit the
# generated JSON.

set -euo pipefail

FASTAPI_POE_VERSION="${FASTAPI_POE_VERSION:-0.0.70}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="$REPO_ROOT/internal/poeproto/testdata"
VENV="$(mktemp -d)/venv"

cleanup() { rm -rf "$(dirname "$VENV")"; }
trap cleanup EXIT

python3 -m venv "$VENV"
# shellcheck source=/dev/null
. "$VENV/bin/activate"
pip install --quiet --disable-pip-version-check "fastapi-poe==${FASTAPI_POE_VERSION}"

mkdir -p "$OUT_DIR"

dump_one() {
    local cls="$1" out="$2"
    python3 - "$cls" "$out" "$FASTAPI_POE_VERSION" <<'PY'
import json, sys, importlib
cls_name, out_path, version = sys.argv[1], sys.argv[2], sys.argv[3]
mod = importlib.import_module("fastapi_poe.types")
cls = getattr(mod, cls_name)
schema = cls.model_json_schema()
schema["$comment"] = (
    f"Generated from fastapi-poe=={version} {cls_name}. "
    "Do not edit by hand. Regenerate with scripts/regen-poe-schema.sh."
)
with open(out_path, "w") as f:
    json.dump(schema, f, indent=2, sort_keys=True)
    f.write("\n")
print(f"wrote {out_path}")
PY
}

dump_one ParameterControls "$OUT_DIR/parameter_controls.schema.json"
dump_one SettingsResponse  "$OUT_DIR/settings_response.schema.json"

echo "fastapi-poe ${FASTAPI_POE_VERSION}" > "$OUT_DIR/SOURCE.txt"
echo "Done. Review the diff before committing."
