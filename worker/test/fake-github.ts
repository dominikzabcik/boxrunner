/**
 * A fake of the GitHub and Actions service endpoints the Worker calls. It
 * keeps real state (scale sets, sessions, runners), checks every credential
 * the way GitHub would (the App JWT's RS256 signature, an installation token's
 * repositories and permissions, the admin token), and records each call.
 */

export const appId = 1001;
export const installationId = 501;
const actionsBase = 'https://actions.fake.test/tenant';

type Repo = { full_name: string; id: number; private: boolean; archived: boolean; installed: boolean };
/** A runner group of the organization; `repos` are the ids its repository access selects. */
export type FakeRunnerGroup = { id: number; name: string; visibility: 'all' | 'selected' | 'private'; allows_public_repositories: boolean; repos: number[] };
/** The organization's account id; the fake keeps an organization's scale sets under minus it, beside the repositories' ids. */
export const orgAccountId = 9001;
export type ForkSettings = {
	run_workflows_from_fork_pull_requests: boolean;
	send_write_tokens_to_workflows: boolean;
	send_secrets_and_variables: boolean;
	require_approval_for_fork_pr_workflows: boolean;
};
type Token = { repositoryIds: number[] | undefined; permissions: Record<string, string> };
export type Call = { method: string; path: string; body: unknown };

const encoder = new TextEncoder();

const fromBase64url = (text: string) =>
	Uint8Array.from(atob(text.replace(/-/g, '+').replace(/_/g, '/')), (char) => char.charCodeAt(0));

/** GitHub sends the App key as PKCS#1; WebCrypto exports PKCS#8, whose last element is the PKCS#1 key. */
function pkcs1Pem(pkcs8: Uint8Array) {
	let offset = 2 + (pkcs8[1]! & 0x80 ? pkcs8[1]! & 0x7f : 0); // outer SEQUENCE header
	offset += 3 + 15; // version INTEGER and the algorithm SEQUENCE
	if (pkcs8[offset] !== 0x04) throw new Error('unexpected PKCS#8 layout');
	const lengthByte = pkcs8[offset + 1]!;
	offset += 2 + (lengthByte & 0x80 ? lengthByte & 0x7f : 0);
	let binary = '';
	for (const byte of pkcs8.subarray(offset)) binary += String.fromCharCode(byte);
	const body = btoa(binary).replace(/(.{64})/g, '$1\n');
	return `-----BEGIN RSA PRIVATE KEY-----\n${body}\n-----END RSA PRIVATE KEY-----\n`;
}

export async function appKeyPair() {
	const pair = (await crypto.subtle.generateKey(
		{ name: 'RSASSA-PKCS1-v1_5', modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: 'SHA-256' },
		true,
		['sign', 'verify'],
	)) as CryptoKeyPair;
	const pkcs8 = new Uint8Array((await crypto.subtle.exportKey('pkcs8', pair.privateKey)) as ArrayBuffer);
	return { publicKey: pair.publicKey, pem: pkcs1Pem(pkcs8) };
}

const json = (body: unknown, status = 200) => Response.json(body, { status });

export class FakeGitHub {
	calls: Call[] = [];
	repos: Repo[] = [
		{ full_name: 'octo/site', id: 123, private: true, archived: false, installed: true },
		{ full_name: 'octo/docs', id: 124, private: true, archived: false, installed: true },
		{ full_name: 'octo/old', id: 125, private: true, archived: true, installed: true },
		{ full_name: 'octo/open', id: 126, private: false, archived: false, installed: true },
	];
	/** The account the App is installed on; `Organization` lets it register runners for the whole organization. */
	account: { login: string; id: number; type: 'User' | 'Organization' } = { login: 'octo', id: orgAccountId, type: 'User' };
	runnerGroups: FakeRunnerGroup[] = [
		{ id: 1, name: 'Default', visibility: 'all', allows_public_repositories: false, repos: [] },
		{ id: 2, name: 'boxrunner', visibility: 'selected', allows_public_repositories: false, repos: [123, 124, 125, 126] },
	];
	/** Keyed by `scope/id`, the scope being a repository's id or minus the organization's: GitHub numbers scale sets per repository or organization, so two of them both have a scale set 1. */
	scaleSets = new Map<string, { name: string; repoId: number; runnerGroupId: number }>();
	sessions = new Map<string, string>();
	private scaleSetIds = new Map<number, number>();
	runners = new Map<number, { scaleSetId: number; repoId: number; busy: boolean }>();
	/** Jobs waiting for a scale set that it has not acquired, by `scope/id`, in the Actions service's shape. */
	acquirable = new Map<string, Record<string, unknown>[]>();
	/** What the installation granted the App; GitHub refuses a token that asks for more. */
	granted: Record<string, 'read' | 'write'> = { administration: 'write', metadata: 'read' };
	/** Fork pull request settings by repository id, as GitHub's REST API names them; a repository missing here allows none. */
	forkSettings = new Map<number, ForkSettings>();
	/** Actions variables by repository id. */
	variables = new Map<number, Map<string, string>>();
	/** Answers to force for a path, checked before the fake's own logic. */
	failures = new Map<string, { status: number; body?: unknown }>();
	manifestCode = 'code-from-github';
	/** An App made before the Worker kept its OAuth client: the conversion leaves out the client and the owner. */
	legacyApp = false;
	clientSecret = 'client-secret';
	private oauthCodes = new Map<string, { login: string; redirectUri: string }>();
	private userTokens = new Map<string, string>();
	private tokens = new Map<string, Token>();
	private adminTokens = new Map<string, number>();
	private registrationTokens = new Map<string, number>();
	private nextId = 1;

