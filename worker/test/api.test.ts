import { afterEach, describe, expect, it, vi } from 'vitest';
import { scaleSetKey, type FakeGitHub } from './fake-github';
import { agent, expectError, fakeGitHub, heartbeatBody, postForm, send, setUp, time, type Agent } from './harness';
import { setupSecret } from './secret';
import { version } from '../src/version';

afterEach(() => {
	vi.useRealTimers();
});

/** What GitHub answers for a private repository whose fork pull requests run no workflows. */
const noForkWorkflows = { runWorkflows: false, requireApproval: false, sendWriteTokens: false, sendSecrets: false };

async function ready(): Promise<{ github: FakeGitHub; call: Agent; token: string }> {
	const github = await fakeGitHub();
	const token = await setUp(github);
	return { github, call: agent(token), token };
}

async function scaleSet(call: Agent, repo = 'octo/site', className = 'boxrunner'): Promise<number> {
	const response = await call('POST', '/v1/scalesets', { repo, class: className });
	expect(response.status).toBe(200);
	return ((await response.json()) as { id: number }).id;
}

async function mint(call: Agent, scaleSetId: number, name = 'box-1-boxrunner-3f9a2c') {
	return call('POST', '/v1/runners', { repo: 'octo/site', scaleSetId, name });
}

describe('every /v1 call', () => {
	it("names the Worker's release, so the agent can show it", async () => {
		const { call, token } = await ready();
		expect(version).toMatch(/^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$/);
		expect((await call('GET', '/v1/repos')).headers.get('boxrunner-worker-version')).toBe(version);
		const refused = await agent(token, '2.0.0')('GET', '/v1/repos');
		expect(refused.headers.get('boxrunner-worker-version')).toBe(version);
	});

	it('answers 503 not_configured before setup', async () => {
		await fakeGitHub();
		await expectError(await agent('anything')('GET', '/v1/repos'), 503, 'not_configured', 'Setup has not finished. Open https://box.test/setup.');
	});

	it('answers 426 to a missing version or a major other than 0 and 1', async () => {
		const { token } = await ready();
		const expected = (sent: string) =>
			`This Worker serves agent versions 0.x and 1.x; the agent sent ${sent}. Upgrade whichever is older.`;
		await expectError(await agent(token, '2.0.0')('GET', '/v1/repos'), 426, 'upgrade_required', expected('"2.0.0"'));
		await expectError(await agent(token, 'v1')('GET', '/v1/repos'), 426, 'upgrade_required', expected('"v1"'));
		const bare = await send('/v1/repos', { headers: { authorization: `Bearer ${token}` } });
		await expectError(bare, 426, 'upgrade_required', expected('no Boxrunner-Version'));
		expect((await agent(token, '1.4.2-rc.1')('GET', '/v1/repos')).status).toBe(200);
	});

	it('answers 401 to a missing token or one from another Worker', async () => {
		await ready();
		await expectError(await send('/v1/repos', { headers: { 'boxrunner-version': '0.1.0' } }), 401, 'unauthorized');
		await expectError(await agent('A'.repeat(43))('GET', '/v1/repos'), 401, 'unauthorized', 'The box token is missing or wrong.');
	});

	it('answers 404 JSON to anything that is not a route', async () => {
		const { call } = await ready();
		for (const [method, path] of [
			['GET', '/index.html'],
			['POST', '/'],
			['GET', '/v1/heartbeat'],
			['GET', '/v1/scalesets'],
			['PUT', '/v1/runners'],
			['GET', '/v1/repos/'],
			['GET', '/setup/'],
		] as const) {
			await expectError(await call(method, path), 404, 'not_found', 'No such endpoint.');
		}
	});

	it('answers 429 rate_limited after 120 requests, and serves again two seconds later', async () => {
		const { call } = await ready();
		vi.useFakeTimers({ toFake: ['Date'] });
		for (let i = 0; i < 120; i++) expect((await call('GET', '/v1/repos')).status).toBe(200);
		const limited = await call('GET', '/v1/repos');
		expect(limited.headers.get('retry-after')).toBe('2');
		await expectError(limited, 429, 'rate_limited', 'Too many requests for this box token.');
		vi.advanceTimersByTime(2_000);
		expect((await call('GET', '/v1/repos')).status).toBe(200);
	});
});

