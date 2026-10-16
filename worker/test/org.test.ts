import { afterEach, describe, expect, it, vi } from 'vitest';
import { orgAccountId, scaleSetKey, type FakeGitHub } from './fake-github';
import { agent, expectError, fakeGitHub, heartbeatBody, postForm, send, setUp, signIn, time, withEnv, type Agent } from './harness';
import { setupSecret } from './secret';

afterEach(() => {
	vi.useRealTimers();
});

const org = { ORG: 'octo', RUNNER_GROUP: 'boxrunner' };
/** Where the fake keeps the organization's scale sets. */
const orgScope = -orgAccountId;

/** An App owned by the organization octo, with Self-hosted runners and Metadata but no permission on its repositories' settings. */
async function ready(): Promise<{ github: FakeGitHub; call: Agent; token: string }> {
	const github = await fakeGitHub();
	const token = await setUp(github);
	github.account = { login: 'octo', id: orgAccountId, type: 'Organization' };
	github.granted = { organization_self_hosted_runners: 'write', metadata: 'read' };
	return { github, call: agent(token), token };
}

async function scaleSet(call: Agent, className = 'boxrunner'): Promise<number> {
	const response = await call('POST', '/v1/scalesets', { org: 'octo', class: className });
	expect(response.status).toBe(200);
	return ((await response.json()) as { id: number }).id;
}

const tokenRequests = (github: FakeGitHub) =>
	github.calls.filter((call) => call.path.endsWith('/access_tokens')).map((call) => call.body as { repository_ids?: number[]; permissions: Record<string, string> });

describe('ORG and RUNNER_GROUP', () => {
	it('stop the API and setup while only one of them is set or ORG is not a login, and tell only the Worker’s log why', async () => {
		const { call } = await ready();
		const logged = vi.spyOn(console, 'error').mockImplementation(() => {});
		const stranger = agent('not-the-box-token');
		const cases: [Record<string, string>, string][] = [
			[{ ORG: 'octo', RUNNER_GROUP: '' }, "The Worker's ORG is set without RUNNER_GROUP, the runner group its scale sets go in."],
			[{ ORG: '', RUNNER_GROUP: 'boxrunner' }, "The Worker's RUNNER_GROUP is set without ORG."],
			[{ ORG: 'octo/site', RUNNER_GROUP: 'boxrunner' }, 'The Worker\'s ORG is "octo/site", which is not a GitHub login.'],
			[{ ORG: 'octo', RUNNER_GROUP: 'a/b' }, 'The Worker\'s RUNNER_GROUP is "a/b", which is not 1 to 64 letters, digits, spaces, dots, dashes or underscores.'],
		];
		for (const [vars, message] of cases) {
			await withEnv(vars, async () => {
				for (const caller of [stranger, call]) {
					await expectError(await caller('GET', '/v1/repos'), 503, 'not_configured', "The Worker's ORG or RUNNER_GROUP variable is not valid; the Worker's log says why.");
				}
			});
			expect(logged).toHaveBeenLastCalledWith(message);
		}
	});
});

