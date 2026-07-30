#!/usr/bin/env bash
# Install local guards that stop this fork pushing into the upstream repository.
#
# This is not paranoia. The release pipeline inherited from upstream had
# `brews.repository` pointing at cloudmanic/spice-edit, so the FIRST release cut from this fork
# tried to commit a Homebrew formula into the upstream author's repo. GitHub refused it with a 403 —
# that permission check is the only thing that stopped it, and a permission check is not a design.
#
# Two layers, because they fail differently:
#   1. `remote.upstream.pushurl = DISABLED`  — stops `git push upstream`. Cheap, but only covers
#      the remote by name, and a `git remote set-url` undoes it silently.
#   2. a pre-push hook                       — refuses any push whose URL mentions the upstream
#      repo, however it was spelled. Survives (1) being undone, and covers a script that builds the
#      URL itself. Independent of whether you happen to lack write access.
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
echo
echo "Verify with:  git push --dry-run https://github.com/${UPSTREAM_MATCH} main"
