#!/usr/bin/env bash
# Exercise the release preflight without starting Docker or changing this repo.
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
trap 'rm -rf -- "$TEST_ROOT"' EXIT
cp "$ROOT_DIR/dockerrelease.sh" "$TEST_ROOT/dockerrelease.sh"
cd "$TEST_ROOT"
git init -q
git config user.email test@example.invalid
git config user.name Test
echo 0.0.1 > VERSION
mkdir backend
git add VERSION dockerrelease.sh
git commit -qm baseline
mkdir bin
printf '#!/usr/bin/env bash\necho DOCKER_WAS_CALLED >&2\nexit 97\n' > bin/docker
chmod +x bin/docker
export PATH="$TEST_ROOT/bin:$PATH"
echo 'package forgotten' > backend/forgotten.go
if bash dockerrelease.sh > result.log 2>&1; then
    echo 'FAIL: untracked source accepted'; exit 1
fi
grep -q 'Working tree is dirty' result.log
if grep -q DOCKER_WAS_CALLED result.log; then
    echo 'FAIL: untracked source reached Docker'; exit 1
fi
git add backend/forgotten.go
git commit -qm source
if bash dockerrelease.sh > result.log 2>&1; then
    echo 'FAIL: expected fake Docker to stop the run'; exit 1
fi
grep -q DOCKER_WAS_CALLED result.log
echo 'PASS: untracked source blocked; committed source reaches Docker'
