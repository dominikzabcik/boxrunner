import { alertText, endText, joinAlert, postAlert, waitText } from './alerts';
import type { Box } from './box';
import { heartbeatGapMs, type BoxStatus } from './model';
import { cause, changes, overdue, track, type ScaleSetName, type Wait } from './waits';

/** The Durable Object's alarm, which wakes the Worker while the box is silent. */
export type Alarms = Pick<DurableObjectStorage, 'getAlarm' | 'setAlarm' | 'deleteAlarm'>;

/** What watches the box. `afterMs` is the silence after which it is down, null when neither fallback nor alerts are on. */
export type Watch = { afterMs: number | null; fallback: boolean; webhook: URL | null };

/** The repository variable a workflow reads in `runs-on`; set while the box is down. */
export const variable = 'BOXRUNNER_DOWN';

const minute = 60_000;
/** Jobs go back to the box once it has reported this long without a gap, so a box that keeps restarting does not take them. */
export const backAfterMs = 2 * minute;
/** Failed writes are retried after one minute, then two, four and so on up to an hour. */
const longestWaitMs = 60 * minute;

/** What the Worker keeps between heartbeats and alarms. Times are milliseconds since the epoch. */
export type Fallback = {
	/** Whether every box has been silent for `Watch.afterMs`; with fallback on, jobs then go to GitHub's runners. */
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
	/** Scale sets whose jobs wait with no runner on the way, as the boxes' heartbeats report them. */
	waits: Wait[];
	/** What the last alert that reached the webhook said (whether the box is down, which scale sets wait too long), posts that failed in a row since, and why the last one did. */
	alert: { down: boolean; waiting: ScaleSetName[]; failures: number; problem: string | null };
};

export const idle: Fallback = {
	down: false,
	since: 0,
	backSince: null,
	written: false,
	failures: 0,
	problem: null,
	waits: [],
	alert: { down: false, waiting: [], failures: 0, problem: null },
};

/** A heartbeat arrived; `previous` is when the box last reported before it. */
export function heard(state: Fallback, previous: number | undefined, now: number): Fallback {
	if (!state.down) return state;
	const unbroken = state.backSince !== null && previous !== undefined && now - previous <= heartbeatGapMs;
	return unbroken ? state : { ...state, backSince: now };
}

const overdueChanged = (a: Fallback, b: Fallback) => JSON.stringify(overdue(a.waits)) !== JSON.stringify(overdue(b.waits));

/** Takes in the waits a heartbeat brought; a change in which waits are overdue gets a fresh try at the webhook. Returns `state` itself when nothing changes. */
export function waited(state: Fallback, waits: Wait[]): Fallback {
	if (JSON.stringify(waits) === JSON.stringify(state.waits)) return state;
	const next = { ...state, waits };
	return overdueChanged(state, next) ? { ...next, alert: { ...state.alert, failures: 0, problem: null } } : next;
}

/** Whether jobs should go to GitHub's runners at `now`; returns `state` itself when nothing changes. */
export function decide(state: Fallback, afterMs: number | null, lastSeen: number | undefined, now: number): Fallback {
	let down: boolean;
	if (afterMs === null) down = false;
	else if (state.down) {
		const back = state.backSince !== null && lastSeen !== undefined && now - lastSeen <= heartbeatGapMs && now - state.backSince >= backAfterMs;
		down = !back;
	} else down = lastSeen !== undefined && now - lastSeen >= afterMs;
	if (down === state.down) return state;
	return { ...state, down, since: now, backSince: null, failures: 0, problem: null, alert: { ...state.alert, failures: 0, problem: null } };
}

/** Whether the variable should be set: only fallback sets it, so turning fallback off deletes it. */
const wanted = (state: Fallback, watch: Watch) => state.down && watch.fallback;
const variableDue = (state: Fallback, watch: Watch) => state.written !== wanted(state, watch);
/** An alert goes out only for a change the webhook has not heard, so an outage that ends before its alert went out sends nothing. */
const alertDue = (state: Fallback, watch: Watch) => {
	if (watch.webhook === null) return false;
	const { started, ended } = changes(state.alert.waiting, state.waits);
	return state.alert.down !== state.down || started.length > 0 || ended.length > 0;
};

