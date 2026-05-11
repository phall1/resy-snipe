# Deployment artifacts

Operator-facing examples for running `resy-snipe serve` in production.
These files are starting points — copy them out, adjust paths and
hostnames for your environment, and reload the relevant service.

The daemon is reverse-proxy native (ADR-0009): it binds loopback,
speaks plain HTTP, and expects something in front of it to terminate
TLS and apply network policy.

## Contents

```
deploy/
  systemd/
    resy-snipe.service   systemd unit (Type=notify, hardened, fixed User=)
    resy-snipe.sysusers  sysusers.d snippet to create the resy-snipe user
    resy-snipe.tmpfiles  tmpfiles.d snippet for /etc/resy-snipe perms
  nginx/
    resy-snipe.conf      nginx vhost: TLS, SSE, optional rate limits
  caddy/
    Caddyfile            Caddy v2 site block: auto-TLS, SSE-safe proxy
```

## Defaults assumed

| Thing                 | Value                            | Where to change                       |
|-----------------------|----------------------------------|---------------------------------------|
| Daemon bind address   | `127.0.0.1:7765`                 | `daemon.bind` in `config.toml`        |
| Config file           | `/etc/resy-snipe/config.toml`    | `--config` in the unit's `ExecStart`  |
| Data directory        | `/var/lib/resy-snipe`            | `daemon.data_dir` in `config.toml`    |
| Secrets keyfile       | `/etc/resy-snipe/key.hex`        | `secrets.keyfile` in `config.toml`    |
| Binary location       | `/usr/local/bin/resy-snipe`      | `ExecStart=` in the unit              |
| Runtime user          | `resy-snipe` (system, nologin)   | sysusers + unit `User=`               |

The daemon's port is **7765** (see `docs/v2/design/daemon.md` §2.8). The
reverse-proxy examples assume the same; update both ends if you move it.

## Install (Linux + systemd)

1. **Drop in the binary.**

   ```
   install -m 0755 resy-snipe /usr/local/bin/resy-snipe
   ```

2. **Provision the user and directories.**

   ```
   install -m 0644 deploy/systemd/resy-snipe.sysusers \
       /etc/sysusers.d/resy-snipe.conf
   install -m 0644 deploy/systemd/resy-snipe.tmpfiles \
       /etc/tmpfiles.d/resy-snipe.conf
   systemd-sysusers
   systemd-tmpfiles --create
   ```

3. **Drop the config in place.**

   ```
   install -d -m 0750 -o root -g resy-snipe /etc/resy-snipe
   install -m 0640 -o root -g resy-snipe config.toml /etc/resy-snipe/config.toml
   ```

   A template `config.toml` is in `docs/v2/design/daemon.md` §3.1.

4. **Install the unit and enable it.**

   ```
   install -m 0644 deploy/systemd/resy-snipe.service \
       /etc/systemd/system/resy-snipe.service
   systemctl daemon-reload
   systemctl enable --now resy-snipe.service
   ```

## First-boot prerequisites

The daemon's boot sequence is fail-fast (`docs/v2/design/daemon.md`
§2). Before `systemctl start` succeeds you need:

- **`config.toml`** at `/etc/resy-snipe/config.toml`, owned `root:resy-snipe`
  with mode `0640`.
- **Secrets material**. Either:
  - **keyfile mode** (recommended for systemd): generate a random key,
    write it to `/etc/resy-snipe/key.hex` mode `0400` owned by
    `resy-snipe`, and set `[secrets] mode = "keyfile"` plus
    `keyfile = "/etc/resy-snipe/key.hex"` in `config.toml`. See
    `docs/v2/design/secrets.md`.
  - **passphrase mode**: keep the passphrase in
    `/etc/resy-snipe/passphrase` (mode `0400`, owned `root`), and
    uncomment the `LoadCredential=` lines in the unit.
- **Operator seed**. The first operator user and token must exist
  before any CLI or MCP client can talk to the daemon. The seed
  procedure lives in `docs/v2/design/multi-user.md`.

## Verify

```
systemctl status resy-snipe.service
journalctl -u resy-snipe.service -n 50
curl -s http://127.0.0.1:7765/healthz   # -> "ok"
curl -s http://127.0.0.1:7765/readyz | jq .
```

The boot banner printed by the daemon (and the `/readyz` body) should
report the bind address, schema version, secrets state, and the
trusted-proxy CIDRs in effect.

## Reverse-proxy choice

Pick one. Both examples expect the daemon on `127.0.0.1:7765`.

- **nginx** (`deploy/nginx/resy-snipe.conf`) — drop-in vhost, TLS via
  `certbot` or `acme.sh`. SSE for `/v1/quests/{id}/events` is wired up
  (no buffering, 1h read timeout, HTTP/1.1, cleared `Connection`
  header). Optional `limit_req_zone`s are present as commented stubs.
- **Caddy** (`deploy/caddy/Caddyfile`) — automatic TLS, automatic
  HTTP→HTTPS redirect. `flush_interval -1` makes SSE work without
  further tuning.

Either way, set **`http.trusted_proxies`** in `config.toml` to a list
that includes the proxy's source IP. If you forget, the audit log
will record every request as coming from `127.0.0.1` and rate limits
will key on the wrong IP — this is the single most common deployment
bug (see `docs/v2/design/daemon.md` §5). For an on-host proxy the
default loopback CIDRs are sufficient.

## Non-systemd deployments

- **Docker / docker-compose**: example sketch in
  `docs/v2/design/daemon.md` §9.3.
- **Tailscale**: `tailscale serve --bg --https=443
  http://127.0.0.1:7765` is the entire reverse-proxy story; no
  Caddy/nginx needed (`docs/v2/design/daemon.md` §9.4).

## See also

- `docs/v2/design/daemon.md` — boot sequence, HTTP transport, deployment shapes
- `docs/v2/design/secrets.md` — keyfile vs passphrase, rotation
- `docs/v2/design/multi-user.md` — operator seed, tokens, scopes
- `docs/v2/adr/0009-reverse-proxy-native-http.md` — why no built-in TLS
