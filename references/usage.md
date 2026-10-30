# User guide

## Install

Download the ZIP for your operating system and processor from **Assets** on [GitHub Releases](https://github.com/radxa-linkr/linkr-now-skill/releases), then extract it. Choose `linkr-now-skill-<version>-<platform>-<architecture>.zip`, where the platform is `windows`, `darwin` (macOS), or `linux`, and the architecture is `amd64` (x64/x86_64 or Intel Mac) or `arm64` (ARM64 or Apple Silicon). The archive contains a `linkr-now-skill` folder with the skill instructions, launchers, `VERSION`, and a ready-to-run executable. You do not need Python, Node.js, Go, or other build tools.

A source installation also works without building. GitHub's **Source code (zip)** and **Source code (tar.gz)** downloads, a Git clone, or a skill installer can provide the scripts and `VERSION` without the prebuilt executable. On first use, the launcher downloads the executable for the current operating system and processor from the GitHub Release matching `VERSION`, verifies it against the release's `SHA256SUMS`, and stores it in the user's cache. Subsequent runs of the same version and platform use the cache and can work offline. The initial download needs Internet access. On macOS and Linux it also needs `curl` or `wget` plus one of `sha256sum`, `shasum`, or `openssl`; no `unzip`, Python, or Go is needed. The Windows launcher uses PowerShell.

Place the entire folder in the skills directory supported by your assistant. For a user installation in Codex, use `~/.agents/skills/linkr-now-skill/` on macOS and Linux, or `$HOME\.agents\skills\linkr-now-skill\` in Windows PowerShell. For a project-only installation, use `<project>/.agents/skills/linkr-now-skill/`. Keep the original folder structure, with `SKILL.md` directly inside `linkr-now-skill`. Codex automatically detects local skills; if it does not appear, restart Codex. These paths follow the [official Codex skills guide](https://learn.chatgpt.com/docs/build-skills#where-codex-loads-local-skills).

Invoke the installed skill with a prompt such as:

```text
Use $linkr-now-skill to scan this computer's reachable Linkr devices and list their names, addresses, and status.
```

The launcher and executable can also run directly from the extracted folder without installing the skill. Keep the original folder structure so the assistant can locate them.

## Run commands

From the extracted or installed `linkr-now-skill` folder, prefer the launcher matching your platform:

```sh
# macOS or Linux
sh ./scripts/linkr-now.sh scan --json
```

```powershell
# Windows; pwsh can be used instead of powershell.exe
powershell.exe -NoProfile -ExecutionPolicy Bypass -File '.\scripts\linkr-now.ps1' scan --json
```

The launcher selects the matching processor automatically and uses a bundled executable before downloading anything. From another directory, replace the relative script path with its quoted absolute path. All command arguments go after the script path.

If you already have a matching binary, you can invoke it directly. For example:

```sh
./bin/linux-amd64/linkr-now scan --json
./bin/darwin-arm64/linkr-now scan --json
```

```powershell
& '.\bin\windows-amd64\linkr-now.exe' scan --json
```

Use the platform table in `SKILL.md` for other processors. Quote executable paths containing spaces; PowerShell requires the `&` invocation operator for a quoted executable path. If an older installation has no launcher or executable, download a matching platform ZIP from [GitHub Releases](https://github.com/radxa-linkr/linkr-now-skill/releases). Source builds are an optional development workflow.

In the examples below, `<executable>` stands for the launcher command, including its script path, or a direct executable path. Replace the example IP `192.0.2.10` with your device's address.

| Task | Command |
| --- | --- |
| Discover USB and LAN devices | `<executable> scan --json` |
| Query a known address | `<executable> info 192.0.2.10 --json` |
| Discover LAN devices through a known device | `<executable> scan --via 192.0.2.10 --no-usb --json` |
| Query only specified targets | `<executable> scan --no-network --no-usb --target 192.0.2.10 --json` |
| Monitor a device for one minute | `<executable> watch 192.0.2.10 --duration 60s --json` |
| Diagnose discovery | `<executable> doctor --json` |
| Diagnose a known address | `<executable> doctor 192.0.2.10 --json` |
| Diagnose the default USB IP | `<executable> doctor 192.168.99.1 --json` |
| Check against the latest official version | `<executable> version 192.0.2.10 --json` |
| Compare versions offline | `<executable> version 192.0.2.10 --latest-version v1.6.0 --json` |

Hosts can be names, IPv4 addresses, or HTTP(S) origins with a port. Use brackets for IPv6, such as `http://[2001:db8::10]:80`. Supply the origin without credentials, a path, a query, or a fragment.

LAN discovery requests the entry point's HTTP device list and runs a five-second local IPv4 mDNS browse in parallel. It combines both results even when the HTTP request succeeds, since one entry point may not see every device. Entries with the same domain are merged, including filling a missing IP address, before the tool queries individual devices. USB discovery remains separate.

The browse queries both `_http._tcp.local` device services and `_linkr._tcp.local` discovery entry points. If the default LAN entry point is unreachable, the tool requests `/linkr/list` using an advertised entry point's IP address and port. `_linkr` advertisements supply discovery entry points without adding an extra physical-device result, avoiding duplicate devices in the output.

Default USB discovery first queries `linkr-usb.local`. If the alias is unavailable, it tries `192.168.99.1`, the device's default USB IP. `scan`, `watch`, and `version` use this sequence whenever they perform USB discovery. A custom endpoint supplied with `--usb-host` is used as provided, without appending the fixed-IP fallback. `--no-usb` disables both default USB attempts.

For a `.local` connection, the tool first queries mDNS through each active adapter that supports IPv4 multicast and tries the returned address. If that does not produce a working connection, it tries the hostname through the system resolver. A known IP avoids hostname resolution.

## Options

| Option | Applies to | Purpose |
| --- | --- | --- |
| `--json` | All commands | Structured output; monitoring emits one JSON object per line |
| `--timeout 10s` | All commands | HTTP request timeout; for monitoring, limits waiting for response headers |
| `--via HOST` | `scan`, `watch`, `version` | LAN discovery entry point; default `linkr.local` |
| `--usb-host HOST` | `scan`, `watch`, `version` | Custom USB endpoint; default `linkr-usb.local` falls back to `192.168.99.1` |
| `--no-usb` | `scan`, `watch`, `version` | Skip USB discovery, including the default alias and IP fallback |
| `--no-network` | `scan`, `watch`, `version` | Skip LAN discovery; explicit targets are still queried |
| `--no-mdns` | `scan`, `watch`, `version` | Skip local multicast service browsing; `.local` hostname resolution still tries mDNS |
| `--target HOST` | `scan`, `watch`, `version` | Add a target; repeat for multiple devices |
| `--duration 60s` | `watch` | Total monitoring time, including initial discovery |
| `--latest-version VERSION` | `version` | Compare against this version without retrieving release metadata |
| `--latest-url URL` | `version` | Use a different release metadata URL |

Durations accept values such as `10s`, `500ms`, or a number of seconds. LAN discovery runs its HTTP list request and five-second multicast browse in parallel, then queries individual devices. The list request, hostname resolution, and multiple batches of device queries can make the total scan duration exceed `--timeout`. Use `--no-mdns` to skip service browsing or `--no-network --no-usb --target DEVICE_IP` to query only a known address.

Without explicit host arguments, `watch` and `version` perform discovery first. Monitoring reconnects five seconds after a stream closes or after 45 seconds without a device event.

## Results

Device results include `source`, `name`, `domain`, `ip`, `url`, `apiUrl`, `status`, `connected`, `version`, `onlineTimeSeconds`, `heartbeat`, `receivedAt`, and `isResetting`. `url` is the device management address; `apiUrl` is the address used for queries, which may use an IP fallback.

When the default USB alias fails and the IP fallback succeeds, `source` remains `usb`, while `ip`, `apiUrl`, and the management `url` show the working `192.168.99.1` endpoint. Use that management URL to reach the device without relying on the unavailable alias.

The `usb` source describes the USB endpoint category, not a verified physical connection. A successful `linkr-usb.local` query may reach a remote device through a VPN, TUN adapter, or forwarding. Check the resolved address, selected route, and active network adapter to confirm whether traffic uses a physical USB link. Neither a successful query nor its source label establishes that path by itself.

| Status | Meaning |
| --- | --- |
| `UNCONFIGURED` | Device setup is incomplete |
| `IDLE` | Device is configured and has no active KVM connection |
| `USING` | Device has an active KVM connection |
| `Connecting` | Waiting for valid information on a new connection |
| `Disconnected` | Device information could not be refreshed |

Monitoring reports `INFO`, `CLICK`, `RESET`, `CONNECTING`, `DISCONNECTED`, and `ERROR` events. To identify a device, briefly press its physical button once and look for `CLICK`. A long press can trigger recovery. `RESET` is a notification from the device; the tool does not send reset commands.

Errors are reported alongside successful results. Exit code 0 indicates at least one confirmed device, 1 indicates an unsuccessful operation or no confirmed devices, and 2 indicates invalid arguments. A monitoring session succeeds if it receives valid device information before finishing. A device listed under both USB and LAN may be the same physical device.

## Troubleshooting

- Run the tool on the computer connected to the device or on the same LAN. A remotely hosted assistant cannot discover devices on your computer's local network unless it can execute commands there.
- If `.local` names fail, try `doctor`, then use a known device IP with `info`, `--target`, or `--via`.
- Multicast discovery queries active network adapters that support IPv4 multicast. Network isolation or blocked multicast can prevent discovery. Direct IP queries remain available.
- For a physical USB connection, check that the USB network adapter and device network service are available; a USB cable alone may not provide network access. The USB alias may also be reachable through an existing routed connection, so confirm its resolved IP and route before attributing a result to physical USB. If `linkr-usb.local` fails, the default discovery path automatically tries `192.168.99.1`. You can test that address directly with `<executable> doctor 192.168.99.1 --json` or `<executable> info 192.168.99.1 --json`.
- Device traffic bypasses environment HTTP proxies. No browser permission is required. Requests do not automatically follow redirects.
- If the device reports HTTP 429, wait briefly before retrying. Querying several aliases for one device can reach its request limit.
- For an offline version comparison, supply `--latest-version`. Other commands do not require Internet access.

## Proxy, VPN, and TUN troubleshooting

The tool bypasses environment HTTP proxies such as `HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY`. A VPN or a proxy's TUN mode can still change operating-system routes, intercept DNS, or replace hostname answers with synthetic addresses (often called Fake-IP). Clearing HTTP proxy variables does not bypass those mechanisms. A browser may use different DNS, cached information, or networking behavior, so browser discovery alone does not prove which mechanism caused a command to fail.

Start with diagnostics and compare the same device by IP and hostname:

```text
<executable> doctor --json
<executable> doctor 192.0.2.10 --json
<executable> doctor linkr-example.local --json
<executable> info 192.0.2.10 --json
<executable> info linkr-example.local --json
```

Replace `192.0.2.10` and `linkr-example.local` with that device's known IP and hostname. For every `.local` target, `doctor` queries system DNS and mDNS independently, even if system DNS returns an answer. Compare `resolvedAddresses` with `mdnsAddresses` and the device HTTP response. An unexpected system DNS address can reveal possible DNS/Fake-IP interception; a difference alone does not establish the cause. A DNS error and a successful HTTP result can appear together when the mDNS connection succeeds.

| Observation | Next check |
| --- | --- |
| The IP works, but the hostname fails or resolves to an unexpected address | Check `.local` resolution, mDNS, and DNS/Fake-IP interception |
| Both the IP and hostname fail | Check the device connection, LAN or USB adapter, routes, and existing traffic filtering |
| Direct queries work, but discovery fails | Check the discovery entry point and multicast path; use `scan --via DEVICE_IP --no-usb --json` meanwhile |

Inspect routes without changing them. Check both the IPv4 mDNS destination and the device IP, since discovery and direct HTTP traffic can take different paths.

Windows PowerShell:

```powershell
Find-NetRoute -RemoteIPAddress 224.0.0.251
Find-NetRoute -RemoteIPAddress 192.0.2.10
route print
```

[`Find-NetRoute`](https://learn.microsoft.com/powershell/module/nettcpip/find-netroute) reports the selected local address and route. If it is unavailable, inspect the interface and route entries in `route print`.

macOS:

```sh
route -n get 224.0.0.251
route -n get 192.0.2.10
```

Linux:

```sh
ip route get 224.0.0.251
ip route get 192.0.2.10
```

Replace the example device IP in these commands. Compare the selected interface, source address, and gateway with the LAN or USB connection that reaches the device. A route through a virtual adapter is useful evidence of possible interception, not proof of the cause. The tool selects active adapters that support IPv4 multicast and sends queries through each; a default multicast route lookup alone does not describe every query it sends.

IPv4 mDNS uses destination `224.0.0.251` on UDP port `5353` and normally stays on the local network link. Check whether multicast can reach the correct LAN or USB adapter and return through it. Wi-Fi client isolation, VLAN boundaries, existing firewall rules, and VPN/TUN handling can affect this path even when direct IP access works.

If the evidence points to the VPN or proxy client, review its local-network bypass settings. Device IPs and the device subnet should remain directly reachable; `.local` names should use local resolution and be excluded from DNS/Fake-IP substitution. Check the client's documented syntax for domain exclusions and multicast handling. An HTTP proxy bypass list alone does not configure TUN routing or multicast.

A temporary comparison with TUN disabled can help isolate its effect, but only if you choose to perform it yourself and can do so without interrupting a connection you need. Repeat the same diagnostics and restore the prior setting afterward. The skill does not automatically disable a proxy or VPN, edit DNS or routes, or change firewall rules. Use the observed difference to choose a specific configuration adjustment; do not assume that disabling TUN is required for every discovery failure.
