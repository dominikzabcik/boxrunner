import type { Box } from './box';
import { variable } from './fallback';
import { dayOf, forkProblems, newer, type ForkPullRequests, type Limit, type Priority, type Repo, type RunnerState } from './model';
import { escape } from './page';
import { retentionDays } from './store';
import { slowAfterMs, unservedReason, type UnservedReason } from './unserved';
import { version as workerVersion } from './version';
import { cause } from './waits';

/** GitHub's published price for its standard (2-core) Linux runner. */
export const dollarsPerMinute = 0.006;
/** The agent reports every 30 seconds; four missed heartbeats mean the box is off. */
export const offlineAfterSeconds = 120;
const recentJobCount = 30;

type Totals = { jobs: number; minutes: number; dollars: number };

/** What `GET /dashboard.json` answers and the page shows. Times are ISO 8601; ages are seconds before `generatedAt`. */
export type Dashboard = {
	generatedAt: string;
	workerVersion: string;
	boxes: {
		name: string;
		version: string;
		/** boxrunner's latest release as the box last found it, or null. */
		latest: string | null;
		image: string;
		lastSeen: string;
		secondsSinceSeen: number;
		online: boolean;
		disk: { free: number; floor: number };
		classes: {
			name: string;
			priority: Priority;
			max: number;
			running: number;
			starting: number;
			paused: boolean;
			/** The cap set for the class on this box on the dashboard, or null. */
			limit: { max: number; by: string; since: string; seconds: number } | null;
		}[];
		runners: { name: string; repo: string; class: string; state: RunnerState; since: string; seconds: number }[];
	}[];
	/** Devices paired to read this dashboard and set limits, such as a laptop's menu bar app. */
	devices: { id: string; name: string; by: string; paired: string; lastSeen: string; secondsSinceSeen: number }[];
	/** Scale sets whose assigned jobs have no runner requested, starting or running, as the boxes last reported them. */
	waiting: { repo: string; class: string; box: string; since: string; seconds: number; overdue: boolean; cause: string | null }[];
	jobs: {
		runner: string;
		repo: string;
		class: string;
		name: string;
		result: string;
		queued: string | null;
		started: string | null;
		finished: string;
		secondsSinceFinished: number;
		waitSeconds: number | null;
		durationSeconds: number;
		minutes: number;
		/** The box that ran it; null for a job stored before the Worker kept it. */
		box: string | null;
		/** CPU time across all cores and the most memory the job held, in bytes; null when its box did not measure them. */
		cpuSeconds: number | null;
		memoryPeak: number | null;
	}[];
	/** Queued jobs no box here will run, oldest first, from the App's `workflow_job` event; see docs/unserved-jobs.md. */
	unserved: {
		repo: string;
		workflow: string | null;
		name: string;
		labels: string[];
		url: string;
		reason: UnservedReason;
		queued: string;
		waitSeconds: number;
	}[];
	/** When GitHub's last signed webhook delivery arrived; null before any did. */
	webhook: { lastDelivery: string | null; secondsSince: number | null };
	totals: { today: Totals; last30Days: Totals };
	dollarsPerMinute: number;
	/** The served repositories as the agent last fetched them; null before it first did. */
	repos: string[] | null;
	/** Each served repository's fork pull request settings as last read; null before a read succeeded. */
	forkPullRequests: Record<string, ForkPullRequests | null>;
	/** With `ORG` set, the organization, its runner group and why the Worker serves none of it, as the last listing found them; `problem` is the variables' own while they are unreadable. Null while the Worker serves repositories one by one. */
	organization: { name: string; group: string; problem: string | null } | null;
	classes: string[];
	classesProblem: string | null;
	/** The Worker's classes paused on the dashboard: who paused each and when. */
	paused: { class: string; by: string; since: string; seconds: number }[];
	fallback: {
		/** Minutes without a heartbeat before jobs go to GitHub's runners; null while fallback is off. */
		minutes: number | null;
		configProblem: string | null;
		/** Whether jobs should go to GitHub's runners now, since when, and whether every served repository's variable says so. */
		down: boolean;
		since: string | null;
		secondsSince: number | null;
		written: boolean;
		/** Why the last write of the variable failed. */
		problem: string | null;
	};
	alerts: {
		/** The webhook's host, null while alerts are off. The rest of its address is a secret and never shown. */
		host: string | null;
		/** Minutes without a heartbeat before the Worker posts; `fallback.minutes` while fallback is on. */
		minutes: number | null;
		configProblem: string | null;
		/** Whether the last alert that reached the webhook said the box is down. */
		sentDown: boolean;
		/** Why the last post failed. */
		problem: string | null;
	};
};

