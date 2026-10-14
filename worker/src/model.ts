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

/** A scale set the Worker created or adopted, for one repository and class. */
export type OwnedScaleSet = { repo: Repo; className: ClassName; id: ScaleSetId };

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
	className: matching<ClassName>(classPattern),
	runnerName: matching<RunnerName>(runnerNamePattern),
	boxName: matching<BoxName>(boxNamePattern),
	login: matching<Login>(loginPattern),
	sessionId: (value: unknown) => matching<SessionId>(uuidPattern)(value)?.toLowerCase() as SessionId | undefined,
	scaleSetId: (value: unknown) => positiveInteger(value) as ScaleSetId | undefined,
	runnerId: (value: unknown) => positiveInteger(value) as RunnerId | undefined,
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

/** Reads the optional `ADMINS` variable: logins besides the App's owner that may sign in, compared case-insensitively. */
export function mayOpenDashboard(owner: string | null, admins: string | undefined): (login: Login) => boolean {
	const allowed = new Set(
		[owner ?? '', ...(admins ?? '').split(',')].map((name) => name.trim().toLowerCase()).filter(Boolean),
	);
	return (login) => allowed.has(login.toLowerCase());
}

// ---- Heartbeat: what the agent reports about its box ----

export const runnerStates = ['requested', 'starting', 'running', 'removing', 'exited'] as const;
export type RunnerState = (typeof runnerStates)[number];
export type Priority = 'normal' | 'idle';

/** Times are milliseconds since the epoch. */
export type BoxRunner = { name: RunnerName; repo: RepoName; className: ClassName; state: RunnerState; since: number };

/** A job one of the box's runners finished; `startedAt` is null when it was canceled before it started. */
export type FinishedJob = {
	runner: RunnerName;
	repo: RepoName;
	className: ClassName;
	name: string;
	result: string;
	queuedAt: number;
	startedAt: number | null;
	finishedAt: number;
};

export type BoxStatus = {
	box: BoxName;
	version: string;
	image: string;
	disk: { free: number; floor: number };
	classes: { name: ClassName; priority: Priority; max: number }[];
	runners: BoxRunner[];
};

export type Heartbeat = BoxStatus & { jobs: FinishedJob[] };

export const heartbeatLimits = { classes: 32, runners: 128, jobs: 50 } as const;

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

const rfc3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/;
const instant = (value: unknown) => {
	const time = typeof value === 'string' && rfc3339.test(value) ? Date.parse(value) : Number.NaN;
	return Number.isFinite(time) ? time : undefined;
};

/** Parses `POST /v1/heartbeat`. Unknown fields are ignored, so a newer agent can add some. */
export function parseHeartbeat(body: Record<string, unknown>): { ok: true; heartbeat: Heartbeat } | { ok: false; field: string } {
	try {
		const disk = record(body.disk, 'disk');
		return {
			ok: true,
			heartbeat: {
				box: need(parse.boxName(body.box), 'box'),
				version: need(typeof body.version === 'string' && semverPattern.test(body.version) && body.version.length <= 64 ? body.version : undefined, 'version'),
				image: need(label(128)(body.image), 'image'),
				disk: { free: need(count(disk.free), 'disk.free'), floor: need(count(disk.floor), 'disk.floor') },
				classes: list(body.classes, 'classes', heartbeatLimits.classes).map((raw, i) => {
					const at = `classes[${i}]`;
					const value = record(raw, at);
					return {
						name: need(parse.className(value.name), `${at}.name`),
						priority: need(oneOf<Priority>(['normal', 'idle'])(value.priority), `${at}.priority`),
						max: need(positiveInteger(value.max), `${at}.max`),
					};
				}),
				runners: list(body.runners, 'runners', heartbeatLimits.runners).map((raw, i) => {
					const at = `runners[${i}]`;
					const value = record(raw, at);
					return {
						name: need(parse.runnerName(value.name), `${at}.name`),
						repo: need(parse.repo(value.repo), `${at}.repo`),
						className: need(parse.className(value.class), `${at}.class`),
						state: need(oneOf(runnerStates)(value.state), `${at}.state`),
						since: need(instant(value.since), `${at}.since`),
					};
				}),
				jobs: list(body.jobs, 'jobs', heartbeatLimits.jobs).map((raw, i) => {
					const at = `jobs[${i}]`;
					const value = record(raw, at);
					return {
						runner: need(parse.runnerName(value.runner), `${at}.runner`),
						repo: need(parse.repo(value.repo), `${at}.repo`),
						className: need(parse.className(value.class), `${at}.class`),
						name: need(label(200)(value.name), `${at}.name`),
						result: need(matching(/^[a-z_]{1,32}$/)(value.result), `${at}.result`),
						queuedAt: need(instant(value.queued), `${at}.queued`),
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
