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
	scaleSets = new Map<number, { name: string; repoId: number }>();
	sessions = new Map<number, string>();
	runners = new Map<number, { scaleSetId: number; busy: boolean }>();
	/** Answers to force for a path, checked before the fake's own logic. */
	failures = new Map<string, { status: number; body?: unknown }>();
	manifestCode = 'code-from-github';
	private tokens = new Map<string, Token>();
	private adminTokens = new Map<string, number>();
	private registrationTokens = new Map<string, number>();
	private nextId = 1;

	constructor(private readonly publicKey: CryptoKey, private readonly pem: string) {}

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
			return json({ id: appId, slug: 'boxrunner-test', pem: this.pem, webhook_secret: 'hook-secret', client_secret: 'x' }, 201);
		}
		if (method === 'POST' && path.startsWith('/app-manifests/')) return json({ message: 'Not Found' }, 404);

		if (method === 'GET' && path === '/app/installations') {
			if (!(await this.validJwt(auth))) return json({ message: 'bad JWT' }, 401);
			return json(url.searchParams.get('page') === '1' ? [{ id: installationId }] : []);
		}
		if (method === 'POST' && path === `/app/installations/${installationId}/access_tokens`) {
			if (!(await this.validJwt(auth))) return json({ message: 'bad JWT' }, 401);
			const request = body as { repository_ids?: number[]; permissions: Record<string, string> };
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
		const registration = /^\/repos\/(.+)\/actions\/runners\/registration-token$/.exec(path);
		if (method === 'POST' && registration) {
			const repo = this.repo(registration[1]!);
			if (!repo || !this.allows(this.token(auth), repo, 'administration', 'write')) return json({ message: 'Resource not accessible' }, 403);
			const token = `reg_${this.nextId++}`;
			this.registrationTokens.set(token, repo.id);
			return json({ token, expires_at: '2099-01-01T00:00:00Z' }, 201);
		}
		if (method === 'POST' && path === '/actions/runner-registration') {
			const repoId = this.registrationTokens.get(auth.replace(/^RemoteAuth /, ''));
			const repo = this.repos.find((candidate) => candidate.id === repoId);
			if (!repo || (body as { url: string }).url !== `https://github.com/${repo.full_name}`) return json({ message: 'bad' }, 401);
			const token = `admin_${this.nextId++}`;
			this.adminTokens.set(token, repo.id);
			return json({ url: `${actionsBase}/`, token });
		}
		const repoPath = /^\/repos\/([^/]+\/[^/]+)$/.exec(path);
		if (method === 'GET' && repoPath) {
			const repo = this.repo(repoPath[1]!);
			if (!repo || !this.allows(this.token(auth), repo, 'metadata', 'read')) return json({ message: 'Not Found' }, 404);
			return json({ full_name: repo.full_name, id: repo.id, private: repo.private, archived: repo.archived });
		}
		return json({ message: `fake has no ${method} ${path}` }, 404);
	}

	/** Scale sets belong to a repository: an admin token sees only its own repository's. */
	private actions(repoId: number, method: string, path: string, url: URL, body: unknown): Response {
		if (method === 'GET' && path === '/_apis/runtime/runnerscalesets') {
			const name = url.searchParams.get('name');
			const found = [...this.scaleSets].filter(([, set]) => set.name === name && set.repoId === repoId).map(([id, set]) => ({ id, name: set.name }));
			return json({ count: found.length, value: found });
		}
		if (method === 'POST' && path === '/_apis/runtime/runnerscalesets') {
			const request = body as { name: string; runnerGroupId: number; labels: { name: string }[] };
			const id = this.nextId++;
			this.scaleSets.set(id, { name: request.name, repoId });
			return json({ id, name: request.name, labels: request.labels });
		}
		const scaleSet = /^\/_apis\/runtime\/runnerscalesets\/(\d+)(\/.*)?$/.exec(path);
		if (scaleSet) {
			const id = Number(scaleSet[1]);
			const rest = scaleSet[2] ?? '';
			if (this.scaleSets.get(id)?.repoId !== repoId) return json({ typeName: 'RunnerScaleSetNotFoundException' }, 404);
			if (method === 'DELETE' && rest === '') {
				this.scaleSets.delete(id);
				this.sessions.delete(id);
				return new Response(null, { status: 204 });
			}
			if (method === 'POST' && rest === '/sessions') {
				if (this.sessions.has(id)) return json({ typeName: 'TaskAgentSessionConflictException' }, 409);
				const sessionId = crypto.randomUUID();
				this.sessions.set(id, sessionId);
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
				const open = this.sessions.get(id) === session[1];
				if (method === 'DELETE') {
					if (open) this.sessions.delete(id);
					return new Response(null, { status: open ? 204 : 404 });
				}
				if (method === 'PATCH') {
					if (!open) return json({ typeName: 'TaskAgentSessionExpiredException' }, 404);
					return json({ sessionId: session[1], messageQueueUrl: `${actionsBase}/queue/${id}`, messageQueueAccessToken: 'queue-token-refreshed' });
				}
			}
			if (method === 'POST' && rest === '/generatejitconfig') {
				const runnerId = this.nextId++;
				this.runners.set(runnerId, { scaleSetId: id, busy: false });
				return json({ runner: { id: runnerId, name: (body as { name: string }).name }, encodedJITConfig: `jit-for-${runnerId}` });
			}
		}
		const runner = /^\/_apis\/distributedtask\/pools\/0\/agents\/(\d+)$/.exec(path);
		if (runner) {
			const found = this.runners.get(Number(runner[1]));
			if (!found) return json({ typeName: 'AgentNotFoundException' }, 404);
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

