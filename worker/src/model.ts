import type { AppKey } from './crypto';

declare const brand: unique symbol;
type Brand<T, Name> = T & { readonly [brand]: Name };

/** `owner/name` as the agent sends it; matched against the served list case-insensitively. */
export type RepoName = Brand<string, 'RepoName'>;
/** A runner class the Worker serves: a scale set's name and its only label. */
export type ClassName = Brand<string, 'ClassName'>;
export type ScaleSetId = Brand<number, 'ScaleSetId'>;
export type RunnerId = Brand<number, 'RunnerId'>;
export type SessionId = Brand<string, 'SessionId'>;
export type RunnerName = Brand<string, 'RunnerName'>;
export type BoxName = Brand<string, 'BoxName'>;

/** A private repository the App is installed on, as GitHub names it. */
export type Repo = { name: RepoName; id: number; installationId: number };

/** A private repository's settings for workflows from fork pull requests (Settings, Actions, General). */
export type ForkPullRequests = { runWorkflows: boolean; requireApproval: boolean; sendWriteTokens: boolean; sendSecrets: boolean };

/** Each setting that lets fork pull requests run unchecked on the box or with more than a read token, and how to change it. */
export function forkProblems(settings: ForkPullRequests): { problem: string; fix: string }[] {
	if (!settings.runWorkflows) return [];
	const problems: { problem: string; fix: string }[] = [];
	if (!settings.requireApproval) problems.push({ problem: 'without approval', fix: 'turn on “Require approval for fork pull request workflows”' });
	if (settings.sendWriteTokens) problems.push({ problem: 'with write tokens', fix: 'turn off “Send write tokens to workflows from pull requests”' });
	if (settings.sendSecrets) problems.push({ problem: 'with secrets', fix: 'turn off “Send secrets to workflows from pull requests”' });
	return problems;
}

/**
 * The organization the Worker serves through organization scale sets, as its
 * last listing found it. `id` and `installationId` are null while the App is
 * not installed on it, `groupId` while the runner group was not found, and
 * `problem` says why the Worker serves none of its repositories.
 */
export type Org = { login: Login; id: number | null; installationId: number | null; group: string; groupId: number | null; problem: string | null };

/** What a scale set serves: one repository, or the repositories an organization's runner group admits. */
export type Scope = { kind: 'repo'; repo: Repo } | { kind: 'org'; org: Org & { id: number; installationId: number } };

/** Scale sets and session holders are stored by a number: a repository's id, or minus an organization's account id. */
export const scopeId = (scope: Scope) => (scope.kind === 'repo' ? scope.repo.id : -scope.org.id);

/** How the scope is named to GitHub and in messages: `owner/name`, or the organization's login. */
export const scopeName = (scope: Scope) => (scope.kind === 'repo' ? scope.repo.name : scope.org.login);

/** A scale set the Worker created or adopted, for one scope and class. */
export type OwnedScaleSet = { scope: Scope; className: ClassName; id: ScaleSetId };

export type App = {
	id: number;
	slug: string;
	key: AppKey;
	webhookSecret: string | null;
	clientId: string | null;
	clientSecret: string | null;
	owner: string | null;
};

/** A GitHub account's login: letters, digits and single dashes. */
export type Login = Brand<string, 'Login'>;

const repoPattern = /^[A-Za-z0-9-]{1,39}\/[A-Za-z0-9._-]{1,100}$/;
const loginPattern = /^[A-Za-z0-9][A-Za-z0-9-]{0,38}$/;
/** The agent's semver; the first group is the major. */
export const semverPattern = /^(\d+)\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]*)?$/;

/** Whether release `a` comes after `b`, by their numbers; a pre-release comes before its release. */
export function newer(a: string, b: string): boolean {
	const split = (v: string) => {
		const [core = '', pre = ''] = v.split('+')[0]!.split(/-(.*)/s);
		return { numbers: core.split('.').map(Number), pre };
	};
	const x = split(a);
	const y = split(b);
	for (let i = 0; i < 3; i++) {
		if (x.numbers[i] !== y.numbers[i]) return (x.numbers[i] ?? 0) > (y.numbers[i] ?? 0);
	}
	if (x.pre === y.pre) return false;
	return x.pre === '' || (y.pre !== '' && x.pre > y.pre);
}
const classPattern = /^[a-z0-9][a-z0-9-]{0,62}$/;
const runnerNamePattern = /^[a-z0-9-]{1,64}$/;
const boxNamePattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

