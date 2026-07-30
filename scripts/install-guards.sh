#!/usr/bin/env bash
# Install local guards that stop this fork pushing into the upstream repository.
#
# This is not paranoia. The release pipeline inherited from upstream had
# `brews.repository` pointing at cloudmanic/spice-edit, so the FIRST release cut from this fork
# tried to commit a Homebrew formula into the upstream author's repo. GitHub refused it with a 403 —
# that permission check is the only thing that stopped it, and a permission check is not a design.
#
# Three layers, because they fail differently:
#   1. `remote.upstream.pushurl = DISABLED`  — stops `git push upstream`. Cheap, but only covers
#      the remote by name, and a `git remote set-url` undoes it silently.
#   2. a pre-push hook                       — refuses any push whose URL mentions the upstream
#      repo, however it was spelled. Survives (1) being undone, and covers a script that builds the
#      URL itself. Independent of whether you happen to lack write access.
#   3. `remote.origin.gh-resolved = base`    — pins `gh`'s default repo to THIS fork.
#
# Layer 3 exists because layers 1 and 2 are both git hooks, and `gh pr create` performs no git
# operation at all — so neither can see it. In a repo GitHub knows is a fork, bare `gh pr create`
# defaults its BASE to the parent, opening the PR against cloudmanic/spice-edit. It fails with
# "No commits between cloudmanic:main and vonzelle-vzt:<branch>", which reads like a branch problem
# and is actually the wrong repository. Setting gh's default repo is the only thing that closes it.
#
# Hooks live in .git/ and are therefore NOT cloned. Re-run this after a fresh clone.
# Idempotent: safe to run repeatedly.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

UPSTREAM_MATCH="cloudmanic/spice-edit"

# --- layer 1 -----------------------------------------------------------------------------------
if git remote | grep -qx upstream; then
  git remote set-url --push upstream DISABLED
  echo "  upstream push url  -> DISABLED"
else
  echo "  no 'upstream' remote; skipping push-url guard"
fi

# --- layer 2 -----------------------------------------------------------------------------------
HOOK=".git/hooks/pre-push"
mkdir -p .git/hooks
cat > "$HOOK" <<EOF
#!/usr/bin/env bash
# Refuse to push anything into the upstream repository this project was forked from.
# Installed by scripts/install-guards.sh — see that file for why.
#
# git passes the remote name as \$1 and its URL as \$2.
set -uo pipefail
remote_url="\${2:-}"
case "\$remote_url" in
  *${UPSTREAM_MATCH}*)
    echo "pre-push: refusing to push to \$remote_url" >&2
    echo "  That is the UPSTREAM repository this project was forked from, not yours." >&2
    echo "  Contribute there by opening a pull request instead." >&2
    echo "  Override deliberately with --no-verify if you really mean it." >&2
    exit 1
    ;;
esac
exit 0
EOF
chmod +x "$HOOK"
echo "  pre-push hook      -> installed ($HOOK)"

# --- layer 3 -----------------------------------------------------------------------------------
# "base" is gh's spelling for "the repo this remote points at IS the default", which is what we
# want: origin is the fork. Written with git config rather than `gh repo set-default` so the guard
# installs identically on a machine with no gh, or with gh unauthenticated.
if git remote | grep -qx origin; then
  origin_url="$(git remote get-url origin 2>/dev/null || echo)"
  case "$origin_url" in
    *${UPSTREAM_MATCH}*)
      echo "  gh default repo    -> REFUSED: origin points at upstream ($origin_url)" >&2
      ;;
    *)
      git config --local remote.origin.gh-resolved base
      echo "  gh default repo    -> origin ($origin_url)"
      ;;
  esac
else
  echo "  no 'origin' remote; skipping gh default-repo guard"
fi

echo
echo "Verify with:"
echo "  git push --dry-run https://github.com/${UPSTREAM_MATCH} main   # must be refused"
echo "  gh repo set-default --view                                     # must NOT say ${UPSTREAM_MATCH}"
