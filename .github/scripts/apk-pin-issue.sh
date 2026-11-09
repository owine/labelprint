#!/usr/bin/env bash
# Keeps one issue, labelled apk-pin-satisfied, in step with docker-build's
# "Flag apk pins the base image already satisfies" check on main: open while
# any Dockerfile apk pin is something the base image already ships, updated
# when that list changes, closed once it's empty. The warnings alone land in
# Renovate base-bump PRs that automerge, where nobody reads them.
#
# Env: FINDINGS (one finding per line, may be empty), OK ("true" only if the
# check really ran), GH_TOKEN, GITHUB_REPOSITORY, GITHUB_SHA,
# GITHUB_SERVER_URL, GITHUB_RUN_ID.
set -euo pipefail

if [ "${OK:-}" != "true" ]; then
  # A check that didn't run isn't "no findings": leave any issue as it is.
  echo "the pin check didn't run; leaving the issue alone"
  exit 0
fi

repo=$GITHUB_REPOSITORY
label=apk-pin-satisfied
title="apk pins the base image already satisfies"
sha=$GITHUB_SHA
run_url="$GITHUB_SERVER_URL/$repo/actions/runs/$GITHUB_RUN_ID"
marker="<!-- apk-pin-findings -->"

gh label create "$label" --repo "$repo" --color FBCA04 \
  --description "A Dockerfile apk pin the base image already satisfies" >/dev/null 2>&1 || true

# Listing issues by label is eventually consistent: for a few seconds after a
# change it can miss a new issue or still show a closed one. So each listed
# issue's state is confirmed with a direct get, and if two runs ever race and
# open two issues (main's runs are minutes apart, so it's rare), the oldest is
# kept and the newer ones are closed as duplicates of it.
open=""
for n in $(gh api "repos/$repo/issues?labels=$label&state=all&per_page=100" \
  --jq '[.[] | select(.pull_request == null) | .number] | sort | .[]'); do
  if [ "$(gh api "repos/$repo/issues/$n" --jq .state)" = open ]; then
    open="$open$n"$'\n'
  fi
done
issue=$(printf '%s' "$open" | sed -n 1p)
printf '%s\n' "$open" | sed 1d | while read -r dup; do
  [ -n "$dup" ] || continue
  gh issue close "$dup" --repo "$repo" --reason "not planned" --comment "Duplicate of #$issue."
done

findings=$(printf '%s\n' "${FINDINGS:-}" | sed '/^[[:space:]]*$/d')

if [ -z "$findings" ]; then
  if [ -n "$issue" ]; then
    gh issue comment "$issue" --repo "$repo" --body "No longer applies as of $sha ($run_url)."
    gh issue close "$issue" --repo "$repo" --reason completed
    echo "closed #$issue: no pin is satisfied by the base any more"
  else
    echo "no findings and no open issue"
  fi
  exit 0
fi

list=$(printf '%s\n' "$findings" | sed 's/^/- /')

# The findings sit between two markers, so a later run compares only them:
# the commit and run link change on every push and mustn't count as a change.
body=$(printf '%s\n' \
  "The CI pin check on \`main\` found Dockerfile apk pins that the base image already satisfies:" \
  "" \
  "$marker" \
  "$list" \
  "$marker" \
  "" \
  "**What to do:** drop each pin, and its comment, from the runtime \`apk add\` in the \`Dockerfile\` (CLAUDE.md," \
  "Dependency management, the paragraph on pinning a package that's already in the base image). The Renovate PR that" \
  "bumps the base is the natural place." \
  "" \
  "Found at $sha in $run_url." \
  "" \
  "This issue is kept in step by the \`apk-pin-issue\` CI job: updated when the findings change, closed when none are" \
  "left.")

if [ -z "$issue" ]; then
  gh issue create --repo "$repo" --title "$title" --label "$label" --body "$body"
  exit 0
fi

current=$(gh issue view "$issue" --repo "$repo" --json body --jq .body |
  awk -v m="$marker" '$0 == m { n++; next } n == 1')
if [ "$current" = "$list" ]; then
  echo "#$issue already lists these findings"
else
  gh issue edit "$issue" --repo "$repo" --body "$body"
  echo "updated #$issue"
fi
