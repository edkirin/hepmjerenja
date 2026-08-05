#!/usr/bin/env bash
set -euo pipefail

# Check current branch is develop
BRANCH=$(git rev-parse --abbrev-ref HEAD)
if [ "$BRANCH" != "develop" ]; then
    echo "Error: must be on develop branch (currently on '$BRANCH')" >&2
    exit 1
fi

# Check for uncommitted changes
if ! git diff --quiet || ! git diff --cached --quiet; then
    echo "Error: there are uncommitted changes, commit or stash them first" >&2
    exit 1
fi

git checkout main
git merge develop -m "Merge develop into main for release"

VERSION_TAG=$(date +%y%m%d.%H%M)
make docker-build VERSION_TAG="$VERSION_TAG"
make docker-push VERSION_TAG="$VERSION_TAG"

git push origin main

git checkout develop
git merge main -m "Merge main back into develop after release"
git push origin develop