const iso = (time: number) => new Date(time).toISOString();
const secondsBetween = (from: number, to: number) => Math.max(0, Math.round((to - from) / 1000));

function totals(days: { jobs: number; minutes: number }[]): Totals {
	const jobs = days.reduce((sum, day) => sum + day.jobs, 0);
	const minutes = days.reduce((sum, day) => sum + day.minutes, 0);
	return { jobs, minutes, dollars: Math.round(minutes * dollarsPerMinute * 100) / 100 };
}

/** Reads only the Durable Object: a page view never calls GitHub. */
export function dashboardData(box: Box, now: number): Dashboard {
	const { store } = box;
	const classes = box.classes();
	const fallback = box.fallback();
	const alerts = box.alerts();
	const { webhook, afterMs } = box.watch();
	const today = dayOf(now);
	const watch = store.fallback();
	const days = store.daysFrom(today - retentionDays + 1);
	const repos = store.lastCached('repos');
	const servedRepos: Repo[] = repos === undefined ? [] : (JSON.parse(repos) as Repo[]);
	const served = classes.ok ? classes.classes : [];
	const allPauses = store.pauses();
	const pauses = allPauses.filter((pause) => served.includes(pause.className));
	const paused = new Set<string>(pauses.map((pause) => pause.className));
	// A box may report a class the Worker no longer serves; its wait still names the pause.
	const pausedAnywhere = new Set(allPauses.map((pause) => pause.className));
	const boxes = store.boxes();
	const limits = store.limits();
	const servedNow = box.served();
	const delivered = store.lastDelivery();
	return {
		generatedAt: iso(now),
		workerVersion,
		boxes: boxes.map(({ status, seenAt }) => {
			const secondsSinceSeen = secondsBetween(seenAt, now);
			return {
				name: status.box,
				version: status.version,
				latest: status.latest ?? null,
				image: status.image,
				lastSeen: iso(seenAt),
				secondsSinceSeen,
				online: secondsSinceSeen <= offlineAfterSeconds,
				disk: status.disk,
				classes: status.classes.map(({ name, priority, max }) => {
					const states = status.runners.filter((runner) => runner.className === name).map((runner) => runner.state);
					return {
						name,
						priority,
						max,
						running: states.filter((state) => state === 'running').length,
						starting: states.filter((state) => state === 'requested' || state === 'starting').length,
						paused: paused.has(name),
						limit: limitOf(limits.find((l) => l.box === status.box && l.className === name), now),
					};
				}),
				runners: status.runners.map((runner) => ({
					name: runner.name,
					repo: runner.repo,
					class: runner.className,
					state: runner.state,
					since: iso(runner.since),
					seconds: secondsBetween(runner.since, seenAt),
				})),
			};
		}),
		devices: store.devices().map((d) => ({
			id: d.id,
			name: d.name,
			by: d.by,
			paired: iso(d.pairedAt),
			lastSeen: iso(d.seenAt),
			secondsSinceSeen: secondsBetween(d.seenAt, now),
		})),
		waiting: watch.waits.map((wait) => ({
			repo: wait.repo,
			class: wait.className,
			box: wait.box,
			since: iso(wait.since),
			seconds: secondsBetween(wait.since, now),
			overdue: wait.overdue,
			cause: cause(wait, boxes, pausedAnywhere, now),
		})),
		jobs: store.recentJobs(recentJobCount).map((job) => ({
			runner: job.runner,
			repo: job.repo,
			class: job.className,
			name: job.name,
			result: job.result,
			queued: job.queuedAt === null ? null : iso(job.queuedAt),
			started: job.startedAt === null ? null : iso(job.startedAt),
			finished: iso(job.finishedAt),
			secondsSinceFinished: secondsBetween(job.finishedAt, now),
			waitSeconds: job.startedAt === null || job.queuedAt === null ? null : secondsBetween(job.queuedAt, job.startedAt),
			durationSeconds: job.startedAt === null ? 0 : secondsBetween(job.startedAt, job.finishedAt),
			minutes: job.minutes,
			box: job.box,
			cpuSeconds: job.usage?.cpuSeconds ?? null,
			memoryPeak: job.usage?.memoryPeak ?? null,
		})),
		unserved: store.queuedJobs(now).flatMap((job) => {
			const reason = unservedReason(job, servedNow, now - job.queuedAt);
			if (reason === null) return [];
			const { repo, workflow, name, labels, url } = job;
			return [{ repo, workflow, name, labels, url, reason, queued: iso(job.queuedAt), waitSeconds: secondsBetween(job.queuedAt, now) }];
		}),
		webhook: { lastDelivery: delivered === undefined ? null : iso(delivered), secondsSince: delivered === undefined ? null : secondsBetween(delivered, now) },
		totals: { today: totals(days.filter((day) => day.day === today)), last30Days: totals(days) },
		dollarsPerMinute,
		repos: repos === undefined ? null : servedRepos.map((repo) => repo.name),
		forkPullRequests: Object.fromEntries(servedRepos.map((repo) => [repo.name, box.lastForkPullRequests(repo.id)])),
		organization: organization(box),
		classes: [...served],
		classesProblem: classes.ok ? null : classes.problem,
		paused: pauses.map((pause) => ({ class: pause.className, by: pause.by, since: iso(pause.since), seconds: secondsBetween(pause.since, now) })),
		fallback: {
			minutes: fallback.ok && fallback.afterMs !== null ? fallback.afterMs / 60_000 : null,
			configProblem: fallback.ok ? null : fallback.problem,
			down: watch.down,
			since: watch.since === 0 ? null : iso(watch.since),
			secondsSince: watch.since === 0 ? null : secondsBetween(watch.since, now),
			written: watch.written,
			problem: watch.problem,
		},
		alerts: {
			host: webhook?.host ?? null,
			minutes: webhook === null || afterMs === null ? null : afterMs / 60_000,
			configProblem: alerts.ok ? null : alerts.problem,
			sentDown: watch.alert.down,
			problem: watch.alert.problem,
		},
	};
}