describe('GET /v1/repos', () => {
	it('lists private, unarchived repositories and the classes, and caches the list', async () => {
		const { github, call } = await ready();
		const response = await call('GET', '/v1/repos');
		expect(await response.json()).toEqual({
			repos: [
				{ name: 'octo/site', id: 123, forkPullRequests: noForkWorkflows },
				{ name: 'octo/docs', id: 124, forkPullRequests: noForkWorkflows },
			],
			classes: ['boxrunner', 'boxrunner-light'],
			paused: [],
		});
		await call('GET', '/v1/repos');
		expect(github.count('GET', '/installation/repositories')).toBe(1);
		expect(github.calls.find((c) => c.path === '/app/installations/501/access_tokens')?.body).toEqual({
			permissions: { metadata: 'read' },
		});
	});
});

describe('fork pull request settings in GET /v1/repos', () => {
	const forkPath = (repo: string) => `/repos/${repo}/actions/permissions/fork-pr-workflows-private-repos`;
	const repos = async (call: Agent) => ((await (await call('GET', '/v1/repos')).json()) as { repos: { name: string; forkPullRequests: unknown }[] }).repos;
	const settingsOf = async (call: Agent) => Object.fromEntries((await repos(call)).map((repo) => [repo.name, repo.forkPullRequests]));

	it("returns each repository's settings, read with a token for that repository and administration: read", async () => {
		const { github, call } = await ready();
		github.forkSettings.set(123, {
			run_workflows_from_fork_pull_requests: true,
			send_write_tokens_to_workflows: false,
			send_secrets_and_variables: true,
			require_approval_for_fork_pr_workflows: false,
		});
		expect(await settingsOf(call)).toEqual({
			'octo/site': { runWorkflows: true, requireApproval: false, sendWriteTokens: false, sendSecrets: true },
			'octo/docs': noForkWorkflows,
		});
		const narrowed = github.calls.filter((c) => c.path === '/app/installations/501/access_tokens' && c.body && 'repository_ids' in (c.body as object));
		expect(narrowed.map((c) => c.body)).toEqual([
			{ repository_ids: [123], permissions: { administration: 'read' } },
			{ repository_ids: [124], permissions: { administration: 'read' } },
		]);
	});

	it('reads them at most once an hour per repository', async () => {
		const { github, call } = await ready();
		time('2026-10-14T10:00:00Z');
		await call('GET', '/v1/repos');
		github.forkSettings.set(123, {
			run_workflows_from_fork_pull_requests: true,
			send_write_tokens_to_workflows: true,
			send_secrets_and_variables: false,
			require_approval_for_fork_pr_workflows: true,
		});
		time('2026-10-14T10:59:00Z');
		expect((await settingsOf(call))['octo/site']).toEqual(noForkWorkflows);
		expect([github.count('GET', '/installation/repositories'), github.count('GET', forkPath('octo/site'))]).toEqual([2, 1]);
		time('2026-10-14T11:00:01Z');
		expect((await settingsOf(call))['octo/site']).toEqual({ runWorkflows: true, requireApproval: true, sendWriteTokens: true, sendSecrets: false });
		expect(github.count('GET', forkPath('octo/site'))).toBe(2);
	});

	it('reads five repositories per listing and the rest at the next one', async () => {
		const { github, call } = await ready();
		for (let id = 200; id < 205; id++) github.repos.push({ full_name: `octo/repo-${id}`, id, private: true, archived: false, installed: true });
		const first = await settingsOf(call);
		expect(Object.values(first).filter((settings) => settings === null)).toHaveLength(2);
		expect(github.count('GET', /\/fork-pr-workflows-private-repos$/)).toBe(5);
		const second = await settingsOf(call);
		expect(Object.values(second).filter((settings) => settings === null)).toHaveLength(0);
		expect(github.count('GET', /\/fork-pr-workflows-private-repos$/)).toBe(7);
	});

	it('answers the listing when a read fails, keeps what it read before and tries again ten minutes later', async () => {
		const { github, call } = await ready();
		time('2026-10-14T10:00:00Z');
		github.failures.set(forkPath('octo/site'), { status: 500 });
		const response = await call('GET', '/v1/repos');
		expect(response.status).toBe(200);
		expect(((await response.json()) as { repos: { forkPullRequests: unknown }[] }).repos.map((repo) => repo.forkPullRequests)).toEqual([null, noForkWorkflows]);
		github.failures.delete(forkPath('octo/site'));
		time('2026-10-14T10:09:00Z');
		expect((await settingsOf(call))['octo/site']).toBeNull();
		time('2026-10-14T10:10:01Z');
		expect((await settingsOf(call))['octo/site']).toEqual(noForkWorkflows);

		github.failures.set(forkPath('octo/site'), { status: 502 });
		time('2026-10-14T11:10:02Z');
		expect((await settingsOf(call))['octo/site']).toEqual(noForkWorkflows);
		expect(github.count('GET', forkPath('octo/site'))).toBe(3);
	});
});

