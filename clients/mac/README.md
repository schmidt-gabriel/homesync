# HomeSync for macOS

A menu bar app and the sync engine behind it.

```
HomeSyncKit/        the engine — a plain Swift package, no UI, no bundle
HomeSync/           the app — SwiftUI, thin, consumes the engine
HomeSync.xcodeproj  the app target
```

The split is deliberate. Nearly all the interesting code lives in the package,
which builds and tests from the terminal in seconds with `swift test`, without
Xcode, without signing, and without an app bundle. Only the shell of an
interface needs the project.

## Building and running

From Xcode: open `HomeSync.xcodeproj` and press Run. From the terminal:

```bash
xcodebuild -project HomeSync.xcodeproj -scheme HomeSync -configuration Release -derivedDataPath build build
```

Testing the engine on its own, which is the faster loop:

```bash
cd HomeSyncKit && swift test
```

Those tests skip the ones that need a server. To include them, start one and
point them at it:

```bash
docker compose up -d --build                      # from the repository root
TOKEN=$(docker compose exec -T homesync homesync device add mac | grep -oE '[A-Za-z0-9_-]{43}')
cd clients/mac/HomeSyncKit
HOMESYNC_TEST_URL=http://localhost:8420 HOMESYNC_TEST_TOKEN=$TOKEN swift test
```

There is also a headless client, useful for reproducing a sync problem without
launching the app, and it is what the conformance suite drives:

```bash
HOMESYNC_URL=http://localhost:8420 HOMESYNC_TOKEN=$TOKEN HOMESYNC_ROOT=~/Sync \
  swift run homesync-cli
```

## Notes for a first macOS app

Five things decide the shape of this one.

**The app is a bundle, not a binary.** `HomeSync.app` is a folder containing
`Contents/MacOS/HomeSync` and `Contents/Info.plist`. That plist is where
`LSUIElement` is set, which is what makes the app run with no Dock icon and no
main window — exactly what a menu bar app wants.

**`MenuBarExtra` is a SwiftUI `Scene`.** The app declares one instead of a
`WindowGroup` and everything inside is ordinary SwiftUI. There is no
`NSStatusItem` to create or tear down.

**App Sandbox is off, on purpose.** With it on the app only sees its own
container, and reading and writing freely in `~/Library/CloudStorage/HomeSync`
would need security-scoped bookmarks. That is real work for no benefit in an app
you install on your own machines. It would become mandatory for the Mac App
Store; the reasoning is written down in `HomeSync/HomeSync.entitlements`.

**Signing decides whether it opens.** Xcode's "Sign to Run Locally" is enough on
your own machine. An app that arrives as a *download*, including the artifact CI
builds, carries a quarantine flag and Gatekeeper refuses it:

```bash
xattr -dr com.apple.quarantine HomeSync.app
```

Signing it properly needs a paid Apple Developer account.

**Launching at login is `SMAppService`.** It keys off the bundle identifier, so
`dev.schmidt.HomeSync` has to stay stable — changing it later leaves an orphan
entry in System Settings that the app can no longer remove.

## Where the token lives

In the Keychain, never in `UserDefaults`. A device token grants full read and
write access to everything in the sync folder, and `UserDefaults` is a plain
plist that any process running as you can read.