function organization(box: Box): Dashboard['organization'] {
	const config = box.orgConfig();
	if (!config.ok) return { name: '', group: '', problem: `The Worker’s ${config.problem}.` };
	if (config.org === null) return null;
	const found = box.org();
	return { name: config.org.login, group: config.org.group, problem: found?.problem ?? null };
}

// ---- Rendering ----

const integer = new Intl.NumberFormat('en-US');
const plural = (count: number, unit: string) => `${integer.format(count)} ${unit}${count === 1 ? '' : 's'}`;

function ago(seconds: number) {
	if (seconds < 60) return `${plural(seconds, 'second')} ago`;
	if (seconds < 3600) return `${plural(Math.floor(seconds / 60), 'minute')} ago`;
	if (seconds < 172_800) return `${plural(Math.floor(seconds / 3600), 'hour')} ago`;
	return `${plural(Math.floor(seconds / 86_400), 'day')} ago`;
}

function duration(seconds: number) {
	if (seconds < 60) return `${seconds} s`;
	const minutes = Math.floor(seconds / 60);
	if (minutes < 60) return `${minutes} min ${seconds % 60} s`;
	return `${Math.floor(minutes / 60)} h ${minutes % 60} min`;
}

const gigabytes = (bytes: number) => `${(bytes / 1e9).toFixed(1)} GB`;
const dollars = (amount: number) => `$${amount.toFixed(2)}`;
const time = (at: string, text: string) => `<time datetime="${at}">${escape(text)}</time>`;

type Column = { title: string; number?: boolean };

function table(columns: Column[], rows: string[][], empty: string) {
	if (rows.length === 0) return `<p>${escape(empty)}</p>`;
	const cell = (column: Column, html: string) => `<td${column.number ? ' class="number"' : ''}>${html}</td>`;
	return `<div class="scroll"><table>
<thead><tr>${columns.map((c) => `<th scope="col"${c.number ? ' class="number"' : ''}>${escape(c.title)}</th>`).join('')}</tr></thead>
<tbody>
${rows.map((row) => `<tr>${row.map((html, i) => cell(columns[i]!, html)).join('')}</tr>`).join('\n')}
</tbody>
</table></div>`;
}

