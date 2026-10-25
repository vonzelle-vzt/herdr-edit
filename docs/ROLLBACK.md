# Rolling back herdr-edit

herdr-edit is a single static Go binary distributed through GitHub Releases and
a Homebrew formula that lives in this repo (`Formula/herdr-edit.rb`). There is
no hosted service, no Vercel deployment, no mobile app and no database, so
there are no migrations to revert. "Rolling back" means getting users back onto
an earlier release.

## How a release happens (so you know what to undo)

Every push to `main` runs `.github/workflows/release.yml`: it reads
`internal/version/version.go`, tags `v<x.y.z>`, GoReleaser publishes the
archives to a GitHub Release, and writes `Formula/herdr-edit.rb` back to
`main`. Users get the new version from the Release archives, `install.sh`, or
`brew upgrade`.

## 1. Stop the bleeding: mark the bad release

1. On the GitHub Releases page, edit the bad release and tick **Set as a
   pre-release** (or delete it). `install.sh` and "latest" links then stop
   resolving to it.
2. Leave the git tag in place unless the release is dangerous; deleting a
   published tag breaks anyone who pinned it.

## 2. Point Homebrew back at the previous version

The formula is rewritten on every release, so revert that file to the last good
one:

```sh
git checkout main && git pull
git log --oneline -- Formula/herdr-edit.rb      # find the last good formula commit
git checkout <good-sha> -- Formula/herdr-edit.rb
git commit -m "Revert herdr-edit formula to v<good-version>"
git push origin main
```

Open the change as a pull request if branch protection requires it. Use the
`vonzelle-vzt/herdr-edit` remote only; never push to `cloudmanic/spice-edit`.

## 3. Ship the fix forward

Reverting the formula does not undo the code on `main`. Either:

- `git revert <bad-commit>` and merge it. The push to `main` auto-bumps the
  patch version and releases the revert as a new version (preferred, since
  version numbers only move forward), or
- fix the bug and merge the fix.

Do not hand-edit `internal/version/version.go` to a lower number: the release
workflow uses it to tag, and an already-existing tag will make the job fail.

## 4. Individual users

```sh
brew uninstall herdr-edit
brew install vonzelle-vzt/herdr-edit/herdr-edit   # after the formula revert
```

Or download the archive for a specific tag from the Releases page and replace
the binary on `PATH`. A source build goes stale silently, so rebuild
(`make install`) after switching versions.

## Database migrations

None. The editor keeps only local files (`~/.config/spiceedit/config.json`,
persisted undo history, breakpoints), which older versions read tolerantly.