const positiveInteger = (value: unknown): number | undefined => {
	const number = typeof value === 'string' && /^[1-9][0-9]{0,15}$/.test(value) ? Number(value) : value;
	return typeof number === 'number' && Number.isSafeInteger(number) && number > 0 ? number : undefined;
};

const matching = <T extends string>(pattern: RegExp) => (value: unknown) =>
	typeof value === 'string' && pattern.test(value) ? (value as T) : undefined;

export const parse = {
	repo: matching<RepoName>(repoPattern),
	/** A repository, or an organization's login for an organization scale set, as a heartbeat names a scale set's scope. */
	scope: (value: unknown) => matching<RepoName>(repoPattern)(value) ?? (matching<Login>(loginPattern)(value) as unknown as RepoName | undefined),
	className: matching<ClassName>(classPattern),
	runnerName: matching<RunnerName>(runnerNamePattern),
	boxName: matching<BoxName>(boxNamePattern),
	login: matching<Login>(loginPattern),
	sessionId: (value: unknown) => matching<SessionId>(uuidPattern)(value)?.toLowerCase() as SessionId | undefined,
	scaleSetId: (value: unknown) => positiveInteger(value) as ScaleSetId | undefined,
	runnerId: (value: unknown) => positiveInteger(value) as RunnerId | undefined,
	/** The repositories of the jobs an organization scale set holds for runners to take: at most 128, each owner/name. */
	jobRepos: (value: unknown): RepoName[] | undefined => {
		if (!Array.isArray(value) || value.length > 128) return undefined;
		const repos = value.map((repo) => matching<RepoName>(repoPattern)(repo));
		return repos.every((repo) => repo !== undefined) ? (repos as RepoName[]) : undefined;
	},
};

// GitHub-hosted runners answer to these labels; a scale set named after one
// would take jobs meant for GitHub's machines.
const hostedLabel = /^(ubuntu-|windows-|macos-)|^self-hosted$/i;

export type Classes = { ok: true; classes: readonly ClassName[] } | { ok: false; problem: string };

/** Reads the `CLASSES` variable: a comma-separated list of class names. */
export function parseClasses(value: string | undefined): Classes {
	const names = (value ?? '')
		.split(',')
		.map((name) => name.trim())
		.filter(Boolean);
	if (names.length === 0) return { ok: false, problem: 'CLASSES is empty' };
	const classes: ClassName[] = [];
	for (const name of names) {
		const className = parse.className(name);
		if (!className) return { ok: false, problem: `CLASSES has "${name}", which is not lower-case letters, digits and dashes` };
		if (hostedLabel.test(name)) return { ok: false, problem: `CLASSES has "${name}", a label GitHub's own runners use` };
		if (!classes.includes(className)) classes.push(className);
	}
	return { ok: true, classes };
}

/**
 * Reads the optional `REPOS` variable, a comma-separated list of `owner/name`.
 * The Worker then serves only those of the App's repositories; empty serves
 * every private repository the App is installed on.
 */
export function repoFilter(value: string | undefined): (name: string) => boolean {
	const names = new Set(
		(value ?? '')
			.split(',')
			.map((name) => name.trim().toLowerCase())
			.filter(Boolean),
	);
	return names.size === 0 ? () => true : (name) => names.has(name.toLowerCase());
}

/** The organization and runner group named by `ORG` and `RUNNER_GROUP`; null serves repositories one by one. */
export type OrgConfig = { ok: true; org: { login: Login; group: string } | null } | { ok: false; problem: string };

// GitHub's own limit on a runner group's name is not documented; this keeps it printable and short.
const groupPattern = /^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$/;

/**
 * Reads the optional `ORG` and `RUNNER_GROUP` variables. With `ORG` set, the
 * Worker makes one scale set per class for the organization, in the runner
 * group `RUNNER_GROUP`, which must then be set too.
 */