function renderTotals({ totals: { today, last30Days } }: Dashboard) {
	const row = (label: string, t: Totals) =>
		`<tr><th scope="row">${label}</th><td class="number">${integer.format(t.jobs)}</td><td class="number">${integer.format(t.minutes)}</td><td class="number">${dollars(t.dollars)}</td></tr>`;
	return `<section aria-labelledby="totals">
<h2 id="totals">GitHub minutes not billed</h2>
<div class="scroll"><table>
<thead><tr><td></td><th scope="col" class="number">Jobs</th><th scope="col" class="number">Minutes</th><th scope="col" class="number">At GitHub’s price</th></tr></thead>
<tbody>
${row('Today (UTC)', today)}
${row('Last 30 days', last30Days)}
</tbody>
</table></div>
<p class="hint">Each job’s time is rounded up to whole minutes, as GitHub bills it, and valued at $${dollarsPerMinute} a minute, the price GitHub publishes for its standard 2-core Linux runner. Minutes within your plan’s free allowance would not have cost money.</p>
</section>`;
}

const upgradingDocs = 'https://boxrunner.zabcik.me/docs/upgrading/';

/** A version, and the newer release when there is one. */
function versionWithLatest(version: string, latest: string | null) {
	if (latest === null || !newer(latest, version)) return escape(version);
	return `${escape(version)}, <span class="alert">${escape(latest)} is out</span> (<a href="${upgradingDocs}">upgrading</a>)`;
}

function limitOf(limit: Limit | undefined, now: number) {
	return limit === undefined ? null : { max: limit.max, by: limit.by, since: iso(limit.since), seconds: secondsBetween(limit.since, now) };
}

/** A class's limit on a box, and the form that sets it or, left empty, removes it. */
function limitForm(csrf: string, box: string, c: Dashboard['boxes'][number]['classes'][number]) {
	const set = c.limit === null ? '' : `<p>${c.limit.max} by ${escape(c.limit.by)}, ${time(c.limit.since, ago(c.limit.seconds))}</p>`;
	return `${set}<form method="post" action="/boxes/limit" class="inline">
<input type="hidden" name="csrf" value="${escape(csrf)}">
<input type="hidden" name="box" value="${escape(box)}">
<input type="hidden" name="class" value="${escape(c.name)}">
<input type="number" name="max" min="0" max="${c.max}" value="${c.limit?.max ?? ''}" aria-label="Limit for ${escape(c.name)} on ${escape(box)}">
<button type="submit" aria-label="Set the limit for ${escape(c.name)} on ${escape(box)}">Set</button>
</form>`;
}

function renderBox(box: Dashboard['boxes'][number], index: number, workerVersion: string, csrf: string) {
	const seen = `last seen ${time(box.lastSeen, ago(box.secondsSinceSeen))}`;
	const belowFloor = box.disk.free < box.disk.floor;
	return `<section aria-labelledby="box-${index}">
<h2 id="box-${index}">${escape(box.name)}</h2>
${box.online ? `<p>Online, ${seen}</p>` : `<p class="alert">Offline, ${seen}</p>`}
<dl>
<dt>Agent</dt><dd>${versionWithLatest(box.version, box.latest)}</dd>
<dt>Worker</dt><dd>${versionWithLatest(workerVersion, box.latest)}</dd>
<dt>Image</dt><dd><code>${escape(box.image)}</code></dd>
<dt>Disk</dt><dd${belowFloor ? ' class="alert"' : ''}>${gigabytes(box.disk.free)} free, floor ${gigabytes(box.disk.floor)}${belowFloor ? ', so no runner starts' : ''}</dd>
</dl>
<h3>Classes</h3>
${table(
	[{ title: 'Class' }, { title: 'Priority' }, { title: 'Running', number: true }, { title: 'Starting', number: true }, { title: 'Max', number: true }, { title: 'Limit' }],
	box.classes.map((c) => [`${escape(c.name)}${c.paused ? ' (paused)' : ''}`, c.priority, String(c.running), String(c.starting), String(c.max), limitForm(csrf, box.name, c)]),
	'The box reported no classes.',
)}
${box.classes.length === 0 ? '' : '<p class="hint">A limit caps a class on this box below its max, and 0 stops it taking jobs. Runners waiting for a job over the limit go, running jobs finish. The box learns of it within 30 seconds; leave the field empty to remove the limit.</p>'}
<h3>Runners</h3>
${table(
	[{ title: 'Runner' }, { title: 'Repository' }, { title: 'Class' }, { title: 'State' }, { title: 'For', number: true }],
	box.runners.map((r) => [escape(r.name), escape(r.repo), escape(r.class), r.state, duration(r.seconds)]),
	'No runners right now.',
)}
</section>`;
}

