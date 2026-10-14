import type { BoxName, RepoName, RunnerId, RunnerName, ScaleSetId, SessionId } from './model';

/** GitHub answered with a status the call does not expect, or a body it cannot read. */
export class GitHubError extends Error {
	constructor(
		readonly call: string,
		readonly status: number,
		readonly typeName = '',
	) {
		super(status === 0 ? `GitHub did not answer ${call}` : `GitHub answered ${status} to ${call}`);
	}
}

const restBase = 'https://api.github.com';
const userAgent = 'boxrunner-worker';

type Call = {
	method: 'GET' | 'POST' | 'PATCH' | 'DELETE';
	url: string;
	label: string;
	authorization?: string;
	body?: unknown;
	/** Statuses the caller handles itself; anything else throws a GitHubError. */
	expect: readonly number[];
	rest?: boolean;
};

async function send({ method, url, label, authorization, body, expect, rest = true }: Call) {
	const headers: Record<string, string> = { 'user-agent': userAgent };
	if (rest) {
		headers.accept = 'application/vnd.github+json';
		headers['x-github-api-version'] = '2022-11-28';
	}
	if (authorization) headers.authorization = authorization;
	if (body !== undefined) headers['content-type'] = 'application/json';
	const response = await fetch(url, {
		method,
		headers,
		body: body === undefined ? null : JSON.stringify(body),
	}).catch(() => {
		throw new GitHubError(`${method} ${label}`, 0);
	});
	if (expect.includes(response.status)) return response;
	throw new GitHubError(`${method} ${label}`, response.status, await typeNameOf(response));
}

async function typeNameOf(response: Response) {
	const body: unknown = await response.json().catch(() => undefined);
	return isObject(body) && typeof body.typeName === 'string' ? body.typeName : '';
}

const isObject = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null;
const text = (value: unknown) => (typeof value === 'string' && value !== '' ? value : undefined);
const integer = (value: unknown) => (typeof value === 'number' && Number.isSafeInteger(value) ? value : undefined);

async function read<T>(response: Response, label: string, shape: (body: Record<string, unknown>) => T | undefined) {
	const body: unknown = await response.json().catch(() => undefined);
	const parsed = isObject(body) ? shape(body) : undefined;
	if (parsed === undefined) throw new GitHubError(`${label} (unreadable answer)`, response.status);
	return parsed;
}

const rest = (method: Call['method'], path: string, authorization: string, expect: readonly number[], body?: unknown) =>
	send({ method, url: `${restBase}${path}`, label: path.replace(/\?.*$/, ''), authorization, body, expect });

// ---- GitHub REST, as the App ----

export async function installationIds(jwt: string): Promise<number[]> {
	const ids: number[] = [];
	for (let page = 1; ; page++) {
		const response = await rest('GET', `/app/installations?per_page=100&page=${page}`, `Bearer ${jwt}`, [200]);
		const body: unknown = await response.json().catch(() => undefined);
		if (!Array.isArray(body)) throw new GitHubError('GET /app/installations (unreadable answer)', 200);
		for (const installation of body) {
			const id = isObject(installation) ? integer(installation.id) : undefined;
			if (id !== undefined) ids.push(id);
		}
		if (body.length < 100) return ids;
	}
}

export type Permissions = Partial<Record<'administration' | 'metadata', 'read' | 'write'>>;

/** An installation token; with `repositoryId`, narrowed to that one repository. */
export async function installationToken(
	jwt: string,
	installationId: number,
	permissions: Permissions,
	repositoryId?: number,
): Promise<string> {
	const path = `/app/installations/${installationId}/access_tokens`;
	const body = repositoryId === undefined ? { permissions } : { repository_ids: [repositoryId], permissions };
	const response = await rest('POST', path, `Bearer ${jwt}`, [201], body);
	return read(response, `POST ${path}`, (body) => text(body.token));
}

export type InstalledRepo = { name: RepoName; id: number; private: boolean; archived: boolean };

export async function installationRepos(token: string): Promise<InstalledRepo[]> {
	const repos: InstalledRepo[] = [];
	for (let page = 1; ; page++) {
		const response = await rest('GET', `/installation/repositories?per_page=100&page=${page}`, `Bearer ${token}`, [200]);
		const list = await read(response, 'GET /installation/repositories', (body) =>
			Array.isArray(body.repositories) ? body.repositories : undefined,
		);
		for (const repo of list) {
			if (!isObject(repo)) continue;
			const name = text(repo.full_name);
			const id = integer(repo.id);
			if (name === undefined || id === undefined) continue;
			repos.push({ name: name as RepoName, id, private: repo.private === true, archived: repo.archived === true });
		}
		if (list.length < 100) return repos;
	}
}