describe('serving an organization', () => {
	it('lists the private, unarchived repositories the runner group admits and names the organization', async () => {
		const { github, call } = await ready();
		await withEnv(org, async () => {
			const response = await call('GET', '/v1/repos');
			expect(response.status).toBe(200);
			expect(await response.json()).toEqual({
				repos: [
					{ name: 'octo/site', id: 123, forkPullRequests: null },
					{ name: 'octo/docs', id: 124, forkPullRequests: null },
				],
				classes: ['boxrunner', 'boxrunner-light'],
				paused: [],
				organization: { name: 'octo', group: 'boxrunner', problem: null },
			});
		});
		// The App reads the organization's runner groups, never a repository's settings.
		expect(tokenRequests(github)).toEqual([{ permissions: { organization_self_hosted_runners: 'read' } }]);
		expect(github.count('GET', /fork-pr-workflows/)).toBe(0);
	});

	it('narrows the group’s repositories to REPOS, and serves none the group leaves out', async () => {
		const { github, call } = await ready();
		github.repos.push({ full_name: 'octo/other', id: 127, private: true, archived: false, installed: true });
		await withEnv({ ...org, REPOS: 'octo/docs,octo/other' }, async () => {
			const { repos } = (await (await call('GET', '/v1/repos')).json()) as { repos: { name: string }[] };
			expect(repos.map((repo) => repo.name)).toEqual(['octo/docs']);
		});
	});

	it('makes one scale set per class for the organization, in the runner group, with organization tokens only', async () => {
		const { github, call } = await ready();
		await withEnv(org, async () => {
			const id = await scaleSet(call);
			expect(await scaleSet(call)).toBe(id);
			expect(github.scaleSets.get(scaleSetKey(orgScope, id))).toEqual({ name: 'boxrunner', repoId: orgScope, runnerGroupId: 2 });
			const light = await scaleSet(call, 'boxrunner-light');
			expect(github.scaleSets.get(scaleSetKey(orgScope, light))).toEqual({ name: 'boxrunner-light', repoId: orgScope, runnerGroupId: 2 });

			const opened = await call('POST', '/v1/sessions', { org: 'octo', scaleSetId: id, owner: 'box-1' });
			expect(opened.status).toBe(200);
			const { sessionId } = (await opened.json()) as { sessionId: string };
			const minted = await call('POST', '/v1/runners', { org: 'octo', scaleSetId: id, name: 'box-1-boxrunner-3f9a2c', jobRepos: ['octo/site'] });
			expect(minted.status).toBe(200);
			const { runnerId } = (await minted.json()) as { runnerId: number };
			expect((await call('DELETE', `/v1/runners/${runnerId}?org=octo&scaleSetId=${id}`)).status).toBe(204);
			expect((await call('DELETE', `/v1/sessions/${sessionId}?org=octo&scaleSetId=${id}`)).status).toBe(204);
		});
		expect(github.count('POST', '/orgs/octo/actions/runners/registration-token')).toBe(1);
		expect(github.count('POST', /^\/repos\/.*registration-token$/)).toBe(0);
		for (const request of tokenRequests(github)) {
			expect(request.repository_ids).toBeUndefined();
			expect(Object.keys(request.permissions)).toEqual(['organization_self_hosted_runners']);
		}
	});

	it('names the box that holds an organization scale set’s session', async () => {
		const { call } = await ready();
		await withEnv(org, async () => {
			const id = await scaleSet(call);
			expect((await call('POST', '/v1/heartbeat', heartbeatBody({ box: 'box-2' }))).status).toBe(200);
			expect((await call('POST', '/v1/sessions', { org: 'octo', scaleSetId: id, owner: 'box-2' })).status).toBe(200);
			await expectError(
				await call('POST', '/v1/sessions', { org: 'octo', scaleSetId: id, owner: 'box-1' }),
				409,
				'session_exists',
				`box-2 holds the session of scale set ${id}, so it serves octo boxrunner.`,
			);
		});
	});

	it('refuses a repository scale set, another organization, and an organization while it serves repositories', async () => {
		const { call } = await ready();
		await withEnv(org, async () => {
			await expectError(
				await call('POST', '/v1/scalesets', { repo: 'octo/site', class: 'boxrunner' }),
				410,
				'repo_ineligible',
				'This Worker serves octo through organization scale sets, so it makes none for octo/site alone. Upgrade the agent to one that knows organization scale sets.',
			);
			await expectError(
				await call('POST', '/v1/scalesets', { org: 'hubot', class: 'boxrunner' }),
				410,
				'repo_ineligible',
				'This Worker serves the organization octo, not hubot.',
			);
		});
		await expectError(
			await call('POST', '/v1/scalesets', { org: 'octo', class: 'boxrunner' }),
			410,
			'repo_ineligible',
			'This Worker serves repositories one by one, not the organization octo.',
		);
	});

	it('accepts heartbeats that name the organization as a scale set’s scope', async () => {
		const { call } = await ready();
		const body = heartbeatBody({
			runners: [{ name: 'box-1-boxrunner-3f9a2c', repo: 'octo', class: 'boxrunner', state: 'running', since: '2026-10-14T09:55:00Z' }],
			scaleSets: [{ repo: 'octo', class: 'boxrunner', assigned: 1 }],
		});
		expect((await call('POST', '/v1/heartbeat', body)).status).toBe(200);
	});
});