function renderJobs({ jobs }: Dashboard) {
	return `<section aria-labelledby="jobs">
<h2 id="jobs">Recent jobs</h2>
${table(
	[
		{ title: 'Repository' },
		{ title: 'Class' },
		{ title: 'Job' },
		{ title: 'Box' },
		{ title: 'Result' },
		{ title: 'Waited', number: true },
		{ title: 'Took', number: true },
		{ title: 'CPU', number: true },
		{ title: 'Peak memory', number: true },
		{ title: 'Finished' },
	],
	jobs.map((job) => [
		escape(job.repo),
		escape(job.class),
		escape(job.name),
		job.box === null ? 'Unknown' : escape(job.box),
		job.result === 'failed' ? `<span class="alert">${escape(job.result)}</span>` : escape(job.result),
		job.started === null ? 'Never started' : job.waitSeconds === null ? 'Unknown' : duration(job.waitSeconds),
		duration(job.durationSeconds),
		job.cpuSeconds === null ? 'Unknown' : duration(Math.round(job.cpuSeconds)),
		job.memoryPeak === null ? 'Unknown' : gigabytes(job.memoryPeak),
		time(job.finished, ago(job.secondsSinceFinished)),
	]),
	'No finished jobs in the last 30 days.',
)}
</section>`;
}

function renderWaiting({ waiting }: Dashboard) {
	if (waiting.length === 0) return '';
	return `<section aria-labelledby="waiting">
<h2 id="waiting">Jobs waiting for a runner</h2>
${table(
	[{ title: 'Repository' }, { title: 'Class' }, { title: 'Box' }, { title: 'Waiting', number: true }, { title: 'Likely cause' }],
	waiting.map((wait) => [
		escape(wait.repo),
		escape(wait.class),
		escape(wait.box),
		wait.overdue ? `<span class="alert">${duration(wait.seconds)}</span>` : duration(wait.seconds),
		wait.cause === null ? 'Unknown' : escape(wait.cause),
	]),
	'',
)}
<p class="hint">Jobs assigned to a box with no runner requested, starting or running for their repository and class. A box stops reporting a scale set while its class is paused, while the box drains or restarts, so a row stays until a box reports the scale set again.</p>
</section>`;
}

const unservedDocs = 'https://boxrunner.zabcik.me/docs/unserved-jobs/';

const why: Record<UnservedReason, string> = {
	label: 'runs-on is not one of this Worker’s classes',
	public: 'The repository is public, which the Worker never serves',
	archived: 'The repository is archived',
	unlisted: 'The repository is not in REPOS',
	paused: 'Its class is paused',
	slow: `No box took it within ${slowAfterMs / 60_000} minutes`,
};

function renderUnserved({ unserved, webhook }: Dashboard) {
	const heading = '<h2 id="unserved">Jobs nobody here runs</h2>';
	if (webhook.lastDelivery === null || webhook.secondsSince === null) {
		return `<section aria-labelledby="unserved">
${heading}
<p>Off. With the App’s <code>workflow_job</code> event, this lists the jobs that wait for a runner no box here will start (<a href="${unservedDocs}">turning it on</a>).</p>
</section>`;
	}
	return `<section aria-labelledby="unserved">
${heading}
${table(
	[{ title: 'Repository' }, { title: 'Job' }, { title: 'runs-on' }, { title: 'Why' }, { title: 'Waiting', number: true }],
	unserved.map((job) => [
		escape(job.repo),
		`<a href="${escape(job.url)}">${escape(job.workflow === null ? job.name : `${job.workflow} / ${job.name}`)}</a>`,
		`<code>${escape(job.labels.join(', '))}</code>`,
		`<span class="alert">${escape(why[job.reason])}</span>`,
		duration(job.waitSeconds),
	]),
	'None.',
)}
<p class="hint">From GitHub’s <code>workflow_job</code> event; the last delivery arrived ${time(webhook.lastDelivery, ago(webhook.secondsSince))}.</p>
</section>`;
}

