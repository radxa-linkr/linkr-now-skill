# Building from source

End users can use the prebuilt platform packages without a language runtime. Building requires Go 1.27.0 or later. Dependencies are pinned in `go.mod` and `go.sum`.

Run from the repository root:

```sh
go test -race ./...
go vet ./...
go run ./tools/package.go
```

The packager reads the release version from `VERSION` and builds Windows, macOS, and Linux executables for amd64 and arm64 with `CGO_ENABLED=0`. An explicit `--version` must match `VERSION`. Executables are written to `bin/<platform>-<architecture>/`. The `dist/` directory contains six platform skill ZIPs, six standalone executables for automatic setup, `LICENSE`, `THIRD_PARTY_NOTICES.txt`, and `SHA256SUMS` covering all fourteen files.

The public archive contains `SKILL.md`, `agents/openai.yaml`, `references/usage.md`, `LICENSE`, `THIRD_PARTY_NOTICES.txt`, `VERSION`, both launchers, and the matching executable. Build instructions, source files, tests, and repository metadata are excluded. Builds use `-trimpath` and disable VCS stamping to avoid embedding checkout paths or revision metadata.

Tests use loopback HTTP fixtures and synthetic DNS-SD service events. Device interoperability and native execution on each supported platform should be checked separately before a release.

## Publish a release

`bin/` and `dist/` are generated files and remain ignored by Git. Users obtain prebuilt packages from [GitHub Releases](https://github.com/radxa-linkr/linkr-now-skill/releases), independently of the source checkout.

Update `VERSION`, commit the reviewed source and workflow, and push that commit. Then create and push its matching version tag, for example:

```sh
git tag -a v0.1.0 -m 'Release v0.1.0'
git push origin v0.1.0
```

The [release workflow](.github/workflows/release.yml) checks that the tag matches `VERSION`, runs tests and vet, builds all six platforms, validates archive contents and checksums, and publishes all fifteen assets. Tags with a prerelease suffix, such as `v0.2.0-beta.1`, are marked as prereleases. The workflow uses the repository's built-in GitHub token with `contents: write`; no separate release token is required.

The source installation's launchers download the standalone executable for their pinned `VERSION`. Publish that release before recommending the corresponding source revision to users. A platform ZIP includes the executable and works without a first-run download. Updating `VERSION` selects a separate cache entry, so an older cached executable cannot silently satisfy a newer skill version.
