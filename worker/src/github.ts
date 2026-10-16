import { parse, type BoxName, type RunnerGroup, type ForkPullRequests, type Login, type RepoName, type RunnerId, type RunnerName, type ScaleSetId, type SessionId } from './model';

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
	accept?: string;
};

async function send({ method, url, label, authorization, body, expect, rest = true, accept }: Call) {
	const headers: Record<string, string> = { 'user-agent': userAgent };
	if (rest) {
		headers.accept = 'application/vnd.github+json';
		headers['x-github-api-version'] = '2022-11-28';
	} else if (accept) {
		headers.accept = accept;
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

/** An installation of the App, and the account it is installed on. */
export type Installation = { id: number; login: string; accountId: number; organization: boolean };

export async function installations(jwt: string): Promise<Installation[]> {
	const found: Installation[] = [];
	for (let page = 1; ; page++) {
		const response = await rest('GET', `/app/installations?per_page=100&page=${page}`, `Bearer ${jwt}`, [200]);
		const body: unknown = await response.json().catch(() => undefined);
		if (!Array.isArray(body)) throw new GitHubError('GET /app/installations (unreadable answer)', 200);
		for (const installation of body) {
			if (!isObject(installation)) continue;
			const id = integer(installation.id);
			const account = isObject(installation.account) ? installation.account : {};
			if (id === undefined) continue;
			found.push({ id, login: text(account.login) ?? '', accountId: integer(account.id) ?? 0, organization: account.type === 'Organization' });
		}
		if (body.length < 100) return found;
	}
}

export type Permissions = Partial<Record<'administration' | 'metadata' | 'actions_variables' | 'organization_self_hosted_runners', 'read' | 'write'>>;

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

export const installationRepos = (token: string) => repoPages(token, '/installation/repositories');

/** The repositories a runner group admits. Needs the organization's `organization_self_hosted_runners: read`. */
export const runnerGroupRepos = (token: string, org: string, groupId: number) =>
	repoPages(token, `/orgs/${org}/actions/runner-groups/${groupId}/repositories`);

async function repoPages(token: string, path: string): Promise<InstalledRepo[]> {
	const repos: InstalledRepo[] = [];
	for (let page = 1; ; page++) {
		const response = await rest('GET', `${path}?per_page=100&page=${page}`, `Bearer ${token}`, [200]);
		const list = await read(response, `GET ${path}`, (body) => (Array.isArray(body.repositories) ? body.repositories : undefined));
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

const runnerGroupOf = (body: Record<string, unknown>): RunnerGroup | undefined => {
	const id = integer(body.id);
	const name = text(body.name);
	const visibility = text(body.visibility);
	if (id === undefined || name === undefined || visibility === undefined || typeof body.allows_public_repositories !== 'boolean') return undefined;
	// A group an enterprise shares with the organization can allow public repositories there too.
	const allowsPublicRepositories = body.allows_public_repositories || body.inherited_allows_public_repositories === true;
	return { id, name, visibility, allowsPublicRepositories };
};

/** The organization's runner group of that name, compared case-insensitively, or undefined when it has none. */
export async function findRunnerGroup(token: string, org: string, name: string): Promise<RunnerGroup | undefined> {
	const path = `/orgs/${org}/actions/runner-groups`;
	for (let page = 1; ; page++) {
		const response = await rest('GET', `${path}?per_page=100&page=${page}`, `Bearer ${token}`, [200]);
		const list = await read(response, `GET ${path}`, (body) => (Array.isArray(body.runner_groups) ? body.runner_groups : undefined));
		for (const raw of list) {
			const group = isObject(raw) ? runnerGroupOf(raw) : undefined;
			if (group?.name.toLowerCase() === name.toLowerCase()) return group;
		}
		if (list.length < 100) return undefined;
	}
}

/** The runner group as it is now, or undefined when it is gone. */
export async function runnerGroup(token: string, org: string, id: number): Promise<RunnerGroup | undefined> {
	const path = `/orgs/${org}/actions/runner-groups/${id}`;
	const response = await rest('GET', path, `Bearer ${token}`, [200, 404]);
	if (response.status === 404) return undefined;
	return read(response, `GET ${path}`, runnerGroupOf);
}

/** Needs `administration: read`. GitHub answers 422 for a public repository, which has no such settings. */
export async function forkPullRequests(token: string, repo: RepoName): Promise<ForkPullRequests> {
	const path = `/repos/${repo}/actions/permissions/fork-pr-workflows-private-repos`;
	const response = await rest('GET', path, `Bearer ${token}`, [200]);
	return read(response, `GET ${path}`, (body) => {
		const settings = {
			runWorkflows: body.run_workflows_from_fork_pull_requests,
			requireApproval: body.require_approval_for_fork_pr_workflows,
			sendWriteTokens: body.send_write_tokens_to_workflows,
			sendSecrets: body.send_secrets_and_variables,
		};
		return Object.values(settings).every((flag) => typeof flag === 'boolean') ? (settings as ForkPullRequests) : undefined;
	});
}

/** Creates or updates an Actions variable of the repository. */
export async function setVariable(token: string, repo: RepoName, name: string, value: string): Promise<void> {
	const updated = await rest('PATCH', `/repos/${repo}/actions/variables/${name}`, `Bearer ${token}`, [204, 404], { name, value });
	if (updated.status === 404) await rest('POST', `/repos/${repo}/actions/variables`, `Bearer ${token}`, [201], { name, value });
}

export async function deleteVariable(token: string, repo: RepoName, name: string): Promise<void> {
	await rest('DELETE', `/repos/${repo}/actions/variables/${name}`, `Bearer ${token}`, [204, 404]);
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

/** The App's OAuth client id and owner, for an App set up before the Worker kept them. */
export async function appProfile(jwt: string): Promise<{ clientId: string; owner: Login }> {
	const response = await rest('GET', '/app', `Bearer ${jwt}`, [200]);
	return read(response, 'GET /app', (body) => {
		const clientId = text(body.client_id);
		const owner = isObject(body.owner) ? parse.login(body.owner.login) : undefined;
		return clientId && owner ? { clientId, owner } : undefined;
	});
}

// ---- Signing in to the dashboard, as a user of the App ----

export type OAuthClient = { id: string; secret: string };

/** Trades the code GitHub sent back for a user access token, or undefined when GitHub refuses the code. */
export async function userToken(client: OAuthClient, code: string, redirectUri: string): Promise<string | undefined> {
	const response = await send({
		method: 'POST',
		url: 'https://github.com/login/oauth/access_token',
		label: '/login/oauth/access_token',
		body: { client_id: client.id, client_secret: client.secret, code, redirect_uri: redirectUri },
		expect: [200],
		rest: false,
		accept: 'application/json',
	});
	// GitHub answers 200 with an "error" field when it refuses the code.
	const body: unknown = await response.json().catch(() => undefined);
	return isObject(body) ? text(body.access_token) : undefined;
}

export async function userLogin(token: string): Promise<Login> {
	const response = await rest('GET', '/user', `Bearer ${token}`, [200]);
	return read(response, 'GET /user', (body) => parse.login(body.login));
}

// ---- The Actions service, which owns runner scale sets ----

/** The Actions service URL and an admin token for one repository or organization (the token lives ten minutes). */
export type AdminConnection = { url: string; token: string };

/** `scope` is `owner/name` for a repository's scale sets, or an organization's login for its own, as `actions/scaleset` tells them apart. */
export async function adminConnection(installationToken: string, scope: string): Promise<AdminConnection> {
	const tokenPath = scope.includes('/') ? `/repos/${scope}/actions/runners/registration-token` : `/orgs/${scope}/actions/runners/registration-token`;
	const registration = await read(
		await rest('POST', tokenPath, `Bearer ${installationToken}`, [201]),
		`POST ${tokenPath}`,
		(body) => text(body.token),
	);
	const response = await rest('POST', '/actions/runner-registration', `RemoteAuth ${registration}`, [200, 201], {
		url: `https://github.com/${scope}`,
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
export const defaultRunnerGroup = 1;

/** The Actions service's id for the organization's runner group, looked up by name as actions-runner-controller does; undefined when it has none. */
export async function actionsRunnerGroupId(connection: AdminConnection, name: string): Promise<number | undefined> {
	const path = '_apis/runtime/runnergroups/';
	const response = await actions(connection, 'GET', path, [200], undefined, { groupName: name });
	const found = await read(response, `GET ${path}`, (body) => (Array.isArray(body.value) ? body.value : undefined));
	const first: unknown = found.length === 1 ? found[0] : undefined;
	return isObject(first) ? integer(first.id) : undefined;
}

export async function findScaleSet(connection: AdminConnection, name: string, runnerGroupId = defaultRunnerGroup): Promise<ScaleSetId | undefined> {
	const response = await actions(connection, 'GET', scaleSets, [200], undefined, {
		runnerGroupId: String(runnerGroupId),
		name,
	});
	const found = await read(response, `GET ${scaleSets}`, (body) => (Array.isArray(body.value) ? body.value : undefined));
	const first: unknown = found[0];
	return isObject(first) ? scaleSetIdOf(first) : undefined;
}

export async function createScaleSet(connection: AdminConnection, name: string, runnerGroupId = defaultRunnerGroup): Promise<ScaleSetId> {
	const response = await actions(connection, 'POST', scaleSets, [200, 201], {
		name,
		runnerGroupId,
		labels: [{ name, type: 'System' }],
		RunnerSetting: { disableUpdate: false },
	});
	return read(response, `POST ${scaleSets}`, scaleSetIdOf);
}

/** The runner group the scale set is in at GitHub, or undefined when the scale set is gone. */
export async function scaleSetGroup(connection: AdminConnection, id: ScaleSetId): Promise<number | undefined> {
	const path = `${scaleSets}/${id}`;
	const response = await actions(connection, 'GET', path, [200, 404]);
	if (response.status === 404) return undefined;
	return read(response, `GET ${path}`, (body) => integer(body.runnerGroupId));
}

/** A job waiting for a scale set that it has not acquired, as the Actions service names its fields. */
export type AcquirableJob = {
	runnerRequestId: number;
	ownerName: string;
	repositoryName: string;
	jobWorkflowRef: string;
	eventName: string;
	requestLabels: string[];
};

/**
 * The jobs the scale set may still acquire, or undefined when the scale set is
 * gone. GitHub offers a job in one message; an agent that turned it down, or
 * missed the message, finds it here, as actions-runner-controller's first
 * listener did when a session opened.
 */
export async function acquirableJobs(connection: AdminConnection, id: ScaleSetId): Promise<AcquirableJob[] | undefined> {
	const path = `${scaleSets}/${id}/acquirablejobs`;
	const response = await actions(connection, 'GET', path, [200, 204, 404]);
	if (response.status === 404) return undefined;
	if (response.status === 204) return [];
	const list = await read(response, `GET ${path}`, (body) => (Array.isArray(body.value) ? body.value : undefined));
	const jobs: AcquirableJob[] = [];
	for (const raw of list) {
		if (!isObject(raw)) continue;
		const runnerRequestId = integer(raw.runnerRequestId);
		if (runnerRequestId === undefined) continue;
		jobs.push({
			runnerRequestId,
			ownerName: typeof raw.ownerName === 'string' ? raw.ownerName : '',
			repositoryName: typeof raw.repositoryName === 'string' ? raw.repositoryName : '',
			jobWorkflowRef: typeof raw.jobWorkflowRef === 'string' ? raw.jobWorkflowRef : '',
			eventName: typeof raw.eventName === 'string' ? raw.eventName : '',
			requestLabels: Array.isArray(raw.requestLabels) ? raw.requestLabels.filter((label): label is string => typeof label === 'string') : [],
		});
	}
	return jobs;
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