function renderFallback({ fallback }: Dashboard) {
	const name = `<code>${variable}</code>`;
	const switched = fallback.since === null || fallback.secondsSince === null ? '' : ` The Worker switched ${time(fallback.since, ago(fallback.secondsSince))}.`;
	const lines: string[] = [];
	if (fallback.configProblem !== null) lines.push(`<p class="alert">The Worker’s ${escape(fallback.configProblem)}, so fallback is off.</p>`);
	else if (fallback.minutes === null) lines.push('<p>Off: jobs wait for the box while it is down.</p>');
	else if (fallback.down) lines.push(`<p class="alert">The box is down, so workflows that read ${name} run on GitHub’s runners.${switched}</p>`);
	else lines.push(`<p>After ${plural(fallback.minutes, 'minute')} without a heartbeat, workflows that read ${name} run on GitHub’s runners.</p>`);
	if (fallback.written !== fallback.down && fallback.problem !== null) {
		lines.push(`<p class="alert">The Worker could not ${fallback.down ? 'set' : 'delete'} ${name}: ${escape(fallback.problem)}. It tries again.</p>`);
	}
	return `<h3>Fallback to GitHub’s runners</h3>\n${lines.join('\n')}`;
}

function renderAlerts({ alerts, fallback }: Dashboard) {
	const lines: string[] = [];
	if (alerts.configProblem !== null) lines.push(`<p class="alert">The Worker’s ${escape(alerts.configProblem)}, so alerts are off.</p>`);
	else if (alerts.host === null || alerts.minutes === null) lines.push('<p>Off: set <code>ALERT_WEBHOOK</code> to hear when the box goes silent or jobs wait.</p>');
	else {
		const host = `<code>${escape(alerts.host)}</code>`;
		const minutes = plural(alerts.minutes, 'minute');
		lines.push(`<p>After ${minutes} without a heartbeat, the Worker posts to ${host}, and again once the box is back. It also posts when a repository’s jobs have waited ${minutes} with no runner starting, and when they no longer wait.</p>`);
		if (alerts.problem !== null) {
			const which = alerts.sentDown === fallback.down ? 'waiting jobs' : fallback.down ? 'down' : 'recovery';
			lines.push(`<p class="alert">The Worker could not post the ${which} alert: ${escape(alerts.problem)}. It tries again.</p>`);
		}
	}
	return `<h3>Alerts</h3>\n${lines.join('\n')}`;
}

function classAction(csrf: string, name: string, paused: boolean) {
	const [action, verb] = paused ? ['resume', 'Resume'] : ['pause', 'Pause'];
	return `<form method="post" action="/classes/${action}">
<input type="hidden" name="csrf" value="${escape(csrf)}">
<input type="hidden" name="class" value="${escape(name)}">
<button type="submit" aria-label="${verb} ${escape(name)}">${verb}</button>
</form>`;
}

function renderClasses({ classes, classesProblem, paused }: Dashboard, csrf: string) {
	if (classesProblem !== null) return `<p class="alert">The Worker’s ${escape(classesProblem)}.</p>`;
	const rows = classes.map((name) => {
		const pause = paused.find((p) => p.class === name);
		const state = pause ? `Paused by ${escape(pause.by)}<br>${time(pause.since, ago(pause.seconds))}` : 'Serving';
		return [escape(name), state, classAction(csrf, name, pause !== undefined)];
	});
	return `${table([{ title: 'Class' }, { title: 'State' }, { title: 'Action' }], rows, '')}
<p class="hint">A paused class starts no new runners on any box. Its running jobs finish, and its new jobs wait at GitHub until you resume it. Boxes learn of a pause or a resume within 30 seconds. An agent from before pausing existed learns of it when it next lists the repositories, within 5 minutes.</p>`;
}

function renderDevices({ devices }: Dashboard, csrf: string) {
	const unpair = (d: Dashboard['devices'][number]) => `<form method="post" action="/devices/unpair">
<input type="hidden" name="csrf" value="${escape(csrf)}">
<input type="hidden" name="id" value="${escape(d.id)}">
<button type="submit" aria-label="Unpair ${escape(d.name)}">Unpair</button>
</form>`;
	return `<section aria-labelledby="devices">
<h2 id="devices">Paired devices</h2>
${table(
	[{ title: 'Device' }, { title: 'Paired by' }, { title: 'Last seen' }, { title: 'Action' }],
	devices.map((d) => [escape(d.name), escape(d.by), time(d.lastSeen, ago(d.secondsSinceSeen)), unpair(d)]),
	'No device is paired.',
)}
<p class="hint">A paired device, such as the boxrunner menu bar app, reads what this page shows and sets limits, as the person who paired it. Pair one from the app; it opens a page here to approve.</p>
</section>`;
}

