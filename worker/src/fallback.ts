import type { Box } from './box';

/** The Durable Object's alarm, which wakes the Worker while the box is silent. */
export type Alarms = Pick<DurableObjectStorage, 'getAlarm' | 'setAlarm' | 'deleteAlarm'>;

/** The repository variable a workflow reads in `runs-on`; set while the box is down. */
export const variable = 'BOXRUNNER_DOWN';

const minute = 60_000;
/** Jobs go back to the box once it has reported this long without a gap, so a box that keeps restarting does not take them. */
export const backAfterMs = 2 * minute;
/** The agent reports every 30 seconds; three missed heartbeats break its run. */
const gapMs = 90_000;
/** Failed writes are retried after one minute, then two, four and so on up to an hour. */
const longestWaitMs = 60 * minute;

/** What the Worker keeps between heartbeats and alarms. Times are milliseconds since the epoch. */
export type Fallback = {
	/** Whether jobs should go to GitHub's runners. */
	down: boolean;
	/** When `down` last changed. */
	since: number;
	/** While down: when the box's current run of heartbeats began, or null while it is silent. */
	backSince: number | null;
	/** Whether the variable is set in every served repository, as of the last write that worked in all of them. */
	written: boolean;
	/** Writes that failed in a row, and why the last one did. */
	failures: number;
	problem: string | null;
};

export const idle: Fallback = { down: false, since: 0, backSince: null, written: false, failures: 0, problem: null };

/** A heartbeat arrived; `previous` is when the box last reported before it. */
export function heard(state: Fallback, previous: number | undefined, now: number): Fallback {
	if (!state.down) return state;
	const unbroken = state.backSince !== null && previous !== undefined && now - previous <= gapMs;
	return unbroken ? state : { ...state, backSince: now };
}

/** Whether jobs should go to GitHub's runners at `now`; returns `state` itself when nothing changes. */
export function decide(state: Fallback, afterMs: number | null, lastSeen: number | undefined, now: number): Fallback {
	let down: boolean;
	if (afterMs === null) down = false;
	else if (state.down) {
		const back = state.backSince !== null && lastSeen !== undefined && now - lastSeen <= gapMs && now - state.backSince >= backAfterMs;
		down = !back;
	} else down = lastSeen !== undefined && now - lastSeen >= afterMs;
	return down === state.down ? state : { ...state, down, since: now, backSince: null, failures: 0, problem: null };
}

/** When the alarm should fire next, or null when only a heartbeat can change anything. */
function nextCheck(state: Fallback, afterMs: number | null, lastSeen: number | undefined, now: number): number | null {
	if (state.written !== state.down) return now + Math.min(longestWaitMs, minute * 2 ** Math.max(0, state.failures - 1));
	// While down, an hourly look notices fallback being turned off even if the box never returns.
	if (state.down) return now + longestWaitMs;
	if (afterMs === null || lastSeen === undefined) return null;
	return lastSeen + afterMs;
}

/** Runs on every heartbeat, before the Worker stores it. It never calls GitHub: the alarm does. */
export async function onHeartbeat(box: Box, now: number): Promise<void> {
	const afterMs = box.fallbackAfterMs();
	const stored = box.store.fallback();
	if (afterMs === null && !stored.down && !stored.written) return;
	const state = heard(stored, box.store.lastSeen(), now);
	if (state !== stored) box.store.saveFallback(state);
	if (decide(state, afterMs, now, now) !== state) return box.alarms.setAlarm(now);
	if ((await box.alarms.getAlarm()) !== null) return;
	const at = nextCheck(state, afterMs, now, now);
	if (at !== null) await box.alarms.setAlarm(at);
}

/** Runs when the alarm fires: decides, writes the variable where it is out of date and sets the next alarm. */
export async function check(box: Box, now: number): Promise<void> {
	const afterMs = box.fallbackAfterMs();
	const lastSeen = box.store.lastSeen();
	const stored = box.store.fallback();
	let state = decide(stored, afterMs, lastSeen, now);
	if (state !== stored) box.store.saveFallback(state);
	if (state.written !== state.down) {
		const problem = await box.writeFallback(state.down, state.since);
		// A heartbeat may have moved `backSince` while GitHub answered; nothing but this alarm changes the rest.
		const fresh = box.store.fallback();
		state = problem === null ? { ...fresh, written: state.down, failures: 0, problem: null } : { ...fresh, failures: fresh.failures + 1, problem };
		box.store.saveFallback(state);
		if (problem === null) console.log(`${variable} ${state.down ? 'set: jobs go to GitHub\'s runners' : 'deleted: jobs go to the box again'}.`);
		else console.warn(`Could not ${state.down ? 'set' : 'delete'} ${variable} (attempt ${state.failures}): ${problem}`);
	}
	const at = nextCheck(state, afterMs, lastSeen, now);
	if (at === null) await box.alarms.deleteAlarm();
	else await box.alarms.setAlarm(at);
}