export function parseOrg(org: string | undefined, group: string | undefined): OrgConfig {
	const login = (org ?? '').trim();
	const name = (group ?? '').trim();
	if (login === '') return name === '' ? { ok: true, org: null } : { ok: false, problem: 'RUNNER_GROUP is set without ORG' };
	const parsed = parse.login(login);
	if (!parsed) return { ok: false, problem: `ORG is "${login}", which is not a GitHub login` };
	if (name === '') return { ok: false, problem: 'ORG is set without RUNNER_GROUP, the runner group its scale sets go in' };
	if (!groupPattern.test(name)) return { ok: false, problem: `RUNNER_GROUP is "${name}", which is not 1 to 64 letters, digits, spaces, dots, dashes or underscores` };
	return { ok: true, org: { login: parsed, group: name } };
}

/** The organization that setup makes the App under, or null for the account that runs setup. */
export type AppOwnerConfig = { ok: true; owner: Login | null } | { ok: false; problem: string };

/** Reads the optional `APP_OWNER` variable. With `ORG` set, the App belongs to that organization, so `APP_OWNER` may only repeat it. */
export function parseAppOwner(value: string | undefined, org: Login | null): AppOwnerConfig {
	const login = (value ?? '').trim();
	const parsed = parse.login(login);
	if (login !== '' && !parsed) return { ok: false, problem: `APP_OWNER is "${login}", which is not a GitHub login` };
	if (org !== null && parsed && parsed.toLowerCase() !== org.toLowerCase()) {
		return { ok: false, problem: `APP_OWNER is "${login}", but the App of a Worker that serves an organization belongs to that organization, ORG "${org}"` };
	}
	return { ok: true, owner: org ?? parsed ?? null };
}

/** A runner group as GitHub's REST API describes it. */
export type RunnerGroup = { id: number; name: string; visibility: string; allowsPublicRepositories: boolean };

/**
 * Why the Worker must not register runners in the group, or null when it may:
 * the group admits only repositories picked one by one, and no public one.
 */
export function groupProblem(org: string, group: RunnerGroup): string | null {
	if (group.allowsPublicRepositories) return `The runner group “${group.name}” of ${org} allows public repositories; turn off “Allow public repositories” in its settings.`;
	if (group.visibility !== 'selected') {
		const admits = group.visibility === 'all' ? 'every repository' : group.visibility === 'private' ? 'every private repository' : `repositories by “${group.visibility}”`;
		return `The runner group “${group.name}” admits ${admits} of ${org}; set its repository access to selected repositories.`;
	}
	return null;
}

/** A class paused on the dashboard: boxes start no new runners for it until someone resumes it. */
export type Pause = { className: ClassName; since: number; by: Login };

/** A cap set on the dashboard for one class on one box, below the class's max there; 0 starts none. */
export type Limit = { box: BoxName; className: ClassName; max: number; since: number; by: Login };

/** Splits the Worker's classes into those boxes serve now and those paused on the dashboard. */
export function splitPaused(classes: readonly ClassName[], pauses: readonly Pause[]) {
	const paused = new Set(pauses.map((pause) => pause.className));
	return { serving: classes.filter((name) => !paused.has(name)), paused: classes.filter((name) => paused.has(name)) };
}

export type FallbackConfig = { ok: true; afterMs: number | null } | { ok: false; problem: string };

/** Reads a variable of whole minutes from 2 to 1440; empty gives null. */
function parseMinutes(name: string, value: string | undefined): FallbackConfig {
	const text = (value ?? '').trim();
	if (text === '') return { ok: true, afterMs: null };
	const minutes = /^\d{1,4}$/.test(text) ? Number(text) : Number.NaN;
	if (!(minutes >= 2 && minutes <= 1440)) {
		return { ok: false, problem: `${name} is "${text}", not a whole number of minutes from 2 to 1440` };
	}
	return { ok: true, afterMs: minutes * 60_000 };
}

/**
 * Reads the optional `FALLBACK_MINUTES` variable: how long the box may stay
 * silent before its jobs go to GitHub's runners. Empty turns fallback off.
 */
export const parseFallback = (value: string | undefined) => parseMinutes('FALLBACK_MINUTES', value);

/** Long enough that an agent upgrade or a reboot of the box usually passes without an alert. */
export const defaultAlertMinutes = 10;

