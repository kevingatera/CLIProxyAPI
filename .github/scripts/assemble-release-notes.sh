#!/usr/bin/env bash
# Usage: assemble-release-notes.sh <release-notes> <changelog-entries> <generated-notes> <compare-link> <output>
#
# Writes the release body to <output>. GitHub rejects release bodies longer than
# 125000 characters, so when the assembled body is too large, changelog lines are
# kept in order until the budget is spent and the rest is replaced by a notice and
# the compare link. Only whole lines are cut, so multi-byte characters stay intact.
set -euo pipefail

release_notes_file="$1"
changelog_entries_file="$2"
generated_notes_file="$3"
compare_link_file="$4"
output_file="$5"

# Stay below GitHub's 125000 character limit. Byte counts are never smaller
# than character counts, so a byte budget is a safe upper bound.
max_body_bytes="${MAX_RELEASE_BODY_BYTES:-120000}"
truncation_notice='_Changelog truncated to fit the GitHub release body limit. See the full changelog link below._'

byte_count() {
  LC_ALL=C wc -c < "$1" | tr -d '[:space:]'
}

changelog_file="$(mktemp)"
frame_file="$(mktemp)"
trap 'rm -f "$changelog_file" "$frame_file"' EXIT

if [[ -s "$changelog_entries_file" ]]; then
  printf '## Changelog\n\n' > "$changelog_file"
  cat "$changelog_entries_file" >> "$changelog_file"
  printf '\n\n' >> "$changelog_file"
fi
cat "$generated_notes_file" >> "$changelog_file"

{
  cat "$release_notes_file"
  printf '\n'
  cat "$changelog_file"
} > "$output_file"

if (( $(byte_count "$output_file") <= max_body_bytes )); then
  exit 0
fi

# Measure everything except the kept changelog lines, then spend the rest on lines.
{
  cat "$release_notes_file"
  printf '\n'
  printf '%s\n\n' "$truncation_notice"
  cat "$compare_link_file"
} > "$frame_file"
budget=$(( max_body_bytes - $(byte_count "$frame_file") ))
if (( budget < 0 )); then
  budget=0
fi

{
  cat "$release_notes_file"
  printf '\n'
  LC_ALL=C awk -v max="$budget" '
    { total += length($0) + 1; if (total > max) exit; print }
  ' "$changelog_file"
  printf '\n%s\n\n' "$truncation_notice"
  cat "$compare_link_file"
} > "$output_file"

printf '::warning::Release body exceeded %s bytes; changelog was truncated.\n' "$max_body_bytes" >&2