describe('the runner group', () => {
	const cases: [string, (github: FakeGitHub) => void, string][] = [
		[
			'admits every repository',
			(github) => (github.runnerGroups[1]!.visibility = 'all'),
			'The runner group “boxrunner” admits every repository of octo; set its repository access to selected repositories.',
		],
		[
			'admits every private repository',
			(github) => (github.runnerGroups[1]!.visibility = 'private'),
			'The runner group “boxrunner” admits every private repository of octo; set its repository access to selected repositories.',
		],
		[
			'allows public repositories',
			(github) => (github.runnerGroups[1]!.allows_public_repositories = true),
			'The runner group “boxrunner” of octo allows public repositories; turn off “Allow public repositories” in its settings.',
		],
		['is missing', (github) => (github.runnerGroups[1]!.name = 'other'), 'octo has no runner group named “boxrunner”.'],
		['cannot be read without the organization permission', (github) => (github.granted = { metadata: 'read' }), 'The App lacks the organization permission Self-hosted runners (read and write), or octo has not approved it.'],
		['belongs to an account the App is not installed on as an organization', (github) => (github.account.type = 'User'), 'The App is not installed on the organization octo.'],
	];
	for (const [what, change, problem] of cases) {
		it(`serves nothing when it ${what}`, async () => {
			const { github, call } = await ready();
			change(github);
			await withEnv(org, async () => {
				const listed = (await (await call('GET', '/v1/repos')).json()) as { repos: unknown[]; organization: unknown };
				expect(listed.repos).toEqual([]);
				expect(listed.organization).toEqual({ name: 'octo', group: 'boxrunner', problem });
				await expectError(await call('POST', '/v1/scalesets', { org: 'octo', class: 'boxrunner' }), 409, 'group_ineligible', problem);
			});
			expect(github.scaleSets.size).toBe(0);
		});
	}

	it('is read again before a session opens or a runner is minted; one that turned public loses its scale sets', async () => {
		const { github, call } = await ready();
		await withEnv(org, async () => {
			const id = await scaleSet(call);
			github.runnerGroups[1]!.visibility = 'all';
			await expectError(
				await call('POST', '/v1/sessions', { org: 'octo', scaleSetId: id, owner: 'box-1' }),
				409,
				'group_ineligible',
				'The runner group “boxrunner” admits every repository of octo; set its repository access to selected repositories.',
			);
			expect(github.scaleSets.has(scaleSetKey(orgScope, id))).toBe(true);

			github.runnerGroups[1]!.visibility = 'selected';
			github.runnerGroups[1]!.allows_public_repositories = true;
			await expectError(
				await call('POST', '/v1/runners', { org: 'octo', scaleSetId: id, name: 'box-1-boxrunner-3f9a2c', jobRepos: [] }),
				409,
				'group_ineligible',
				'The runner group “boxrunner” of octo allows public repositories; turn off “Allow public repositories” in its settings.',
			);
			expect(github.scaleSets.has(scaleSetKey(orgScope, id))).toBe(false);
		});
	});

	it('takes the repository scale sets of a Worker that served repositories one by one off GitHub once it serves the organization', async () => {
		const github = await fakeGitHub();
		const call = agent(await setUp(github));
		const response = await call('POST', '/v1/scalesets', { repo: 'octo/site', class: 'boxrunner' });
		const repoScaleSet = ((await response.json()) as { id: number }).id;
		expect(github.scaleSets.has(scaleSetKey(123, repoScaleSet))).toBe(true);
		// The owner adds the organization permission first and keeps Administration until the Worker has listed once.
		github.account.type = 'Organization';
		github.granted = { administration: 'write', organization_self_hosted_runners: 'write', metadata: 'read' };
		// The repository list is cached for a minute.
		vi.useFakeTimers({ toFake: ['Date'] });
		vi.setSystemTime(Date.now() + 61_000);
		await withEnv(org, async () => {
			expect((await call('GET', '/v1/repos')).status).toBe(200);
		});
		expect(github.scaleSets.has(scaleSetKey(123, repoScaleSet))).toBe(false);
	});
});

