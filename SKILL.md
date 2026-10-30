---
name: linkr-now-skill
description: Discover Radxa Linkr devices over LAN or USB networking without opening linkr.now. Query device status, identify devices through physical button events, diagnose connectivity, and compare firmware versions.
---

# Linkr.now Skill

Use the `linkr-now` launcher to discover and monitor devices. No browser, Python, Node.js, or Go installation is required. Run it on a computer connected to the devices through USB networking or the same LAN.

## Choose the launcher

Prefer the launcher in this skill's installed directory. Replace `<skill>` with that directory's absolute path:

```sh
# macOS or Linux
sh "<skill>/scripts/linkr-now.sh" scan --json
```

```powershell
# Windows; pwsh can be used instead of powershell.exe
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "<skill>\scripts\linkr-now.ps1" scan --json
```

Platform ZIPs include the matching executable, so the launcher can run without a download. For a source installation with no bundled executable, the launcher reads `VERSION`, downloads the matching executable from the project's GitHub Release, verifies it against `SHA256SUMS`, and stores it in the user's cache. Later runs of that version can use the cache offline. On macOS and Linux, the initial download needs `curl` or `wget` and one of `sha256sum`, `shasum`, or `openssl`; it does not need an archive extractor or a programming language.

Use the launcher before considering a build or installing tools. If it is missing, use an existing matching executable directly or obtain the platform ZIP from [GitHub Releases](https://github.com/radxa-linkr/linkr-now-skill/releases). Build from source only when the user chooses that workflow.

## Direct executables

Select the binary for the computer running the command, relative to this skill's installed directory:

| Platform | Architecture | Executable |
| --- | --- | --- |
| Windows | x64 | `bin/windows-amd64/linkr-now.exe` |
| Windows | ARM64 | `bin/windows-arm64/linkr-now.exe` |
| macOS | Apple Silicon | `bin/darwin-arm64/linkr-now` |
| macOS | Intel | `bin/darwin-amd64/linkr-now` |
| Linux | x86_64 | `bin/linux-amd64/linkr-now` |
| Linux | ARM64 | `bin/linux-arm64/linkr-now` |

Each platform package includes its matching executable. Use its absolute path so commands work from any directory. In PowerShell, invoke a quoted executable path with `&`. If the executable is missing, download the matching platform ZIP from **Assets** on [GitHub Releases](https://github.com/radxa-linkr/linkr-now-skill/releases). GitHub's Source code downloads do not include the executable.

For installation, shell examples, command options, and troubleshooting, read [the user guide](references/usage.md) as needed.

## Discover and query

Replace `<executable>` below with the launcher command, including its script path, or the selected executable path. Pass command arguments after it. Add `--json` when processing results.

```text
<executable> scan --json
<executable> info 192.0.2.10 --json
<executable> scan --via 192.0.2.10 --no-usb --json
<executable> scan --no-network --no-usb --target 192.0.2.10 --json
<executable> doctor --json
```

`192.0.2.10` is an example address; replace it with the device's actual IP.

For a discovery request, run `scan`. It checks `linkr-usb.local` and combines the LAN entry point's HTTP device list with a five-second local IPv4 mDNS browse, running both LAN discovery methods in parallel. It merges LAN entries by domain, fills missing IP addresses, and queries the discovered devices. Local browsing also runs when the HTTP list succeeds, because one entry point may have an incomplete list. `--no-mdns` skips this browse but does not disable mDNS hostname resolution. When the device IP is known, use `info IP` directly or `--via IP` to use that device as a LAN discovery entry point.

Local browsing covers `_http._tcp.local` device services and `_linkr._tcp.local` discovery entry points. When the default LAN entry point is unreachable, the tool uses an advertised entry point's IP and port to request `/linkr/list`. Use `_linkr` advertisements only to obtain device lists; they do not add a separate physical-device result.

Default USB discovery tries `linkr-usb.local` first, then the device's default USB IP `192.168.99.1` if the alias is unavailable. This applies whenever `scan`, `watch`, or `version` performs USB discovery. After a successful IP fallback, report the usable IP in `ip`, `apiUrl`, and the management `url`. A custom `--usb-host` endpoint does not add the fixed-IP fallback. `--no-usb` skips both USB attempts. To diagnose the default USB IP directly, run `<executable> doctor 192.168.99.1 --json`.

For `.local` connections, the tool queries mDNS through each active IPv4 multicast interface before trying the system resolver if needed. `doctor` independently compares system DNS and mDNS for `.local` names even when system DNS succeeds, so an unexpected DNS or Fake-IP answer remains visible.

Report the device name, connection source, address, management URL, status, and any discovery errors. Partial failures do not mean that no devices exist. Keep USB and LAN entries separate, since one device may be reachable through both connections.

`source: usb` identifies the USB endpoint category; it does not prove a direct physical USB connection. The alias can reach a device through a VPN, TUN adapter, or forwarding, including a remote device. To establish the physical path, inspect the resolved IP, routes, and active network adapter. Keep the default alias-to-`192.168.99.1` fallback order and existing network settings unless the user requests a change.

## Identify and monitor

```text
<executable> watch --duration 45s --json
<executable> watch 192.0.2.10 --duration 45s --json
```

To identify a physical device, start monitoring, ask the user to briefly press its button once, and match the resulting `CLICK` event. A long press can trigger device recovery and should not be used for identification.

With explicit hosts, monitoring starts directly. Otherwise, it discovers targets first and keeps trying the USB entry point so a newly connected USB device can appear. Rerun discovery to find additional LAN devices. The default duration is 60 seconds and includes initial discovery. Output is one JSON object per event.

`RESET` is a device notification, not a command sent by this skill or confirmation that a reset has completed. The next valid `INFO` clears `isResetting`.

## Compare versions

```text
<executable> version 192.0.2.10 --json
<executable> version 192.0.2.10 --latest-version v1.6.0 --json
```

The first command reads official release metadata from the Internet. The second compares against a supplied version offline. Version checks do not install updates.

## Interpret results

`scan` and `info` return `devices` and `errors`. A listed LAN device can remain present as `Disconnected`; an unavailable USB entry is omitted. Exit code 0 means at least one device was confirmed, 1 means no device was confirmed or the operation failed, and 2 means invalid arguments. `watch` returns 0 after receiving valid `INFO` during the monitoring period.

`UNCONFIGURED`, `IDLE`, and `USING` describe device activity. `connected` means the query or current monitoring connection obtained valid device information. `onlineTimeSeconds` is device uptime; `heartbeat` uses the device's Unix clock; `receivedAt` records the receiving computer's UTC time. Do not treat these timestamps as interchangeable. Old uptime and heartbeat values are cleared on disconnection and reconnection.

If discovery fails, run `doctor` and explain whether the problem is name resolution, reachability, or a device response error. Prefer a known IP when multicast discovery is unavailable. The program connects directly without using environment HTTP proxies or following redirects. Physical USB access requires an active USB network connection; the USB alias may instead be reachable through an existing routed connection.

When a browser finds devices but this tool does not, a proxy/VPN/TUN is active, or a hostname resolves to an unexpected address, read [Proxy, VPN, and TUN troubleshooting](references/usage.md#proxy-vpn-and-tun-troubleshooting). Compare `doctor` results for a known device IP and its hostname, then inspect routes using the operating system's read-only commands. Bypassing HTTP proxies does not bypass TUN routing or DNS interception. Browser success alone does not identify the cause.

Do not automatically change proxy, DNS, route, or firewall settings. A temporary comparison with TUN disabled is optional and must be performed by the user; explain the findings before suggesting a specific configuration change.