describe('POST /v1/scalesets', () => {
	it('creates the scale set once, named and labelled after the class', async () => {
		const { github, call } = await ready();
		const response = await call('POST', '/v1/scalesets', { repo: 'octo/site', class: 'boxrunner' });
		const created = (await response.json()) as { id: number; name: string };
		expect(created.name).toBe('boxrunner');
		expect(github.calls.find((c) => c.method === 'POST' && c.path === '/_apis/runtime/runnerscalesets')?.body).toEqual({
			name: 'boxrunner',
			runnerGroupId: 1,
			labels: [{ name: 'boxrunner', type: 'System' }],
			RunnerSetting: { disableUpdate: false },
		});
		expect(await scaleSet(call)).toBe(created.id);
		expect(github.count('POST', '/_apis/runtime/runnerscalesets')).toBe(1);
	});

	it('creates one scale set for two concurrent requests', async () => {
		const { github, call } = await ready();
		const [a, b] = await Promise.all([scaleSet(call), scaleSet(call)]);
		expect(a).toBe(b);
		expect(github.count('POST', '/_apis/runtime/runnerscalesets')).toBe(1);
	});

	it('adopts a scale set that already exists at GitHub under the class name', async () => {
		const { github, call } = await ready();
		github.scaleSets.set(scaleSetKey(123, 77), { name: 'boxrunner-light', repoId: 123, runnerGroupId: 1 });
		expect(await scaleSet(call, 'OCTO/Site', 'boxrunner-light')).toBe(77);
		expect(github.count('POST', '/_apis/runtime/runnerscalesets')).toBe(0);
	});

	it('refuses classes the Worker does not serve and malformed bodies', async () => {
		const { call, token } = await ready();
		await expectError(
			await call('POST', '/v1/scalesets', { repo: 'octo/site', class: 'gpu' }),
			400,
			'unknown_class',
			`"gpu" is not one of this Worker's classes: boxrunner, boxrunner-light.`,
		);
		await expectError(await call('POST', '/v1/scalesets', { repo: 'octo/site' }), 400, 'bad_request', '"class" is missing or malformed.');
		await expectError(await call('POST', '/v1/scalesets', { repo: 'site', class: 'boxrunner' }), 400, 'bad_request', '"repo" is missing or malformed.');
		const notJson = await send('/v1/scalesets', {
			method: 'POST',
			headers: { authorization: `Bearer ${token}`, 'boxrunner-version': '0.1.0' },
			body: 'repo=octo/site',
		});
		await expectError(notJson, 400, 'bad_request', 'The body must be a JSON object.');
	});

	it('refuses public, archived and unknown repositories with 410', async () => {
		const { call } = await ready();
		for (const repo of ['octo/open', 'octo/old', 'octo/elsewhere']) {
			await expectError(
				await call('POST', '/v1/scalesets', { repo, class: 'boxrunner' }),
				410,
				'repo_ineligible',
				`${repo} is not a private repository the App is installed on.`,
			);
		}
	});
});

