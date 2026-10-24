# Menu bar app

A macOS menu bar app for a boxrunner fleet. It shows:

- every box, online or not, with its running and starting runners, each class's runners against its max or limit, free disk and version;
- the jobs waiting for a runner, and why;
- the latest jobs that finished, with the box that ran each, how long it took and its peak memory.

Its switch takes your Mac's box out of the fleet and back. Off sets every class on the box to a limit of 0, so the box finishes its running jobs and takes no new one, while the other boxes go on. On removes the limits ([dashboard](../../docs/dashboard.md#limiting-a-class-on-one-box)).

The menu is drawn like a terminal UI, dark and monospaced, with each action's key on its bottom line. It opens on the fleet's totals: jobs running, jobs waiting and boxes online. Each box shows one cell per runner its classes may run: ■ while a job runs, □ free, · past a limit. The menu bar icon shows how many jobs run, or a pause sign while your box is off.

## Pair it

1. Build it: `app/macos/build-app.sh`, then move `app/macos/build/boxrunner.app` to Applications and open it. It needs macOS 14.
2. Click its menu bar item, enter the Worker's address and press **connect** (⏎). It pairs under this Mac's name.
3. Your browser opens the Worker's pairing page. Sign in if asked, check that it shows the same code as the app, and press **Pair** there.
4. The menu shows the fleet within a few seconds. Choose which box is this Mac, so the switch knows what to turn off.

The app keeps its token in the login keychain. **Unpair** in the app forgets it; **Unpair** on the dashboard stops it at once. Whoever approved the pairing must be able to open the dashboard, or the token stops working.

## Develop

`swift test` runs the tests. `fleet.json` is the Worker's real answer to `GET /v1/fleet`: the Worker's `devices.test.ts` checks it, so the two cannot drift apart. `SNAPSHOT_DIR=/tmp swift test` also renders the menu in each phase to PNG files there.
