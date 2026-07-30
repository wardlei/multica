#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Apply the tracked Review Loop configuration to an existing Multica workspace.

Usage:
  config/review-loop/apply.sh --server-url <url> --workspace-id <uuid>

The caller must already be authenticated with the Multica CLI. The workspace
must contain agents named "Reviewer" and a squad named "Review Loop".
EOF
}

server_url=""
workspace_id=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --server-url)
      server_url="${2:-}"
      shift 2
      ;;
    --workspace-id)
      workspace_id="${2:-}"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$server_url" || -z "$workspace_id" ]]; then
  usage >&2
  exit 2
fi

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cli=(multica --server-url "$server_url" --workspace-id "$workspace_id")

find_id_by_name() {
  local resource="$1"
  local name="$2"
  "${cli[@]}" "$resource" list --output json | python3 -c '
import json
import sys

name = sys.argv[1]
resource = sys.argv[2]
matches = [item for item in json.load(sys.stdin) if item.get("name") == name]
if len(matches) != 1:
    raise SystemExit(f"expected exactly one {resource} named {name!r}, found {len(matches)}")
print(matches[0]["id"])
' "$name" "$resource"
}

skill_id="$("${cli[@]}" skill list --output json | python3 -c '
import json
import sys

matches = [item for item in json.load(sys.stdin) if item.get("name") == "open-code-review"]
if len(matches) > 1:
    raise SystemExit("found multiple skills named open-code-review")
print(matches[0]["id"] if matches else "")
')"

skill_description="Use OCR-backed, evidence-based reviews for implementation changes and diffs."
if [[ -z "$skill_id" ]]; then
  skill_id="$("${cli[@]}" skill create \
    --name open-code-review \
    --description "$skill_description" \
    --content-file "$root_dir/open-code-review/SKILL.md" \
    --output json | python3 -c 'import json, sys; print(json.load(sys.stdin)["id"])')"
else
  "${cli[@]}" skill update "$skill_id" \
    --description "$skill_description" \
    --content-file "$root_dir/open-code-review/SKILL.md" \
    --output json >/dev/null
fi

reviewer_id="$(find_id_by_name agent Reviewer)"
squad_id="$(find_id_by_name squad 'Review Loop')"

"${cli[@]}" agent skills add "$reviewer_id" --skill-ids "$skill_id" --output json >/dev/null
"${cli[@]}" agent update "$reviewer_id" \
  --description 'Performs evidence-based diff reviews and drives the review-modify-review quality gate.' \
  --instructions "$(<"$root_dir/reviewer-instructions.md")" \
  --output json >/dev/null
"${cli[@]}" squad update "$squad_id" \
  --description 'Planner-coordinated, evidence-based review-modify-review loop for scoped code, design, and documentation work.' \
  --instructions "$(<"$root_dir/squad-instructions.md")" \
  --output json >/dev/null

echo "Review Loop configuration applied to workspace $workspace_id."