/** When the alarm should fire next, or null when only a heartbeat can change anything. */
function nextCheck(state: Fallback, watch: Watch, lastSeen: number | undefined, now: number): number | null {
	const failures = [variableDue(state, watch) ? state.failures : null, alertDue(state, watch) ? state.alert.failures : null].filter((n) => n !== null);
	if (failures.length > 0) return now + Math.min(longestWaitMs, minute * 2 ** Math.max(0, Math.min(...failures) - 1));
	// While down, an hourly look notices fallback and alerts being turned off even if the box never returns.
	if (state.down) return now + longestWaitMs;
	if (watch.afterMs === null || lastSeen === undefined) return null;
	return lastSeen + watch.afterMs;
}

const pausedClasses = (box: Box) => new Set(box.store.pauses().map((pause) => pause.className));

/** Runs on every heartbeat, before the Worker stores it. It never calls GitHub or the webhook: the alarm does. */
export async function onHeartbeat(box: Box, status: BoxStatus, now: number): Promise<void> {
	const watch = box.watch();
	const stored = box.store.fallback();
	const boxes = [...box.store.boxes().filter((other) => other.status.box !== status.box), { status, seenAt: now }];
	const waits = track(stored.waits, boxes, pausedClasses(box), watch.afterMs, now);
	const state = waited(heard(stored, box.store.lastSeen(), now), waits);
	if (state !== stored) box.store.saveFallback(state);
	if (watch.afterMs === null && !state.down && !state.written) return;
	if (decide(state, watch.afterMs, now, now) !== state) return box.alarms.setAlarm(now);
	if (overdueChanged(stored, state) && alertDue(state, watch)) return box.alarms.setAlarm(now);
	if ((await box.alarms.getAlarm()) !== null) return;
	const at = nextCheck(state, watch, now, now);
	if (at !== null) await box.alarms.setAlarm(at);
}

/** Runs when the alarm fires: decides, writes the variable where it is out of date, posts the alert and sets the next alarm. */
export async function check(box: Box, now: number): Promise<void> {
	const watch = box.watch();
	const lastSeen = box.store.lastSeen();
	const stored = box.store.fallback();
	let state = decide(stored, watch.afterMs, lastSeen, now);
	if (state !== stored) box.store.saveFallback(state);
	// A heartbeat may move `backSince` or the waits while GitHub or the webhook answers, so each result lands on a fresh read; nothing but this alarm changes the rest.
	if (variableDue(state, watch)) {
		const set = wanted(state, watch);
		const problem = await box.writeFallback(set, state.since);
		const fresh = box.store.fallback();
		state = problem === null ? { ...fresh, written: set, failures: 0, problem: null } : { ...fresh, failures: fresh.failures + 1, problem };
		box.store.saveFallback(state);
		if (problem === null) console.log(`${variable} ${set ? 'set: jobs go to GitHub\'s runners' : 'deleted: jobs go to the box again'}.`);
		else console.warn(`Could not ${set ? 'set' : 'delete'} ${variable} (attempt ${state.failures}): ${problem}`);
	}
	if (watch.webhook !== null && alertDue(state, watch)) {
		const { down, waits } = state;
		const boxes = box.store.boxes();
		const paused = pausedClasses(box);
		const { started, ended } = changes(state.alert.waiting, waits);
		const lines = [
			...(state.alert.down === down ? [] : [alertText(down, watch.fallback, lastSeen, boxes.find((b) => b.seenAt === lastSeen)?.status.box)]),
			...started.map((wait) => waitText(wait, cause(wait, boxes, paused, now))),
			...ended.map(endText),
		];
		const what = state.alert.down === down ? 'waiting jobs' : down ? 'down' : 'recovery';
		const problem = await postAlert(watch.webhook, joinAlert(lines));
		const fresh = box.store.fallback();
		const alert = problem === null ? { down, waiting: overdue(waits), failures: 0, problem: null } : { ...fresh.alert, failures: fresh.alert.failures + 1, problem };
		state = { ...fresh, alert };
		box.store.saveFallback(state);
		if (problem === null) console.log(`Alert sent: ${{ down: 'the box is down', recovery: 'the box is back', 'waiting jobs': 'jobs wait or stopped waiting' }[what]}.`);
		else console.warn(`Could not send the ${what} alert (attempt ${alert.failures}): ${problem}`);
	}
	const at = nextCheck(state, watch, lastSeen, now);
	if (at === null) await box.alarms.deleteAlarm();
	else await box.alarms.setAlarm(at);
}
