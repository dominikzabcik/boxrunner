import { heartbeatGapMs, type BoxName, type BoxStatus, type ClassName, type Limit, type RepoName } from './model';
import type { StoredBox } from './store';

/**
 * How many of a scale set's jobs a box should hold: the ones its runners took
 * or are starting for, plus the waiting ones it gets. `repos` are the
 * repositories of the waiting jobs of an organization scale set, which the box
 * names when it mints a runner.
 */
export type Share = { repo: RepoName; className: ClassName; jobs: number; repos: RepoName[] };

const holding = new Set(['requested', 'starting', 'running']);

/**
 * Splits each scale set's jobs over the boxes that serve its class, when two or
 * more of them report. One box holds the scale set's session and reports its
 * assigned jobs; the others cannot open it, so without a share they would never
 * start a runner for it. Waiting jobs go to boxes in rank order, then by name,
 * each up to the room its class has, so every box that asks computes the same
 * split from the same heartbeats. A class served by one box gets no shares, and
 * that box decides alone, as it always has.
 */
export function shares(boxes: readonly StoredBox[], paused: ReadonlySet<ClassName>, limits: readonly Limit[], now: number): Map<BoxName, Share[]> {
	const fresh = boxes
		.filter((b) => now - b.seenAt <= heartbeatGapMs)
		.map((b) => b.status)
		.sort((a, b) => a.rank - b.rank || (a.box < b.box ? -1 : a.box > b.box ? 1 : 0));
	const out = new Map<BoxName, Share[]>();
	const classes = new Set(fresh.flatMap((b) => b.classes.map((c) => c.name)));
	for (const className of [...classes].sort()) {
		const servers = fresh.filter((b) => b.classes.some((c) => c.name === className));
		if (servers.length < 2) continue;
		const room = new Map(servers.map((b) => [b.box, paused.has(className) ? 0 : free(b, className, limits, now)]));
		for (const { repo, assigned, pending } of scaleSets(servers, className)) {
			const held = new Map(servers.map((b) => [b.box, b.runners.filter((r) => r.className === className && r.repo === repo && holding.has(r.state)).length]));
			let waiting = Math.max(0, assigned - [...held.values()].reduce((a, b) => a + b, 0));
			for (const b of servers) {
				const add = Math.min(room.get(b.box) ?? 0, waiting);
				room.set(b.box, (room.get(b.box) ?? 0) - add);
				waiting -= add;
				const list = out.get(b.box) ?? [];
				list.push({ repo, className, jobs: (held.get(b.box) ?? 0) + add, repos: add > 0 ? pending : [] });
				out.set(b.box, list);
			}
		}
	}
	return out;
}

/** The runners a box may still start for a class, up to its max or the dashboard's lower limit: none while it drains, holds the class's starts or lacks disk. */
function free(b: BoxStatus, className: ClassName, limits: readonly Limit[], now: number): number {
	const c = b.classes.find((c) => c.name === className);
	if (c === undefined || b.draining || b.disk.free < b.disk.floor || (c.heldUntil !== null && c.heldUntil > now)) return 0;
	const active = b.runners.filter((r) => r.className === className && r.state !== 'exited').length;
	const limit = limits.find((l) => l.box === b.box && l.className === className);
	return Math.max(0, Math.min(c.max, limit?.max ?? c.max) - active);
}

/** Each scale set of the class as the box holding its session reports it: the one with the most assigned jobs. */
function scaleSets(servers: readonly BoxStatus[], className: ClassName) {
	const byRepo = new Map<RepoName, { repo: RepoName; assigned: number; pending: RepoName[] }>();
	for (const b of servers) {
		for (const ss of b.scaleSets) {
			if (ss.className !== className) continue;
			const seen = byRepo.get(ss.repo);
			if (seen === undefined || ss.assigned > seen.assigned) byRepo.set(ss.repo, { repo: ss.repo, assigned: ss.assigned, pending: ss.pending });
		}
	}
	return [...byRepo.values()].sort((a, b) => (a.repo < b.repo ? -1 : a.repo > b.repo ? 1 : 0));
}
