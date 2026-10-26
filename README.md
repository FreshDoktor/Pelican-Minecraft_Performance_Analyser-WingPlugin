# MPA Bridge

The node part of **Minecraft Performance Analyser (MPA)**: a small service on a [Pelican](https://pelican.dev) Wings node that looks into the JVM of a Minecraft server for the panel plugin [minecraft-performance-analyser](https://github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-PanelPlugin). A panel plugin cannot reach the Java process, the bridge can.

## Features

- **No process in the container**: metrics come from the JVM's own performance counters (hsperfdata, the file `jstat` reads), commands use the HotSpot attach protocol directly. Nothing is started inside the container, so a tight memory limit is never exceeded
- **JVM profile**: Java version, start arguments, garbage collector, heap settings and the container's memory and CPU limits
- **Live metrics**: heap per generation, GC count and time per collector, threads, classes, safepoints
- **Thread dump and class histogram** as JSON, `VM.flags` and the live heap
- **Lag analysis** with [async-profiler](https://github.com/async-profiler/async-profiler) (wall clock, CPU, allocations): collapsed stacks per thread plus GC pauses and CPU time in the same period. The library is embedded in the binary; nothing is printed to the server console
- **TPS and MSPT** for the panel's console: a light tick monitor that only runs while someone looks at the console
- **Uses the Wings credentials**: the node token of Wings is the bearer token, no second secret

## Requirements

- A Pelican Wings node, **linux-amd64**, with Docker and systemd
- Root, like Wings: the bridge reads `/proc/<pid>/root` of the server containers, signals their JVMs and talks to the Docker socket
- The panel must reach the bridge's port (default `8090`), directly or in an internal network
- Java servers with Java 17 or newer get every feature; Java 8 and 11 work, but are less tested

## Installation

On the node, as root:

```bash
curl -fsSLO https://github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/releases/latest/download/install.sh
bash install.sh
```

The script downloads `mpa-bridge-linux-amd64` from the latest release, checks its SHA-256 and installs it as `/usr/local/bin/mpa-bridge`. It asks for the **IP of your panel** (the only address allowed to connect) and the port, writes `/etc/pelican/mpa-bridge.yml`, installs the systemd service `mpa-bridge` and starts it.

Without questions, e.g. for automation:

```bash
bash install.sh --allowed-ips 10.0.0.2 --port 8090 --yes
```

| Option | Description |
| --- | --- |
| `--allowed-ips LIST` | IPs or CIDRs the panel connects from, comma separated (only for a new config) |
| `--port PORT` | listen port, default `8090` (only for a new config) |
| `--version X.Y.Z` | install this release instead of the latest |
| `--binary FILE` | install this binary instead of downloading one (a `FILE.sha256` next to it is checked) |
| `--yes` | don't ask, take the defaults |
| `--uninstall` | stop and remove the service and the binary, keep the config (`--purge` removes it too) |

Without internet access on the node: download `mpa-bridge-linux-amd64`, `mpa-bridge-linux-amd64.sha256` and `install.sh` of a release, copy them into one folder on the node and run `bash install.sh` there. The unit file is also attached to every release (`mpa-bridge.service`) for a manual installation.

**Then, in the panel:** _Admin_ → _Nodes_ → the node → tab _MPA Bridge_. Leave the address empty if the panel reaches the node under its FQDN on the bridge's port, else enter the address it can reach, e.g. `http://10.0.0.5:8090`. _Test connection_ checks that the bridge answers, belongs to this node and accepts its token.

### Update

Run `install.sh` again. It replaces the binary, keeps `/etc/pelican/mpa-bridge.yml` and restarts the service. A restart of the bridge doesn't touch the Minecraft servers; running lag analyses are lost, finished ones are already stored on the panel.

## Configuration

The bridge reads the Wings config `/etc/pelican/config.yml` (`token_id`, `token`, `api.ssl`, `docker.tmpfs_size`), resolving the token like Wings (`WINGS_TOKEN`, `${VAR}`, `file://`). A rotated token is picked up within 10 seconds. Its own settings are in `/etc/pelican/mpa-bridge.yml`, every one optional:

```yaml
port: 8090                  # listen port
host: 0.0.0.0               # listen address
allowed_ips:                # IPs or CIDRs allowed to connect, i.e. the panel; empty = everyone
  - 10.0.0.2
max_concurrent_profiles: 2  # lag analyses at the same time on this node
max_profile_seconds: 120    # upper limit for a lag analysis, longer requests are refused (the panel offers 10, 30 or 60 s)
max_tick_monitors: 10       # servers with TPS/MSPT at the same time on this node
# wings_config: /etc/pelican/config.yml
# docker_socket: /var/run/docker.sock   # default: DOCKER_HOST if it is unix://…
```

After a change: `systemctl restart mpa-bridge`.

**TLS:** the bridge speaks HTTPS with the Wings certificate if Wings has `api.ssl.enabled: true` (a renewed certificate is picked up), otherwise plain HTTP. With plain HTTP keep the port in an internal network or behind a firewall, and always set `allowed_ips`: the token travels with every request.

## What it does to a server

| Action in the panel | Effect on the server |
| --- | --- |
| Overview, live values | none: the counters are read from the host. The live heap every 30 s needs a short safepoint |
| TPS and MSPT under the console | very low: async-profiler counts the ticks of the `Server thread`. Starts with the first look at the console, stops a minute after it was closed |
| Lag analysis | low: async-profiler samples for 10 to 60 s. Needs 8 MiB in the container's `/tmp` (Wings mounts a tmpfs of `docker.tmpfs_size`, 100 MB by default) |
| Thread dump | a very short stop |
| Memory histogram | **the server stands still** while the JVM walks the heap, usually under a second; the panel warns first |

To attach, the bridge may have to start the JVM's attach listener: it creates a trigger file in the container's `/tmp` and sends `SIGQUIT`, which a JVM only answers with a thread dump. It never does that when it isn't safe: with `-XX:+DisableAttachMechanism`, with `-Xrs` (there `SIGQUIT` would stop the server) and in the first 30 seconds of a JVM. The panel then shows why the feature is unavailable. Files the bridge puts into `/tmp` (`mpa-*`) are removed after each run and when the bridge starts; `/tmp/mpa-libasyncProfiler.so` stays while the JVM runs and is reused.

## Troubleshooting

- **Log:** `journalctl -u mpa-bridge -f`. The version: `mpa-bridge -version`
- **Test connection fails with a timeout:** the panel can't reach the port. Check the firewall of the node and whether a proxy in front of the node only forwards the Wings port; then enter the internal address in the panel
- **The panel says the bridge doesn't accept its IP (`ip_not_allowed`):** the panel connects from an address that isn't in `allowed_ips`, e.g. its internal one. `ss -tn | grep 8090` on the node shows the peer while the panel tests
- **`wrong_node` / `unauthorized`:** the address points to another node, or the node's token in the panel and in `/etc/pelican/config.yml` differ
- **A feature is greyed out in the panel:** the reason is shown next to it, e.g. `-XX:+DisableAttachMechanism` in the start arguments or a server that has just started
- **`insufficient_tmp_space`:** raise `docker.tmpfs_size` in the Wings config (takes effect when the server restarts)

## License

Copyright (C) 2026 FreshDoktor. Licensed under the [GNU General Public License v3.0](LICENSE).

The binary bundles [async-profiler](https://github.com/async-profiler/async-profiler) 4.1 (`libasyncProfiler.so`, Apache License 2.0), see [assets/NOTICE](assets/NOTICE) and [assets/LICENSE-async-profiler](assets/LICENSE-async-profiler).