describe('the scale sets’ runner group', () => {
	const open = (call: Agent, id: number) => call('POST', '/v1/sessions', { org: 'octo', scaleSetId: id, owner: 'box-1' });
	const mint = (call: Agent, id: number) => call('POST', '/v1/runners', { org: 'octo', scaleSetId: id, name: 'box-1-boxrunner-3f9a2c', jobRepos: [] });

	it('is the group the Worker checked: a scale set left in a renamed group is deleted, and the group that took the name gets a new one', async () => {
		const { github, call } = await ready();
		vi.useFakeTimers({ toFake: ['Date'] });
		await withEnv(org, async () => {
			const id = await scaleSet(call);
			Object.assign(github.runnerGroups[1]!, { name: 'old', visibility: 'all', allows_public_repositories: true });
			github.runnerGroups.push({ id: 3, name: 'boxrunner', visibility: 'selected', allows_public_repositories: false, repos: [123] });
			vi.setSystemTime(Date.now() + 61_000);
			await expectError(await open(call, id), 404, 'not_found', `Scale set ${id} is not one this Worker made for octo.`);
			expect(github.scaleSets.has(scaleSetKey(orgScope, id))).toBe(false);

			const again = await scaleSet(call);
			expect(github.scaleSets.get(scaleSetKey(orgScope, again))).toEqual({ name: 'boxrunner', repoId: orgScope, runnerGroupId: 3 });
			expect((await open(call, again)).status).toBe(200);
			expect((await mint(call, again)).status).toBe(200);
		});
	});

	it('loses its scale sets when it is renamed, even before the listing finds the new name', async () => {
		const { github, call } = await ready();
		await withEnv(org, async () => {
			const id = await scaleSet(call);
			github.runnerGroups[1]!.name = 'old';
			await expectError(await open(call, id), 409, 'group_ineligible', 'The runner group “boxrunner” of octo is now named “old”, so the Worker deleted its scale sets.');
			expect(github.scaleSets.has(scaleSetKey(orgScope, id))).toBe(false);
		});
	});

	it('follows RUNNER_GROUP: the scale sets of the group it named before are deleted', async () => {
		const { github, call } = await ready();
		vi.useFakeTimers({ toFake: ['Date'] });
		let id = 0;
		await withEnv(org, async () => {
			id = await scaleSet(call);
		});
		github.runnerGroups[1]!.visibility = 'all';
		github.runnerGroups.push({ id: 3, name: 'newgroup', visibility: 'selected', allows_public_repositories: false, repos: [123] });
		vi.setSystemTime(Date.now() + 61_000);
		await withEnv({ ORG: 'octo', RUNNER_GROUP: 'newgroup' }, async () => {
			expect((await call('GET', '/v1/repos')).status).toBe(200);
			expect(github.scaleSets.has(scaleSetKey(orgScope, id))).toBe(false);
			await expectError(await mint(call, id), 404, 'not_found', `Scale set ${id} is not one this Worker made for octo.`);
			const again = await scaleSet(call);
			expect(github.scaleSets.get(scaleSetKey(orgScope, again))).toEqual({ name: 'boxrunner', repoId: orgScope, runnerGroupId: 3 });
		});
	});

	it('is deleted: GitHub moves its scale sets to the Default group, and the Worker deletes them', async () => {
		const { github, call } = await ready();
		await withEnv(org, async () => {
			const id = await scaleSet(call);
			github.runnerGroups.splice(1, 1);
			github.scaleSets.get(scaleSetKey(orgScope, id))!.runnerGroupId = 1;
			await expectError(
				await mint(call, id),
				409,
				'group_ineligible',
				'The runner group “boxrunner” of octo was deleted, which moves its runners to the Default group, so the Worker deleted its scale sets.',
			);
			expect(github.scaleSets.has(scaleSetKey(orgScope, id))).toBe(false);
		});
	});

	it('is read at GitHub for the scale set itself: one in another group is deleted before a session or a runner', async () => {
		const { github, call } = await ready();
		await withEnv(org, async () => {
			const id = await scaleSet(call);
			github.scaleSets.get(scaleSetKey(orgScope, id))!.runnerGroupId = 1;
			await expectError(await open(call, id), 404, 'not_found', `Scale set ${id} is not in the runner group “boxrunner”, so the Worker deleted it; find or create it again.`);
			expect(github.scaleSets.has(scaleSetKey(orgScope, id))).toBe(false);
			const again = await scaleSet(call);
			expect(github.scaleSets.get(scaleSetKey(orgScope, again))?.runnerGroupId).toBe(2);
			expect((await mint(call, again)).status).toBe(200);
		});
	});
});