describe('sessions', () => {
	it('opens a session with what the agent needs for the queue, then 409 while it is open', async () => {
		const { call } = await ready();
		const id = await scaleSet(call);
		const response = await call('POST', '/v1/sessions', { repo: 'octo/site', scaleSetId: id, owner: 'box-1' });
		const session = (await response.json()) as Record<string, unknown>;
		expect(response.status).toBe(200);
		expect(session).toEqual({
			sessionId: session.sessionId,
			queueUrl: `https://actions.fake.test/tenant/queue/${id}`,
			queueToken: `queue-token-${String(session.sessionId)}`,
			acquireJobsUrl: `https://actions.fake.test/tenant/_apis/runtime/runnerscalesets/${id}/acquirejobs?api-version=6.0-preview`,
			statistics: {
				totalAvailableJobs: 0,
				totalAcquiredJobs: 0,
				totalAssignedJobs: 1,
				totalRunningJobs: 0,
				totalRegisteredRunners: 0,
				totalBusyRunners: 0,
				totalIdleRunners: 0,
			},
		});
		await expectError(
			await call('POST', '/v1/sessions', { repo: 'octo/site', scaleSetId: id, owner: 'box-1' }),
			409,
			'session_exists',
			`Another session holds scale set ${id}.`,
		);
	});

	it('names the other box that holds the session while that box reports, and forgets it when the session closes', async () => {
		time('2026-10-14T10:00:00Z');
		const { github, call } = await ready();
		const id = await scaleSet(call);
		const open = (owner: string) => call('POST', '/v1/sessions', { repo: 'octo/site', scaleSetId: id, owner });
		const answer = async (response: Response) => ({ status: response.status, body: await response.json() });
		expect((await call('POST', '/v1/heartbeat', heartbeatBody({ box: 'box-2', runners: [] }))).status).toBe(200);
		const opened = (await (await open('box-2')).json()) as { sessionId: string };

		expect(await answer(await open('box-1'))).toEqual({
			status: 409,
			body: { error: 'session_exists', message: `box-2 holds the session of scale set ${id}, so it serves octo/site boxrunner.`, heldBy: 'box-2' },
		});
		const orphaned = { status: 409, body: { error: 'session_exists', message: `Another session holds scale set ${id}.` } };
		expect(await answer(await open('box-2'))).toEqual(orphaned);

		time('2026-10-14T10:02:01Z');
		expect(await answer(await open('box-1'))).toEqual(orphaned);

		time('2026-10-14T10:02:30Z');
		expect((await call('POST', '/v1/heartbeat', heartbeatBody({ box: 'box-2', runners: [] }))).status).toBe(200);
		expect((await call('DELETE', `/v1/sessions/${opened.sessionId}?repo=octo/site&scaleSetId=${id}`)).status).toBe(204);
		github.sessions.set(scaleSetKey(123, id), crypto.randomUUID());
		expect(await answer(await open('box-1'))).toEqual(orphaned);
	});

	it('learns who holds a session from a refresh that names its owner, as after a Worker upgrade', async () => {
		time('2026-10-14T10:00:00Z');
		const { github, call } = await ready();
		const id = await scaleSet(call);
		const sessionId = crypto.randomUUID();
		github.sessions.set(scaleSetKey(123, id), sessionId);
		expect((await call('POST', '/v1/heartbeat', heartbeatBody({ box: 'box-2', runners: [] }))).status).toBe(200);
		const refresh = (body: Record<string, unknown>) => call('PATCH', `/v1/sessions/${sessionId}`, { repo: 'octo/site', scaleSetId: id, ...body });
		const open = async () => {
			const response = await call('POST', '/v1/sessions', { repo: 'octo/site', scaleSetId: id, owner: 'box-1' });
			return { status: response.status, body: await response.json() };
		};

		expect((await refresh({})).status).toBe(200);
		expect(await open()).toEqual({ status: 409, body: { error: 'session_exists', message: `Another session holds scale set ${id}.` } });

		expect((await refresh({ owner: 'box-2' })).status).toBe(200);
		expect(await open()).toEqual({
			status: 409,
			body: { error: 'session_exists', message: `box-2 holds the session of scale set ${id}, so it serves octo/site boxrunner.`, heldBy: 'box-2' },
		});
		await expectError(await refresh({ owner: 'not a box' }), 400, 'bad_request', '"owner" is missing or malformed.');
	});

	it('forgets who holds a session when setup starts over', async () => {
		time('2026-10-14T10:00:00Z');
		const { github, call } = await ready();
		const id = await scaleSet(call);
		const open = (call: Agent, owner: string) => call('POST', '/v1/sessions', { repo: 'octo/site', scaleSetId: id, owner });
		expect((await call('POST', '/v1/heartbeat', heartbeatBody({ box: 'box-2', runners: [] }))).status).toBe(200);
		expect((await open(call, 'box-2')).status).toBe(200);

		expect((await postForm('/setup/reset', { secret: setupSecret })).status).toBe(200);
		const again = agent(await setUp(github));
		expect((await again('POST', '/v1/heartbeat', heartbeatBody({ box: 'box-2', runners: [] }))).status).toBe(200);
		expect(await scaleSet(again)).toBe(id);
		const refused = await open(again, 'box-1');
		expect({ status: refused.status, body: await refused.json() }).toEqual({
			status: 409,
			body: { error: 'session_exists', message: `Another session holds scale set ${id}.` },
		});
	});

	it('refreshes and closes a session, and closing twice still answers 204', async () => {
		const { github, call } = await ready();
		const id = await scaleSet(call);
		const opened = (await (await call('POST', '/v1/sessions', { repo: 'octo/site', scaleSetId: id, owner: 'box-1' })).json()) as {
			sessionId: string;
		};
		const refreshed = await call('PATCH', `/v1/sessions/${opened.sessionId}`, { repo: 'octo/site', scaleSetId: id });
		expect(((await refreshed.json()) as { queueToken: string }).queueToken).toBe('queue-token-refreshed');
		const close = `/v1/sessions/${opened.sessionId}?repo=octo/site&scaleSetId=${id}`;
		expect((await call('DELETE', close)).status).toBe(204);
		expect(github.sessions.has(scaleSetKey(123, id))).toBe(false);
		expect((await call('DELETE', close)).status).toBe(204);
		await expectError(
			await call('PATCH', `/v1/sessions/${opened.sessionId}`, { repo: 'octo/site', scaleSetId: id }),
			404,
			'not_found',
			'The session is gone; open a new one.',
		);
		await expectError(await call('DELETE', `/v1/sessions/not-a-uuid?repo=octo/site&scaleSetId=${id}`), 400, 'bad_request');
	});

	it('tells a box its session is gone once another box holds the scale set', async () => {
		const { call } = await ready();
		const id = await scaleSet(call);
		const open = async (owner: string) =>
			((await (await call('POST', '/v1/sessions', { repo: 'octo/site', scaleSetId: id, owner })).json()) as { sessionId: string }).sessionId;
		const first = await open('box-1');
		expect((await call('DELETE', `/v1/sessions/${first}?repo=octo/site&scaleSetId=${id}`)).status).toBe(204);
		await open('box-2');
		await expectError(
			await call('PATCH', `/v1/sessions/${first}`, { repo: 'octo/site', scaleSetId: id }),
			404,
			'not_found',
			'The session is gone; open a new one.',
		);
	});

	it('refuses scale sets the Worker did not make for that repository', async () => {
		const { github, call } = await ready();
		const docs = await scaleSet(call, 'octo/docs');
		github.scaleSets.set(scaleSetKey(123, 900), { name: 'boxrunner', repoId: 123, runnerGroupId: 1 });
		for (const scaleSetId of [docs, 900]) {
			await expectError(
				await call('POST', '/v1/sessions', { repo: 'octo/site', scaleSetId, owner: 'box-1' }),
				404,
				'not_found',
				`Scale set ${scaleSetId} is not one this Worker made for octo/site.`,
			);
		}
		expect(github.sessions.size).toBe(0);
	});

	it('forgets a scale set GitHub deleted, so the agent can create it again', async () => {
		const { github, call } = await ready();
		const id = await scaleSet(call);
		github.scaleSets.delete(scaleSetKey(123, id));
		await expectError(
			await call('POST', '/v1/sessions', { repo: 'octo/site', scaleSetId: id, owner: 'box-1' }),
			404,
			'not_found',
			`Scale set ${id} no longer exists at GitHub; find or create it again.`,
		);
		const again = await scaleSet(call);
		expect(again).not.toBe(id);
		expect(github.scaleSets.get(scaleSetKey(123, again))).toEqual({ name: 'boxrunner', repoId: 123, runnerGroupId: 1 });
	});
});