/** "a", "a and b", "a, b and c". */
const joined = (items: string[]) => (items.length < 2 ? items.join('') : `${items.slice(0, -1).join(', ')} and ${items.at(-1)}`);

/** A served repository, and what its fork pull request settings let through with the settings that stop it. */
function renderRepo(repo: string, problems: ReturnType<typeof forkProblems>) {
	if (problems.length === 0) return `<li>${escape(repo)}</li>`;
	const what = joined(problems.map((p) => p.problem));
	const fixes = joined(problems.map((p) => p.fix));
	return `<li>${escape(repo)}: <span class="alert">runs workflows from fork pull requests ${what}.</span> In its settings, under Actions, General, Fork pull request workflows, ${fixes}, or turn off “Run workflows from fork pull requests”.</li>`;
}

function renderOrganization({ organization }: Dashboard) {
	if (organization === null) return '';
	if (organization.name === '') return `<h3>Organization</h3>\n<p class="alert">${escape(organization.problem ?? '')}</p>`;
	const what = `${escape(organization.name)}, through one scale set per class in the runner group “${escape(organization.group)}”.`;
	const state = organization.problem === null ? '' : ` <span class="alert">${escape(organization.problem)}</span> Until then the box serves none of its repositories.`;
	return `<h3>Organization</h3>\n<p>${what}${state}</p>`;
}

function renderRepos({ repos, forkPullRequests, organization }: Dashboard) {
	if (repos === null) return '<p>The box has not asked for its repositories yet.</p>';
	if (repos.length === 0 && organization !== null) return `<p>None: add a private repository to the runner group “${escape(organization.group)}”.</p>`;
	if (repos.length === 0) return '<p>None: install the App on a private repository.</p>';
	const problems = repos.map((repo) => {
		const settings = forkPullRequests[repo];
		return settings ? forkProblems(settings) : [];
	});
	const hint = problems.some((list) => list.length > 0)
		? '\n<p class="hint">A workflow from a fork pull request runs the fork’s code on your box, and a fork can belong to anyone who can read the repository.</p>'
		: '';
	return `<ul>\n${repos.map((repo, i) => renderRepo(repo, problems[i]!)).join('\n')}\n</ul>${hint}`;
}

function renderServed(data: Dashboard, csrf: string) {
	return `<section aria-labelledby="served">
<h2 id="served">What this Worker serves</h2>
${renderOrganization(data)}
<h3>Repositories</h3>
${renderRepos(data)}
<h3 id="classes">Classes</h3>
${renderClasses(data, csrf)}
${renderFallback(data)}
${renderAlerts(data)}
</section>`;
}

/** The part of the page the script refreshes. Its forms carry the session's CSRF token. */
export function renderLive(data: Dashboard, csrf: string): string {
	const boxes =
		data.boxes.length === 0
			? '<section aria-labelledby="no-box"><h2 id="no-box">Box</h2><p>No box has reported yet. A running agent reports every 30 seconds.</p></section>'
			: data.boxes.map((box, index) => renderBox(box, index, data.workerVersion, csrf)).join('\n');
	return [boxes, renderWaiting(data), renderUnserved(data), renderJobs(data), renderTotals(data), renderServed(data, csrf), renderDevices(data, csrf)].filter(Boolean).join('\n');
}

/** Refreshes the page every 10 seconds while the tab is visible. Without it, a reload does the same. */
export const refreshScript = `const live = document.getElementById('live');
const note = document.getElementById('refreshing');
note.hidden = false;
let timer;
const schedule = () => { clearTimeout(timer); if (!document.hidden) timer = setTimeout(refresh, 10000); };
async function refresh() {
  // Replacing the tables would drop the focus of someone scrolling one with the keyboard.
  if (live.contains(document.activeElement)) return schedule();
  try {
    const response = await fetch('/dashboard.json', { cache: 'no-store' });
    if (response.status === 401) return location.reload();
    if (!response.ok) throw new Error(String(response.status));
    live.innerHTML = (await response.json()).html;
    note.textContent = 'Updates every 10 seconds.';
  } catch {
    note.textContent = 'The last update failed. Trying again in 10 seconds.';
  }
  schedule();
}
document.addEventListener('visibilitychange', () => (document.hidden ? clearTimeout(timer) : refresh()));
schedule();`;