describe('a runner for an organization scale set', () => {
	it('is minted only while every job the agent acquired is of a repository the Worker serves', async () => {
		const { github, call } = await ready();
		await withEnv({ ...org, REPOS: 'octo/site' }, async () => {
			const id = await scaleSet(call);
			const mint = (jobRepos?: unknown) => call('POST', '/v1/runners', { org: 'octo', scaleSetId: id, name: 'box-1-boxrunner-3f9a2c', jobRepos });
			await expectError(await mint(['octo/site', 'octo/docs']), 410, 'repo_ineligible', 'octo/docs is not a repository this Worker serves, so it mints no runner for its job.');
			await expectError(await mint(), 400, 'bad_request', '"jobRepos" is missing or malformed.');
			expect(github.runners.size).toBe(0);
			expect((await mint(['Octo/Site'])).status).toBe(200);
			expect((await mint([])).status).toBe(200);
		});
	});
});

describe('GET /v1/jobs', () => {
	it('lists the jobs an organization scale set may still acquire, of repositories the Worker serves', async () => {
		const { github, call } = await ready();
		await withEnv({ ...org, REPOS: 'octo/site' }, async () => {
			const id = await scaleSet(call);
			const listed = async () => {
				const response = await call('GET', `/v1/jobs?org=octo&scaleSetId=${id}`);
				expect(response.status).toBe(200);
				return response.json();
			};
			expect(await listed()).toEqual({ jobs: [] });
			const job = (runnerRequestId: number, repositoryName: string) => ({
				acquireJobUrl: 'https://actions.fake.test/acquire',
				messageType: 'JobAvailable',
				runnerRequestId,
				repositoryName,
				ownerName: 'octo',
				jobWorkflowRef: 'octo/x/.github/workflows/ci.yml@refs/heads/main',
				eventName: 'push',
				requestLabels: ['boxrunner'],
			});
			github.acquirable.set(scaleSetKey(orgScope, id), [job(501, 'site'), job(502, 'docs'), job(503, 'other')]);
			expect(await listed()).toEqual({
				jobs: [{ runnerRequestId: 501, ownerName: 'octo', repositoryName: 'site', jobWorkflowRef: 'octo/x/.github/workflows/ci.yml@refs/heads/main', eventName: 'push', requestLabels: ['boxrunner'] }],
			});
		});
	});
});