describe('runners', () => {
	it('mints a one-job config with tokens narrowed to the repository', async () => {
		const { github, call } = await ready();
		const id = await scaleSet(call);
		const response = await mint(call, id);
		const minted = (await response.json()) as { runnerId: number; jitConfig: string };
		expect(minted).toEqual({ runnerId: minted.runnerId, jitConfig: `jit-for-${minted.runnerId}` });
		expect(github.runners.get(minted.runnerId)).toEqual({ scaleSetId: id, repoId: 123, busy: false });
		expect(github.calls.find((c) => c.path.endsWith('/generatejitconfig'))?.body).toEqual({
			name: 'box-1-boxrunner-3f9a2c',
			workFolder: '_work',
		});
		const narrowed = github.calls.filter((c) => c.path === '/app/installations/501/access_tokens' && c.body && 'repository_ids' in (c.body as object));
		expect(narrowed.map((c) => c.body)).toEqual([
			{ repository_ids: [123], permissions: { administration: 'write' } },
			{ repository_ids: [123], permissions: { metadata: 'read' } },
		]);
	});

	it('refuses a runner name that is not [a-z0-9-] up to 64 characters', async () => {
		const { call } = await ready();
		const id = await scaleSet(call);
		for (const name of ['Box_1', 'a'.repeat(65), '']) {
			await expectError(await mint(call, id, name), 400, 'bad_request', '"name" is missing or malformed.');
		}
	});

	it('removes a runner that has not taken a job, and answers 204 when it is already gone', async () => {
		const { github, call } = await ready();
		const id = await scaleSet(call);
		const { runnerId } = (await (await mint(call, id)).json()) as { runnerId: number };
		const remove = `/v1/runners/${runnerId}?repo=octo/site&scaleSetId=${id}`;
		expect((await call('DELETE', remove)).status).toBe(204);
		expect(github.runners.has(runnerId)).toBe(false);
		expect((await call('DELETE', remove)).status).toBe(204);
	});

	it('answers 409 runner_busy for a runner running a job', async () => {
		const { github, call } = await ready();
		const id = await scaleSet(call);
		const { runnerId } = (await (await mint(call, id)).json()) as { runnerId: number };
		github.runners.set(runnerId, { scaleSetId: id, repoId: 123, busy: true });
		await expectError(
			await call('DELETE', `/v1/runners/${runnerId}?repo=octo/site&scaleSetId=${id}`),
			409,
			'runner_busy',
			`Runner ${runnerId} is running a job.`,
		);
		expect(github.runners.has(runnerId)).toBe(true);
	});

	it('refuses to remove a runner of another scale set', async () => {
		const { github, call } = await ready();
		const ours = await scaleSet(call);
		const light = await scaleSet(call, 'octo/site', 'boxrunner-light');
		const { runnerId } = (await (await mint(call, light)).json()) as { runnerId: number };
		await expectError(
			await call('DELETE', `/v1/runners/${runnerId}?repo=octo/site&scaleSetId=${ours}`),
			404,
			'not_found',
			`Runner ${runnerId} does not belong to scale set ${ours}.`,
		);
		expect(github.runners.has(runnerId)).toBe(true);
	});

	it('answers 502 github_error naming the call when GitHub fails', async () => {
		const { github, call } = await ready();
		const id = await scaleSet(call);
		github.failures.set(`/_apis/runtime/runnerscalesets/${id}/generatejitconfig`, { status: 500 });
		await expectError(
			await mint(call, id),
			502,
			'github_error',
			`GitHub answered 500 to POST _apis/runtime/runnerscalesets/${id}/generatejitconfig`,
		);
	});
});