export type Visibility = 'private' | 'public' | 'archived' | 'gone';

export async function visibility(token: string, repo: RepoName): Promise<Visibility> {
	const response = await rest('GET', `/repos/${repo}`, `Bearer ${token}`, [200, 401, 403, 404]);
	if (response.status !== 200) return 'gone';
	return read(response, `GET /repos/${repo}`, (body) =>
		body.private === false ? 'public' : body.archived === true ? 'archived' : body.private === true ? 'private' : undefined,
	);
}

/** The App GitHub created; the OAuth client and owner are kept for signing in to the Worker later. */
export type ManifestApp = {
	id: number;
	slug: string;
	pem: string;
	webhookSecret: string | null;
	clientId: string | null;
	clientSecret: string | null;
	owner: string | null;
};

export async function convertManifest(code: string): Promise<ManifestApp> {
	const response = await send({
		method: 'POST',
		url: `${restBase}/app-manifests/${encodeURIComponent(code)}/conversions`,
		label: '/app-manifests/{code}/conversions',
		expect: [201],
	});
	return read(response, 'POST /app-manifests/{code}/conversions', (body) => {
		const id = integer(body.id);
		const slug = text(body.slug);
		const pem = text(body.pem);
		if (id === undefined || slug === undefined || pem === undefined) return undefined;
		const owner = typeof body.owner === 'object' && body.owner !== null ? text((body.owner as Record<string, unknown>).login) : undefined;
		return {
			id,
			slug,
			pem,
			webhookSecret: text(body.webhook_secret) ?? null,
			clientId: text(body.client_id) ?? null,
			clientSecret: text(body.client_secret) ?? null,
			owner: owner ?? null,
		};
	});
}

// ---- The Actions service, which owns runner scale sets ----

/** The Actions service URL and an admin token for one repository (the token lives ten minutes). */
export type AdminConnection = { url: string; token: string };

export async function adminConnection(installationToken: string, repo: RepoName): Promise<AdminConnection> {
	const tokenPath = `/repos/${repo}/actions/runners/registration-token`;
	const registration = await read(
		await rest('POST', tokenPath, `Bearer ${installationToken}`, [201]),
		`POST ${tokenPath}`,
		(body) => text(body.token),
	);
	const response = await rest('POST', '/actions/runner-registration', `RemoteAuth ${registration}`, [200, 201], {
		url: `https://github.com/${repo}`,
		runner_event: 'register',
	});
	return read(response, 'POST /actions/runner-registration', (body) => {
		const url = text(body.url);
		const token = text(body.token);
		return url && token ? { url, token } : undefined;
	});
}

const scaleSets = '_apis/runtime/runnerscalesets';
const runners = '_apis/distributedtask/pools/0/agents';

export function actionsUrl(connection: AdminConnection, path: string, query: Record<string, string> = {}) {
	const search = new URLSearchParams({ ...query, 'api-version': '6.0-preview' });
	return `${connection.url.replace(/\/+$/, '')}/${path}?${search}`;
}

const actions = (
	connection: AdminConnection,
	method: Call['method'],
	path: string,
	expect: readonly number[],
	body?: unknown,
	query?: Record<string, string>,
) =>
	send({
		method,
		url: actionsUrl(connection, path, query),
		label: path,
		authorization: `Bearer ${connection.token}`,
		body,
		expect,
		rest: false,
	});

const scaleSetIdOf = (body: Record<string, unknown>) => integer(body.id) as ScaleSetId | undefined;

/** Runner group 1 is the default group, the only one a personal account's repository has. */
const defaultRunnerGroup = 1;

export async function findScaleSet(connection: AdminConnection, name: string): Promise<ScaleSetId | undefined> {
	const response = await actions(connection, 'GET', scaleSets, [200], undefined, {
		runnerGroupId: String(defaultRunnerGroup),
		name,
	});
	const found = await read(response, `GET ${scaleSets}`, (body) => (Array.isArray(body.value) ? body.value : undefined));
	const first: unknown = found[0];
	return isObject(first) ? scaleSetIdOf(first) : undefined;
}

export async function createScaleSet(connection: AdminConnection, name: string): Promise<ScaleSetId> {
	const response = await actions(connection, 'POST', scaleSets, [200, 201], {
		name,
		runnerGroupId: defaultRunnerGroup,
		labels: [{ name, type: 'System' }],
		RunnerSetting: { disableUpdate: false },
	});
	return read(response, `POST ${scaleSets}`, scaleSetIdOf);
}

export async function deleteScaleSet(connection: AdminConnection, id: ScaleSetId): Promise<void> {
	await actions(connection, 'DELETE', `${scaleSets}/${id}`, [200, 204, 404]);
}

