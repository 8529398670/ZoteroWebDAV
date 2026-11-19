# WebDAV for Zotero

ZoteroWebDAV is two servers in one process:

| Port | What | Who uses it |
|---|---|---|
| `server.port` | The web app: sign-in, the WebDAV/Library screens, the JSON API | People in a browser, scripts with an API key |
| `webdav.port` | WebDAV, pure Go (`golang.org/x/net/webdav`) | Zotero, Finder, Cyberduck, rclone, PRMA |

Every account has its own folder, `<webdav.public_url><webdav.prefix>/<name>/`,
stored on disk as `<app dir>/dav/<user id>/`. A WebDAV client can only reach
the folder of the account it signed in as. The admins see names and usage,
never contents.

## Connecting Zotero

1. Sign in to the web app, open **WebDAV**, choose a WebDAV name.
2. Make an **app password** there (it is shown once).
3. In Zotero: Settings → Sync → File Syncing → *Sync attachment files in My
   Library using* **WebDAV**.
   - Protocol and URL: as the WebDAV screen shows, e.g. `https` and
     `dav.example.com/ada`. Zotero adds `/zotero/` itself.
   - Username: your WebDAV name. Password: the app password.
4. **Verify Server**. The `zotero/` folder already exists, so Zotero does not
   ask to create it.

Group libraries always sync files through Zotero's own storage. Zotero's
WebDAV sync covers the personal library only.

## Deploying

- **Hostname.** The default `webdav.prefix: ""` expects a hostname of its own,
  for example `dav.example.com`, routed to `docker.webdav_host_port`. With a
  single hostname, set `webdav.prefix: /dav` and route `/dav/*` to the WebDAV
  port. A cloudflared ingress for either:

  ```yaml
  ingress:
    - hostname: dav.example.com
      service: http://localhost:35786
    - hostname: app.example.com
      service: http://localhost:35785
    - service: http_status:404
  ```

- **`webdav.public_url`.** Set it to the address people use, so the WebDAV
  screen shows the right thing to copy.
- **TLS.** Basic auth sends the app password with every request. Use HTTPS
  only.
- **Cloudflare's 100 MB limit.** Its proxy refuses request bodies over 100 MB
  on the Free and Pro plans, and a Zotero attachment is one zip. If you have
  bigger attachments, use a DNS-only record with your own TLS (Caddy) for the
  WebDAV hostname.
- **Backups.** `manage backup` and the scheduled backups cover the database
  and `storage/`. They do not cover `dav/`, which is the files and can be
  large. Back up `<data_dir>/dav/` separately (rsync, restic). If
  `webdav.encrypt_at_rest` is on, those files can only be read with the
  database that holds the WebDAV master key.

## What is enforced

- **Sign-in.** HTTP Basic, with the WebDAV name and an app password. App
  passwords reach WebDAV only, and API keys do not work as WebDAV passwords.
  An address gets five failed tries, then 15 minutes of 429s. A `401` always
  carries `WWW-Authenticate`.
- **Isolation.** The path must start with the signed-in account's own name.
  Anything else is 404, so other names are never confirmed. Paths are cleaned,
  symbolic links are refused, and COPY/MOVE may only land in the same folder.
- **Writes are atomic.** An upload streams to a hidden file beside its target
  and replaces the old file only once the whole body has arrived. A cut-off
  upload leaves the old file as it was. Nothing is held in memory.
- **Limits.**
  - `webdav.max_file_mb`: a larger file gets 413.
  - Each account's quota (`webdav.default_quota_gb`, or a value an admin sets)
    and `webdav.min_free_mb` of free disk: a write past either gets 507.
  - A per-account rate limit: 429 with `Retry-After`.
  - `PROPFIND` with infinite depth: 403.
  - Request bodies other than a PUT: 1 MB.
- **Timeouts.** Headers must arrive within 10 s. After that, each body read or
  write must make progress within 60 s. A large upload on a slow line is fine
  as long as it keeps moving.
- **Maintenance.** In `read_only` maintenance writes get 503. In `full`
  maintenance everything does.
- **Logging.** Writes and refused sign-ins go to the activity log, and reads
  too with `webdav.log_reads`.

## Encryption at rest (`webdav.encrypt_at_rest`, off by default)

- **Format.** Each file gets its own key and is sealed in 64 KiB
  XChaCha20-Poly1305 segments. Any byte can be read without reading what comes
  before it, so Range requests and reading a zip in place still work.
- **Keys.** The file keys are sealed by a WebDAV master key kept in the
  database, which `manage rotate-key` moves to the new secret key. No file is
  rewritten when the key rotates.
- **Mixed folders.** Changing the setting only affects new writes; both kinds
  of file stay readable. With the server stopped,
  `manage dav-convert -to encrypted|plain -yes` rewrites the existing files.

## The library API (for PRMA and other clients)

Zotero's WebDAV sync carries files only:

- `zotero/<KEY>.zip`, everything in `storage/<KEY>/`;
- `zotero/<KEY>.prop`, `<properties version="1"><mtime>ms</mtime><hash>md5</hash></properties>`, written last.

The metadata travels through the web app's API instead. Use an API key
(`Authorization: Bearer`), and the account's own library is the one used.
Every route is in `config/api.md`.

| Route | Use |
|---|---|
| `POST /api/bulk/library/items` | Push up to 100 items: `{"items":[{"item_key","version","data"}]}`. `data` is Zotero's item JSON. Unchanged ones keep their change number. |
| `POST /api/bulk/library/collections` | The same for collections. |
| `POST /api/library/items/{key}/delete` | Leaves a tombstone for `webdav.tombstone_days`. |
| `GET /api/library/changes?since=<rev>` | Pull what changed, oldest first, tombstones included. Repeat with `since=rev` while `more` is true. `full_resync: true` means start again from 0. |
| `POST /api/library/attachments/check` | `{"attachments":[{"key","md5"}]}` → `present` / `missing` / `different`. A file counts only with both its `.zip` and `.prop`. |
| `GET /api/library/attachments/{key}/file` | Streams the main file (usually the PDF) straight out of the zip, with nothing unpacked on disk. |

### How PRMA is expected to use it (not built yet)

- **Local Zotero plus this server.**
  1. PRMA reads `zotero.sqlite` as it does today.
  2. It pushes items and collections in bulk.
  3. It asks `attachments/check` which imported attachments (`linkMode` 0 or 1)
     are missing.
  4. For each missing one, it PUTs `<KEY>.zip` and then `<KEY>.prop` over
     WebDAV with an app password. It never replaces a `.prop` that is already
     there.
- **This server only.** PRMA pulls `library/changes` and streams PDFs from
  `attachments/{key}/file`.
- **What PRMA leaves alone.** It does not touch zotero.org. Other Zotero
  desktops download a file only when zotero.org's attachment record carries
  its mtime and md5, which Zotero sets for its own uploads.

## Admin CLI

```
zoterowebdav manage dav-list
zoterowebdav manage dav-set-name -user-id 3 -name ada
zoterowebdav manage dav-create-password -user-id 3 -name "Zotero laptop"
zoterowebdav manage dav-convert -to encrypted -yes      # server stopped
```

The hourly `dav-reconcile` job (**WebDAV upkeep** on the Jobs screen) does the
housekeeping:

- recounts usage;
- rebuilds the attachment index;
- removes uploads a crash left behind and trash past `webdav.trash_days`;
- removes the folders of deleted accounts.