describe('repositories that stop being private', () => {
	it('deletes the scale sets and answers 410 when a repository turns public between two mints', async () => {
		const { github, call } = await ready();
		const id = await scaleSet(call);
		const light = await scaleSet(call, 'octo/site', 'boxrunner-light');
		expect((await mint(call, id, 'runner-a')).status).toBe(200);

		github.repos.find((repo) => repo.full_name === 'octo/site')!.private = false;
		await expectError(await mint(call, id, 'runner-b'), 410, 'repo_ineligible', 'octo/site is public, so this Worker no longer serves it.');
		expect([github.scaleSets.has(scaleSetKey(123, id)), github.scaleSets.has(scaleSetKey(123, light))]).toEqual([false, false]);
		expect(github.count('POST', `/_apis/runtime/runnerscalesets/${id}/generatejitconfig`)).toBe(1);
		expect(((await (await call('GET', '/v1/repos')).json()) as { repos: unknown[] }).repos).toEqual([{ name: 'octo/docs', id: 124, forkPullRequests: noForkWorkflows }]);
		await expectError(
			await call('POST', '/v1/sessions', { repo: 'octo/site', scaleSetId: id, owner: 'box-1' }),
			410,
			'repo_ineligible',
			'octo/site is not a private repository the App is installed on.',
		);
	});

	it('deletes the scale sets of a repository that turned public at the next listing, without the agent asking for it', async () => {
		const { github, call } = await ready();
		const id = await scaleSet(call);
		const light = await scaleSet(call, 'octo/site', 'boxrunner-light');
		const docs = await scaleSet(call, 'octo/docs');

		github.repos.find((repo) => repo.full_name === 'octo/site')!.private = false;
		vi.useFakeTimers({ toFake: ['Date'], now: Date.now() + 61_000 });
		expect(((await (await call('GET', '/v1/repos')).json()) as { repos: unknown[] }).repos).toEqual([{ name: 'octo/docs', id: 124, forkPullRequests: noForkWorkflows }]);
		expect([scaleSetKey(123, id), scaleSetKey(123, light), scaleSetKey(124, docs)].map((key) => github.scaleSets.has(key))).toEqual([
			false,
			false,
			true,
		]);

		github.repos.find((repo) => repo.full_name === 'octo/site')!.private = true;
		vi.setSystemTime(Date.now() + 61_000);
		expect(await scaleSet(call)).not.toBe(id);
	});

	it('forgets the scale sets of a repository the App was removed from, which it can no longer delete', async () => {
		const { github, call } = await ready();
		await scaleSet(call);
		github.repos.find((repo) => repo.full_name === 'octo/site')!.installed = false;
		vi.useFakeTimers({ toFake: ['Date'], now: Date.now() + 61_000 });
		await call('GET', '/v1/repos');
		expect(github.count('DELETE', /runnerscalesets\/\d+$/)).toBe(0);

		github.repos.find((repo) => repo.full_name === 'octo/site')!.installed = true;
		vi.setSystemTime(Date.now() + 61_000);
		await scaleSet(call);
		expect(github.count('GET', /runnerscalesets/)).toBe(2);
	});

	it('answers 410 without minting when the App was removed from the repository', async () => {
		const { github, call } = await ready();
		const id = await scaleSet(call);
		github.repos.find((repo) => repo.full_name === 'octo/site')!.installed = false;
		await expectError(
			await mint(call, id),
			410,
			'repo_ineligible',
			'octo/site no longer has the App installed, so this Worker no longer serves it.',
		);
		expect(github.count('POST', /generatejitconfig$/)).toBe(0);
	});
});

