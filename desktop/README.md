# Ender for desktop (Tauri)

The photo-backup server as a desktop app: a window with the settings page and a tray/menu-bar icon.

- The Go server (`../server`) is bundled as a Tauri sidecar and started with `serve -desktop`.
  It prints `PHOTO_BACKUP_URL=…` and the window opens that page. If a server is already
  running on the configured port (e.g. the one `scripts/service` installs), the window shows
  that one instead of starting a second.
- Closing the window keeps Ender in the tray so phones can keep backing up; **Quit Ender** in
  the tray menu stops the server. The server also exits if the app dies (it watches its stdin).
- Tray menu: Open Ender, Start at login (starts hidden, in the tray), Quit Ender.

## Build

Needs Go, Rust (`rustup`), Node and, on Windows, the MSVC build tools (WebView2 ships with Windows 11).

```sh
npm install
npm run dev     # builds the server sidecar, runs the app
npm run build   # installers: Windows .msi + setup .exe, macOS .app + .dmg
                # (over SSH on a Mac, set CI=true: the .dmg step otherwise scripts Finder)
```

Video compression uses the computer's own `ffmpeg` (found on PATH), as with the plain server.

## Signing

- **macOS:** signed with a Developer ID Application certificate and notarized. Build with
  `APPLE_SIGNING_IDENTITY="Developer ID Application: <name> (<team>)" npm run build -- --bundles app,dmg`,
  then `xcrun notarytool submit <dmg> --keychain-profile <profile> --wait` and
  `xcrun stapler staple` on the .dmg and the .app.
- **Windows:** not signed yet, so SmartScreen shows "Windows protected your PC" (More info ›
  Run anyway).
