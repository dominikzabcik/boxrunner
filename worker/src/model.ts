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

const repoPattern = /^[A-Za-z0-9-]{1,39}\/[A-Za-z0-9._-]{1,100}$/;
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