	constructor(private readonly publicKey: CryptoKey, private readonly pem: string) {}

	/** A user approves the App on GitHub's authorize page; GitHub sends the browser back with this code. */
	authorize(login: string, redirectUri = 'https://box.test/login/callback') {
		const code = `oauth-code-${this.nextId++}`;
		this.oauthCodes.set(code, { login, redirectUri });
		return code;
	}

	count(method: string, path: string | RegExp) {
		return this.calls.filter((call) => call.method === method && (typeof path === 'string' ? call.path === path : path.test(call.path))).length;
	}

	fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
		// A real round trip yields to the event loop, which lets a Durable Object
		// start another request meanwhile; an instant fake would serialize them.
		await new Promise((resolve) => setTimeout(resolve, 2));
		const request = new Request(input, init);
		const url = new URL(request.url);
		const text = await request.text();
		const body: unknown = text ? JSON.parse(text) : undefined;
		const path = url.hostname === 'actions.fake.test' ? url.pathname.replace(/^\/tenant/, '') : url.pathname;
		this.calls.push({ method: request.method, path, body });
		const forced = this.failures.get(path);
		if (forced) return json(forced.body ?? { message: 'forced' }, forced.status);
		if (request.headers.get('user-agent') !== 'boxrunner-worker') return json({ message: 'no user agent' }, 403);
		const auth = request.headers.get('authorization') ?? '';
		if (url.hostname === 'api.github.com') return this.rest(request.method, url, body, auth);
		if (url.hostname === 'github.com' && request.method === 'POST' && path === '/login/oauth/access_token') {
			if (request.headers.get('accept') !== 'application/json') return new Response('access_token=form-encoded', { status: 200 });
			return this.exchange(body as Record<string, string>);
		}
		if (url.hostname === 'actions.fake.test') {
			if (url.searchParams.get('api-version') !== '6.0-preview') return json({ message: 'api-version' }, 400);
			const repoId = this.adminTokens.get(auth.replace(/^Bearer /, ''));
			if (repoId === undefined) return json({ message: 'bad admin token' }, 401);
			return this.actions(repoId, request.method, path, url, body);
		}
		return json({ message: 'unknown host' }, 599);
	};

	private async validJwt(auth: string) {
		const [header, payload, signature] = auth.replace(/^Bearer /, '').split('.');
		if (!header || !payload || !signature) return false;
		const ok = await crypto.subtle.verify(
			'RSASSA-PKCS1-v1_5',
			this.publicKey,
			fromBase64url(signature),
			encoder.encode(`${header}.${payload}`),
		);
		const claims = JSON.parse(new TextDecoder().decode(fromBase64url(payload))) as { iss: string; exp: number; iat: number };
		const now = Date.now() / 1000;
		return ok && claims.iss === String(appId) && claims.exp > now && claims.exp - now <= 600 && claims.iat <= now;
	}

	private token(auth: string) {
		return this.tokens.get(auth.replace(/^Bearer /, ''));
	}

	private repo(name: string) {
		return this.repos.find((repo) => repo.full_name === name);
	}

	private allows(token: Token | undefined, repo: Repo, permission: string, level: 'read' | 'write') {
		if (!token || !repo.installed) return false;
		if (token.repositoryIds && !token.repositoryIds.includes(repo.id)) return false;
		const granted = token.permissions[permission];
		return granted === 'write' || granted === level;
	}

	private async rest(method: string, url: URL, body: unknown, auth: string): Promise<Response> {
		const path = url.pathname;
		if (method === 'POST' && path === `/app-manifests/${this.manifestCode}/conversions`) {
			const client = this.legacyApp ? {} : { client_id: 'Iv1.test', client_secret: this.clientSecret, owner: { login: 'octo' } };
			return json({ id: appId, slug: 'boxrunner-test', pem: this.pem, webhook_secret: 'hook-secret', ...client }, 201);
		}
		if (method === 'POST' && path.startsWith('/app-manifests/')) return json({ message: 'Not Found' }, 404);

		if (method === 'GET' && path === '/app') {
			if (!(await this.validJwt(auth))) return json({ message: 'bad JWT' }, 401);
			return json({ id: appId, slug: 'boxrunner-test', client_id: 'Iv1.test', owner: { login: 'octo' } });
		}
		if (method === 'GET' && path === '/user') {
			const login = this.userTokens.get(auth.replace(/^Bearer /, ''));
			return login ? json({ login, id: 9 }) : json({ message: 'Bad credentials' }, 401);
		}
		if (method === 'GET' && path === '/app/installations') {
			if (!(await this.validJwt(auth))) return json({ message: 'bad JWT' }, 401);
			return json(url.searchParams.get('page') === '1' ? [{ id: installationId, account: this.account }] : []);
		}
		if (method === 'POST' && path === `/app/installations/${installationId}/access_tokens`) {
			if (!(await this.validJwt(auth))) return json({ message: 'bad JWT' }, 401);
			const request = body as { repository_ids?: number[]; permissions: Record<string, string> };
			const exceeds = Object.entries(request.permissions).some(
				([name, level]) => this.granted[name] === undefined || (level === 'write' && this.granted[name] !== 'write'),
			);
			if (exceeds) return json({ message: 'The permissions requested are not granted to this installation.' }, 422);
			const token = `ghs_${this.nextId++}`;
			this.tokens.set(token, { repositoryIds: request.repository_ids, permissions: request.permissions });
			return json({ token }, 201);
		}
		if (method === 'GET' && path === '/installation/repositories') {
			const token = this.token(auth);
			if (!token) return json({ message: 'bad token' }, 401);
			const visible = this.repos.filter((repo) => this.allows(token, repo, 'metadata', 'read'));
			return json({ total_count: visible.length, repositories: url.searchParams.get('page') === '1' ? visible : [] });
		}
		const orgPath = /^\/orgs\/([^/]+)\/(.+)$/.exec(path);
		if (orgPath) return this.org(method, orgPath[1]!, orgPath[2]!, url, auth);
		const registration = /^\/repos\/(.+)\/actions\/runners\/registration-token$/.exec(path);
		if (method === 'POST' && registration) {
			const repo = this.repo(registration[1]!);
			if (!repo || !this.allows(this.token(auth), repo, 'administration', 'write')) return json({ message: 'Resource not accessible' }, 403);
			const token = `reg_${this.nextId++}`;
			this.registrationTokens.set(token, repo.id);
			return json({ token, expires_at: '2099-01-01T00:00:00Z' }, 201);
		}
		if (method === 'POST' && path === '/actions/runner-registration') {
			const scope = this.registrationTokens.get(auth.replace(/^RemoteAuth /, ''));
			const repo = this.repos.find((candidate) => candidate.id === scope);
			const expected = scope === -this.account.id ? `https://github.com/${this.account.login}` : repo && `https://github.com/${repo.full_name}`;
			if (scope === undefined || (body as { url: string }).url !== expected) return json({ message: 'bad' }, 401);
			const token = `admin_${this.nextId++}`;
			this.adminTokens.set(token, scope);
			return json({ url: `${actionsBase}/`, token });
		}
		const variablePath = /^\/repos\/([^/]+\/[^/]+)\/actions\/variables(?:\/([^/]+))?$/.exec(path);
		if (variablePath) {
			const repo = this.repo(variablePath[1]!);
			if (!repo || !this.allows(this.token(auth), repo, 'actions_variables', 'write')) return json({ message: 'Resource not accessible by integration' }, 403);
			const variables = this.variables.get(repo.id) ?? new Map<string, string>();
			this.variables.set(repo.id, variables);
			const name = variablePath[2];
			const request = body as { name: string; value: string };
			if (method === 'POST' && name === undefined) {
				if (variables.has(request.name)) return json({ message: 'Already exists' }, 409);
				variables.set(request.name, request.value);
				return json({}, 201);
			}
			if (name === undefined || !variables.has(name)) return json({ message: 'Not Found' }, 404);
			if (method === 'PATCH') variables.set(name, request.value);
			if (method === 'DELETE') variables.delete(name);
			return new Response(null, { status: 204 });
		}
		const forkPath = /^\/repos\/([^/]+\/[^/]+)\/actions\/permissions\/fork-pr-workflows-private-repos$/.exec(path);
		if (method === 'GET' && forkPath) {
			const repo = this.repo(forkPath[1]!);
			if (!repo || !this.allows(this.token(auth), repo, 'administration', 'read')) return json({ message: 'Resource not accessible by integration' }, 403);
			if (!repo.private) return json({ message: 'Fork PR workflow settings is not allowed for public repositories.' }, 422);
			return json(
				this.forkSettings.get(repo.id) ?? {
					run_workflows_from_fork_pull_requests: false,
					send_write_tokens_to_workflows: false,
					send_secrets_and_variables: false,
					require_approval_for_fork_pr_workflows: false,
				},
			);
		}
		const repoPath = /^\/repos\/([^/]+\/[^/]+)$/.exec(path);
		if (method === 'GET' && repoPath) {
			const repo = this.repo(repoPath[1]!);
			if (!repo || !this.allows(this.token(auth), repo, 'metadata', 'read')) return json({ message: 'Not Found' }, 404);
			return json({ full_name: repo.full_name, id: repo.id, private: repo.private, archived: repo.archived });
		}
		return json({ message: `fake has no ${method} ${path}` }, 404);
	}

	/** The organization's endpoints; each needs the organization permission Self-hosted runners, which only an installation on an organization has. */
	private org(method: string, login: string, rest: string, url: URL, auth: string): Response {
		const token = this.token(auth);
		const allowed = (level: 'read' | 'write') => {
			const granted = token?.permissions.organization_self_hosted_runners;
			return this.account.type === 'Organization' && login === this.account.login && token?.repositoryIds === undefined && (granted === 'write' || granted === level);
		};
		const groupJson = ({ repos: _, ...group }: FakeRunnerGroup) => ({ ...group, default: group.id === 1, inherited: false });
		if (method === 'POST' && rest === 'actions/runners/registration-token') {
			if (!allowed('write')) return json({ message: 'Resource not accessible by integration' }, 403);
			const registration = `reg_${this.nextId++}`;
			this.registrationTokens.set(registration, -this.account.id);
			return json({ token: registration, expires_at: '2099-01-01T00:00:00Z' }, 201);
		}
		if (method !== 'GET') return json({ message: `fake has no ${method} /orgs/${login}/${rest}` }, 404);
		if (!allowed('read')) return json({ message: 'Resource not accessible by integration' }, 403);
		const first = url.searchParams.get('page') !== '2';
		if (rest === 'actions/runner-groups') return json({ total_count: this.runnerGroups.length, runner_groups: first ? this.runnerGroups.map(groupJson) : [] });
		const group = /^actions\/runner-groups\/(\d+)(\/repositories)?$/.exec(rest);
		const found = group && this.runnerGroups.find((candidate) => candidate.id === Number(group[1]));
		if (!found) return json({ message: 'Not Found' }, 404);
		if (!group[2]) return json(groupJson(found));
		const repos = this.repos
			.filter((repo) => found.repos.includes(repo.id))
			.map(({ full_name, id, private: hidden, archived }) => ({ full_name, id, private: hidden, archived }));
		return json({ total_count: repos.length, repositories: first ? repos : [] });
	}

	/** GitHub answers 200 even when it refuses a code, with an `error` field instead of a token. */
	private exchange(body: Record<string, string>): Response {
		const grant = this.oauthCodes.get(body.code ?? '');
		if (body.client_id !== 'Iv1.test' || body.client_secret !== this.clientSecret) return json({ error: 'incorrect_client_credentials' });
		if (!grant) return json({ error: 'bad_verification_code' });
		if (grant.redirectUri !== body.redirect_uri) return json({ error: 'redirect_uri_mismatch' });
		this.oauthCodes.delete(body.code!);
		const token = `ghu_${this.nextId++}`;
		this.userTokens.set(token, grant.login);
		return json({ access_token: token, token_type: 'bearer', scope: '' });
	}

	/** Scale sets belong to a repository or organization: an admin token sees only its own scope's. */
	private actions(repoId: number, method: string, path: string, url: URL, body: unknown): Response {
		if (method === 'GET' && path === '/_apis/runtime/runnergroups/') {
			// A repository has only the default group.
			const groups = repoId < 0 ? this.runnerGroups : this.runnerGroups.filter((group) => group.id === 1);
			const found = groups.filter((group) => group.name === url.searchParams.get('groupName'));
			return json({ count: found.length, value: found.map((group) => ({ id: group.id, name: group.name, isDefaultGroup: group.id === 1 })) });
		}
		if (method === 'GET' && path === '/_apis/runtime/runnerscalesets') {
			const name = url.searchParams.get('name');
			const group = Number(url.searchParams.get('runnerGroupId'));
			const found = [...this.scaleSets]
				.filter(([, set]) => set.name === name && set.repoId === repoId && set.runnerGroupId === group)
				.map(([key, set]) => ({ id: Number(key.split('/')[1]), name: set.name }));
			return json({ count: found.length, value: found });
		}
		if (method === 'POST' && path === '/_apis/runtime/runnerscalesets') {
			const request = body as { name: string; runnerGroupId: number; labels: { name: string }[] };
			const id = (this.scaleSetIds.get(repoId) ?? 0) + 1;
			this.scaleSetIds.set(repoId, id);
			this.scaleSets.set(scaleSetKey(repoId, id), { name: request.name, repoId, runnerGroupId: request.runnerGroupId });
			return json({ id, name: request.name, labels: request.labels });
		}
		const scaleSet = /^\/_apis\/runtime\/runnerscalesets\/(\d+)(\/.*)?$/.exec(path);
		if (scaleSet) {
			const id = Number(scaleSet[1]);
			const key = scaleSetKey(repoId, id);
			const rest = scaleSet[2] ?? '';
			const found = this.scaleSets.get(key);
			if (!found) return json({ typeName: 'RunnerScaleSetNotFoundException' }, 404);
			if (method === 'GET' && rest === '') {
				const group = this.runnerGroups.find((candidate) => candidate.id === found.runnerGroupId);
				return json({ id, name: found.name, runnerGroupId: found.runnerGroupId, runnerGroupName: group?.name ?? 'Default', labels: [{ name: found.name, type: 'System' }] });
			}
			if (method === 'GET' && rest === '/acquirablejobs') {
				const jobs = this.acquirable.get(key) ?? [];
				if (jobs.length === 0) return new Response(null, { status: 204 });
				return json({ count: jobs.length, value: jobs });
			}
			if (method === 'DELETE' && rest === '') {
				this.scaleSets.delete(key);
				this.sessions.delete(key);
				return new Response(null, { status: 204 });
			}
			if (method === 'POST' && rest === '/sessions') {
				if (this.sessions.has(key)) return json({ typeName: 'TaskAgentSessionConflictException' }, 409);
				const sessionId = crypto.randomUUID();
				this.sessions.set(key, sessionId);
				return json({
					sessionId,
					ownerName: (body as { ownerName: string }).ownerName,
					messageQueueUrl: `${actionsBase}/queue/${id}`,
					messageQueueAccessToken: `queue-token-${sessionId}`,
					statistics: { totalAssignedJobs: 1 },
				});
			}
			const session = /^\/sessions\/(.+)$/.exec(rest);
			if (session) {
				const open = this.sessions.get(key) === session[1];
				if (method === 'DELETE') {
					if (open) this.sessions.delete(key);
					return new Response(null, { status: open ? 204 : 404 });
				}
				if (method === 'PATCH') {
					if (!open && this.sessions.has(key)) return json({ typeName: 'TaskAgentSessionConflictException' }, 400);
					if (!open) return json({ typeName: 'TaskAgentSessionExpiredException' }, 404);
					return json({ sessionId: session[1], messageQueueUrl: `${actionsBase}/queue/${id}`, messageQueueAccessToken: 'queue-token-refreshed' });
				}
			}
			if (method === 'POST' && rest === '/generatejitconfig') {
				const runnerId = this.nextId++;
				this.runners.set(runnerId, { scaleSetId: id, repoId, busy: false });
				return json({ runner: { id: runnerId, name: (body as { name: string }).name }, encodedJITConfig: `jit-for-${runnerId}` });
			}
		}
		const runner = /^\/_apis\/distributedtask\/pools\/0\/agents\/(\d+)$/.exec(path);
		if (runner) {
			const found = this.runners.get(Number(runner[1]));
			if (!found || found.repoId !== repoId) return json({ typeName: 'AgentNotFoundException' }, 404);
			if (method === 'GET') return json({ id: Number(runner[1]), runnerScaleSetId: found.scaleSetId });
			if (method === 'DELETE') {
				if (found.busy) return json({ typeName: 'GitHub.DistributedTask.WebApi.JobStillRunningException', message: 'busy' }, 400);
				this.runners.delete(Number(runner[1]));
				return new Response(null, { status: 204 });
			}
		}
		return json({ message: `fake has no ${method} ${path}` }, 404);
	}
}


export const scaleSetKey = (repoId: number, id: number) => `${repoId}/${id}`;
