#!/usr/bin/env bash
set -euo pipefail

ARG=${1:-"none"}
PR_TITLE=${PR_TITLE:-$ARG}
BREAKING_PREFIX="BREAKING CHANGE:"
REMOTE=$(git remote -v | grep 'aws/karpenter' | head -n1 | awk '{print $1}')

if [[ "${PR_TITLE}" == "${BREAKING_PREFIX}"* ]] || git --no-pager log --oneline ${REMOTE}/main..HEAD | grep "${BREAKING_PREFIX}"; then
    git --no-pager diff ${REMOTE}/main HEAD --name-only | grep "preview/upgrade-guide" || (echo "❌ No upgrade instructions found on a breaking change"; exit 1);
else
    echo "✅ PR is not a breaking change"
    exit 0
fi

echo "✅ PR is breaking but includes upgrade instructions"
