# Ender Backup

The computer side of [Ender](https://ender.luisg.xyz): it receives the photos and videos from the
Ender iPhone app over Wi‑Fi (or Tailscale) and keeps them in a folder on your Mac, Windows or Linux
computer. No cloud, no account. Download it from https://ender.luisg.xyz.

- **`desktop/`** — Ender Backup, the desktop app (Tauri): a window with the settings page and a
  tray/menu-bar icon. It bundles the server below. See [desktop/README.md](desktop/README.md).
- **`server/`** — the engine, a Go CLI (`photo-backup`). Starts an HTTP server announced over
  Bonjour, with a local settings page; stores files in `<dest>` (optionally one subfolder per
  phone) and keeps a SQLite index (`<dest>/.photo-backup/index.db`) of everything received. It
  also runs on its own, e.g. headless on a home server.
- **`scripts/`** — `service/` runs the server permanently (systemd, Windows login);
  `make-installer-art.py` draws the Windows installer images.

The Ender iPhone app is not part of this repository.

## Server

```sh
cd server && go install .                # installs `photo-backup` into $(go env GOPATH)/bin
# or: go build -o ../bin/photo-backup .  # Windows: -o ../bin/photo-backup.exe
```

Run `photo-backup` with no arguments (or double-click it) to start the server and open the
**settings page** at http://localhost:8765 in your browser. From there you can:

- pick the destination folder with the system folder dialog (Explorer / Finder), or open it;
- change the computer name shown in the app, and choose whether each phone gets its own subfolder;
- scan the pairing QR code, or create a new pairing code;
- see each phone's backup totals and a live feed of received files.

The settings page only answers requests from the computer itself; phones only reach the
token-protected `/api/*` endpoints.

Headless and scripting use:

```sh
photo-backup serve [-dest DIR] [-port 8765] [-name NAME] [-host ADDRS] [-new-token] [-open]
photo-backup status                      # per-device summary
photo-backup verify                      # report indexed files missing from disk
```

Settings (folder, port, name, pairing token) live in `config.json` in the user config dir
(`%AppData%\photo-backup` on Windows, `~/Library/Application Support/photo-backup` on macOS),
or wherever `PHOTO_BACKUP_CONFIG` points. Command-line flags override them for one run; changes
made on the settings page are saved. The index database lives inside the destination folder, so
switching folders never mixes backups, and the pairing token is kept, so the phone stays paired.

By default files go straight into the destination folder. Turn on **Separate folder for each
phone** on the settings page (or pass `-device-folders`) to put each phone's files in a subfolder
named after the device the first time it uploads (`iPhone de Luis/IMG_0001.HEIC`). That name then
stays fixed even if the phone is renamed; a second phone with the same name gets
`iPhone de Luis (2)`. Changing the setting only affects new uploads. Name clashes get a `_1`,
`_2`… suffix, and file modification times are set to the capture date.

`-host` sets the addresses put in the QR (auto-detected, LAN first).

On Windows, allow the binary through the firewall on private networks the first time it runs.

Cross-compile for macOS: `GOOS=darwin GOARCH=arm64 go build -o ../bin/photo-backup-darwin-arm64 .`

### Running it permanently

- **Linux (systemd user service):** copy the binary to `~/.local/bin/photo-backup`, then
  `cp scripts/service/photo-backup.service ~/.config/systemd/user/ && systemctl --user enable --now photo-backup`.
  To keep it running without a login session: `sudo loginctl enable-linger $USER`.
  Logs: `journalctl --user -u photo-backup -f`.
- **Windows:** `powershell -ExecutionPolicy Bypass -File scripts\service\install-windows.ps1` installs to
  `%LOCALAPPDATA%\photo-backup` and starts it hidden at every login (Startup folder launcher).
- **macOS:** run `photo-backup` from Terminal, or add it to Login Items.

On a computer without a screen, approve a phone from the terminal (works over SSH):
`photo-backup approve` (or `photo-backup approve -wait 2m` to wait for the phone). If port 8765 is
taken (e.g. by Docker/Rancher Desktop), set another `port` in `config.json`; phones find it via Bonjour.

## Pairing

- **Bonjour (no QR):** the server announces itself as `_photobackup._tcp` on the local network.
  The app lists the computers it finds; tap one and click **Approve** in the dialog that pops up
  on that computer (or on its settings page — answering one closes the other; `-no-dialogs` turns
  the dialog off). The pairing token is only released after approval, so other devices on the
  network can't pair on their own. A phone remembers computers it has paired with, so switching
  back to one needs no new approval.
- **QR code:** scan the code on the settings page (or copy its link and paste it in the app).

Paired phones remember the server's ID, so if the computer's IP address changes they find it
again over Bonjour automatically.

## Tests

- `cd server && go test ./...` — server, settings page and pairing.

## Photos already in the folder

When the server starts, or the destination changes, it indexes the files already in the folder
(name, size and modification time; hidden and system folders are skipped) in the background — the
settings page shows progress and has a **Rescan** button. Photos on the phone with the same name
and size count as backed up, and an upload whose name and size match an existing file is compared
by hash and not stored twice. The app waits for indexing to finish before uploading. This works
for copies that kept the iPhone's original filenames (Image Capture, Photos "Export Unmodified
Original"); files that were renamed or converted (e.g. HEIC to JPG) can't be recognised.

## Favorites

The app sends each photo's favorite flag with every status check. The server records favorites in
its index and, on macOS, adds a red **Favorite** Finder tag to the backed-up file (other tags are
kept; unfavoriting removes it). This also applies to photos that were already in the folder, so the
first status check after updating tags everything that matches.

## Away from home: Tailscale

With [Tailscale](https://tailscale.com) on the iPhone and the computers, the app works away from
home too. Bonjour doesn't cross Tailscale, so instead:

- the app remembers every computer it has seen, with all its addresses (home and Tailscale), and
  when the computer list opens it tries them directly — the ones that answer only over Tailscale
  are marked *via Tailscale*;
- each server lists the other photo-backup servers on its tailnet (`GET /pair/peers`), found with
  the Tailscale app installed on it (`tailscale status`) and checked on ports 8765–8770; the app
  asks every server it reaches, so a computer it has never seen still shows up.

A paired computer is reached at whichever of its addresses answers first.

## Compressing videos

**Compress videos** (its own switch, off by default; also in the app) saves an AV1 copy of each
video next to it (`IMG_1234.av1.mp4`) with ffmpeg's SVT-AV1 encoder, when ffmpeg is installed
(`winget install ffmpeg`, `brew install ffmpeg`, `apt install ffmpeg`). Quality is a VMAF target —
*Visually lossless* 98, *Excellent* 96 (default), *Very good* 94, *Good* 92 — and the encoder's
CRF is searched per video on two 2-second samples: the same CRF gave VMAF 94 to 99.9 on four
iPhone clips, and 18% to 113% of the size. A copy that wouldn't save 10% isn't kept. The copy keeps HDR (when the source has it), the exact frame timing, the
audio, date and place, and is checked to be as long as the original. Videos are done one at a
time after the photos, at low priority — about 4 s of work per second of 4K 60 fps video.
"Delete originals" applies to videos too. ffmpeg can't write timed metadata tracks into an
AV1 MP4, so after encoding the server copies them over itself (carrytracks.go): Apple's metadata
tracks, DJI flight telemetry (with the drone's model, firmware and metadata versions) and the
original's user data (DJI tags, cover image, camera name), sample for sample. Dolby Vision is kept too
when ffmpeg can encode it (ffmpeg 7+: an iPhone's profile 8.4 becomes AV1 profile 10.4, playable as
plain HLG where Dolby Vision for AV1 isn't supported). Originals whose copy lacks anything (e.g.
Dolby Vision with an older ffmpeg) are never deleted, and the activity log says what's missing.

## Album folders

With **Folder for each album** (on by default, settings page) a photo that's in an album on the
phone — one you made, or one an app made like *WhatsApp* — is saved in a folder named after the
album. A photo in several albums goes in the first one (A–Z) and is copied into the others; the
copies are hard links where the disk supports it (APFS, NTFS, ext4…), so they take no extra
space, and real copies on exFAT/FAT drives.

At each status check the computer follows the phone: files are moved into their album folder,
including matching files that were already in the folder before (e.g. from Image Capture), and
copies in albums the photo was removed from are deleted. A photo in no album is left where it
is. Album names with `/` become `_`. Compressed (AVIF) copies move with their photo.

## Compressing photos

**Compress photos** on the settings page (off by default) converts every photo in the folder
(JPEG, PNG, HEIC — uploaded or already there) to **AVIF**, saved next to the original with the same
name (`IMG_1234.HEIC` → `IMG_1234.avif`). Videos are left alone. It runs in the background, one
photo per CPU thread in parallel, in a low-priority helper process (so the server keeps answering
the phone instantly and other programs come first), within a memory budget (large panoramas run
fewer at a time); the settings page shows progress and space saved.

- Date taken, location, camera and the rest of the EXIF data are copied into the AVIF, and the
  image is stored the right way up. HDR (the brighter highlights of recent iPhone photos) is not
  kept: the AVIF is standard 8-bit.
- Quality is chosen as how close to the original the copy should look, not as an encoder number:
  *Visually lossless*, *Indistinguishable side by side* (default), *High*, *Good* or *Smallest* —
  SSIMULACRA 2 targets 85/80/75/70/60. Each maps to an AVIF quality per kind of photo, measured
  on real HEICs, JPEGs and PNG screenshots (at the default: HEIC ~64%, JPEG ~20%, screenshots ~9%
  of the original). A copy that wouldn't save 10% isn't kept (often an iPhone HEIC at the highest
  level); the original stays as it is.
- The index remembers each original's name and size, so a photo still counts as backed up after
  its original is gone and is never sent again.
- **Delete originals after compressing** is off by default so you can compare both files first.
  When on, each original is deleted once its AVIF is saved (earlier conversions included).
  Deleted originals can't be recovered.
- libavif/libheif are built into the binary (as WebAssembly). If they're installed on the system
  (e.g. `brew install libavif libheif`), those are used instead and conversion is faster.

## How "safe to delete" works

1. The app exports each resource of an asset (original photo/video, Live Photo video,
   RAW, edited render if any), hashes it (SHA-256) and uploads it.
2. The server verifies size + hash, stores it, and records it in the index. Identical content
   from the same phone is stored once.
3. After all resources are uploaded the app **commits** the asset.
4. An asset is reported as backed up only if it is committed **and** every file is still on
   disk with the recorded size (or, for a compressed photo, its AVIF copy is). The app re-checks right before deleting; iOS asks for
   confirmation and keeps deleted items in *Recently Deleted* for 30 days.
5. With **Keep favorites** (on by default) photos marked as favorites are never deleted, even
   when backed up.

## API

`/api/*` endpoints need `Authorization: Bearer <token>`. Pairing endpoints are open:
`GET /pair/info`, `POST /pair/request` → `{request_id}`, `GET /pair/status?id=` → the token once
approved on the settings page (handed out once).

| Method | Path           | Purpose |
|--------|----------------|---------|
| GET    | `/api/info`    | server name, ID + API version |
| POST   | `/api/check`   | `{device_id, asset_ids}` → `{backed_up: [...]}` |
| POST   | `/api/upload`  | raw body; query `device_id, asset_id, resource, filename, created, size, sha256` |
| POST   | `/api/commit`  | `{device_id, asset_id, media_type, created_at, resources}`; 409 lists missing resources |
| POST   | `/api/deleted` | `{device_id, asset_ids}` removed from the phone |
