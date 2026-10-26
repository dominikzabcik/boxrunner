# Menu bar app

A macOS menu bar app for a boxrunner fleet. It shows:

- every box, online or not, with its running and starting runners, each class's runners against its max or limit, free disk and version;
- the jobs waiting for a runner, and why;
- the latest jobs that finished, with the box that ran each, how long it took and its peak memory.

Its switch takes your Mac's box out of the fleet and back. Off sets every class on the box to a limit of 0, so the box finishes its running jobs and takes no new one, while the other boxes go on. On removes the limits ([dashboard](../../docs/dashboard.md#limiting-a-class-on-one-box)).

The menu is drawn like a terminal UI, dark and monospaced, with each action's key on its bottom line. It opens on the fleet's totals: jobs running, jobs waiting and boxes online. Each box shows one cell per runner its classes may run: ■ while a job runs, □ free, · past a limit. The menu bar icon shows how many jobs run, or a pause sign while your box is off.

## Pair it

1. Get the app: download `boxrunner-macos.zip` from the [latest release](https://github.com/dominikzabcik/boxrunner/releases/latest), unzip it and move `boxrunner.app` to Applications. It is signed ad hoc, not notarized, so macOS blocks a browser's download until you run `xattr -dr com.apple.quarantine /Applications/boxrunner.app` once. Or build it: `app/macos/build-app.sh`, then move `app/macos/build/boxrunner.app` to Applications. Open it. It needs macOS 14.
2. Click its menu bar item, enter the Worker's address and press **connect** (⏎). It pairs under this Mac's name.
3. Your browser opens the Worker's pairing page. Sign in if asked, check that it shows the same code as the app, and press **Pair** there.
4. The menu shows the fleet within a few seconds, with the switch for this Mac's box. The app finds that box on its own when exactly one box of the fleet is named like this Mac or like a Linux VM that OrbStack or Lima runs on it. Otherwise choose it under **this mac**.

The app keeps its token in the login keychain. **Unpair** in the app forgets it; **Unpair** on the dashboard stops it at once. Whoever approved the pairing must be able to open the dashboard, or the token stops working.

## Updates

The app keeps itself on the latest release. At launch and once a day it asks github.com which release is the latest, the way the agent does, which needs no API quota. When that is newer than the app's own version it:

1. downloads `boxrunner-macos.zip` and the release's `checksums.txt`, and stops unless the zip's SHA-256 is the one listed there;
2. unpacks the zip into a hidden folder next to the app and checks that it is `boxrunner.app` at the new version, with the same bundle identifier and an intact signature;
3. swaps the new app in for the running one in one step, so the old app is gone only once the new one is in place, and starts the new one.

Nothing changes when any step fails. The menu's bottom line then reads `update failed:` with the reason, and the app tries again in six hours. It reads `updated to v0.6.0` after an update. A failed check, such as having no network, is silent and tried again in an hour.

**Update automatically** in Settings turns this off (it is on by default); the app then never contacts github.com on its own. The folder the app lives in must be writable by you, which `/Applications` is for an administrator. The release workflow builds the zip on a GitHub macOS runner, lists it in `checksums.txt` and attests it with the agent's binaries, so `gh attestation verify boxrunner-macos.zip --repo dominikzabcik/boxrunner` checks a download by hand. The app does not check the attestation itself.

## Develop

`swift test` runs the tests. `fleet.json` is the Worker's real answer to `GET /v1/fleet`: the Worker's `devices.test.ts` checks it, so the two cannot drift apart. `SNAPSHOT_DIR=/tmp swift test` also renders the menu in each phase to PNG files there.

To try an update without publishing a release, serve GitHub's paths from a folder and start the app's binary with `BOXRUNNER_UPDATE_SOURCE=http://127.0.0.1:PORT`: `/releases/latest` answers with a redirect to `/releases/tag/vX.Y.Z`, and `/releases/download/vX.Y.Z/` holds `boxrunner-macos.zip` and `checksums.txt`. Only https addresses and this Mac are accepted. Give the test build its own bundle identifier first, or it shares the installed app's settings and keychain entry, and give the served build the same identifier at a higher version.
