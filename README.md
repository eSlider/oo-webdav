# oo-webdav

[![Go Reference](https://pkg.go.dev/badge/github.com/eSlider/oo-webdav.svg)](https://pkg.go.dev/github.com/eSlider/oo-webdav)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8.svg)](https://go.dev)
[![Tests](https://github.com/eSlider/oo-webdav/actions/workflows/test.yml/badge.svg)](https://github.com/eSlider/oo-webdav/actions/workflows/test.yml)
[![Latest Release](https://img.shields.io/github/v/tag/eSlider/oo-webdav?sort=semver&label=release)](https://github.com/eSlider/oo-webdav/releases)
[![Docker](https://img.shields.io/badge/container-ghcr.io/eSlider/oo-webdav-2496ed.svg)](https://github.com/eSlider/oo-webdav/pkgs/container/oo-webdav)
[![GitHub Stars](https://img.shields.io/github/stars/eSlider/oo-webdav?style=social)](https://github.com/eSlider/oo-webdav/stargazers)

A pure-Go **WebDAV server for ONLYOFFICE Workspace**. It exposes the portal's
Documents over WebDAV — replacing the buggy Node `ASC.WebDav` service — with
no Node/npm, and authenticates against the portal's own users.

Serves the portal's Files at `https://<portal>/webdav`, with the original root
layout: **In projects · Shared with me · My documents · Common · Favorites ·
Recent · Trash**.

Built on [`github.com/eslider/go-onlyoffice`](https://github.com/eslider/go-onlyoffice)
(a lean REST client for the ONLYOFFICE API) and `golang.org/x/net/webdav`.

## Features

- Pure Go — no Node, no npm, no extra runtimes. Static binary in a tiny image.
- HTTP Basic auth against portal users (plaintext password accepted by
  `/api/2.0/authentication.json`; the portal hashes it internally).
- Full CRUD: list, read, write, mkdir, move, copy, delete, rename.
- Root mirrors the portal's virtual sections (`@root`).
- Metadata-only PROPFIND (no file-body downloads), lazy content streaming.
- Skips 0-byte uploads (Office `.~lock.*` files) that the portal rejects.

## Architecture

```
Windows / macOS / rclone
   └─► HTTPS https://<portal>/webdav
          └─► your reverse proxy (Nginx Proxy Manager / nginx / caddy / traefik)
                 └─ location /webdav ─► oo-webdav container (:8088)
                        └─► ONLYOFFICE portal API (onlyoffice:80 or your host)
```

`oo-webdav` is a sibling container on the same docker network as your
ONLYOFFICE portal, so it reaches the portal API at `http://onlyoffice:80`.

## Prerequisites

1. **ONLYOFFICE Workspace / Community Server** reachable over HTTP from a
   container on your docker network (e.g. `http://onlyoffice:80`).
2. **A reverse proxy with HTTPS + a trusted certificate** for the WebDAV URL
   (Windows WebClient refuses to send credentials over plain HTTP).
3. **Portal storage is correctly configured.** File *downloads* use the
   portal's `filehandler.ashx`, which 302-redirects to a presigned URL. If the
   portal stores files in **S3/MinIO**, the S3 endpoint in the portal config
   must point at a reachable MinIO. A misconfigured endpoint (e.g. defaulting
   to `s3.amazonaws.com`) makes **large-file downloads return 403**; small
   files stored on local disc still work. This is a portal setting, not a
   sidecar setting.

## Deploy

Add a service to your existing `docker-compose.yml` (the one that runs your
`onlyoffice` container):

```yaml
services:
  onlyoffice:
    # ... your existing onlyoffice service ...
    # (must be on the same docker network as oo-webdav)

  oo-webdav:
    image: ghcr.io/eslider/oo-webdav:latest   # or build locally
    build: ./oo-webdav
    depends_on:
      - onlyoffice
    environment:
      PORTAL_URL: "http://onlyoffice:80"      # portal API base (same network)
      LISTEN_ADDR: ":8088"
      WEBDAV_PREFIX: "/webdav"                # path prefix (strip before FS map)
      WEBDAV_ROOT_ID: "@root"                 # "@root" sections or "@my"
    ports:
      - "8098:8088"                           # optional: direct access for testing
    restart: unless-stopped
```

### Route `/webdav` through your reverse proxy

Your reverse proxy must send `/webdav` to the `oo-webdav` container while
passing the full URI (do **not** strip the prefix — `oo-webdav` strips it).

Nginx (or Nginx Proxy Manager custom location):

```nginx
location /webdav/ {
    proxy_pass http://172.17.0.1:8098;   # or http://oo-webdav:8088 on a shared network
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    client_max_body_size 0;              # no upload size limit
    proxy_connect_timeout 60s;
    proxy_send_timeout 3600s;
    proxy_read_timeout 3600s;
}
```

> Keep a `location = /webdav { return 301 $scheme://$host/webdav/; }` so the
> trailing slash is normalized.

### Build the image

```sh
docker build -t ghcr.io/eslider/oo-webdav:latest .
```

## Configuration

| Env var          | Default            | Meaning                                        |
|------------------|--------------------|------------------------------------------------|
| `PORTAL_URL`     | `http://onlyoffice:80` | Portal API base URL (no trailing slash)    |
| `LISTEN_ADDR`    | `:8088`            | HTTP listen address                            |
| `WEBDAV_PREFIX`  | `/webdav`          | URL path prefix stripped before filesystem map |
| `WEBDAV_ROOT_ID` | `@root`            | Root source: `@root` (sections) or `@my`       |
| `CACHE_TTL`      | `10s`              | Per-user folder-listing cache TTL              |
| `SESSION_TTL`    | `15m`              | Per-user session re-auth interval              |

## Authentication

HTTP Basic credentials are portal login + password (email or username).
`oo-webdav` POSTs them to `/api/2.0/authentication.json` (the portal accepts
the **plaintext** password and hashes it internally). The token and folder
cache are kept per user in memory; passwords are never written to disk.

## Windows 11 client

1. Start the WebClient service (needed for WebDAV):
   ```
   net start webclient
   ```
   (Set it to Automatic in `services.msc` so it survives reboots.)
2. Map a drive:
   ```
   net use Z: https://<portal>/webdav /user:you@example.com
   ```
   or Explorer → *This PC* → *Map network drive* → enter the URL and portal
   credentials ("Connect using different credentials").
3. Browse, upload, edit. Changes appear in ONLYOFFICE Documents.

> The URL must be **HTTPS** with a trusted certificate. If the drive shows
> empty, confirm WebClient is running and the URL resolves over HTTPS.

## Verification

```sh
# list root sections
curl -u user:pass -X PROPFIND -H 'Depth: 1' https://<portal>/webdav/

# upload
curl -u user:pass -T file.bin https://<portal>/webdav/My%20documents/file.bin

# download
curl -u user:pass -o file.bin https://<portal>/webdav/My%20documents/file.bin
```

## Troubleshooting

- **Large-file download returns 403/500.** The portal redirected to a
  presigned S3 URL pointing at the wrong host. Fix the portal's S3/MinIO
  endpoint so `filehandler.ashx` presigned URLs point to your reachable MinIO.
  (Small files on local disc are unaffected.)
- **`.~lock.*` / empty-file PUTs.** Office creates empty lock files; these are
  skipped (not uploaded) and reported as success. Non-empty writes work.
- **Drive shows empty.** Windows WebClient not running; or the URL isn't HTTPS
  with a trusted cert.
- **`client_max_body_size`.** Ensure your reverse proxy does not cap request
  bodies (`client_max_body_size 0`).

## Development

```sh
go build ./...
go test ./...
```

## License

MIT
