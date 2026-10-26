# MPA Bridge HTTP API v1

This is the contract between the MPA Bridge (this repo) and the panel plugin
[minecraft-performance-analyser](https://github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-PanelPlugin).
Every response format is shown in [`examples/`](examples/). The examples are
generated from the bridge's response types and the fixtures in `testdata/`
(`go test ./internal/api -run Examples -update`), and a test fails when they
drift apart. The panel may use them 1:1 as fake responses.

If the contract changes, this file and the examples change first, then both sides.

- [Conventions](#conventions)
- [Transport (TLS)](#transport-tls)
- [Authentication and headers](#authentication-and-headers)
- [Versioning](#versioning)
- [Endpoints](#endpoints): [node status](#get-apiv1status), [server status](#get-status), [vm](#get-vm), [metrics](#get-metrics), [threads](#get-threads), [histogram](#get-histogram), [profiles](#profiles), [ticks](#get-ticks)
- [Errors](#errors)
- [Examples](#examples)

## Conventions

- Base URL: `https://<node>:8090`, plain `http://` only with `tls.disable: true`, see [Transport (TLS)](#transport-tls).
- Server endpoints live under `/api/v1/servers/{uuid}/…`. `{uuid}` is the Pelican server UUID
  (`xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`, hex, case-insensitive). It must name a container with the
  Wings labels `Service=Pelican` and `ContainerType=server_process`; anything else is `server_not_found`.
- Responses are `application/json; charset=utf-8`, UTF-8, keys in `snake_case`.
- **Every documented field is always present.** Fields marked *nullable* are `null` when unknown or
  not applicable, they are never omitted. Arrays are never `null` (empty array instead). Objects
  documented as *map* have dynamic keys.
- Clients must ignore unknown fields and treat unknown enum values like `unknown` (see [Versioning](#versioning)).
- **Units** are part of the field name and never vary:

  | Suffix / name | Unit | JSON type |
  | --- | --- | --- |
  | `_bytes` | bytes | integer |
  | `_ms` (durations, uptimes) | milliseconds, fractional values rounded to 3 decimals (µs) | number |
  | `_at` | Unix time in **milliseconds** (UTC) | integer |
  | `_us` | microseconds (Docker CPU quota/period only) | integer |
  | `_percent` | percent, 0–100 (can exceed 100 with several cores) | number |
  | `count`, `instances`, … | plain count | integer |

  JVM ticks never appear in JSON; the bridge converts them with `sun.os.hrt.frequency`.
- Timestamps come from the clock of the node (`sampled_at`), JVM times from hsperfdata.

## Transport (TLS)

The bridge serves HTTPS (TLS 1.2 or newer) unless TLS is switched off explicitly. The `tls` section of
`mpa-bridge.yml` decides which certificate it serves, the first that applies:

1. `tls.cert` and `tls.key`: an own certificate (PEM, the chain in `cert`).
2. The certificate of Wings (`api.ssl.cert` and `api.ssl.key` in the Wings config), if Wings has
   `api.ssl.enabled: true`.
3. A **self-signed certificate** the bridge creates on its first start: ECDSA P-256, valid for 10
   years, for the host name and `localhost`. It is kept at `tls.self_signed_cert` and
   `tls.self_signed_key` (default `/var/lib/mpa-bridge/tls.crt`, `0644`, and
   `/var/lib/mpa-bridge/tls.key`, `0600`) and reused on later starts. `/var/lib/mpa-bridge` is the
   `StateDirectory` of the systemd unit (`0700`), the only place the sandboxed service may write;
   a directory that does not exist yet is created `0700`. The bridge creates both files
   only if neither exists (`O_EXCL`, it neither overwrites a file nor follows a link); if only one of
   them exists it refuses to start, remove it to get a new pair.
4. Plain HTTP only with `tls.disable: true`. The token then crosses the network in cleartext; the
   bridge logs a warning at start unless `host` is a loopback address. Meant for a proxy on the node
   that terminates TLS.

| Key in `mpa-bridge.yml` | Default | Meaning |
| --- | --- | --- |
| `tls.cert`, `tls.key` | none | own certificate and key; only together |
| `tls.disable` | `false` | plain HTTP; not together with `tls.cert`/`tls.key` |
| `tls.self_signed_cert`, `tls.self_signed_key` | `/var/lib/mpa-bridge/tls.crt`, `/var/lib/mpa-bridge/tls.key` | where the self-signed certificate is kept; elsewhere the service can only create it with a `ReadWritePaths=` for that directory (drop-in) |

`cert` without `key` (or the other way round) and `disable` together with `cert`/`key` are errors,
the bridge does not start. The unit hides `/home`, `/root` and `/run/user` from the bridge
(`ProtectHome=yes`), so an own certificate must not be kept there. Changes to the `tls` section need
a restart. A certificate file that is replaced (certbot renewal, a new self-signed pair) is picked up
within 10 seconds; until the new pair loads, the previous one stays in use. `tls` in [`GET /api/v1/status`](#get-apiv1status) tells whether
the bridge serves HTTPS.

The panel checks the certificate by its **public-key pin** (cURL's `CURLOPT_PINNEDPUBLICKEY`),
entered per node, instead of a CA, so the self-signed certificate is enough and its name does not
matter. Without a pin it checks the certificate against the CA store of the panel. The pin is
`sha256//` followed by the base64 SHA-256 of the DER `SubjectPublicKeyInfo` of the leaf certificate,
e.g. `sha256//+6IzzDvfXgsEHYmcF3t3lfm9IAnYwJmz/ureqK1JWOg=`; it changes only with the key, not with a
renewal that keeps the key.

```
mpa-bridge -fingerprint [-config /etc/pelican/mpa-bridge.yml]
```

prints the pin of the certificate the bridge serves (or would serve; the self-signed one is created if
it does not exist yet) on stdout and exits with status 0. With `tls.disable` it says so on stderr and
exits with status 3, on any other error with status 1. `install.sh` prints the pin together with the
token. The same is computed by
`openssl x509 -in <cert> -pubkey -noout | openssl pkey -pubin -outform der | openssl dgst -sha256 -binary | base64`.

## Authentication and headers

| Header | Direction | Value |
| --- | --- | --- |
| `Authorization` | request | `Bearer <token>`: the **bridge token**, `token` in `/etc/pelican/mpa-bridge.yml` (at least 32 characters, generated by `install.sh`), entered in the panel per node. It is independent of the Wings token, which the bridge neither reads nor accepts (since 1.1.0). Compared in constant time. |
| `User-Agent` | **response** | `MPA Bridge/v<version> (id:<token_id>)`, e.g. `MPA Bridge/v1.0.0 (id:Xy12Ab34Cd56Ef78)`. `<token_id>` is `token_id` of the Wings config (`daemon_token_id` in the panel). Sent on **every** response to an address in `allowed_ips`, including 401, 404 and 5xx, but **not** on `403 ip_not_allowed`: an address outside `allowed_ips` learns neither the version nor the `token_id`. The panel rejects a response whose id does not match the node, or that lacks the header (`wrong_node`); a `403` without it whose `error` is `ip_not_allowed` it reports as `ip_not_allowed` instead (the node cannot be verified then). |
| `Content-Type` | request | `application/json` for request bodies (`POST /profiles`). Bodies are limited to 4 KiB. |

The bridge resolves its token like Wings resolves its own: `MPA_BRIDGE_TOKEN` takes precedence over
the file, then `${VAR}` is expanded and a `file://<path>` value is read from that file. Without a
token, or with one shorter than 32 characters, the bridge refuses to start. `token_id` is resolved the
same way from the Wings config (`WINGS_TOKEN_ID`). A changed `mpa-bridge.yml` (new token) or
`config.yml` (new `token_id`) is picked up within 10 seconds.

Order of checks: client IP (`allowed_ips` in `mpa-bridge.yml`, `ip_not_allowed`) → token
(`unauthorized`) → route (`not_found`, `method_not_allowed`) → parameters (`invalid_request`) → server state.
The client IP is the TCP peer; `X-Forwarded-For` is ignored.

The panel calls the bridge server-side only. The bridge sends no CORS headers.

## Versioning

- The major API version is in the path (`/api/v1`) and in `api_version` (integer, `1`).
- Within v1 the bridge only makes **additive** changes: new endpoints, new fields, new enum values,
  new error codes, new `features`. Removing or renaming a field, changing its type or unit, or
  changing the meaning of a value requires `/api/v2`.
- The bridge version (semver) is in `User-Agent`, `bridge_version` and `bridge.version`.
- `features` lists what this bridge build implements. Endpoints of later milestones exist in every
  build but answer `501 not_implemented` until their feature is listed:

  | Feature | Endpoint / field | Milestone | Bridge version |
  | --- | --- | --- | --- |
  | `status`, `vm`, `metrics` | `/status`, `/vm`, `/metrics` | 1 | 0.1.0 |
  | `threads` | `GET /threads` | 2 | 0.1.0 |
  | `histogram` | `GET /histogram` | 2 | 0.1.0 |
  | `vm_flags` | `flags` in `/vm` | 2 | 0.1.0 |
  | `live_heap` | `live_heap` in `/metrics?live=1` | 2 | 0.1.0 |
  | `profiles` | `/profiles…` | 3 | 0.1.0 |
  | `ticks` | `GET /ticks`, `limits.max_tick_monitors` | 3 | 0.2.0 (counting all the time since 0.3.0, state `idle` since 0.3.1) |

  Bridge 1.0.0 is the first release and has every feature; the milestone builds only existed during
  development. The panel still handles them, which keeps it working with older or newer bridges that
  lack a feature.

## Endpoints

| Method + path | Purpose | Cost for the server | Milestone |
| --- | --- | --- | --- |
| `GET /api/v1/status` | Bridge version, features (node level, no server) | none | 1 |
| `GET /api/v1/servers/{uuid}/status` | Container running? Java found? PIDs, Java version, attach | none | 1 |
| `GET /api/v1/servers/{uuid}/vm` | Java, start arguments, GC, container limits (+ `VM.flags`) | none (+ attach) | 1 (2) |
| `GET /api/v1/servers/{uuid}/metrics` | Heap, GC, threads, classes, safepoints (+ live heap) | none (+ short safepoint) | 1 (2) |
| `GET /api/v1/servers/{uuid}/threads` | Thread dump as JSON | short safepoint | 2 |
| `GET /api/v1/servers/{uuid}/histogram` | Class histogram | **heap walk with a pause** (warn the user) | 2 |
| `POST /api/v1/servers/{uuid}/profiles` | Start a profile | low | 3 |
| `GET /api/v1/servers/{uuid}/profiles` | Profiles of the server (without results) | – | 3 |
| `GET /api/v1/servers/{uuid}/profiles/{id}` | Status, result when done | – | 3 |
| `DELETE /api/v1/servers/{uuid}/profiles/{id}` | Cancel / discard | – | 3 |
| `GET /api/v1/servers/{uuid}/ticks` | Approximate TPS and MSPT with a history of 10 minutes (tick monitor) | very low | 3 |

### `GET /api/v1/status`

Node level, for the bridge test button and detection. Same authentication. Examples:
[`node-status.json`](examples/node-status.json) (all features),
[`node-status-m1.json`](examples/node-status-m1.json), [`node-status-m2.json`](examples/node-status-m2.json) and
[`node-status-m3.json`](examples/node-status-m3.json) (what a milestone-1 / milestone-2 bridge and one with
profiles but without the tick monitor report).

| Field | Type | Description |
| --- | --- | --- |
| `bridge_version` | string | semver, e.g. `1.0.0` |
| `api_version` | integer | `1` |
| `features` | string[] | see [Versioning](#versioning) |
| `started_at` | Unix ms | start of the bridge process |
| `uptime_ms` | integer | uptime of the bridge |
| `sampled_at` | Unix ms | clock of the node |
| `tls` | bool | the bridge serves HTTPS (`false` only with `tls.disable`), see [Transport (TLS)](#transport-tls) |
| `limits.max_concurrent_profiles` | integer | profiles at the same time on this node |
| `limits.max_profile_seconds` | integer | upper bound of `seconds` in `POST /profiles` |
| `limits.max_tick_monitors` | integer | tick monitors at the same time on this node (feature `ticks`; older bridges omit it) |

### `GET /status`

Always `200` as long as the container exists (also when it is stopped), so the panel can show the
state. `404 server_not_found` otherwise, `503 docker_unavailable` if Docker cannot be reached. Reads only Docker, `/proc` and hsperfdata.

Examples: [`status.json`](examples/status.json) (ready),
[`status-not-running.json`](examples/status-not-running.json),
[`status-no-java.json`](examples/status-no-java.json),
[`status-hsperf-unavailable.json`](examples/status-hsperf-unavailable.json).

| Field | Type | Description |
| --- | --- | --- |
| `state` | enum | `ready` · `server_not_running` · `no_java_process` · `hsperf_unavailable`: `ready` means `/vm` and `/metrics` will work; otherwise the error code they would return |
| `bridge.version` | string | bridge version |
| `bridge.api_version` | integer | `1` |
| `bridge.features` | string[] | see [Versioning](#versioning) |
| `server.uuid` | string | |
| `server.container_state` | enum | Docker state: `created` · `running` · `paused` · `restarting` · `removing` · `exited` · `dead` |
| `server.running` | bool | `container_state == "running"` (a paused container counts as not running) |
| `server.started_at` | Unix ms, nullable | Docker `State.StartedAt` (last start, also for stopped containers); `null` if never started |
| `java` | object, nullable | `null` unless running and a `java` process was found |
| `java.host_pid` | integer | PID on the node |
| `java.container_pid` | integer | PID inside the container (`NSpid`), e.g. `26` |
| `java.user` | string, nullable | user of `/tmp/hsperfdata_<user>`, e.g. `container`; `null` without hsperfdata |
| `java.hsperf_available` | bool | hsperfdata file found and readable |
| `java.version` | string, nullable | `java.version`, e.g. `25.0.4.1` |
| `java.vm_name` | string, nullable | e.g. `OpenJDK 64-Bit Server VM` |
| `java.vm_vendor` | string, nullable | e.g. `Eclipse Adoptium` |
| `java.started_at` | Unix ms, nullable | JVM start (`sun.rt.createVmBeginTime`) |
| `java.uptime_ms` | integer, nullable | `sampled_at - java.started_at` |
| `attach` | object, nullable | `null` when `java` is `null`; see [Attach](#attach-object) |
| `sampled_at` | Unix ms | |

#### Attach object

Used in `/status` and `/vm`. Tells whether milestone-2/3 features (thread dump, histogram,
`VM.flags`, live heap, profiles) can work for this JVM.

| Field | Type | Description |
| --- | --- | --- |
| `supported` | bool, nullable | attach mechanism enabled (`sun.rt.jvmCapabilities`, first flag); `null` without hsperfdata |
| `listener_running` | bool | the attach socket `/tmp/.java_pid<container_pid>` exists, no signal needed |
| `available` | bool | the bridge would attach: `supported`, and either the listener runs or it may be started safely |
| `reason` | enum, nullable | why not available, `null` if available: `disabled` (`-XX:+DisableAttachMechanism`) · `xrs` (`-Xrs`: the start signal `SIGQUIT` would **kill** the server, and the listener does not run yet) · `jvm_starting` (JVM younger than 30 s, listener not running; temporary) · `unknown` (no hsperfdata, cannot be checked safely) |

#### Attach commands

Endpoints that need the JVM (`/threads`, `/histogram`, `flags` in `/vm`, `/metrics?live=1`) attach
through the socket `/tmp/.java_pid<container_pid>` of the container, from the host; nothing is started
inside the container. If the listener does not run yet and `available` is `true`, the bridge starts it
on the first such request: it creates `/tmp/.attach_pid<container_pid>`, sends `SIGQUIT` to the JVM,
waits up to 5 s for the socket and removes the trigger file. With `available: false` no signal is
ever sent and the endpoint answers `attach_unavailable` (`details.reason` as above).

The bridge reaches every file in the container (hsperfdata, the attach files, the profiler's `mpa-*`
files) from the host through `/proc/<pid>/root` and follows no symlink in any part of the path: a
`/tmp`, `hsperfdata_<user>` directory or file that is a symlink counts as missing (e.g. a `/tmp`
symlinked elsewhere gives `hsperf_unavailable`).

The JVM's PID is looked up once per request, and the JVM can end in between and its PID go to another
process, e.g. one of the host. So the bridge remembers the JVM's start time (field 22 of
`/proc/<pid>/stat`) and checks it before every step: `SIGQUIT` goes through a pidfd opened before the
check (`pidfd_open`, `pidfd_send_signal`; on kernels without pidfds the check is followed by `kill`),
and the container's files through a descriptor of `/proc/<pid>` on which the start time was checked.
If the start time differs, nothing is signalled or touched and the request answers `no_java_process`.

| Command | Used by | Timeout (incl. listener start) |
| --- | --- | --- |
| `Thread.print -l` | `/threads` | 15 s |
| `GC.class_histogram -all` | `/histogram` | 90 s |
| `VM.flags` | `/vm` | 5 s |
| `GC.heap_info`, `VM.metaspace` | `/metrics?live=1` | 5 s each |

The output of one command is limited to 32 MiB (`output_too_large`). Commands to the same JVM run
one after another.

### `GET /vm`

JVM profile from hsperfdata plus Docker limits. No attach in milestone 1; with feature `vm_flags`
the bridge additionally runs `VM.flags` via attach (fails soft: `flags` is `null`, `flags_error` set).

Examples: [`vm.json`](examples/vm.json) (with `flags`, milestone 2),
[`vm-without-flags.json`](examples/vm-without-flags.json) (JVM 12 s old, listener not running:
`flags_error: attach_unavailable`; a milestone-1 bridge sends `not_implemented` instead).
Errors: `server_not_found`, `server_not_running`, `no_java_process`, `hsperf_unavailable`, `docker_unavailable`.
A container in state `paused` (or any state other than `running`) always gets `409 server_not_running`
with `details.container_state`, on `/vm`, `/metrics`, `/threads`, `/histogram` and `/profiles`.

| Field | Type | Description |
| --- | --- | --- |
| `sampled_at` | Unix ms | |
| `java.version` | string, nullable | `java.version`, e.g. `25.0.4.1` |
| `java.feature_version` | integer, nullable | `25` (`1.8.0_392` → `8`), for the Java/Minecraft version rule |
| `java.vm_name` / `vm_vendor` / `vm_version` / `vm_info` | string, nullable | e.g. `OpenJDK 64-Bit Server VM`, `Eclipse Adoptium`, `25.0.4.1+1-LTS`, `mixed mode, sharing` |
| `java.home` | string, nullable | e.g. `/opt/java/openjdk` |
| `java.internal_version` | string, nullable | `sun.rt.internalVersion` |
| `launch.vm_args` | string, nullable | **raw** `java.rt.vmArgs`, all JVM arguments joined by spaces, e.g. `-Xms128M -Xmx1907M` (can contain container paths) |
| `launch.vm_flags` | string, nullable | `java.rt.vmFlags` (flags file), usually `""` |
| `launch.java_command` | string, nullable | `sun.rt.javaCommand`, main class/jar and program arguments, e.g. `server.jar` |
| `launch.class_path` | string, nullable | `java.class.path` |
| `launch.started_at` | Unix ms, nullable | JVM start |
| `launch.uptime_ms` | integer, nullable | |
| `memory_settings.xms_bytes` | integer, nullable | from `-Xms` / `-XX:InitialHeapSize`, `null` if not set |
| `memory_settings.xmx_bytes` | integer, nullable | from `-Xmx` / `-XX:MaxHeapSize`, `null` if not set (then the JVM takes `MaxRAMPercentage`, default 25 %, of the container limit) |
| `memory_settings.xss_bytes` | integer, nullable | `-Xss` / `-XX:ThreadStackSize` |
| `memory_settings.max_metaspace_bytes` | integer, nullable | `-XX:MaxMetaspaceSize` |
| `memory_settings.max_ram_percentage` | number, nullable | `-XX:MaxRAMPercentage`, `null` if not set |
| `memory_settings.initial_ram_percentage` | number, nullable | `-XX:InitialRAMPercentage` |
| `memory_settings.explicit_gc` | enum, nullable | GC selected by a flag (`-XX:+UseG1GC` …): `serial` · `parallel` · `g1` · `zgc` · `shenandoah`; `null` = chosen by the JVM |
| `memory_settings.xrs` | bool | `-Xrs` / `-XX:+ReduceSignalUsage` set |
| `memory_settings.disable_attach` | bool | `-XX:+DisableAttachMechanism` set |
| `gc.type` | enum | `serial` · `parallel` · `g1` · `zgc` · `shenandoah` · `unknown`, derived from the collector names (fallback `sun.gc.policy.name`) |
| `gc.selected_by` | enum | `flag` (explicit) · `ergonomics` (the JVM chose it: Serial when it sees < 2 CPUs or < 1792 MiB of memory) |
| `gc.policy` | string, nullable | `sun.gc.policy.name`, e.g. `Copy:MSC`; missing with ZGC/Shenandoah |
| `gc.collectors[]` | array | see [Collector](#collector-object) (without counters) |
| `heap_max_bytes` | integer | maximum heap, same as `heap.max_bytes` in `/metrics` |
| `metaspace` | object, nullable | `used_bytes`, `committed_bytes`, `max_bytes`, `source` (`after_gc`), same as in `/metrics`, for the rule "Xmx + metaspace + overhead > limit" |
| `attach` | object | see [Attach](#attach-object) |
| `container.memory_limit_bytes` | integer, nullable | Docker `HostConfig.Memory` (hard limit incl. the Wings overhead), `null` = unlimited |
| `container.memory_reservation_bytes` | integer, nullable | `HostConfig.MemoryReservation` = memory of the server in the panel |
| `container.cpu_limit` | number, nullable | CPUs: `NanoCpus / 1e9` or `CpuQuota / CpuPeriod` (Wings: CPU % / 100), `null` = unlimited |
| `container.cpu_quota_us` / `cpu_period_us` | integer, nullable | raw Docker values |
| `container.cpuset_cpus` | string, nullable | `HostConfig.CpusetCpus`, e.g. `0-3` |
| `container.cpuset_count` | integer, nullable | number of CPUs in `cpuset_cpus` |
| `container.host_cpus` | integer | logical CPUs of the node |
| `container.effective_cpus` | number | `min(cpu_limit, cpuset_count, host_cpus)`; what the JVM sees as available processors (rounded up) |
| `container.tmpfs_size_bytes` | integer | size of the container's `/tmp` (Wings `docker.tmpfs_size`, default 100 MB) |
| `flags` | map string → string, nullable | `VM.flags` (milestone 2): flag name → value as printed, booleans as `"true"`/`"false"`, e.g. `"MaxHeapSize": "1999634432"`, `"UseSerialGC": "true"` |
| `flags_error` | error code, nullable | why `flags` is `null`: `not_implemented` · `attach_unavailable` · `attach_failed` · `attach_timeout` · `output_too_large` · `no_java_process` (the JVM ended meanwhile); `null` when `flags` is set |

### `GET /metrics`

Counters from hsperfdata; reading them costs the JVM nothing. Poll every 2–5 s. Counters are
**cumulative since JVM start**; rates (e.g. GC time per minute) are the difference of two samples
divided by the difference of `sampled_at` (or `uptime_ms`). A changed `started_at` means the JVM restarted
(all counters start again at 0).

Examples: [`metrics.json`](examples/metrics.json), [`metrics-live.json`](examples/metrics-live.json) (`?live=1`, milestone 2).
Errors: as `/vm`.

Query parameter `live=1` (milestone 2, feature `live_heap`): additionally runs `GC.heap_info` via
attach (a short safepoint) and fills `live_heap`; on JDKs whose `GC.heap_info` does not print the
metaspace (JDK 25) also `VM.metaspace` for `live_heap.metaspace`. Without `live=1`, `live_heap` and
`live_heap_error` are `null`. hsperfdata reports metaspace 0 until the first GC; right after a start
use `live_heap.metaspace` (e.g. for "Xmx + metaspace + overhead > limit").

**Used values are "after GC".** hsperfdata updates `used` only when a collection runs, so all values
with `source: "after_gc"` are the state after the last GC (eden often shows 0). For the old generation
this is exactly the value the diagnosis needs. Live values only come from `live_heap` (`source: "live"`).

**Fill levels must be computed against `max_bytes`, not `committed_bytes`:** the JVM grows a
generation only when needed (fixture: old gen 99.98 % of the committed 512356352 bytes, but only 38 % of the max 1333788672 bytes = 1272 MiB).

| Field | Type | Description |
| --- | --- | --- |
| `sampled_at` | Unix ms | |
| `started_at` | Unix ms, nullable | JVM start (`sun.rt.createVmBeginTime`), same as `java.started_at` in `/status`; changes on every JVM restart |
| `uptime_ms` | integer, nullable | JVM uptime |
| `heap.used_bytes` | integer | sum of all spaces |
| `heap.committed_bytes` | integer | sum of the generation capacities |
| `heap.max_bytes` | integer | **Serial/Parallel: sum** of the generation maxima; **G1/ZGC/Shenandoah/unknown: maximum** (each generation reports the whole heap there). May differ from Xmx by alignment (fixture: 1908 MiB for `-Xmx1907M`) |
| `heap.source` | enum | `after_gc` |
| `generations[]` | array | in index order |
| `generations[].index` | integer | hsperfdata index |
| `generations[].name` | string | raw name: `new`, `old` (Serial/Parallel), `young`, `old` (G1, ZGC), `Young`, `Heap` (Shenandoah) |
| `generations[].kind` | enum | `young` · `old` · `heap` · `unknown` |
| `generations[].used_bytes` / `committed_bytes` / `max_bytes` | integer | used = sum of its spaces |
| `generations[].source` | enum | `after_gc` |
| `generations[].spaces[]` | array | `index`, `name` (`eden`, `s0`, `s1`, `old`, `space`, `Heap`), `used_bytes`, `committed_bytes`, `max_bytes`, `source` |
| `metaspace` | object, nullable | `used_bytes`, `committed_bytes`, `max_bytes`, `source`; 0 until the first GC |
| `class_space` | object, nullable | compressed class space, same fields; `null` without compressed class pointers |
| `gc.type` | enum | as in `/vm` |
| `gc.count` | integer | collections of all collectors |
| `gc.time_ms` | number | time of all collectors |
| `gc.last_cause` | string, nullable | e.g. `Allocation Failure`, `G1 Evacuation Pause` |
| `gc.current_cause` | string, nullable | `No GC` unless a collection runs right now |
| `gc.collectors[]` | array | see [Collector](#collector-object) |
| `threads.live` / `daemon` / `peak` / `started` | integer | `java.threads.*` (`started` cumulative) |
| `classes.loaded` / `unloaded` | integer | `java.cls.loadedClasses` / `unloadedClasses` (cumulative, without CDS classes) |
| `classes.shared_loaded` / `shared_unloaded` | integer | the same for CDS (shared) classes |
| `classes.current` | integer | currently loaded: `loaded + shared_loaded - unloaded - shared_unloaded` |
| `safepoints.count` | integer | cumulative |
| `safepoints.time_ms` | number | total time in safepoints (all threads stopped) |
| `safepoints.sync_time_ms` | number | time to reach the safepoints |
| `jit.compiles` / `time_ms` / `threads` | integer / number / integer | JIT compiler |
| `live_heap` | object, nullable | milestone 2, only with `?live=1` |
| `live_heap.sampled_at` | Unix ms | |
| `live_heap.source` | enum | `live` |
| `live_heap.used_bytes` | integer | current heap usage from `GC.heap_info` |
| `live_heap.committed_bytes` | integer, nullable | |
| `live_heap.max_bytes` | integer, nullable | only where `GC.heap_info` prints it (G1 reserved, ZGC, Shenandoah) |
| `live_heap.metaspace` | object, nullable | current metaspace: `used_bytes`, `committed_bytes`, `reserved_bytes` (address space, **not** a limit; the limit is `MaxMetaspaceSize`), `source` (`live`); `null` if neither `GC.heap_info` nor `VM.metaspace` delivered it |
| `live_heap.raw` | string | the text of `GC.heap_info` |
| `live_heap_error` | error code, nullable | with `?live=1` and `live_heap` `null`: `not_implemented` · `attach_unavailable` · `attach_failed` · `attach_timeout` · `output_too_large` · `no_java_process` (the JVM ended meanwhile) · `parse_failed` (unknown `GC.heap_info` format) |

#### Collector object

| Field | Type | Description |
| --- | --- | --- |
| `index` | integer | hsperfdata index. **Indices can have gaps** (ZGC: `0` and `2`); never use them as array positions |
| `name` | string | raw name, e.g. `Serial young collection pauses`, `G1 concurrent cycle pauses`, `ZGC major collection pauses`, `Shenandoah full`; older JDKs: `Copy`, `MSC`, `PSScavenge`, `G1 incremental collections` |
| `kind` | enum | `young` · `old` (ZGC major) · `full` · `concurrent` · `unknown` |
| `time_is_pause` | bool, nullable | `true` if `time_ms` measures stop-the-world pauses, `false` if it measures concurrent cycles, `null` if unknown (Shenandoah). For "GC share of runtime" sum only collectors with `true` |
| `count` | integer | `/metrics` only: invocations, cumulative |
| `time_ms` | number | `/metrics` only: cumulative time |
| `last_started_uptime_ms` | number, nullable | `/metrics` only: start of the last collection, ms since JVM start; `null` if `count == 0` |
| `last_started_at` | Unix ms, nullable | `/metrics` only: `java.started_at + last_started_uptime_ms` (approximate, the JVM clock starts with the VM) |
| `last_duration_ms` | number, nullable | `/metrics` only: duration of the last collection |

### `GET /threads`

Milestone 2. `Thread.print -l` via attach, parsed. Causes a short safepoint.

Example: [`threads.json`](examples/threads.json) (from `testdata/mpa-threads.txt`).
Errors: as `/vm` plus `attach_unavailable`, `attach_failed`, `attach_timeout`, `output_too_large`, `not_implemented`.
`sampled_at` is the start of the command, `duration_ms` its runtime (both endpoints).

| Field | Type | Description |
| --- | --- | --- |
| `sampled_at` | Unix ms | |
| `duration_ms` | number | time of the attach command |
| `dumped_at_local` | string, nullable | timestamp printed by the JVM (`2026-10-25 13:09:43`, container time zone) |
| `vm` | string, nullable | e.g. `OpenJDK 64-Bit Server VM (25.0.4.1+1-LTS mixed mode, sharing)` |
| `summary.total` / `java` / `vm` / `daemon` | integer | thread counts (`vm` = VM-internal threads without Java id) |
| `summary.by_state` | map state → integer | all six states always present: `NEW`, `RUNNABLE`, `BLOCKED`, `WAITING`, `TIMED_WAITING`, `TERMINATED` (Java threads only) |
| `threads[]` | array | in dump order |
| `threads[].name` | string | e.g. `Server thread` |
| `threads[].java_id` | integer, nullable | `#30`; `null` for VM threads |
| `threads[].os_id` | integer, nullable | native thread id (`nid`), matches `tid` in profiles |
| `threads[].vm_thread` | bool | VM-internal thread (`VM Thread`, …): no Java id, no state, no stack |
| `threads[].daemon` | bool | |
| `threads[].priority` / `os_priority` | integer, nullable | |
| `threads[].cpu_ms` | number, nullable | CPU time of the thread |
| `threads[].elapsed_ms` | number, nullable | lifetime of the thread |
| `threads[].state` | enum, nullable | `java.lang.Thread.State` line (**authoritative**): `NEW` · `RUNNABLE` · `BLOCKED` · `WAITING` · `TIMED_WAITING` · `TERMINATED`; `null` for VM threads |
| `threads[].state_detail` | string, nullable | text in brackets: `parking`, `sleeping`, `on object monitor` |
| `threads[].os_state` | string, nullable | text of the header line (`runnable`, `waiting on condition`, `in Object.wait()`, `waiting for monitor entry`); can contradict `state`, ignore it for filtering |
| `threads[].stack[]` | array | top frame first |
| `threads[].stack[].method` | string | `net.minecraft.server.MinecraftServer.waitForTasks` (dots, as printed) |
| `threads[].stack[].module` | string, nullable | `java.base@25.0.4.1` for JDK frames |
| `threads[].stack[].file` / `line` | string / integer, nullable | `MinecraftServer.java`, `870` |
| `threads[].stack[].native` | bool | `Native Method` |
| `threads[].stack[].locks[]` | array | lock lines below the frame, see [Lock](#lock-object) |
| `threads[].locked_synchronizers[]` | array | `Locked ownable synchronizers`, see [Lock](#lock-object) (`type` = `owned`) |
| `threads[].info[]` | string[] | other lines, e.g. `No compile task` |
| `deadlocks[]` | array | one entry per `Found one Java-level deadlock` |
| `deadlocks[].threads[]` | array | `name`, `waiting_for` (string, nullable, e.g. `waiting to lock monitor 0x… (object 0x…, a java.lang.Object)`), `held_by` (thread name, nullable) |
| `jni_refs` | object, nullable | `global`, `weak` |
| `raw` | string | complete dump text, for "copy" |

#### Lock object

| Field | Type | Description |
| --- | --- | --- |
| `type` | enum | `waiting_on` · `locked` · `parking` · `waiting_to_lock` · `waiting_to_relock` · `eliminated` · `owned` |
| `address` | string, nullable | `0x00000000b82a7ff0` |
| `class` | string, nullable | `java.lang.String` |

### `GET /histogram`

Milestone 2. `GC.class_histogram -all` via attach. **Walks the whole heap and pauses the server**
(fixture: 150 MB heap); the panel must warn first. `-all` counts unreachable objects too.

Example: [`histogram.json`](examples/histogram.json) (`?limit=100`, from the fixture).
Errors: as `/threads`.

Query parameter `limit` (integer 1–100000, default 2000): number of classes returned, largest
`bytes` first. The rest is summed in `omitted`.

| Field | Type | Description |
| --- | --- | --- |
| `sampled_at` | Unix ms | |
| `duration_ms` | number | time of the command, upper bound of the pause |
| `all` | bool | `true` (`-all`) |
| `limit` | integer | effective limit |
| `total.classes` / `instances` / `bytes` | integer | all classes (fixture: 16670 classes, 3594114 instances, 150843200 bytes) |
| `omitted.classes` / `instances` / `bytes` | integer | classes beyond `limit` |
| `classes[]` | array | sorted by `bytes` descending |
| `classes[].rank` | integer | rank printed by the JVM |
| `classes[].name` | string | JVM name as printed: `[B`, `[Ljava.lang.Object;`, `[[Lnet.minecraft…StateHolder;`, `net.minecraft…BlockState` |
| `classes[].element_type` | string | for arrays the element type (`byte`, `java.lang.Object`, `net.minecraft…StateHolder`), else `name`; **use this for the mod mapping** |
| `classes[].array_depth` | integer | `0` for non-arrays |
| `classes[].module` | string, nullable | `java.base@25.0.4.1`, only for JDK classes |
| `classes[].instances` / `bytes` | integer | |

### Profiles

Milestone 3. async-profiler, loaded into the JVM via attach. A profile runs in the background:
start it, poll its state (every 1–2 s), fetch the result. One running profile per server,
`max_concurrent_profiles` per node.

Examples: [`profile-created.json`](examples/profile-created.json) (response of `POST`),
[`profile-running.json`](examples/profile-running.json), [`profile-done.json`](examples/profile-done.json),
[`profile-done-no-samples.json`](examples/profile-done-no-samples.json) (CPU mode on an idle/paused server),
[`profile-failed.json`](examples/profile-failed.json), [`profiles.json`](examples/profiles.json) (list),
[`profiles-empty.json`](examples/profiles-empty.json) (list without profiles).
They are recorded from the real handlers and the profile manager with a fake attach socket. The
stacks in `profile-done.json` are **constructed** from the thread dump fixture, the format is final.
Real results from servers with mods (cut down to the `Server thread`) are in `testdata/profile-*.json`.

How a profile runs (nothing of the request except the checked numbers and the mode enum reaches the JVM):

- The bridge copies the embedded `libasyncProfiler.so` (async-profiler 4.1, 0.55 MB) to
  `/tmp/mpa-libasyncProfiler.so` in the container, unless the JVM has it loaded already, and loads it with the
  attach operation `load`. The library stays there while the JVM runs and is reused.
- Free space needed in the container's `/tmp`: 8 MiB plus the library if it is not there yet
  (`insufficient_tmp_space` otherwise).
- Profiler messages go to `/tmp/mpa-<id>.log`, the profile to `/tmp/mpa-<id>.collapsed`, never to the
  server console. Both are deleted after every run, on `DELETE`, and when the bridge starts.
- Profiles live in the memory of the bridge. A restart of the bridge loses them; on start it stops a
  profiler that is still active and removes leftover `mpa-*` files (never the library).
- The bridge keeps at most **2 profiles per server**, running and finished together. A finished
  profile is kept 15 minutes (`expires_at`) unless a new one starts earlier: then the oldest finished
  or failed ones are dropped (`404 profile_not_found`) until there is room, so the newest finished
  one survives the next start. A running profile is never dropped. The panel stores results itself.

#### `POST /profiles`

Body (JSON, unknown fields are rejected):

| Field | Type | Description |
| --- | --- | --- |
| `seconds` | integer, required | 1 – `max_profile_seconds` (default 120); the panel offers 10/30/60 |
| `mode` | enum, optional | `wall` (default, tick analysis: every thread is sampled whether it runs or waits) · `cpu` (`itimer`, CPU time only) · `alloc` (allocations) |
| `interval_ms` | integer, optional | `wall`: 1–1000, default 50; `cpu`: 1–1000, default 10; must be absent or `null` for `alloc` (fixed 512 KiB sampling interval) |

Response `202 Accepted` with the profile object (`state` `running`) and header
`Location: /api/v1/servers/{uuid}/profiles/{id}`. The profiler is already running when the response
arrives; failures to start are returned right away as errors.

Errors: `invalid_request` (400), `profile_running` (409, `details.profile_id`), `too_many_profiles`
(429, `details.limit`), `attach_unavailable`, `attach_failed` (also when the agent refuses to start,
`details.code` = its return code), `attach_timeout`, `insufficient_tmp_space` (507,
`details.free_bytes`, `details.required_bytes`), `internal_error` (local file problems), plus the
server errors of `/vm`. Starting (listener, library, `load`) has a timeout of 15 s.

#### `GET /profiles`

`{"profiles": [ … ]}`: all profiles of the server the bridge still knows (running and unexpired
finished ones), newest first, **without** `result` (always `null` here). Works for stopped servers too;
`404 server_not_found` if the container does not exist.

#### `GET /profiles/{id}`

`{id}` is 16 lowercase hex characters. The profile object; `404 profile_not_found` if unknown,
deleted or expired.

#### `DELETE /profiles/{id}`

Running: stops the profiler and removes its files from the container's `/tmp`. Finished: discards the
result. `204 No Content` on success, `404 profile_not_found` otherwise.

#### Profile object

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | 16 hex characters |
| `server_uuid` | string | |
| `mode` | enum | `wall` · `cpu` · `alloc` |
| `seconds` | integer | requested duration |
| `interval_ms` | integer, nullable | sampling interval (`wall`, `cpu`); `null` for `alloc` |
| `alloc_interval_bytes` | integer, nullable | `alloc` only (524288) |
| `state` | enum | `running` → `processing` (stopping, parsing; usually < 1 s) → `done` or `failed` |
| `started_at` | Unix ms | |
| `ends_at` | Unix ms | planned end (`started_at + seconds`) |
| `finished_at` | Unix ms, nullable | when `done`/`failed` was reached |
| `elapsed_ms` | integer | since `started_at` (until `finished_at`) |
| `progress` | number | 0–1 (`elapsed_ms / (seconds·1000)`, capped at 1) |
| `expires_at` | Unix ms, nullable | finished profiles are kept 15 minutes, then 404 (earlier if dropped by a newer one, see [Profiles](#profiles)); `null` while running. The panel stores results itself |
| `error` | error object, nullable | `failed` only: same shape as [error responses](#errors): `server_not_running` (`details.container_state`, the server stopped), `no_java_process` (the JVM ended, container still running), `attach_failed`, `attach_timeout`, `output_too_large`, `internal_error` (a bug of the bridge, see its log) |
| `result` | object, nullable | `done` only, see below |

#### Profile result

| Field | Type | Description |
| --- | --- | --- |
| `duration_ms` | number | actual profiling time |
| `unit` | enum | `samples` (`wall`, `cpu`: **time = `count` × `interval_ms`**) · `bytes` (`alloc`: `count` = allocated bytes) |
| `interval_ms` | integer, nullable | same as in the profile object, repeated so the result is self-contained |
| `total` | integer | sum of `count` over all threads |
| `warnings[]` | enum[] | `no_samples` (nothing recorded; with `cpu` typical for an idle or paused server, e.g. `pause-when-empty-seconds`) · `server_thread_missing` (no thread named `Server thread`) · `truncated` (the collapsed output exceeded 32 MiB, the rest was dropped) |
| `threads[]` | array | **one entry per thread**, sorted by `count` descending |
| `threads[].name` | string | thread name as async-profiler records it, e.g. `Server thread`; JVM-internal threads are cut to 15 characters (`C1 CompilerThre`); `""` if the profiler recorded no thread |
| `threads[].tid` | integer, nullable | native thread id, matches `os_id` in `/threads` |
| `threads[].count` | integer | samples (or bytes) of this thread |
| `threads[].stacks[]` | array | sorted by `count` descending |
| `threads[].stacks[].stack` | string | collapsed stack **root first**, frames separated by `;`, without the thread frame. Java frames in JVM form with slashes (`net/minecraft/server/MinecraftServer.waitUntilNextTick`, lambdas `…$$Lambda.0x….run`), native and JVM frames as async-profiler names them (`Parker::park(bool, long)`, `__pthread_cond_timedwait`, `epoll_wait`, or the library path such as `/usr/lib/x86_64-linux-gnu/libc.so.6`). With `alloc` the last frame is the allocated class with an annotation, e.g. `byte[]_[i]`. `""` for a thread sampled without frames |
| `threads[].stacks[].count` | integer | |
| `gc.count` / `time_ms` | integer / number | collections and their time during the profile (delta of the counters) |
| `gc.collectors[]` | array | [Collector](#collector-object) `index`, `name`, `kind`, `time_is_pause`, `count`, `time_ms` (deltas), all collectors |
| `gc.pauses[]` | array | individual collections observed by polling hsperfdata: `collector_index`, `kind`, `start_offset_ms` (from `started_at`), `duration_ms` |
| `gc.pauses_complete` | bool | `false` if collections happened faster than the bridge could observe them; `gc.count` is exact anyway |
| `safepoints.count` / `time_ms` | integer / number | safepoints during the profile |
| `cpu.process_ms` | number | CPU time of the Java process during the profile (`/proc`, 10 ms resolution) |
| `cpu.process_percent` | number | `process_ms / duration_ms · 100` (one core = 100) |

Idle detection is up to the panel: with `wall` the `Server thread` is sampled every interval;
frames below `MinecraftServer.waitUntilNextTick` / `waitForTasks` are idle time between ticks.
The method names depend on the mappings, see below.

While a profile runs, the [tick monitor](#get-ticks) of that server pauses (`suspended`): async-profiler
has one session per JVM. The profile stops it before starting; it resumes on its own afterwards.

### Tick and idle methods

The names of the tick and idle methods of the `Server thread` in each mapping the server can run with.
The panel uses them for the idle time of a profile, the [tick monitor](#get-ticks) for its load and the
method it counts; both keep the same table. Checked against Mojang's server mappings, Fabric's
intermediary and MCPConfig (2026-10-25), and against the profiles in `testdata/`.

A stack (root first) is looked at from the root: the **first** frame of a class ending in
`/MinecraftServer` that is a tick or idle method decides. Idle method first: the thread waits between
two ticks. Tick method first: tick work, including waiting inside the tick (e.g. a mod waits for a chunk
in `BlockableEventLoop.waitForTasks`), which is lag. Neither: tick work (e.g. `[unknown_Java]`, stacks
the profiler couldn't walk).

| Mapping | Runs on | Tick: `tickServer`, `tickChildren` | Idle: `waitUntilNextTick`, `waitForTasks` |
| --- | --- | --- | --- |
| `mojmap` | vanilla from 26.1, Paper from 1.20.5, NeoForge from 1.20.2, Forge from 1.20.6 | `tickServer`, `tickChildren` | `waitUntilNextTick`, `waitForTasks` |
| `intermediary` | Fabric, Quilt (same names in every version, class names `net/minecraft/class_…` except `MinecraftServer`) | `method_3748`, `method_3813` | `method_16208`, `method_20813` |
| `srg` | Forge 1.17 – 1.20, NeoForge 1.20.1 (classes in Mojmap, methods in SRG) | `m_5705_`, `m_5703_` | `m_130012_`, `m_5667_` |
| `mcp` | Forge 1.13 – 1.16 (MCP class names, SRG ids stay the same across versions) | `func_71217_p`, `func_71190_q` | `func_213202_o`, `func_223705_bi` |

- **Up to 1.13** there is no `waitUntilNextTick`: `MinecraftServer.run` (not obfuscated) calls
  `java/lang/Thread.sleep` itself. A stack with `MinecraftServer.run` directly followed by
  `java/lang/Thread.sleep` is idle; it counts as `mcp`.
- **Not covered:** obfuscated vanilla before 26.1 and Spigot/Paper before 1.20.5 (method names change
  with every version). No idle detection there: the panel shows no tick load, the tick monitor answers
  `unsupported` (`unknown_mapping`).
- `waitForTasks` is a method of `BlockableEventLoop` (`class_1255` in intermediary,
  `ThreadTaskExecutor` in mcp); `MinecraftServer` overrides it in newer versions. Only the frame of
  `MinecraftServer` counts for the decision above.

Seen in the profiles (`testdata/`): Fabric without players 520 of 599 samples idle, Fabric with
Immersive Snow 28 resp. 52 of 1200 (the mod waits for chunks inside the tick), the Forge 1.12 modpack
17 of 1196 (`Thread.sleep` in `run`).

### `GET /ticks`

Feature `ticks`. TPS and approximate MSPT of the server for a display under the console, with a
history of 10 minutes. Poll every 5 s while the console is open. Answers within milliseconds; the
measuring runs in the background.

Examples: [`ticks-starting.json`](examples/ticks-starting.json) (first call), [`ticks.json`](examples/ticks.json),
[`ticks-idle.json`](examples/ticks-idle.json), [`ticks-overloaded.json`](examples/ticks-overloaded.json),
[`ticks-suspended.json`](examples/ticks-suspended.json), [`ticks-unsupported.json`](examples/ticks-unsupported.json),
[`ticks-jvm-starting.json`](examples/ticks-jvm-starting.json).
Errors: as `/vm` plus `attach_unavailable` (except `jvm_starting`, which is the state `starting`),
`attach_failed`, `attach_timeout`, `insufficient_tmp_space` (as for profiles, 4 MiB plus the library),
`too_many_tick_monitors`, `not_implemented`. Errors are only returned while the monitor starts; later
failures show up as `state: "error"` and the monitor tries again in the next window.

**Monitor.** The first call starts a tick monitor for the server (attach listener as for the other
attach endpoints, then async-profiler as for profiles). It ends when no call came for
`idle_timeout_ms` (60 s, the console was closed), when the JVM ends, after an internal error of the
bridge (logged; the next call starts a new one), or with the bridge. A new JVM
(other PID or start time) starts a new monitor with an empty history. At most `max_tick_monitors`
monitors run on a node (`mpa-bridge.yml`, default 10); they don't count against `max_concurrent_profiles`.
Files in the container's `/tmp`: `mpa-ticks-<id>.collapsed` and `.log` for a few milliseconds per
window, removed right away and when the monitor ends.

**How it measures** (since 0.3.0; checked against a JVM with a tick loop like Minecraft's: 10, 30 and
70 ms of work per tick were measured as 10.0 ms, 30.0 ms and 14.2 TPS):

- `sampling` (the first window of a monitor): async-profiler samples only the `Server thread` (wall
  clock, every `interval_ms` = 20 ms, about 250 samples per window). The samples tell the `mapping`.
  Samples in an idle method (see [Tick and idle methods](#tick-and-idle-methods)) are idle time, the
  rest is tick work. `load_percent` is the share of tick work; at 20 TPS
  `tick_time_ms = load × 50 ms` and `tps` is **assumed** to be `target_tps` (20; `/tick rate` is not
  taken into account). At 95 % load and more the samples can't tell the TPS: the window is
  `overloaded` with `tps` and `tick_time_ms` `null`.
- `counting` (every window after the mapping is known, until the monitor ends): async-profiler counts
  the calls of `MinecraftServer.tickServer` (the name of the mapping), `tps` is exact (above 20 while
  the server catches up after a lag). The time comes from the kernel: the `Server thread` is busy
  while it runs on a CPU or waits for one (`/proc/<pid>/task/<tid>/schedstat`, the wait includes the
  container's CPU limit), plus the JVM's safepoints (`sun.rt.safepointTime`, e.g. GC pauses) in the
  share the thread was busy in the rest of the window. `load_percent` = busy / duration. While the
  server keeps up, `tick_time_ms = busy / ticks`; time the thread is blocked inside a tick (locks,
  waiting for chunks) is missing there. When it falls behind (`tps` < 19.5) there is no idle time:
  `tick_time_ms = duration / ticks`. Without `schedstat` (kernel without `CONFIG_SCHED_INFO`) a window
  that keeps up has `tps` only. Counting instruments the class `MinecraftServer` (a short safepoint),
  once per session: when the monitor starts and after a profile.
- Stays `sampling` when the tick frames aren't of `net/minecraft/server/MinecraftServer` (counting
  instruments that class); then an `overloaded` window has no TPS.

| Field | Type | Description |
| --- | --- | --- |
| `sampled_at` | Unix ms | |
| `state` | enum | `starting` · `ok` · `idle` · `suspended` · `unsupported` · `error`, see below |
| `reason` | enum, nullable | `starting`: `first_window` (no window measured yet) or `jvm_starting` (JVM younger than 30 s, listener not running; no monitor started); `suspended`: `profile_running`; `unsupported`: `no_server_thread` or `unknown_mapping`; else `null` |
| `method` | enum, nullable | `sampling` · `counting`: what the monitor measures right now; `null` while it doesn't (suspended, unsupported, error) |
| `mapping` | enum, nullable | mapping of the tick loop as recognised in the samples: `mojmap` · `intermediary` · `srg` · `mcp`; `null` until known |
| `target_tps` | integer | `20` |
| `window_ms` | integer | `5000` |
| `interval_ms` | integer | sampling interval, `20` |
| `idle_timeout_ms` | integer | `60000` |
| `monitor_started_at` | Unix ms, nullable | start of this monitor (= of its history); `null` for `jvm_starting` |
| `current` | window, nullable | the last entry of `history`, `null` without one |
| `history[]` | array | windows, oldest first, at most 120 (10 minutes). Gaps in time are windows that measured nothing (e.g. right after the start or a profile) |
| `error` | error object, nullable | `state: "error"` only: why the last window failed (`attach_failed`, `attach_timeout`, `insufficient_tmp_space`, `internal_error`) |

`state`:

| State | Meaning | What to show |
| --- | --- | --- |
| `starting` | monitor or JVM just started | nothing yet, poll again |
| `ok` | `current` has values (or is `overloaded`) | TPS/MSPT |
| `idle` | the Server thread had hardly anything to do: `load_percent` < 0.5 %, e.g. nobody online, or paused by `pause-when-empty-seconds` (the bridge can't tell which). `counting` windows keep `tps` (`tickServer` still runs; `null` with 0 ticks) and `tick_time_ms`. Called `paused` up to 0.3.0: read `paused` of older bridges as `idle` | "idle", with the numbers if there are any |
| `suspended` | a profile of this server runs; the monitor resumes after it | a gap |
| `unsupported` | `no_server_thread` (a proxy such as Velocity, or the server is still starting) or `unknown_mapping` (samples without a known tick or idle method: obfuscated versions, or the world is still loading). Not final, the monitor keeps looking | nothing |
| `error` | the last window failed, see `error` | nothing / last values |

Window (`current`, `history[]`):

| Field | Type | Description |
| --- | --- | --- |
| `started_at` / `ended_at` | Unix ms | the measured span |
| `duration_ms` | number | `ended_at - started_at`, about 5000 |
| `state` | enum | `ok` · `idle` · `suspended` · `unsupported` |
| `method` | enum, nullable | `sampling` · `counting`; `null` for `suspended` |
| `tps` | number, nullable | ticks per second (2 decimals); `null` when unknown (`overloaded` or `idle` sampling, `idle` counting without ticks, `unsupported`, `suspended`) |
| `tick_time_ms` | number, nullable | average duration of a tick (MSPT, 3 decimals); `null` when unknown (also `counting` without `schedstat` while the server keeps up) |
| `load_percent` | number, nullable | how busy the Server thread was, 0–100. `sampling`: share of tick work in the samples; `counting`: busy time (see above) / duration, `null` without `schedstat` |
| `overloaded` | bool | no idle time left in the tick loop (`sampling`: load ≥ 95 %; `counting`: `tps` < 19.5) |
| `samples` | integer, nullable | `sampling`: samples of the Server thread |
| `ticks` | integer, nullable | `counting`: calls of `tickServer` in the window |

## Errors

Every error has this body (example: [`error-server-not-running.json`](examples/error-server-not-running.json)):

```json
{"error": "server_not_running", "message": "The server container is not running.", "details": {"container_state": "exited"}}
```

| Field | Type | Description |
| --- | --- | --- |
| `error` | string | code from the table below; **clients switch on this** |
| `message` | string | English text for logs, may change |
| `details` | object, nullable | code-specific fields listed below, else `null` |

| HTTP | `error` | When | `details` |
| --- | --- | --- | --- |
| 400 | `invalid_request` | malformed UUID or profile id, bad query parameter, invalid JSON body or field | `field`, `reason` |
| 401 | `unauthorized` | `Authorization` header missing, not `Bearer`, or wrong token | – |
| 403 | `ip_not_allowed` | client IP not in `allowed_ips`; without `User-Agent` | – |
| 404 | `not_found` | unknown path | – |
| 405 | `method_not_allowed` | known path, wrong method (`Allow` header set) | – |
| 404 | `server_not_found` | no container with this name, or not a Pelican server container (labels) | – |
| 409 | `server_not_running` | container not in state `running` (also `paused`) | `container_state` |
| 409 | `no_java_process` | container runs, but no `java` process (or it ended during the request) | – |
| 409 | `hsperf_unavailable` | Java runs but has no readable hsperfdata (`-XX:-UsePerfData`, `-XX:+PerfDisableSharedMem`, JVM still starting) | – |
| 409 | `attach_unavailable` | attach not possible, see [Attach](#attach-object) | `reason` |
| 502 | `attach_failed` | attach protocol error or the command failed | `code` (result code of the JVM, e.g. `-1`) and `output` (first 300 bytes) if the JVM answered, else `null` |
| 504 | `attach_timeout` | the JVM did not answer in time | – |
| 502 | `output_too_large` | the JVM output exceeds the size limit (32 MiB) | – |
| 502 | `parse_failed` | the JVM answered, but the output has an unknown format (`/histogram`; in `/metrics` only as `live_heap_error`) | – |
| 409 | `profile_running` | a profile is already running for this server | `profile_id` |
| 429 | `too_many_profiles` | `max_concurrent_profiles` reached on this node | `limit` |
| 404 | `profile_not_found` | unknown, deleted or expired profile id | – |
| 429 | `too_many_tick_monitors` | `max_tick_monitors` reached on this node (`GET /ticks` for a server without a monitor) | `limit` |
| 507 | `insufficient_tmp_space` | not enough free space in the container's `/tmp` for the profiler | `free_bytes`, `required_bytes` |
| 501 | `not_implemented` | endpoint of a later milestone, see `features` | – |
| 503 | `docker_unavailable` | Docker API not reachable | – |
| 500 | `internal_error` | anything else | – |

`server_not_found` and `not_found` share 404; distinguish them by `error`. Every error code has an
example `examples/error-<code>.json` (underscores replaced by dashes).

## Examples

Fixed inputs (not from the fixtures): server UUID `1a7ce997-259b-452e-8b4e-cecc464142ca`, host PIDs,
all timestamps, the bridge start, the container limits (Wings: server memory 1907 MB × overhead
1.15 = 2299579597 bytes ≈ 2.142 GiB, CPU limit 100 % = 1 CPU), host CPUs 24.
All examples (except the `error-*` bodies) are recorded from the real HTTP handlers, with Docker,
`/proc`, the attach socket and the clock faked: the attach answers are the fixture outputs
(`Thread.print -l`, `GC.class_histogram -all`), command durations are fixed.
Illustrative (format final, values plausible but not recorded): the `VM.flags`, `GC.heap_info` and
`VM.metaspace` outputs behind `vm.json` and `metrics-live.json`, the stacks and GC/CPU numbers of the
profile examples, the samples, call counts and `schedstat` times behind the `ticks*` examples.
Everything else comes from `testdata/` (Java 25.0.4.1 Temurin, Serial GC, `-Xms128M -Xmx1907M`).

| File | Endpoint / state |
| --- | --- |
| `node-status.json` | `GET /api/v1/status` (all features) |
| `node-status-m1.json` | `GET /api/v1/status` of a milestone-1 bridge |
| `node-status-m2.json` | `GET /api/v1/status` of a milestone-2 bridge |
| `node-status-m3.json` | `GET /api/v1/status` of a bridge with profiles, without the tick monitor (0.1.0) |
| `status.json` | `GET /status`, ready |
| `status-not-running.json` | `GET /status`, container stopped |
| `status-no-java.json` | `GET /status`, no Java process |
| `status-hsperf-unavailable.json` | `GET /status`, Java without hsperfdata |
| `vm.json` | `GET /vm` with `VM.flags` (milestone 2) |
| `vm-without-flags.json` | `GET /vm`, JVM 12 s old, listener not running (`flags_error: attach_unavailable`) |
| `metrics.json` | `GET /metrics` |
| `metrics-live.json` | `GET /metrics?live=1` (milestone 2) |
| `threads.json` | `GET /threads` |
| `histogram.json` | `GET /histogram?limit=100` |
| `profile-created.json` | `POST /profiles` (202) |
| `profile-running.json` | `GET /profiles/{id}`, running |
| `profile-done.json` | `GET /profiles/{id}`, done (`wall`, 10 s) |
| `profile-done-no-samples.json` | `GET /profiles/{id}`, done (`cpu`, nothing sampled) |
| `profile-failed.json` | `GET /profiles/{id}`, failed |
| `profiles.json` | `GET /profiles` |
| `profiles-empty.json` | `GET /profiles`, no profiles |
| `ticks-starting.json` | `GET /ticks`, first call: monitor started, no window yet |
| `ticks.json` | `GET /ticks`, a sampling window, then two counting windows (4–5 ms per tick) |
| `ticks-idle.json` | `GET /ticks`, the Server thread only waits (nobody online): sampling, then counting |
| `ticks-overloaded.json` | `GET /ticks`, no idle time left: the sampling window has no TPS, the counting windows about 14 |
| `ticks-suspended.json` | `GET /ticks` while a profile runs |
| `ticks-unsupported.json` | `GET /ticks` on a proxy (no Server thread) |
| `ticks-jvm-starting.json` | `GET /ticks`, JVM 12 s old, listener not running |
| `error-*.json` | one per error code |
