# SRT Relay Dashboard

A lightweight SRT relay with a built-in web dashboard. It forwards SRT streams
from one port to another (ingress -> egress), so **both the sender and the
receiver can connect as SRT callers** — no streamid required, no inbound NAT
hole-punching, no client configuration beyond the URL.

Built for the scenario where a relay server with a fixed public IP sits between
two NAT'd machines (senders and receivers behind firewalls that only allow
outbound traffic).

![SRT Relay Dashboard](srt-relay-dashboard.png)

## Highlights

- **Per-port relay**: each stream = one ingress/egress port pair. Because the
  mapping is by port, neither OBS side ever needs to send a streamid.
- **No transcoding**: packets are copied as-is (`-c copy` equivalent), so CPU
  cost per stream is tiny. 20+ streams run comfortably on a 2 vCPU VM.
- **Codec detection**: sniffing MPEG-TS PAT/PMT tables, the dashboard shows the
  video/audio codec of every incoming stream (H.264, HEVC, AAC, ...).
- **Live health stats**: bitrate, resent packets, lost packets, jitter (TSBPD
  buffer delay) and RTT, with a color-coded health light per stream.
- **Dynamic stream management**: add/remove streams from the web UI, picking a
  free port from a dropdown. No config-file editing or restarts.
- **Single static binary** for Linux (Go + libsrt), deployed with one file.

## User Guide

