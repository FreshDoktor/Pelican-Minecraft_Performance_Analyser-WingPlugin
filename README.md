# MPA Bridge

The node part of **Minecraft Performance Analyser (MPA)**: a small service on a [Pelican](https://pelican.dev) Wings node that looks into the JVM of a Minecraft server for the panel plugin [minecraft-performance-analyser](https://github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-PanelPlugin). A panel plugin cannot reach the Java process, the bridge can.

## Features

- **No process in the container**: metrics come from the JVM's own performance counters (hsperfdata, the file `jstat` reads), commands use the HotSpot attach protocol directly. Nothing is started inside the container, so a tight memory limit is never exceeded
- **JVM profile**: Java version, start arguments, garbage collector, heap settings and the container's memory and CPU limits
- **Live metrics**: heap per generation, GC count and time per collector, threads, classes, safepoints
- **Thread dump and class histogram** as JSON, `VM.flags` and the live heap
- **Lag analysis** with [async-profiler](https://github.com/async-profiler/async-profiler) (wall clock, CPU, allocations): collapsed stacks per thread plus GC pauses and CPU time in the same period. The library is embedded in the binary; nothing is printed to the server console
- **TPS and MSPT** for the panel's console: a light tick monitor that only runs while someone looks at the console
- **Secure by default**: an own token instead of the one of Wings, HTTPS with a certificate the panel pins, only the panel's IPs may connect, and a sandboxed systemd service, see [Security](#security)

## Requirements

- A Pelican Wings node, **linux-amd64**, with Docker and systemd
- Root, like Wings: the bridge reads `/proc/<pid>/root` of the server containers, signals their JVMs and talks to the Docker socket. The service keeps only the capabilities it needs for that
- The panel must reach the bridge's port (default `8090`), directly or in an internal network
- Java servers with Java 17 or newer get every feature; Java 8 and 11 work, but are less tested

## Installation

On the node, as root:

```bash
curl -fsSLO https://github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/releases/latest/download/install.sh
bash install.sh
```

The script downloads `mpa-bridge-linux-amd64` from the latest release, checks its SHA-256 and installs it as `/usr/local/bin/mpa-bridge`. It asks for the **IP of your panel** (only the panel may connect) and the port, writes `/etc/pelican/mpa-bridge.yml` with a new random **bridge token**, installs the systemd service `mpa-bridge` and starts it. At the end it prints everything the panel needs:

- the **address** of this node, e.g. `https://10.0.0.5:8090`, taken from the Wings config (with a certificate of a public CA, the name in that certificate)
- the **bridge token**
- the **certificate pin** (`sha256//…`), with which the panel checks the bridge's certificate

Without questions, e.g. for automation:

```bash
bash install.sh --allowed-ips 10.0.0.2 --port 8090 --yes
```

| Option | Description |
| --- | --- |
| `--allowed-ips LIST` | IPs or CIDRs the panel connects from, comma separated (for a new config, or one without `allowed_ips`) |
| `--port PORT` | listen port, default `8090` (only for a new config) |
| `--version X.Y.Z` | install this release instead of the latest |
| `--binary FILE` | install this binary instead of downloading one (a `FILE.sha256` next to it is checked) |
| `--yes` | don't ask, take the defaults (needs `--allowed-ips` when the config has none) |
| `--uninstall` | stop and remove the service and the binary, keep the config (`--purge` removes it and `/var/lib/mpa-bridge` too) |

Without internet access on the node: download `mpa-bridge-linux-amd64`, `mpa-bridge-linux-amd64.sha256`, `install.sh` and `mpa-bridge.service` of a release, copy them into one folder on the node and run `bash install.sh` there. The unit file `mpa-bridge.service` is also meant for a manual installation.

**Then, in the panel:** _Admin_ → _Nodes_ → the node → tab _MPA Bridge_. Enter the address, paste the bridge token and the certificate pin the script printed, then _Test connection_ and save. The test checks that the bridge answers, belongs to this node, has the pinned certificate and accepts the token. Without an address and a token the panel doesn't contact the node at all.

**Which address?** The one the panel reaches the node under. The printed one is a guess from the Wings config (Wings' `api.host`, else the address the node reaches the panel from); if the panel reaches the node under another address, e.g. through a VPN, enter that one. With the pin, an IP or any name of the node works. With the certificate of Wings or one of a public CA, enter the name in the certificate and leave the pin empty: the panel then checks the certificate like a browser, and a renewal by certbot doesn't break it.

### Update

Run `install.sh` again. It replaces the binary, keeps `/etc/pelican/mpa-bridge.yml` and restarts the service. A restart of the bridge doesn't touch the Minecraft servers; running lag analyses are lost, finished ones are already stored on the panel.

**From 1.0.0 to 1.1.0, once per node:** 1.1.0 no longer uses the token of Wings, serves HTTPS by default and requires `allowed_ips`. `install.sh` adds a bridge token to the existing config, asks for the panel's IP if `allowed_ips` is empty, and prints address, token and pin. Enter all three in the tab _MPA Bridge_ of the node, with `https://` in the address. Until then the panel treats the node as one without a bridge. Update the panel plugin to 1.1.0 as well.

## Configuration

The bridge reads the Wings config `/etc/pelican/config.yml` (`token_id`, `api.host`, `api.ssl`, `docker.tmpfs_size`); it doesn't read or accept the Wings token. Its own settings are in `/etc/pelican/mpa-bridge.yml` (mode `0600`, it holds the token). `token` and `allowed_ips` are required, the rest is optional:

```yaml
token: "…"                  # bridge token, at least 32 characters; MPA_BRIDGE_TOKEN, ${VAR} and file://<path> work like in Wings
port: 8090                  # listen port
host: 0.0.0.0               # listen address
allowed_ips:                # IPs or CIDRs allowed to connect, i.e. the panel; required
  - 10.0.0.2
# allow_all: true           # instead of allowed_ips: every address may connect (only the token protects the bridge then)
max_concurrent_profiles: 2  # lag analyses at the same time on this node
max_profile_seconds: 120    # upper limit for a lag analysis, longer requests are refused (the panel offers 10, 30 or 60 s)
max_tick_monitors: 10       # servers with TPS/MSPT at the same time on this node
# tls:                      # see TLS below
#   cert: /etc/letsencrypt/live/node.example.com/fullchain.pem
#   key: /etc/letsencrypt/live/node.example.com/privkey.pem
#   disable: true           # plain HTTP, not together with cert/key
# wings_config: /etc/pelican/config.yml
# docker_socket: /var/run/docker.sock   # default: DOCKER_HOST if it is unix://…
```

After a change: `systemctl restart mpa-bridge` (a new token is picked up within 10 seconds without one). Without `allowed_ips` and without `allow_all: true` the bridge doesn't start; `allow_all` logs a warning at every start.

**TLS:** the bridge always speaks HTTPS, with the first certificate that applies:

1. an own one, `tls.cert` and `tls.key`
2. the one of Wings, if Wings has `api.ssl.enabled: true`
3. a self-signed one the bridge creates on its first start, in `/var/lib/mpa-bridge/` (`tls.crt` and `tls.key`), valid for 10 years

A renewed certificate is picked up within 10 seconds. `mpa-bridge -fingerprint` prints the pin of the certificate in use; it changes only when the certificate gets a new key (certbot without `--reuse-key`), then enter the new pin in the panel. The service can't see `/home` and `/root`, so keep an own certificate elsewhere, e.g. under `/etc`. Plain HTTP only with `tls.disable: true`, meant for a proxy on the node that terminates TLS: the panel then accepts `http://` only for loopback and private addresses (`10.x`, `172.16–31.x`, `192.168.x`, `100.64–127.x`, `fc00::/7`), as the token travels with every request.

## What it does to a server

| Action in the panel | Effect on the server |
| --- | --- |
| Overview, live values | none: the counters are read from the host. The live heap every 30 s needs a short safepoint |
| TPS and MSPT under the console | very low: async-profiler counts the ticks of the `Server thread`. Starts with the first look at the console, stops a minute after it was closed |
| Lag analysis | low: async-profiler samples for 10 to 60 s. Needs 8 MiB in the container's `/tmp` (Wings mounts a tmpfs of `docker.tmpfs_size`, 100 MB by default). The bridge keeps at most 2 results per server, for 15 minutes; the panel fetches and stores them right away |
| Thread dump | a very short stop |
| Memory histogram | **the server stands still** while the JVM walks the heap, usually under a second; the panel warns first |

To attach, the bridge may have to start the JVM's attach listener: it creates a trigger file in the container's `/tmp` and sends `SIGQUIT`, which a JVM only answers with a thread dump. It never does that when it isn't safe: with `-XX:+DisableAttachMechanism`, with `-Xrs` (there `SIGQUIT` would stop the server) and in the first 30 seconds of a JVM. The panel then shows why the feature is unavailable. Files the bridge puts into `/tmp` (`mpa-*`) are removed after each run and when the bridge starts; `/tmp/mpa-libasyncProfiler.so` stays while the JVM runs and is reused.

## Security

The bridge runs as root on the node and is reachable over the network, so it is built to give an attacker as little as possible, from outside and from inside a server container:

- **Own token**: the panel authenticates with a random bridge token of its own, never with the token of Wings. A leaked bridge token gives no access to Wings
- **Only the panel**: addresses outside `allowed_ips` are refused before anything else and learn nothing about the node, not even the version
- **HTTPS with a pinned certificate**: the panel checks the key of the bridge's certificate, so nobody in between can read the token; the panel never sends it over plain `http://` to a public address, and doesn't follow redirects
- **Nothing in the container is trusted**: the server's owner controls the files in the container's `/tmp`. The bridge opens every path there step by step without following any symlink, never opens device files, and checks that a PID still belongs to the same JVM before it signals it or touches its files
- **Sandboxed service**: the systemd unit keeps only the capabilities the bridge needs (`CAP_KILL`, `CAP_SYS_PTRACE`, `CAP_DAC_OVERRIDE`, `CAP_FOWNER`, `CAP_NET_BIND_SERVICE`), the system read-only, `/home` hidden, a system call filter and a memory limit of 1 GiB. `systemd-analyze security mpa-bridge.service` rates it about 2.4 (OK)
- **Verifiable releases**: the binary, `install.sh` and the unit carry a build attestation of the GitHub release workflow:

  ```bash
  gh attestation verify mpa-bridge-linux-amd64 --repo FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin
  ```

  The bundled `libasyncProfiler.so` is the unmodified one of async-profiler 4.1, see [assets/NOTICE](assets/NOTICE).

## Troubleshooting

- **Log:** `journalctl -u mpa-bridge -f`. The version: `mpa-bridge -version`. The line `listening` names the certificate in use (`cert=own`, `wings` or `self-signed`) and its pin
- **The bridge doesn't start, the log says `no token` or `allowed_ips`:** run `install.sh` again, it adds a token or asks for the panel's IP
- **The bridge doesn't start because of the certificate:** an own certificate under `/home` or `/root` can't be read by the service, move it. If only one of `/var/lib/mpa-bridge/tls.crt` and `tls.key` exists, remove it; the bridge creates a new pair (and a new pin)
- **Test connection fails with a timeout:** the panel can't reach the port. Check the firewall of the node and whether a proxy in front of the node only forwards the Wings port; then enter the internal address in the panel
- **The panel says the certificate doesn't match the pin:** the certificate got a new key, or something else answers on the address. `mpa-bridge -fingerprint` on the node prints the current pin
- **The panel says the certificate isn't trusted:** no pin is set and the certificate is the self-signed one, or the address doesn't carry the name in the certificate. Paste the pin, or enter the name in the certificate
- **The panel refuses an `http://` address:** use `https://`, the bridge serves it by default. `http://` is only accepted for loopback and private IPs, and only with `tls.disable: true` on the bridge
- **The panel says the bridge doesn't accept its IP (`ip_not_allowed`):** the panel connects from an address that isn't in `allowed_ips`, e.g. its internal one. `ss -tn | grep 8090` on the node shows the peer while the panel tests
- **`wrong_node` / `unauthorized`:** the address points to another node, or the bridge token in the panel and in `/etc/pelican/mpa-bridge.yml` differ
- **A feature is greyed out in the panel:** the reason is shown next to it, e.g. `-XX:+DisableAttachMechanism` in the start arguments or a server that has just started
- **`insufficient_tmp_space`:** raise `docker.tmpfs_size` in the Wings config (takes effect when the server restarts)

## License

Copyright (C) 2026 FreshDoktor. Licensed under the [GNU General Public License v3.0](LICENSE).

The binary bundles [async-profiler](https://github.com/async-profiler/async-profiler) 4.1 (`libasyncProfiler.so`, Apache License 2.0), see [assets/NOTICE](assets/NOTICE) and [assets/LICENSE-async-profiler](assets/LICENSE-async-profiler).