export type AlertConfig = { ok: true; webhook: URL | null; afterMs: number } | { ok: false; problem: string };

/**
 * Reads the optional `ALERT_WEBHOOK` secret, an https address the Worker posts
 * to when the box goes silent or jobs wait and when that ends, and `ALERT_MINUTES`.
 * Empty turns alerts off. A problem never quotes the address: it is a secret.
 */
export function parseAlerts(webhook: string | undefined, minutes: string | undefined): AlertConfig {
	const address = (webhook ?? '').trim();
	const after = parseMinutes('ALERT_MINUTES', minutes);
	if (!after.ok) return after;
	const afterMs = after.afterMs ?? defaultAlertMinutes * 60_000;
	if (address === '') return { ok: true, webhook: null, afterMs };
	const url = URL.canParse(address) ? new URL(address) : null;
	if (url?.protocol !== 'https:') return { ok: false, problem: 'ALERT_WEBHOOK is not an https:// address' };
	return { ok: true, webhook: url, afterMs };
}

/** Reads the optional `ADMINS` variable: logins besides the App's owner that may sign in, compared case-insensitively. */
export function mayOpenDashboard(owner: string | null, admins: string | undefined): (login: Login) => boolean {
	const allowed = new Set([owner ?? '', ...adminLogins(admins)].map((name) => name.trim().toLowerCase()).filter(Boolean));
	return (login) => allowed.has(login.toLowerCase());
}

/** The names in `ADMINS`, trimmed, without empty ones. */
export const adminLogins = (admins: string | undefined) =>
	(admins ?? '')
		.split(',')
		.map((name) => name.trim())
		.filter(Boolean);

// ---- Heartbeat: what the agent reports about its box ----

export const runnerStates = ['requested', 'starting', 'running', 'removing', 'exited'] as const;
export type RunnerState = (typeof runnerStates)[number];
export type Priority = 'normal' | 'idle';

/** Times are milliseconds since the epoch. */
export type BoxRunner = { name: RunnerName; repo: RepoName; className: ClassName; state: RunnerState; since: number };

/** What a job's container used, from its cgroup: CPU time across all cores, and the most memory it held at once, in bytes. */
export type JobUsage = { cpuSeconds: number; memoryPeak: number };

/**
 * A job one of the box's runners finished; `startedAt` is null when it was canceled before it started, `queuedAt` when an older agent did not record it.
 * `box` is the box that ran it, null for a job stored before the Worker kept it. `usage` is null when the agent saw no sample of its container, or is older than it.
 */
export type FinishedJob = {
	box: BoxName | null;
	usage: JobUsage | null;
	runner: RunnerName;
	repo: RepoName;
	className: ClassName;
	name: string;
	result: string;
	queuedAt: number | null;
	startedAt: number | null;
	finishedAt: number;
};

/**
 * A scale set the box listens to and its assigned jobs, including those runners already took.
 * `pending` names the repositories of the jobs no runner took yet, for an organization scale set; other boxes of the fleet mint runners for them.
 */
export type BoxScaleSet = { repo: RepoName; className: ClassName; assigned: number; pending: RepoName[] };

export type BoxStatus = {
	box: BoxName;
	version: string;
	/** boxrunner's latest release as the agent last found it; null before it did, or from an agent too old to look. */
	latest: string | null;
	image: string;
	disk: { free: number; floor: number };
	/** `heldUntil` is set while the class's runners fail before taking a job, so the box starts them slowly. */
	classes: { name: ClassName; priority: Priority; max: number; heldUntil: number | null }[];
	runners: BoxRunner[];
	/** Whether `boxrunner drain` closed the box's sessions. */
	draining: boolean;
	/** The scale sets the box listens to; empty from an agent too old to send them. */
	scaleSets: BoxScaleSet[];
	/** Where the box stands when several boxes serve a class: lower ranks get jobs first. 0 from an agent too old to send it. */
	rank: number;
};

export type Heartbeat = BoxStatus & { jobs: FinishedJob[] };

/** The agent reports every 30 seconds; three missed heartbeats break its run. */
export const heartbeatGapMs = 90_000;

export const heartbeatLimits = { classes: 32, runners: 128, jobs: 50, scaleSets: 128, pending: 128 } as const;

