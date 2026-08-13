# HomeInOne-wifi-helper

Single-binary Go daemon that wraps `nmcli` behind a Unix-socket HTTP API. Lives on the HIO pad (Armbian, RK3566) so the [HomeInOne-pad](../HomeInOne-pad) Caddy container — which can't reach NetworkManager directly — can drive WiFi onboarding without being granted host privileges.

Endpoints (paths after Caddy strips the `/api/wifi` prefix):

- `GET  /scan` — `[{ ssid, signal, security }]`
- `GET  /status` — `{ connected, ssid, ip }`
- `POST /connect` — body `{ ssid, password }`, returns the post-connect status
- `POST /disconnect` — returns the post-disconnect status

Display endpoints (Caddy maps `/api/display/*` onto the same socket, stripping only `/api`), see `display.go`:

- `GET  /display/brightness` — `{ pct }` (5-100)
- `POST /display/brightness` — body `{ pct }`, applies + persists the level
- `GET  /display/hold` — `{ sec }`, seconds of screen-on lease remaining (0 = none)
- `POST /display/hold` — body `{ sec }` (0-300), keep the backlight on for `sec` from now; `0` releases

`/display/hold` is a **renewable lease, not a latch** — the caller (an intercom call in
`HomeInOne-pad`) re-POSTs it every 10s, so a crashed kiosk can't pin the screen on. It only
records intent: `hio-display-idle.sh` polls the lease once a second and stays the sole writer
of the backlight. That split matters — the backlight has two gates, `brightness` (PWM duty)
and `bl_power` (FB blank state), and writing brightness alone while `bl_power=4` leaves the
PWM at duty 0, i.e. the screen stays dark.

The socket defaults to `/run/hio-wifi.sock`, owned `root:docker` mode `0660`. JSON shapes match `HomeInOne-pad/src/lib/wifi/client.ts` and `src/lib/display/client.ts`.

> **Restarting this service breaks the UI's API until `pad-ui` is restarted too.** The socket
> is bind-mounted into the Caddy container as a *file*; a restart recreates it with a new
> inode and the container keeps serving the dead one (502 on every `/api/wifi/*` and
> `/api/display/*`). After `systemctl restart hio-wifi-helper`, run
> `docker restart pad-ui`.

## Build

Native (needs Go 1.23+):

```sh
go build -ldflags="-s -w" -o bin/hio-wifi-helper .
```

Cross-compile for the pad (RK3566 / linux/arm64) without a host Go toolchain:

```sh
docker run --rm -v "$PWD:/app" -w /app -e CGO_ENABLED=0 \
    -e GOOS=linux -e GOARCH=arm64 golang:1.23-alpine \
    go build -ldflags="-s -w" -o bin/hio-wifi-helper.arm64 .
```

## Install on the pad

```sh
sudo install -m 0755 bin/hio-wifi-helper.arm64 /usr/local/bin/hio-wifi-helper
sudo install -m 0644 systemd/hio-wifi-helper.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now hio-wifi-helper
```

`HomeInOne-deploy/pad/setup-pad.sh` will own this once step 5 of [HIO-Pad-Web-UI-Split](../HIO_Brain/Projects/HIO-Pad-Web-UI-Split.md) lands.

## Smoke test (any Linux host with NetworkManager)

```sh
sudo HIO_WIFI_SOCKET=/tmp/hio-wifi.sock ./bin/hio-wifi-helper &
curl --unix-socket /tmp/hio-wifi.sock http://localhost/status
curl --unix-socket /tmp/hio-wifi.sock http://localhost/scan
curl --unix-socket /tmp/hio-wifi.sock -X POST \
    -H 'Content-Type: application/json' \
    -d '{"ssid":"Example","password":"hunter2"}' \
    http://localhost/connect
```

## Env knobs (mostly for tests)

| var | default | purpose |
|---|---|---|
| `HIO_WIFI_SOCKET` | `/run/hio-wifi.sock` | Where to bind |
| `HIO_NMCLI` | `nmcli` | Command path — point at a mock to test parsing offline |
