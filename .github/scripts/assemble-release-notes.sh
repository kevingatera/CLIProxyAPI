#!/usr/bin/env bash
# Usage: assemble-release-notes.sh <release-notes> <changelog-entries> <generated-notes> <compare-link> <output>
#
# Writes the release body to <output>. GitHub rejects release bodies longer than
# 125000 characters, so when the assembled body is too large the oldest changelog
# entries are dropped and the most recent ones are kept in their original
# (oldest first) order. Whatever room remains goes to the start of the generated
# notes, which list the newest changes first, and the body always ends with the
# full changelog link. Only whole lines are cut, so multi-byte characters stay
# intact and the body remains valid UTF-8.
set -euo pipefail

release_notes_file="$1"
changelog_entries_file="$2"
generated_notes_file="$3"
compare_link_file="$4"
output_file="$5"

# Stay below GitHub's 125000 character limit. Byte counts are never smaller
# than character counts, so a byte budget is a safe upper bound.
max_body_bytes="${MAX_RELEASE_BODY_BYTES:-120000}"
truncation_notice='_Changelog trimmed to fit the GitHub release body limit. See the full changelog below._'

byte_count() {
  LC_ALL=C wc -c < "$1" | tr -d '[:space:]'
}

# Print the first whole lines of <file> that fit in <bytes>.
head_lines_within_bytes() {
  LC_ALL=C awk -v max="$2" '
    BEGIN { used = 0 }
    {
      size = length($0) + 1
      if (used + size > max) exit
      used += size
      print
    }
  ' "$1"
}

# Print the last whole lines of <file> that fit in <bytes>, in their original order.
tail_lines_within_bytes() {
  LC_ALL=C awk -v max="$2" '
    { line[NR] = $0 }
    END {
      used = 0
      start = NR + 1
      for (i = NR; i >= 1; i--) {
        size = length(line[i]) + 1
        if (used + size > max) break
        used += size
        start = i
      }
      for (i = start; i <= NR; i++) print line[i]
    }
  ' "$1"
}

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

header_file="$work_dir/header"
frame_file="$work_dir/frame"
entries_file="$work_dir/entries"
notes_file="$work_dir/notes"
suffix_file="$work_dir/suffix"

: > "$header_file"
if [[ -s "$changelog_entries_file" ]]; then
  printf '## Changelog\n\n' > "$header_file"
fi

# Changelog entries are oldest first; generated notes are newest first.
{
  cat "$release_notes_file"
  printf '\n'
  cat "$header_file"
  cat "$changelog_entries_file"
  if [[ -s "$changelog_entries_file" ]]; then
    printf '\n\n'
  fi
  cat "$generated_notes_file"
} > "$output_file"

if (( $(byte_count "$output_file") <= max_body_bytes )); then
  exit 0
fi

# Blank line before the compare link; kept content always ends with a newline.
printf '\n' > "$suffix_file"
cat "$compare_link_file" >> "$suffix_file"

# Measure everything except the kept changelog, then spend the rest on whole lines.
{
  cat "$release_notes_file"
  printf '\n'
  cat "$header_file"
  printf '%s\n\n' "$truncation_notice"
} > "$frame_file"

budget=$(( max_body_bytes - $(byte_count "$frame_file") - $(byte_count "$suffix_file") - 1 ))
if (( budget < 0 )); then
  # The frame alone does not fit. Drop the notice before touching the asset notes.
  {
    cat "$release_notes_file"
    printf '\n'
    cat "$header_file"
  } > "$frame_file"
  budget=$(( max_body_bytes - $(byte_count "$frame_file") - $(byte_count "$suffix_file") - 1 ))
fi

if (( budget < 0 )); then
  # Keep as many leading asset note lines as fit, but always leave room for the
  # compare link.
  notes_cap=$(( max_body_bytes - $(byte_count "$suffix_file") - 1 ))
  if (( notes_cap > 0 )); then
    head_lines_within_bytes "$release_notes_file" "$notes_cap" > "$frame_file"
    printf '\n' >> "$frame_file"
  else
    : > "$frame_file"
  fi
  budget=$(( max_body_bytes - $(byte_count "$frame_file") - $(byte_count "$suffix_file") - 1 ))
fi

if (( budget < 0 )); then
  # Not even the link fits next to the frame. Keep the link when it fits alone.
  if (( $(byte_count "$compare_link_file") <= max_body_bytes )); then
    cp "$compare_link_file" "$output_file"
  else
    : > "$output_file"
  fi
  printf '::warning::Release body exceeded %s bytes; only the full changelog link was kept.\n' "$max_body_bytes" >&2
  exit 0
fi

: > "$entries_file"
if (( budget > 0 )); then
  tail_lines_within_bytes "$changelog_entries_file" "$budget" > "$entries_file"
fi

remaining=$(( budget - $(byte_count "$entries_file") ))
: > "$notes_file"
if (( remaining > 0 )); then
  head_lines_within_bytes "$generated_notes_file" "$remaining" > "$notes_file"
fi

# Generated notes usually end with the compare link; only add it when the kept
# lines do not already carry it.
if [[ -s "$compare_link_file" ]] && grep -Fqx -f "$compare_link_file" <(cat "$entries_file" "$notes_file"); then
  : > "$suffix_file"
fi

{
  cat "$frame_file"
  cat "$entries_file"
  if [[ -s "$entries_file" && -s "$notes_file" ]]; then
    printf '\n'
  fi
  cat "$notes_file"
  cat "$suffix_file"
} > "$output_file"

printf '::warning::Release body exceeded %s bytes; changelog was truncated.\n' "$max_body_bytes" >&2