describe('derived tokens', () => {
	it('signs one App JWT and derives one admin connection for two concurrent mints', async () => {
		const { github, call } = await ready();
		const id = await scaleSet(call);
		vi.useFakeTimers({ toFake: ['Date'], now: Date.now() + 10 * 60_000 });
		const sign = vi.spyOn(crypto.subtle, 'sign');
		const before = { registration: github.count('POST', '/actions/runner-registration'), listing: github.count('GET', '/installation/repositories') };

		const [a, b] = await Promise.all([mint(call, id, 'runner-a'), mint(call, id, 'runner-b')]);
		expect([a.status, b.status]).toEqual([200, 200]);
		expect(sign.mock.calls.filter(([algorithm]) => JSON.stringify(algorithm).includes('RSASSA-PKCS1-v1_5'))).toHaveLength(1);
		expect(github.count('POST', '/actions/runner-registration') - before.registration).toBe(1);
		expect(github.count('POST', '/repos/octo/site/actions/runners/registration-token')).toBe(2);
		expect(github.count('GET', '/installation/repositories') - before.listing).toBe(1);
	});
});

describe('scale sets of several repositories', () => {
	it("keeps each repository's scale sets apart, though GitHub numbers them from 1 in each", async () => {
		const { call } = await ready();
		const site = await scaleSet(call);
		const docsResponse = await call('POST', '/v1/scalesets', { repo: 'octo/docs', class: 'boxrunner' });
		const docs = ((await docsResponse.json()) as { id: number }).id;
		expect([site, docs]).toEqual([1, 1]);
		for (const repo of ['octo/site', 'octo/docs']) {
			const opened = await call('POST', '/v1/sessions', { repo, scaleSetId: 1, owner: 'box-1' });
			expect(opened.status).toBe(200);
		}
	});
});
