# Publishing a release

GrayMatter publishes two Go modules from the same commit: the library tag
`vX.Y.Z` and the CLI tag `cmd/graymatter/vX.Y.Z`. The CLI's library requirement
must name that same version. Published tags are immutable: never delete,
move, or recreate one to repair a release.

## Prepare and validate

1. Move the completed changelog entries into a dated version section. Preserve
   compatibility notices and leave future default changes conditional.
2. Update the library requirement in `cmd/graymatter/go.mod`, README downloads
   and footer, `server.json`, and the website's download command. Keep the
   published module free of `replace` directives.
3. Run `python .github/scripts/release_check.py --tag vX.Y.Z` and
   `python -m unittest discover -s .github/scripts -p 'test_*.py'`.
4. To check a candidate before opening the PR, bring its branch up to date with
   `main` and push it. In GitHub Actions, run **CI** and **Maintenance smoke**
   on that branch. Verify that both runs tested the candidate commit and passed.
   If the candidate or base changes, validate the new combination.
5. Open the release preparation PR. CI and Maintenance smoke must pass,
   including the five GoReleaser snapshot targets and the Homebrew, Scoop and
   Nix manifests. Complete any real-client gates required by the features
   being released.

Before the new library tag exists, commands that load the workspace graph may
try to fetch it even though `go.work` selects local source for compilation.
Every CI job that loads that graph therefore applies this temporary replacement
in its disposable checkout before its first Go build, test, or analysis:

```sh
cd cmd/graymatter
GOWORK=off go mod edit -replace github.com/angelnicolasc/graymatter=../..
```

Do not commit that replacement. For local release checks, use a disposable
checkout too. Release builds use the same patched Go toolchain as the current
CI matrix and the CLI's `toolchain` directive.

## Publish and verify

Merge the validated preparation and create signed annotated tags for both
modules at that exact commit. Push the main update and both tags atomically:

```sh
git tag -s vX.Y.Z -m "GrayMatter vX.Y.Z" HEAD
git tag -s cmd/graymatter/vX.Y.Z -m "GrayMatter CLI vX.Y.Z" HEAD
git push --atomic origin HEAD:refs/heads/main refs/tags/vX.Y.Z refs/tags/cmd/graymatter/vX.Y.Z
```

Verify both tags locally with `git verify-tag`, and confirm the published
commit and annotated tag signatures on GitHub. Commits and tags for this
release must identify `angelnicolasc`; use the registered signing key and the
account's verified email address.

The release workflow checks metadata, matching tag commits and GoReleaser
configuration before publishing. The CLI tag must already exist at the same
commit; a missing tag or mismatch fails without creating or changing a tag.

The build's temporary replacement avoids depending on a newly pushed tag's
availability in the public Go proxy. The subsequent install check warms both
modules and the checksum database, then runs the normal `go install` command
and verifies the CLI and embedded library versions without replacements.

Confirm all five archives and `checksums.txt` exist and both release jobs pass.
Retry a failed workflow against its existing immutable tags when appropriate.
If the published source needs a correction, prepare a new patch version instead.

## Publish the package-manager manifests

GoReleaser generates all three manifests with `skip_upload: true`. The release
workflow installs `nix-hash`, requires the generated files and parses the Scoop
and Nix manifests. It preserves the exact outputs, checksums and build metadata
in the `package-manager-manifests-vX.Y.Z` workflow artifact. Package-manager
publication uses signed Git commits from a maintainer's checkout; CI does not
receive a maintainer's private signing key or credentials for the three taps.

1. Download that artifact from the successful release run. Confirm its run
   identifies the signed release commit, and verify the published archive
   checksums. Check each manifest's version, release URLs and archive hashes
   against these outputs before publication.
2. Clone or update each destination repository in a clean checkout. Copy the
   generated files without regenerating or editing them:

   | Artifact path | Destination repository | Destination path |
   | --- | --- | --- |
   | `homebrew/Formula/graymatter.rb` | `angelnicolasc/homebrew-tap` | `Formula/graymatter.rb` |
   | `scoop/graymatter.json` | `angelnicolasc/scoop-bucket` | `graymatter.json` |
   | `nix/pkgs/graymatter/default.nix` | `angelnicolasc/nur-packages` | `pkgs/graymatter/default.nix` |

3. Review and commit only the intended files in each checkout. Author and
   committer must both be `angelnicolasc` with email
   `108889887+angelnicolasc@users.noreply.github.com`. Sign with the account's
   registered key, run `git verify-commit HEAD`, and push normally without
   rewriting existing history. Confirm GitHub reports a valid signature and
   the expected author and committer for each new commit.
4. Verify each channel's version, release URLs, archive hashes and package
   entry point. Check that `nur-packages` exposes `graymatter` from its root
   `default.nix`. Run channel installation and `graymatter --version` in a
   disposable supported environment when available, using `nix-env -f . -iA
   graymatter` or `nix-build -A graymatter` for Nix.
   A generated derivation alone does not register a repository with NUR or
   provide a flake entry point.

Preserve the generated manifests for a retry if a channel push fails. Complete
all three channel checks before declaring distribution finished. A later
Dockerfile update must reference the new archives only after they are published.
