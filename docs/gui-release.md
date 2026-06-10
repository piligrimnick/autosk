# Building & installing the autosk desktop / iPad app (release)

The desktop GUI is a [Tauri v2](https://tauri.app) app (React/Vite front end +
Rust backend). This guide covers **release** builds and how to install them on a
desktop and on an iPad. For dev runs (`npm run tauri:dev`) and the architecture,
see [`gui/README.md`](../gui/README.md).

Whichever target you build, the app is a **pure JSON-RPC client of `autoskd`** —
it does nothing on its own. It runs in one of two modes (set in the in-app
**Settings** view):

- **Local** — connects over a Unix-domain socket and **auto-spawns** `autoskd`
  from your `PATH`.
- **Remote** — dials a `host:port` and authenticates with a token. Used on iOS
  (the sandbox can't spawn a local daemon) and for any remote host.

> **`autoskd` is not bundled into the app yet** (no Tauri sidecar). So a packaged
> desktop app only works in **local** mode if `autoskd` is already on `PATH`
> (e.g. `make install` from a checkout, or `brew install wierdbytes/autosk/autosk`),
> and the **iPad** app must talk to an `autoskd` running elsewhere (Remote mode).
> `pi` must also be installed wherever the daemon actually runs the agents.

## Prerequisites (all targets)

- A repo checkout, **Node.js 22+**, and a stable **Rust** toolchain.
- Front-end deps installed once:

  ```bash
  cd gui && npm ci
  ```

## Desktop release (macOS / Windows / Linux)

```bash
cd gui
npm ci
npm run tauri:build      # tsc && vite build, then `tauri build`
```

Bundles land under `gui/src-tauri/target/release/bundle/<format>/`:

- **macOS** — `dmg/autosk_<version>_<arch>.dmg` and `macos/autosk.app`
- **Windows** — `msi/` (WiX) and/or `nsis/`
- **Linux** — `deb/`, `appimage/`, `rpm/`

Install: open the `.dmg` and drag **autosk** to `Applications` (macOS); run the
installer (Windows); install the `.deb`/AppImage/rpm (Linux).

**macOS signing/notarization.** An unsigned build trips Gatekeeper — right-click
the app → **Open** the first time, or produce a signed+notarized build by
setting the standard Tauri env vars (`APPLE_SIGNING_IDENTITY`, `APPLE_ID`,
`APPLE_PASSWORD`, `APPLE_TEAM_ID`) before `npm run tauri:build`. The app ships a
Liquid Glass icon for macOS 26+ with a flat fallback for older systems.

After install, make sure `autoskd` is on `PATH` for **Local** mode, or point the
app at a remote daemon (see below).

## iOS / iPad release

There is no App Store distribution — you install a **signed developer build**
directly. The iOS target is already generated under
`gui/src-tauri/gen/apple` (deployment target **iOS 14+**, iPad orientations,
Local-Network + Bonjour `_autosk._tcp` permissions). All commands run on a
**Mac with Xcode**.

### One-time setup

```bash
xcode-select --install                                  # Xcode + CLI tools (full Xcode from the App Store)
rustup target add aarch64-apple-ios aarch64-apple-ios-sim x86_64-apple-ios
brew install cocoapods
cd gui && npm ci
```

**Signing team.** Open the Xcode project once and set a team (a free Apple ID
works to sign onto your own device):

```bash
open gui/src-tauri/gen/apple/autosk-gui.xcodeproj
# target "autosk-gui_iOS" → Signing & Capabilities → Team = your Apple ID
```

…or `export APPLE_DEVELOPMENT_TEAM=XXXXXXXXXX` before building. If the bundle id
`com.autosk.gui` is already taken under your account, change `identifier` in
`gui/src-tauri/tauri.conf.json` and `bundleIdPrefix` in
`gui/src-tauri/gen/apple/project.yml`.

### Build & install

```bash
cd gui
npm run tauri -- ios build           # release; emits a signed .ipa
```

The `.ipa` lands under `gui/src-tauri/gen/apple/build/`. Install it onto the
iPad with any of:

- **Xcode** → Window → *Devices & Simulators* → drag the `.ipa` onto the device,
- **Apple Configurator**, or
- **TestFlight** (requires a paid Apple Developer account).

For a single connected iPad, build + sign + install + launch in one step
(unlock the iPad and tap **Trust** first):

```bash
npm run tauri -- ios dev --release   # drop --release for a debug build
```

After install: on the iPad, **Settings → General → VPN & Device Management →
trust** your developer certificate.

**Caveats.** A free Apple ID build expires after **7 days** (re-run the build to
refresh); a paid Apple Developer account avoids this and enables TestFlight. The
target is arm64-only and requires Metal (any modern iPad).

### Connect the iPad to `autoskd` (Remote mode)

The iPad runs in Remote mode. On your Mac (or a server), start `autoskd` with
the opt-in TCP listener — token auth is automatic:

```bash
autoskd serve --tcp 0.0.0.0:7878
cat ~/.autosk/daemon-token           # the auth token to paste into the app
```

In the app: **Settings → Remote** and enter:

- **host:port** — your Mac's LAN IP + `7878` (e.g. `192.168.1.42:7878`)
- **token** — the value from `~/.autosk/daemon-token`

Save & reconnect, then **allow** the Local-Network prompt on first launch. The
Mac and iPad must share a network and the port must be open in the Mac's
firewall. See [`docs/daemon.md`](daemon.md) for the `--tcp` / token transport.

## See also

- [`gui/README.md`](../gui/README.md) — architecture, scripts, local vs remote.
- [`docs/daemon.md`](daemon.md) — `autoskd`, idle-shutdown, the remote transport.
