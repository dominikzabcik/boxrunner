import type { Box } from './box';
import { variable } from './fallback';
import { dayOf, newer, type Priority, type Repo, type RunnerState } from './model';
import { escape } from './page';
import { retentionDays } from './store';
import { version as workerVersion } from './version';

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
		classes: { name: string; priority: Priority; max: number; running: number; starting: number; paused: boolean }[];
		runners: { name: string; repo: string; class: string; state: RunnerState; since: string; seconds: number }[];
	}[];
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
	}[];
	totals: { today: Totals; last30Days: Totals };
	dollarsPerMinute: number;
	/** The served repositories as the agent last fetched them; null before it first did. */
	repos: string[] | null;
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
	const served = classes.ok ? classes.classes : [];
	const pauses = store.pauses().filter((pause) => served.includes(pause.className));
	const paused = new Set<string>(pauses.map((pause) => pause.className));
	return {
		generatedAt: iso(now),
		workerVersion,
		boxes: store.boxes().map(({ status, seenAt }) => {
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
		})),
		totals: { today: totals(days.filter((day) => day.day === today)), last30Days: totals(days) },
		dollarsPerMinute,
		repos: repos === undefined ? null : (JSON.parse(repos) as Repo[]).map((repo) => repo.name),
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

function renderBox(box: Dashboard['boxes'][number], index: number, workerVersion: string) {
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
	[{ title: 'Class' }, { title: 'Priority' }, { title: 'Running', number: true }, { title: 'Starting', number: true }, { title: 'Max', number: true }],
	box.classes.map((c) => [`${escape(c.name)}${c.paused ? ' (paused)' : ''}`, c.priority, String(c.running), String(c.starting), String(c.max)]),
	'The box reported no classes.',
)}
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
		{ title: 'Result' },
		{ title: 'Waited', number: true },
		{ title: 'Took', number: true },
		{ title: 'Finished' },
	],
	jobs.map((job) => [
		escape(job.repo),
		escape(job.class),
		escape(job.name),
		job.result === 'failed' ? `<span class="alert">${escape(job.result)}</span>` : escape(job.result),
		job.started === null ? 'Never started' : job.waitSeconds === null ? 'Unknown' : duration(job.waitSeconds),
		duration(job.durationSeconds),
		time(job.finished, ago(job.secondsSinceFinished)),
	]),
	'No finished jobs in the last 30 days.',
)}
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
	else if (alerts.host === null || alerts.minutes === null) lines.push('<p>Off: set <code>ALERT_WEBHOOK</code> to hear when the box goes silent.</p>');
	else {
		const host = `<code>${escape(alerts.host)}</code>`;
		lines.push(`<p>After ${plural(alerts.minutes, 'minute')} without a heartbeat, the Worker posts to ${host}, and again once the box is back.</p>`);
		if (alerts.sentDown !== fallback.down && alerts.problem !== null) {
			lines.push(`<p class="alert">The Worker could not post the ${fallback.down ? 'down' : 'recovery'} alert: ${escape(alerts.problem)}. It tries again.</p>`);
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

function renderServed(data: Dashboard, csrf: string) {
	const { repos } = data;
	return `<section aria-labelledby="served">
<h2 id="served">What this Worker serves</h2>
<h3>Repositories</h3>
${
	repos === null
		? '<p>The box has not asked for its repositories yet.</p>'
		: repos.length === 0
			? '<p>None: install the App on a private repository.</p>'
			: `<ul>\n${repos.map((repo) => `<li>${escape(repo)}</li>`).join('\n')}\n</ul>`
}
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
			: data.boxes.map((box, index) => renderBox(box, index, data.workerVersion)).join('\n');
	return [boxes, renderJobs(data), renderTotals(data), renderServed(data, csrf)].join('\n');
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
