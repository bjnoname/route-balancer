#!/usr/bin/env bash
#
# module-options-agree — README.md and the NixOS module describe the same
# options.
#
# The book's options page is generated from nix/module/route-balancer.nix, so
# it cannot drift. The README's tables are written by hand and deliberately
# terse: they are the first thing a reader meets, and a generated page is not
# what that reader wants there. This check is what makes the hand-written copy
# safe — an option renamed, added or removed in the module, or a default
# changed under one of them, fails here rather than in a bug report.
#
# It compares the part that has one right answer: the set of option names, and
# the default of every option whose README cell is a plain literal. Prose is
# not compared. The terse description and the module's long one are both meant
# to exist, and only one of them is the reference.
#
# Usage: module-options-agree.sh <options.json> <README.md>

set -euo pipefail

json=${1:?usage: module-options-agree.sh <options.json> <README.md>}
readme=${2:?usage: module-options-agree.sh <options.json> <README.md>}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# A unit separator rather than a tab: an empty field has to survive `read`,
# and `read` swallows empty fields between whitespace separators.
US=$(printf '\037')

# Options the README documents by reference rather than one row each, and the
# reason each one is not a row of its own.
exempt_reason() {
  case $1 in
  'gateways.<name>.health.probe6.'*)
    echo 'the README documents probe6 as "same fields as probe"' ;;
  *) return 1 ;;
  esac
}

# --- what the module says -------------------------------------------------
# name US leaf|node US default. A node is an option with sub-options: the
# README may name one, but it needs no row of its own.
jq -j --arg us "$US" '
  . as $o
  | ($o | keys) as $ks
  | $ks[]
  | . as $k
  | [ ($k | sub("^services\\.route-balancer\\.?"; "") | gsub("\\.\\*\\."; "[].")),
      (if ([ $ks[] | select(startswith($k + ".")) ] | length) == 0 then "leaf" else "node" end),
      ($o[$k].default
        | if . == null then ""
          elif type == "object" and has("text") then .text
          else tojson end
        | gsub("\\s+"; " ") | ltrimstr(" ") | rtrimstr(" "))
    ]
  | join($us) + "\n"
' "$json" | grep -v "^$US" > "$work/module"

# --- what the README says -------------------------------------------------
# name US qualified-name US default, one per table row under "## NixOS Module
# Options". A "### Heading (`some.path`)" scopes the rows beneath it.
awk -v US="$US" '
  /^## / { insec = ($0 ~ /^## NixOS Module Options/); prefix = ""; next }
  !insec { next }
  /^### / {
    prefix = ""
    if (match($0, /`[^`]+`/)) prefix = substr($0, RSTART + 1, RLENGTH - 2) "."
    next
  }
  /^\|/ {
    if ($0 ~ /^\|[ :|-]+$/) next                    # the ---|--- separator row
    line = $0
    gsub(/\\\|/, "\a", line)                        # an escaped pipe is not a column
    n = split(line, col, "|")
    if (n < 4) next
    name = col[2]; def = col[4]
    gsub(/`/, "", name); gsub(/^[ \t]+|[ \t]+$/, "", name)
    gsub(/`/, "", def);  gsub(/^[ \t]+|[ \t]+$/, "", def)
    gsub(/\a/, "|", def)
    gsub(/[ \t]+/, " ", def)
    if (name == "" || name == "Option") next
    print name US prefix name US def
  }
' "$readme" > "$work/readme"

cut -d"$US" -f1 "$work/module" > "$work/names"

fail=0
say() { printf '  %s\n' "$*" >&2; fail=1; }

# `{ }` and `{}` are the same default written two ways, and only one of them
# is worth a build failure.
norm() { printf '%s' "$1" | sed 's/\[ /[/g; s/ \]/]/g; s/{ /{/g; s/ }/}/g'; }

# --- every option the README names exists, and agrees about its default ----
: > "$work/covered"
while IFS="$US" read -r name qualified def; do
  hit=
  for cand in "$name" "$qualified"; do
    if grep -qxF "$cand" "$work/names"; then hit=$cand; break; fi
  done
  if [ -z "$hit" ]; then
    say "the README documents \`$name\`, and the module has no such option"
    continue
  fi
  printf '%s\n' "$hit" >> "$work/covered"

  # A default is compared only where the README states a bare literal: a cell
  # like "null (256)" or "—" is prose about a default, not the default.
  case $def in
  '' | '—' | *'('* ) continue ;;
  esac
  want=$(awk -F"$US" -v n="$hit" '$1 == n { print $3; exit }' "$work/module")
  [ "$(norm "$def")" = "$(norm "$want")" ] ||
    say "\`$hit\` defaults to '$want' in the module and '$def' in the README"
done < "$work/readme"

# --- and every leaf option the module has is in the README -----------------
while IFS="$US" read -r name kind _; do
  [ "$kind" = leaf ] || continue
  grep -qxF "$name" "$work/covered" && continue
  exempt_reason "$name" >/dev/null && continue
  say "the module has \`$name\`, and the README's tables do not mention it"
done < "$work/module"

if [ "$fail" -ne 0 ]; then
  {
    echo
    echo "README.md and nix/module/route-balancer.nix disagree about the options."
    echo "the book's page is generated from the module, so the module is the one"
    echo "that is right by construction: fix the README to match it."
  } >&2
  exit 1
fi

echo "the README's option tables and the NixOS module agree"
