import { heartbeatGapMs, type BoxName, type ClassName, type RepoName, type RunnerState } from './model';
import type { StoredBox } from './store';

/** A repository and class; the alert state keeps the ones the webhook heard waiting. */
export type ScaleSetName = { repo: RepoName; className: ClassName };

/** A scale set whose assigned jobs have no runner requested, starting or running. Times are milliseconds since the epoch. */
export type Wait = ScaleSetName & {
	/** The box that last reported the jobs waiting. */
	box: BoxName;
	since: number;
	/** Whether a heartbeat showed the jobs still waiting `Watch.afterMs` after `since`. */
	overdue: boolean;
};

const key = ({ repo, className }: ScaleSetName) => `${repo} ${className}`;
const onTheWay: readonly RunnerState[] = ['requested', 'starting', 'running'];
const fresh = (box: StoredBox, now: number) => now - box.seenAt <= heartbeatGapMs;

/**
 * Brings the waits up to date with the boxes' latest heartbeats. A box sees a
 * scale set's jobs only while it listens to it, which it stops doing while it
 * drains or the class is paused. So a wait stays while the box that reported
 * it is silent or draining or the class is paused, and ends once that box
 * reports without it or any box has a runner on the way for it.
 */
export function track(previous: readonly Wait[], boxes: readonly StoredBox[], paused: ReadonlySet<ClassName>, afterMs: number | null, now: number): Wait[] {
	const reporting = boxes.filter((box) => fresh(box, now));
	const withRunner = new Set(
		reporting.flatMap(({ status }) => status.runners.filter((runner) => onTheWay.includes(runner.state)).map(key)),
	);
	const waiting = new Map<string, Wait>();
	for (const { status } of reporting) {
		for (const scaleSet of status.scaleSets) {
			if (scaleSet.assigned === 0 || withRunner.has(key(scaleSet))) continue;
			const before = previous.find((wait) => key(wait) === key(scaleSet));
			waiting.set(key(scaleSet), {
				repo: scaleSet.repo,
				className: scaleSet.className,
				box: status.box,
				since: before?.since ?? now,
				overdue: before?.overdue ?? false,
			});
		}
	}
	const unseeing = (name: BoxName) => {
		const box = boxes.find((candidate) => candidate.status.box === name);
		return box !== undefined && (!fresh(box, now) || box.status.draining);
	};
	for (const wait of previous) {
		if (waiting.has(key(wait)) || withRunner.has(key(wait))) continue;
		if (paused.has(wait.className) || unseeing(wait.box)) waiting.set(key(wait), wait);
	}
	return [...waiting.values()]
		.map((wait) => ({ ...wait, overdue: wait.overdue || (afterMs !== null && now - wait.since >= afterMs) }))
		.sort((a, b) => (key(a) < key(b) ? -1 : 1));
}

export const overdue = (waits: readonly Wait[]): ScaleSetName[] =>
	waits.filter((wait) => wait.overdue).map(({ repo, className }) => ({ repo, className }));

/** What the webhook has not heard: overdue waits it was not told of, and waits it was told of that ended. */
export function changes(heard: readonly ScaleSetName[], waits: readonly Wait[]): { started: Wait[]; ended: ScaleSetName[] } {
	const told = new Set(heard.map(key));
	const now = new Set(overdue(waits).map(key));
	return {
		started: waits.filter((wait) => wait.overdue && !told.has(key(wait))),
		ended: heard.filter((name) => !now.has(key(name))),
	};
}

/** Why the jobs most likely wait, from what the boxes reported; null when nothing they report explains it. */
export function cause(wait: Wait, boxes: readonly StoredBox[], paused: ReadonlySet<ClassName>, now: number): string | null {
	if (paused.has(wait.className)) return 'the class is paused on the dashboard';
	const box = boxes.find((candidate) => candidate.status.box === wait.box);
	if (box === undefined || !fresh(box, now)) return `${wait.box} stopped reporting`;
	if (box.status.draining) return `${wait.box} is draining`;
	const config = box.status.classes.find((c) => c.name === wait.className);
	if (config === undefined) return null;
	if (config.heldUntil !== null) return `${wait.box} holds the class back because its runners failed before taking a job`;
	const active = box.status.runners.filter((runner) => runner.className === wait.className && runner.state !== 'exited').length;
	if (active >= config.max) return `${wait.box} already runs the class’s maximum of ${config.max} runner${config.max === 1 ? '' : 's'} for other repositories`;
	return null;
}
