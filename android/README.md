# Ensemble — Android companion

A thin Android app that finds ensemble **masters** on the LAN via mDNS, lets you pick
one, and shows that node's web UI in a WebView. Because a master serves the whole SPA
+ API with no auth and **proxies to every other node**, opening one master controls the
entire cluster — there is no native UI to maintain.

## How it works
- **Discovery** (`Discovery.kt`): jmdns browses `_ensemble._tcp` while holding a Wi-Fi
  multicast lock, keeps adverts with `role=master`, and reads `name` / `http` / `id`
  from the TXT record (the master side advertises these — see
  `internal/discovery/discovery.go`). Result is a live `StateFlow<List<Master>>`.
- **Picker → WebView** (`MainActivity.kt`): a Compose list of masters; tapping one opens
  `http://<host>:<http>/`. A manual "host[:port]" field is the fallback for Wi-Fi that
  blocks multicast.

## Build
This was scaffolded outside Android Studio, so the Gradle **wrapper jar is not
committed**. Either:

- **Android Studio** (recommended): open the `android/` folder; it generates the wrapper
  and syncs. Run on a **physical device** on the same Wi-Fi as a master (the emulator's
  NAT usually can't see LAN mDNS).
- **CLI**: with a local Gradle ≥ 8.7, run `gradle wrapper` once in `android/`, then
  `./gradlew assembleDebug`. APK lands in `app/build/outputs/apk/debug/`.

## Permissions / gotchas
- **Android 13+** needs the `NEARBY_WIFI_DEVICES` runtime permission (requested at
  launch) or mDNS silently finds nothing.
- A `WifiManager.MulticastLock` is held during discovery — required on most devices.
- Cleartext HTTP is allowed via `res/xml/network_security_config.xml` (the LAN UI is
  plain HTTP). Tighten to your subnet there if you prefer.
- Some access points block client-to-client multicast (AP/client isolation); use the
  manual-address field then.

## Status
Source is complete and idiomatic but was **not compiled or run in CI** (no Android
toolchain here). Expect to align plugin/library versions in Android Studio if its
bundled AGP/Kotlin differ from the pinned ones in `build.gradle.kts`
(AGP 8.5.2 / Kotlin 1.9.24 / Compose compiler 1.5.14 / Compose BOM 2024.06.00).