describe('setup with ORG set', () => {
	it('creates the App under the organization with Self-hosted runners instead of Administration', async () => {
		await fakeGitHub();
		await withEnv(org, async () => {
			const form = await (await send('/setup')).text();
			expect(form).toContain('This creates a private GitHub App under the organization <strong>octo</strong>');
			const response = await postForm('/setup', { secret: setupSecret, name: 'boxrunner-test' });
			const html = await response.text();
			expect(html).toContain('action="https://github.com/organizations/octo/settings/apps/new?state=');
			const manifest = JSON.parse(
				(/name="manifest" value="([^"]+)"/.exec(html)?.[1] ?? '').replace(/&quot;/g, '"').replace(/&amp;/g, '&'),
			) as { default_permissions: Record<string, string> };
			expect(manifest.default_permissions).toEqual({ organization_self_hosted_runners: 'write', metadata: 'read' });
		});
	});

	it('is locked while the variables are unreadable', async () => {
		await fakeGitHub();
		await withEnv({ ORG: 'octo', RUNNER_GROUP: '' }, async () => {
			const response = await send('/setup');
			expect(response.status).toBe(503);
			expect(await response.text()).toContain('The Worker’s ORG is set without RUNNER_GROUP, the runner group its scale sets go in. Fix the variable, then reload this page.');
		});
	});
});

describe('the dashboard', () => {
	it('names the organization and its runner group, or why the Worker serves none of it', async () => {
		time('2026-10-14T10:00:00Z');
		const { github, call } = await ready();
		const { cookie } = await signIn(github);
		const page = async () => {
			const { html, organization, repos } = (await (await send('/dashboard.json', { headers: { cookie: cookie! } })).json()) as {
				html: string;
				organization: unknown;
				repos: unknown;
			};
			return { html: html.replace(/\s+/g, ' '), organization, repos };
		};
		await withEnv(org, async () => {
			await call('GET', '/v1/repos');
			const served = await page();
			expect(served.organization).toEqual({ name: 'octo', group: 'boxrunner', problem: null });
			expect(served.repos).toEqual(['octo/site', 'octo/docs']);
			expect(served.html).toContain('<h3>Organization</h3> <p>octo, through one scale set per class in the runner group “boxrunner”.</p>');

			github.runnerGroups[1]!.visibility = 'all';
			vi.setSystemTime(Date.now() + 61_000);
			await call('GET', '/v1/repos');
			const refused = await page();
			expect(refused.html).toContain(
				'<p>octo, through one scale set per class in the runner group “boxrunner”. <span class="alert">The runner group “boxrunner” admits every repository of octo; set its repository access to selected repositories.</span> Until then the box serves none of its repositories.</p>',
			);
			expect(refused.html).toContain('<p>None: add a private repository to the runner group “boxrunner”.</p>');
		});
	});
});

describe('unserved jobs', () => {
	it('count a private repository the runner group does not admit as one the Worker does not serve', async () => {
		time('2026-10-14T10:00:00Z');
		const { github, call } = await ready();
		const { cookie } = await signIn(github);
		const deliver = async (repo: string, id: number) => {
			const body = JSON.stringify({
				action: 'queued',
				workflow_job: { id, html_url: `https://github.com/${repo}/actions/runs/501/job/${id}`, name: 'test', workflow_name: 'CI', labels: ['boxrunner'], created_at: '2026-10-14T10:00:00Z' },
				repository: { full_name: repo, private: true, archived: false },
			});
			const key = await crypto.subtle.importKey('raw', new TextEncoder().encode('hook-secret'), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
			const mac = new Uint8Array(await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(body)));
			const signature = `sha256=${Array.from(mac, (byte) => byte.toString(16).padStart(2, '0')).join('')}`;
			const response = await send('/webhook', { method: 'POST', headers: { 'x-github-event': 'workflow_job', 'x-hub-signature-256': signature }, body });
			expect(response.status).toBe(204);
		};
		await withEnv(org, async () => {
			await call('GET', '/v1/repos');
			await deliver('octo/site', 1001);
			await deliver('octo/other', 1002);
			const { unserved } = (await (await send('/dashboard.json', { headers: { cookie: cookie! } })).json()) as { unserved: { repo: string; reason: string }[] };
			expect(unserved.map(({ repo, reason }) => ({ repo, reason }))).toEqual([{ repo: 'octo/other', reason: 'unlisted' }]);
		});
	});
});