/** A rank is a whole number from 0 to 1000. */
const rank = (value: unknown) => (typeof value === 'number' && Number.isInteger(value) && value >= 0 && value <= 1000 ? value : undefined);

const dayMs = 86_400_000;
/** The UTC day a time falls on, counted from the epoch. */
export const dayOf = (time: number) => Math.floor(time / dayMs);
export const startOfDay = (day: number) => day * dayMs;

/** GitHub bills each job's time rounded up to whole minutes; a job that never started took none. */
export const billedMinutes = (job: FinishedJob) =>
	job.startedAt === null ? 0 : Math.ceil(Math.max(0, job.finishedAt - job.startedAt) / 60_000);

class Malformed extends Error {}

const need = <T>(value: T | undefined, field: string): T => {
	if (value === undefined) throw new Malformed(field);
	return value;
};

const record = (value: unknown, field: string) =>
	need(typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined, field);

const list = (value: unknown, field: string, max: number) =>
	need(Array.isArray(value) && value.length <= max ? (value as unknown[]) : undefined, field);

const oneOf = <T extends string>(choices: readonly T[]) => (value: unknown) =>
	choices.includes(value as T) ? (value as T) : undefined;

const count = (value: unknown) => (typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : undefined);

const label = (max: number) => (value: unknown) =>
	typeof value === 'string' && value.length > 0 && value.length <= max && !/[\u0000-\u001f\u007f]/.test(value) ? value : undefined;

const semver = (value: unknown) => (typeof value === 'string' && semverPattern.test(value) && value.length <= 64 ? value : undefined);

const seconds = (value: unknown) => (typeof value === 'number' && Number.isFinite(value) && value >= 0 && value < 1e9 ? value : undefined);

const rfc3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/;
const instant = (value: unknown) => {
	const time = typeof value === 'string' && rfc3339.test(value) ? Date.parse(value) : Number.NaN;
	return Number.isFinite(time) ? time : undefined;
};

/** Parses `POST /v1/heartbeat`. Unknown fields are ignored, so a newer agent can add some. */
export function parseHeartbeat(body: Record<string, unknown>): { ok: true; heartbeat: Heartbeat } | { ok: false; field: string } {
	try {
		const disk = record(body.disk, 'disk');
		const box = need(parse.boxName(body.box), 'box');
		return {
			ok: true,
			heartbeat: {
				box,
				version: need(semver(body.version), 'version'),
				latest: body.latest === undefined || body.latest === null ? null : need(semver(body.latest), 'latest'),
				image: need(label(128)(body.image), 'image'),
				disk: { free: need(count(disk.free), 'disk.free'), floor: need(count(disk.floor), 'disk.floor') },
				classes: list(body.classes, 'classes', heartbeatLimits.classes).map((raw, i) => {
					const at = `classes[${i}]`;
					const value = record(raw, at);
					return {
						name: need(parse.className(value.name), `${at}.name`),
						priority: need(oneOf<Priority>(['normal', 'idle'])(value.priority), `${at}.priority`),
						max: need(positiveInteger(value.max), `${at}.max`),
						heldUntil: value.heldUntil === undefined ? null : need(instant(value.heldUntil), `${at}.heldUntil`),
					};
				}),
				runners: list(body.runners, 'runners', heartbeatLimits.runners).map((raw, i) => {
					const at = `runners[${i}]`;
					const value = record(raw, at);
					return {
						name: need(parse.runnerName(value.name), `${at}.name`),
						repo: need(parse.scope(value.repo), `${at}.repo`),
						className: need(parse.className(value.class), `${at}.class`),
						state: need(oneOf(runnerStates)(value.state), `${at}.state`),
						since: need(instant(value.since), `${at}.since`),
					};
				}),
				rank: body.rank === undefined ? 0 : need(rank(body.rank), 'rank'),
				draining: body.draining === undefined ? false : need(typeof body.draining === 'boolean' ? body.draining : undefined, 'draining'),
				scaleSets: list(body.scaleSets ?? [], 'scaleSets', heartbeatLimits.scaleSets).map((raw, i) => {
					const at = `scaleSets[${i}]`;
					const value = record(raw, at);
					return {
						repo: need(parse.scope(value.repo), `${at}.repo`),
						className: need(parse.className(value.class), `${at}.class`),
						assigned: need(count(value.assigned), `${at}.assigned`),
						pending: list(value.pending ?? [], `${at}.pending`, heartbeatLimits.pending).map((repo, j) => need(parse.repo(repo), `${at}.pending[${j}]`)),
					};
				}),
				jobs: list(body.jobs, 'jobs', heartbeatLimits.jobs).map((raw, i) => {
					const at = `jobs[${i}]`;
					const value = record(raw, at);
					const usage = value.usage === undefined || value.usage === null ? null : record(value.usage, `${at}.usage`);
					return {
						box,
						usage:
							usage === null
								? null
								: { cpuSeconds: need(seconds(usage.cpuSeconds), `${at}.usage.cpuSeconds`), memoryPeak: need(count(usage.memoryPeak), `${at}.usage.memoryPeak`) },
						runner: need(parse.runnerName(value.runner), `${at}.runner`),
						repo: need(parse.scope(value.repo), `${at}.repo`),
						className: need(parse.className(value.class), `${at}.class`),
						name: need(label(200)(value.name), `${at}.name`),
						result: need(matching(/^[a-z_]{1,32}$/)(value.result), `${at}.result`),
						queuedAt: value.queued === null || value.queued === undefined ? null : need(instant(value.queued), `${at}.queued`),
						startedAt: value.started === null ? null : need(instant(value.started), `${at}.started`),
						finishedAt: need(instant(value.finished), `${at}.finished`),
					};
				}),
			},
		};
	} catch (error) {
		if (error instanceof Malformed) return { ok: false, field: error.message };
		throw error;
	}
}