This is a step-by-step guide for using the dashboard day-to-day. It assumes the
relay is already running and reachable (see [Quick start](#quick-start)).

### 1. Log in

Open `http://SERVER:3001` in a browser. The default credentials are:

- **username:** `admin`
- **password:** `admin`

Change the password right away (see [Authentication](#authentication)).

### 2. Create a stream

1. On the **Streams** tab, type a **name** for the stream (e.g. `camera1`).
2. Pick a free **ingress port** from the dropdown. Only unassigned ports are
   listed, so you can't accidentally double-book one.
3. Optionally add a **contact** (name/email/phone) — this is shown next to the
   stream everywhere (table, calendar, details, infoscreen) so everyone knows
   who owns it.
4. Click **Add**.

The new stream appears in the list in the **waiting** state (grey light).
Each stream gets an ingress port (sender) and an egress port (receiver),
100 higher by default. The dashboard shows the exact URLs for both.

### 3. Connect the sender (OBS)

A *sender* pushes video **into** the relay. In OBS:

1. Go to **Settings → Stream**.
2. Set **Service** to **Custom...**.
3. **Server:** `srt://SERVER:IN_PORT?mode=caller&streamid=publish:STREAMID`
4. **Stream Key:** leave empty.
5. Click **Start Streaming**.

Example: `srt://91.229.137.199:23001?mode=caller&streamid=publish:camera1`

The stream's light turns **yellow** (ingress connected, no receiver yet) and
the dashboard starts showing codec + bitrate.

> **Tip:** the dashboard displays these complete URLs for every stream — just
> copy the "publish" URL into OBS. No need to type it by hand.

### 4. Connect one or more receivers (OBS)

A *receiver* pulls video **out of** the relay. Multiple receivers can watch the
same stream at once (fan-out) — they each connect independently and the relay
broadcasts to all of them.

In OBS, add a **Media Source** (or VLC source):

1. Add a new **Media Source**.
2. **Input:** `srt://SERVER:OUT_PORT?mode=caller&streamid=read:STREAMID`
3. **Input Format:** `mpegts`.
4. Uncheck "Restart playback when source becomes active" if you don't want it
   to auto-reconnect.

Example: `srt://91.229.137.199:23101?mode=caller&streamid=read:camera1`

Once at least one receiver connects, the stream's light turns **green**
(`relaying` state) and the receiver's OBS shows the video.

> Multiple receivers (two OBS boxes, an OBS box + VLC, etc.) can all connect to
> the same egress port at the same time.

### 5. Read the stream list

Each stream row shows, left to right:

- **Traffic light** — the overall state (see [below](#traffic-lights)).
- **Name** and **state** (`waiting`, `relaying`, `scheduled`).
- **Contact** (if set).
- **Stream ID** and the exact **publish** URL.
- **Codecs** — video/audio detected from the MPEG-TS stream (e.g. H.264, HEVC,
  AAC).
- **Stats** — bitrate, packets resent/lost, jitter, RTT, and a separate
  health color.

#### Traffic lights

| Light | Meaning |
|-------|---------|
| grey | no sender connected yet |
| yellow | sender connected, no receiver yet |
| green | sender + at least one receiver connected |
| red (pulsing) | sender connected but the link is unhealthy (loss / jitter) |

The **health** color (shown near the stats) is a finer-grained signal: green =
healthy, yellow = rising loss/jitter, red = no traffic or heavy loss, grey = no
connection.

### 6. Edit, schedule and remove streams

- **Details** (info icon) — full stream info: ports, URLs, contact, codecs and
  live stats.
- **Schedule** (calendar icon) — set a **start**/**stop** time, a **recurrence**
  (daily/weekly) and **auto-remove**. See [Scheduling](#scheduling).
- **Remove** (✕) — delete the stream. Its port pair is freed immediately.

### 7. Calendar view

The **Calendar** tab shows all streams as a Google-Calendar-style week grid:

- Toggle **Day** / **Week** with the buttons top-left.
- Click a free time slot to create a scheduled stream pre-filled with that
  date/time.
- Click an event to edit it.
- The red **now-line** marks the current time; scheduled and relaying events
  are color-coded.

### 8. Infoscreen (read-only view)

If the relay was started with `--view`, the public page `http://SERVER:3001/view`
shows a read-only, auto-scrolling calendar — ideal for a wall-mounted screen in
a control room. No login required.

## Architecture

```
Sender OBS (SRT caller) ──► srt://server:IN_PORT
                                    │
                          ┌─────────▼──────────┐
                          │   srt-relay-app    │
                          │  (one goroutine    │
                          │   per stream)      │
                          └─────────┬──────────┘
                                    │
Receiver OBS (SRT caller) ◄── srt://server:OUT_PORT
```

- **Sender OBS** connects *out* to the server's ingress port.
- **Receiver OBS** connects *out* to the server's egress port.
- The relay forwards packets between the two sockets while collecting SRT
  statistics and parsing codec metadata.
- The web dashboard (HTTP + WebSocket) shows live status for all streams.

### Port pairing

By default the egress port = ingress port + 100 (`--egress-offset`):

| Stream | Sender connects to | Receiver connects to |
|--------|--------------------|----------------------|
| 1      | `srt://HOST:23001` | `srt://HOST:23101`   |
| 2      | `srt://HOST:23002` | `srt://HOST:23102`   |
| ...    | ...                | ...                  |

## Quick start

### Easiest: download a release binary (Linux)

1. Grab the latest Linux binary + `libsrt.so.1.5.6` from the
   [Releases](https://github.com/markusnygard/srt-relay-dashboard/releases) page.
2. Put both in the same folder on the server and run:

```bash
cd ~
mkdir -p srt-relay && cd srt-relay
wget https://github.com/markusnygard/srt-relay-dashboard/releases/download/v0.1.0/srt-relay-dashboard-linux-amd64
wget https://github.com/markusnygard/srt-relay-dashboard/releases/download/v0.1.0/libsrt.so.1.5.6
chmod +x srt-relay-dashboard-linux-amd64
```

3. Run the relay:

```bash
export LD_LIBRARY_PATH=$(pwd)
./srt-relay-dashboard-linux-amd64 --http 0.0.0.0:3001
```

4. Open `http://SERVER:3001` and sign in with the default `admin` / `admin`
   credentials.

> Note: the binary needs the matching `libsrt` shared library at runtime.
> Keep `libsrt.so.1.5.6` next to it and set `LD_LIBRARY_PATH=$(pwd)` (or run
> the Docker image, which bundles everything).

### Option A: Docker image

```bash
docker build -t srt-relay-dashboard .
docker run --rm -d \
  -p 3001:3001 \
  -p 23001-23100:23001-23100/udp \
  -v $(pwd)/users.json:/users.json \
  srt-relay-dashboard \
  --http 0.0.0.0:3001 --users /users.json
```

### Option B: Standalone binary (recommended for the relay server)

1. Build the static binary (see [BUILD.md](BUILD.md)).
2. Copy `srt-relay-app` and `libsrt.so.1.5.6` to the server.
3. Run:

```bash
export LD_LIBRARY_PATH=$(pwd)
./srt-relay-app \
  --http 0.0.0.0:8080 \
  --port-low 23001 \
  --port-high 23100 \
  --egress-offset 100
```

### Using the dashboard

1. Open `http://SERVER:3001` and sign in (`admin` / `admin`).
2. Type a stream name and pick a free ingress port from the dropdown.
3. The dashboard shows the exact URLs to put in OBS:

   - **Sender OBS** → Settings → Stream → Custom → Server: `srt://SERVER:IN_PORT?mode=caller&streamid=publish:STREAMID` (Stream Key empty)
   - **Receiver OBS** → add Media Source → Input: `srt://SERVER:OUT_PORT?mode=caller&streamid=read:STREAMID` → Input Format: `mpegts`

   The dashboard shows these complete URLs for each stream; just copy them.

4. The stream row turns **relaying** and shows bitrate/codecs/health once both
   sides connect.

## Command-line options

| Flag | Default | Description |
|------|---------|-------------|
| `--http` | `0.0.0.0:3001` | web UI listen address |
| `--host` | `0.0.0.0` | SRT bind host |
| `--latency` | `1000` | SRT latency in ms (raise for lossy links) |
| `--port-low` | `21001` | lowest available ingress port (ignored when `--ports-file` is set) |
| `--port-high` | `21100` | highest available ingress port (ignored when `--ports-file` is set) |
| `--egress-offset` | `100` | egress port = ingress + offset |
| `--ports-file` | (unset) | path to a JSON file listing the allowed ingress ports, e.g. `[23001, 23003, 23005]`. When set, only these ports are offered/valid; the range flags are ignored. |
| `--host-ip` | auto | IP shown to senders/receivers in the web UI (auto-detected if empty; set it if the relay is behind NAT or has multiple NICs) |
| `--users` | `users.json` | path to the users file (created with `admin/admin` on first run) |
| `--streams` | `streams.json` | path to the streams/schedule file (persisted across restarts) |
| `--idle-remove-min` | `0` | remove streams with no publisher after N minutes (0 = disabled) |
| `--view` | off | enable the public read-only view page at `/view` (for infoscreens) |

## Scheduling

Streams can be scheduled with optional **start** and **stop** times, plus a
**recurrence** (`daily` or `weekly`) and an **auto-remove** flag:

- **Start not set** → the stream starts immediately.
- **Stop not set** → the stream only stops manually.
- **Stop set + auto-remove** (one-off) → the stream is removed after it stops.
- **Idle cleanup** (`--idle-remove-min`) → streams with no publisher for N
  minutes are removed (manual/un-scheduled streams only).

Use the **Calendar** tab (week view) to schedule streams — click a free slot to
create one with the date/time prefilled, or click an event to edit it. Free
ports exclude both live and future-scheduled streams, so a port can't be
double-booked.

## Time model

All times are **absolute instants** (RFC3339). The server is authoritative —
it starts/stops streams on its own clock. The browser always displays and
inputs times in the **viewer's local timezone**; on save, the UI converts the
chosen local datetime to the absolute instant. Recurring schedules are anchored
to the **server's local wall clock** (so they follow the server's DST), and the
UI shows their equivalent in your timezone. The server's timezone is exposed in
the config and shown in the dashboard header.

## Contact person

Every stream can carry a free-text **contact** field (name/email/phone) so you
know who owns each stream. It appears in the streams table, the calendar
events, the details popup, and the public view — and in DataMiner metrics.

## Read-only view (infoscreen)

With `--view` enabled, these are public (no login):

- `/view` — read-only week calendar for an infoscreen
- `/api/view/streams` — stable JSON status (no users, no passwords)
- `/api/view/config` — host / ports / timezone
- `/ws/view` — read-only WebSocket updates
- `/metrics` — Prometheus metrics

When `--view` is off, `/view` and the view endpoints are not served.

## DataMiner integration

DataMiner can ingest the relay through two standard paths:

**1. Generic REST API connector** — point it at `http://RELAY:3001/api/view/streams`
(auth not required when `--view` is on). The response is a stable JSON array,
one object per stream, with: `name`, `contact`, `inPort`, `outPort`, `state`,
`startAt`, `stopAt`, `recurrence`, `stats` (bitrate, retransmitted, lost,
jitter, rtt, health), `codecs`. Map the fields you want via JSONPath.

**2. Prometheus** — DataMiner's Prometheus integration (or Grafana) can scrape
`http://RELAY:3001/metrics`. Per stream (labels `stream`, `contact`,
`in_port`, `out_port`):

```
srt_stream_up{...} 1|0
srt_stream_bitrate_kbps{...}
srt_stream_bytes_in_total{...}
srt_stream_bytes_out_total{...}
srt_stream_retransmitted_total{...}
srt_stream_lost_total{...}
srt_stream_jitter_ms{...}
srt_stream_rtt_ms{...}
```

## Authentication

The web UI requires a login (all users have admin role). On first run a
`users.json` file is created with the default credentials:

- **username:** `admin`
- **password:** `admin`

Change it immediately after first login via the **Users** button in the top
right of the dashboard: add new users, delete users, and reset passwords
(🔑). Passwords are stored **hashed (bcrypt)** and are never returned by the
API; the list shows a masked value.

## HTTP API

All endpoints except `POST /api/login` require a session cookie (obtained from
login). `POST /api/logout` is also public so a client can always sign out.
The `/api/view/*`, `/metrics` and `/ws/view` endpoints are public only when
`--view` is enabled.

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/login` | `{"user": "...", "pass": "..."}` — sets session cookie |
| POST | `/api/logout` | clears the session |
| GET  | `/api/me`       | current user |
| GET  | `/api/users`    | list users |
| POST | `/api/users/add` | `{"user": "...", "pass": "..."}` |
| POST | `/api/users/{user}/password` | set/reset a password `{"pass": "..."}` |
| DELETE | `/api/users/{user}` | remove a user |
| GET  | `/api/config`   | host / ports / view flag / server timezone |
| GET  | `/api/streams`       | list streams with stats + schedule |
| POST | `/api/streams/add`   | `{"name","inPort","contact?","startAt?","stopAt?","recurrence?","autoRemove?"}` (times = RFC3339) |
| PATCH | `/api/streams/{id}` | edit schedule / recurrence / contact |
| DELETE | `/api/streams/{id}` | remove a stream |
| GET  | `/api/ports/free`    | list free ingress ports (excludes scheduled) |
| WS   | `/ws`                | live stream updates (auth) |
| GET  | `/api/view/streams`  | public read-only JSON status (when `--view`) |
| GET  | `/api/view/config`   | public config (when `--view`) |
| WS   | `/ws/view`           | public read-only updates (when `--view`) |
| GET  | `/metrics`           | Prometheus metrics (when `--view`) |

## Health light logic

| Color | Meaning |
|-------|---------|
| 🟢 green | bitrate stable, loss < 0.1%, low buffer delay |
| 🟡 yellow | rising jitter / loss 0.1–1% / bitrate dipping |
| 🔴 red   | no traffic, loss > 1%, or buffer delay > 1000 ms |
| ⚪ gray  | no connection yet |

## Platform support

| Platform | Status | Notes |
|----------|--------|-------|
| Linux   | ✅ supported | primary target; single static-ish binary + `libsrt.so` |
| macOS   | ✅ supported | build with `./build.sh darwin` |
| Windows | ⚠️ not recommended | `srtgo` (the Go SRT binding) crashes under cgo on Windows (`0x406d1388` in `srt_startup`) — a binding/toolchain issue, not this app. Use Linux or macOS, or run the Docker image. |

## License

MIT — see [LICENSE](LICENSE).
