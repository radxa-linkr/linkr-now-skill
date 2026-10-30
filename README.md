# Linkr.now Skill

Discover and monitor Radxa Linkr devices directly from your AI assistant, without opening linkr.now.

The skill works over LAN or USB networking. It can list device names, addresses, and status; query a known device; identify a device through its physical button; diagnose connectivity; and compare firmware versions.

Ready-to-run packages include the skill and a native `linkr-now` executable. Source installations can download the matching executable automatically on first use. Users do not need Python, Node.js, Go, or other build tools. Run the skill on the computer connected to the devices or on the same LAN.

## Download

Open [GitHub Releases](https://github.com/radxa-linkr/linkr-now-skill/releases), choose a release, and download the ZIP matching your operating system and processor from **Assets**:

| Platform | Processor | Release asset |
| --- | --- | --- |
| Windows | x64 | `linkr-now-skill-<version>-windows-amd64.zip` |
| Windows | ARM64 | `linkr-now-skill-<version>-windows-arm64.zip` |
| macOS | Apple Silicon | `linkr-now-skill-<version>-darwin-arm64.zip` |
| macOS | Intel | `linkr-now-skill-<version>-darwin-amd64.zip` |
| Linux | x86_64 | `linkr-now-skill-<version>-linux-amd64.zip` |
| Linux | ARM64 | `linkr-now-skill-<version>-linux-arm64.zip` |

`<version>` is the release version. Each platform ZIP contains a `linkr-now-skill` folder with the matching executable. If no matching ZIP appears under Assets, that release does not include a ready-to-run package for your platform.

GitHub's **Source code (zip)** and **Source code (tar.gz)** downloads contain source files and launchers, but no prebuilt executable. A Git clone or source-based skill installation uses the same launchers. Generated `bin/` and `dist/` files are not stored in the repository.

For source installations, the launcher reads the root `VERSION` file, downloads the executable for your operating system and processor from the matching release, verifies it against `SHA256SUMS`, and saves it in your user cache. Later runs of the same version can use the cache offline. On macOS and Linux, the first download requires `curl` or `wget` plus one of `sha256sum`, `shasum`, or `openssl`. Windows uses PowerShell. No archive extractor or programming language is needed for this download.

Choose a platform ZIP to have the executable included from the start. See [Building from source](BUILDING.md) if you want to compile or contribute.

## Install in Codex

Extract the platform ZIP, then place the entire `linkr-now-skill` folder in your user skills directory:

| System | Installed folder |
| --- | --- |
| macOS or Linux | `~/.agents/skills/linkr-now-skill/` |
| Windows | `$HOME\.agents\skills\linkr-now-skill\` in PowerShell |

For a project-only installation, use `<project>/.agents/skills/linkr-now-skill/` instead. Keep the extracted folder intact, including `SKILL.md`, `VERSION`, `scripts/`, and the bundled `bin/` directory. You can also install the source folder at the same location and let its launcher download the executable on first use. Codex detects local skills automatically; if the skill does not appear, restart Codex. These locations follow the [official Codex skills guide](https://learn.chatgpt.com/docs/build-skills#where-codex-loads-local-skills).

Ask Codex:

```text
Use $linkr-now-skill to scan this computer's reachable Linkr devices and list their names, addresses, and status.
```

The assistant must execute commands on the computer that can reach the devices. A remotely hosted assistant cannot scan your local network unless it has an execution connection to that computer.

## Run without an assistant

From the extracted or installed `linkr-now-skill` folder:

```sh
# macOS or Linux
sh ./scripts/linkr-now.sh scan --json
```

```powershell
# Windows; pwsh can be used instead of powershell.exe
powershell.exe -NoProfile -ExecutionPolicy Bypass -File '.\scripts\linkr-now.ps1' scan --json
```

The launcher uses the included executable when present. You can also invoke that executable directly; the paths for all six platforms are listed in [SKILL.md](SKILL.md#direct-executables). See the [user guide](references/usage.md) for command options, result fields, and proxy/VPN troubleshooting.

## License

This project is licensed under the [MIT License](LICENSE). Third-party components retain their own licenses, reproduced in [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt).