// ---- Webhook: GitHub's workflow_job event ----

/** A job as GitHub's `workflow_job` event describes it. `queuedAt` is when GitHub created it. */
export type QueuedJob = {
	id: number;
	repo: RepoName;
	private: boolean;
	archived: boolean;
	labels: string[];
	name: string;
	workflow: string | null;
	/** The job's page on github.com. */
	url: string;
	queuedAt: number;
};

/** What happened to the job: queued, taken by a runner or gone from the queue without one (finished, canceled or held for an environment's approval). */
export type JobEvent = { change: 'queued' | 'started' | 'gone'; job: QueuedJob };

const changes: Partial<Record<string, JobEvent['change']>> = { queued: 'queued', in_progress: 'started', completed: 'gone', waiting: 'gone' };

const flag = (value: unknown) => (typeof value === 'boolean' ? value : undefined);

const jobUrl = (value: unknown) =>
	typeof value === 'string' && value.length <= 512 && /^https:\/\/github\.com\/[^\s"<>]+$/.test(value) ? value : undefined;

/** Parses a `workflow_job` delivery; `event` is null for an action the Worker does not track. Unknown fields are ignored. */
export function parseJobEvent(body: unknown): { ok: true; event: JobEvent | null } | { ok: false; field: string } {
	try {
		const payload = record(body, 'payload');
		const change = typeof payload.action === 'string' ? changes[payload.action] : undefined;
		if (change === undefined) return { ok: true, event: null };
		const job = record(payload.workflow_job, 'workflow_job');
		const repository = record(payload.repository, 'repository');
		return {
			ok: true,
			event: {
				change,
				job: {
					id: need(typeof job.id === 'number' ? positiveInteger(job.id) : undefined, 'workflow_job.id'),
					repo: need(parse.repo(repository.full_name), 'repository.full_name'),
					private: need(flag(repository.private), 'repository.private'),
					archived: need(flag(repository.archived), 'repository.archived'),
					labels: list(job.labels, 'workflow_job.labels', 32).map((value, i) => need(label(100)(value), `workflow_job.labels[${i}]`)),
					name: need(label(200)(job.name), 'workflow_job.name'),
					workflow: job.workflow_name === null || job.workflow_name === undefined ? null : need(label(200)(job.workflow_name), 'workflow_job.workflow_name'),
					url: need(jobUrl(job.html_url), 'workflow_job.html_url'),
					queuedAt: need(instant(job.created_at), 'workflow_job.created_at'),
				},
			},
		};
	} catch (error) {
		if (error instanceof Malformed) return { ok: false, field: error.message };
		throw error;
	}
}