export type Statistics = {
	totalAvailableJobs: number;
	totalAcquiredJobs: number;
	totalAssignedJobs: number;
	totalRunningJobs: number;
	totalRegisteredRunners: number;
	totalBusyRunners: number;
	totalIdleRunners: number;
};

const statisticKeys = [
	'totalAvailableJobs',
	'totalAcquiredJobs',
	'totalAssignedJobs',
	'totalRunningJobs',
	'totalRegisteredRunners',
	'totalBusyRunners',
	'totalIdleRunners',
] as const satisfies readonly (keyof Statistics)[];

/** What the agent needs to talk to its queue directly, as docs/api.md shapes it. */
export type Session = {
	sessionId: SessionId;
	queueUrl: string;
	queueToken: string;
	acquireJobsUrl: string;
	statistics: Statistics;
};

export type SessionResult = { kind: 'open'; session: Session } | { kind: 'conflict' } | { kind: 'gone' };

async function sessionResult(response: Response, connection: AdminConnection, id: ScaleSetId, label: string): Promise<SessionResult> {
	if (response.status === 409) return { kind: 'conflict' };
	if (response.status === 404) return { kind: 'gone' };
	const session = await read(response, label, (body): Session | undefined => {
		const sessionId = text(body.sessionId);
		const queueUrl = text(body.messageQueueUrl);
		const queueToken = text(body.messageQueueAccessToken);
		if (!sessionId || !queueUrl || !queueToken) return undefined;
		const raw = isObject(body.statistics) ? body.statistics : {};
		const statistics = Object.fromEntries(statisticKeys.map((key) => [key, integer(raw[key]) ?? 0])) as Statistics;
		return {
			sessionId: sessionId as SessionId,
			queueUrl,
			queueToken,
			acquireJobsUrl: actionsUrl(connection, `${scaleSets}/${id}/acquirejobs`),
			statistics,
		};
	});
	return { kind: 'open', session };
}

export async function openSession(connection: AdminConnection, id: ScaleSetId, owner: BoxName): Promise<SessionResult> {
	const path = `${scaleSets}/${id}/sessions`;
	const response = await actions(connection, 'POST', path, [200, 404, 409], { ownerName: owner });
	return sessionResult(response, connection, id, `POST ${path}`);
}

export async function refreshSession(connection: AdminConnection, id: ScaleSetId, sessionId: SessionId): Promise<SessionResult> {
	const path = `${scaleSets}/${id}/sessions/${sessionId}`;
	const response = await actions(connection, 'PATCH', path, [200, 404]);
	return sessionResult(response, connection, id, `PATCH ${path}`);
}

export async function closeSession(connection: AdminConnection, id: ScaleSetId, sessionId: SessionId): Promise<void> {
	await actions(connection, 'DELETE', `${scaleSets}/${id}/sessions/${sessionId}`, [200, 204, 404]);
}

export type MintResult = { kind: 'minted'; runnerId: RunnerId; jitConfig: string } | { kind: 'gone' };

export async function generateJitConfig(connection: AdminConnection, id: ScaleSetId, name: RunnerName): Promise<MintResult> {
	const path = `${scaleSets}/${id}/generatejitconfig`;
	const response = await actions(connection, 'POST', path, [200, 404], { name, workFolder: '_work' });
	if (response.status === 404) return { kind: 'gone' };
	return read(response, `POST ${path}`, (body) => {
		const runnerId = isObject(body.runner) ? integer(body.runner.id) : undefined;
		const jitConfig = text(body.encodedJITConfig);
		return runnerId && jitConfig ? { kind: 'minted', runnerId: runnerId as RunnerId, jitConfig } : undefined;
	});
}

/** The scale set a runner belongs to, or undefined when GitHub no longer knows the runner. */
export async function runnerScaleSet(connection: AdminConnection, runnerId: RunnerId): Promise<ScaleSetId | undefined> {
	const path = `${runners}/${runnerId}`;
	const response = await actions(connection, 'GET', path, [200, 404]);
	if (response.status === 404) return undefined;
	return read(response, `GET ${path}`, (body) => integer(body.runnerScaleSetId) as ScaleSetId | undefined);
}

export type RemoveResult = 'removed' | 'busy';

export async function removeRunner(connection: AdminConnection, runnerId: RunnerId): Promise<RemoveResult> {
	try {
		await actions(connection, 'DELETE', `${runners}/${runnerId}`, [200, 204, 404]);
		return 'removed';
	} catch (error) {
		if (error instanceof GitHubError && error.typeName.includes('JobStillRunningException')) return 'busy';
		throw error;
	}
}
