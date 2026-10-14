import { afterEach, describe, expect, it, vi } from 'vitest';
import { expectError, fakeGitHub, postForm, send, setUp, signIn, time, withEnv } from './harness';

afterEach(() => {
	vi.useRealTimers();
});

/** The secret the fake GitHub hands back when setup converts the manifest. */
const storedSecret = 'hook-secret';

async function signature(secret: string, body: string) {
	const key = await crypto.subtle.importKey('raw', new TextEncoder().encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
	const mac = new Uint8Array(await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(body)));
	return `sha256=${Array.from(mac, (byte) => byte.toString(16).padStart(2, '0')).join('')}`;
}

/** Delivers `payload` as GitHub does, signed with `secret`. */
async function deliver(event: string, payload: unknown, secret = storedSecret) {
	const body = JSON.stringify(payload);
	return send('/webhook', {
		method: 'POST',
		headers: { 'content-type': 'application/json', 'x-github-event': event, 'x-hub-signature-256': await signature(secret, body) },
		body,
	});
}

type JobFields = { id?: number; labels?: string[]; created?: string; repo?: string; private?: boolean; archived?: boolean };

/** A trimmed `workflow_job` payload with the fields GitHub sends and the Worker reads. */
const jobEvent = (action: string, { id = 1001, labels = ['boxrunner'], created = '2026-10-14T10:00:00Z', ...repo }: JobFields = {}) => ({
	action,
	workflow_job: {
		id,
		run_id: 501,
		html_url: `https://github.com/${repo.repo ?? 'octo/site'}/actions/runs/501/job/${id}`,
		status: action,
		name: 'test',
		workflow_name: 'CI',
		labels,
		created_at: created,
		steps: [],
	},
	repository: { id: 123, full_name: repo.repo ?? 'octo/site', private: repo.private ?? true, archived: repo.archived ?? false },
	installation: { id: 1 },
});

async function ready() {
	time('2026-10-14T10:00:00Z');
	const github = await fakeGitHub();
	await setUp(github);
	const { cookie } = await signIn(github);
	const page = async (path = '/') => (await send(path, { headers: { cookie: cookie! } })).text();
	const pause = async (className: string) => {
		const csrf = /name="csrf" value="([^"]+)"/.exec(await page())![1]!;
		const response = await postForm('/classes/pause', { csrf, class: className }, { cookie: cookie! });
		expect(response.status).toBe(303);
	};
	return { page, pause };
}

const row = (repo: string, id: number, labels: string, why: string, waited: string) =>
	`<tr><td>${repo}</td><td><a href="https://github.com/${repo}/actions/runs/501/job/${id}">CI / test</a></td><td><code>${labels}</code></td><td><span class="alert">${why}</span></td><td class="number">${waited}</td></tr>`;

const off =
	'<p>Off. With the App’s <code>workflow_job</code> event, this lists the jobs that wait for a runner no box here will start (<a href="https://boxrunner.zabcik.me/docs/unserved-jobs/">turning it on</a>).</p>';

describe('POST /webhook', () => {
	it('lists a queued job whose runs-on names no class, with a link to it, until a runner takes it', async () => {
		const { page } = await ready();
		expect(await page()).toContain(off);
		expect((await deliver('workflow_job', jobEvent('queued', { labels: ['boxruner'] }))).status).toBe(204);
		time('2026-10-14T10:02:00Z');
		const html = await page();
		expect(html).toContain(row('octo/site', 1001, 'boxruner', 'runs-on is not one of this Worker’s classes', '2 min 0 s'));
		expect(html).toContain('the last delivery arrived <time datetime="2026-10-14T10:00:00.000Z">2 minutes ago</time>');
		const { unserved, webhook } = JSON.parse(await page('/dashboard.json')) as Record<string, unknown>;
		expect({ unserved, webhook }).toEqual({
			unserved: [
				{
					repo: 'octo/site',
					workflow: 'CI',
					name: 'test',
					labels: ['boxruner'],
					url: 'https://github.com/octo/site/actions/runs/501/job/1001',
					reason: 'label',
					queued: '2026-10-14T10:00:00.000Z',
					waitSeconds: 120,
				},
			],
			webhook: { lastDelivery: '2026-10-14T10:00:00.000Z', secondsSince: 120 },
		});
		expect((await deliver('workflow_job', jobEvent('in_progress', { labels: ['boxruner'] }))).status).toBe(204);
		expect(await page()).toContain('<h2 id="unserved">Jobs nobody here runs</h2>\n<p>None.</p>');
	});

	it('names repositories the Worker does not serve, and leaves out jobs for GitHub’s runners and other repositories', async () => {
		const { page } = await ready();
		await withEnv({ REPOS: 'octo/site,octo/old' }, async () => {
			const events = [
				jobEvent('queued', { id: 1, repo: 'octo/open', private: false }),
				jobEvent('queued', { id: 2, repo: 'octo/old', archived: true }),
				jobEvent('queued', { id: 3, repo: 'octo/docs' }),
				jobEvent('queued', { id: 4, labels: ['ubuntu-latest'] }),
				jobEvent('queued', { id: 5, repo: 'octo/docs', labels: ['gpu'] }),
				jobEvent('queued', { id: 6, labels: ['boxrunner', 'linux'] }),
			];
			for (const event of events) expect((await deliver('workflow_job', event)).status).toBe(204);
			const html = await page();
			const rows = html.match(/<tr><td>octo\/[^]*?<\/tr>/g)?.filter((tr) => tr.includes('/actions/runs/'));
			expect(rows).toEqual([
				row('octo/open', 1, 'boxrunner', 'The repository is public, which the Worker never serves', '0 s'),
				row('octo/old', 2, 'boxrunner', 'The repository is archived', '0 s'),
				row('octo/docs', 3, 'boxrunner', 'The repository is not in REPOS', '0 s'),
				row('octo/site', 6, 'boxrunner, linux', 'runs-on is not one of this Worker’s classes', '0 s'),
			]);
		});
	});

	it('lists a job on a served class once it waited 10 minutes, and says when its class is paused', async () => {
		const { page, pause } = await ready();
		await deliver('workflow_job', jobEvent('queued', { id: 1 }));
		await deliver('workflow_job', jobEvent('queued', { id: 2, labels: ['boxrunner-light'] }));
		time('2026-10-14T10:09:59Z');
		expect(await page()).toContain('<p>None.</p>');
		await pause('boxrunner-light');
		time('2026-10-14T10:10:00Z');
		const html = await page();
		expect(html).toContain(row('octo/site', 1, 'boxrunner', 'No box took it within 10 minutes', '10 min 0 s'));
		expect(html).toContain(row('octo/site', 2, 'boxrunner-light', 'Its class is paused', '10 min 0 s'));
	});

	it('does not list a job again when its queued delivery arrives after the runner took it, and forgets it once it completes', async () => {
		const { page } = await ready();
		await deliver('workflow_job', jobEvent('in_progress', { labels: ['gpu'] }));
		await deliver('workflow_job', jobEvent('queued', { labels: ['gpu'] }));
		expect(await page()).toContain('<p>None.</p>');
		await deliver('workflow_job', jobEvent('completed', { labels: ['gpu'] }));
		await deliver('workflow_job', jobEvent('queued', { labels: ['gpu'] }));
		expect(await page()).toContain(row('octo/site', 1001, 'gpu', 'runs-on is not one of this Worker’s classes', '0 s'));
		await deliver('workflow_job', jobEvent('completed', { labels: ['gpu'] }));
		expect(await page()).toContain('<p>None.</p>');
	});

	it('forgets jobs after a day, and keeps the newest 200', async () => {
		const { page } = await ready();
		await deliver('workflow_job', jobEvent('queued', { id: 1, labels: ['gpu'], created: '2026-10-13T09:59:59Z' }));
		expect(await page()).toContain('<p>None.</p>');
		for (let id = 2; id <= 202; id++) {
			const created = new Date(Date.parse('2026-10-14T09:00:00Z') + id * 1000).toISOString();
			await deliver('workflow_job', jobEvent('queued', { id, labels: ['gpu'], created }));
		}
		const ids = [...(await page()).matchAll(/\/actions\/runs\/501\/job\/(\d+)"/g)].map((match) => Number(match[1]));
		expect(ids.length).toBe(200);
		expect([ids[0], ids.at(-1)]).toEqual([3, 202]);
	});

	it('acknowledges ping and other events without listing anything', async () => {
		const { page } = await ready();
		expect((await deliver('ping', { zen: 'Keep it logically awesome.', hook_id: 1 })).status).toBe(204);
		expect((await deliver('installation', jobEvent('queued', { labels: ['gpu'] }))).status).toBe(204);
		expect(await page()).toContain('<p>None.</p>');
	});

	it('refuses a delivery whose signature is wrong or missing, and lists nothing', async () => {
		const { page } = await ready();
		await expectError(
			await deliver('workflow_job', jobEvent('queued', { labels: ['gpu'] }), 'another-secret'),
			401,
			'unauthorized',
			'The X-Hub-Signature-256 header is missing or does not match the webhook secret.',
		);
		const unsigned = await send('/webhook', { method: 'POST', headers: { 'x-github-event': 'workflow_job' }, body: JSON.stringify(jobEvent('queued')) });
		await expectError(unsigned, 401, 'unauthorized');
		expect(await page()).toContain(off);
	});

	it('refuses a body over 256 KB, with or without Content-Length', async () => {
		await ready();
		const big = { ...jobEvent('completed', { labels: ['gpu'] }), padding: 'x'.repeat(256 * 1024) };
		await expectError(await deliver('workflow_job', big), 400, 'bad_request', 'The body is larger than 256 KB.');
		const body = JSON.stringify(big);
		const chunked = new ReadableStream({
			start(controller) {
				controller.enqueue(new TextEncoder().encode(body));
				controller.close();
			},
		});
		const streamed = await send('/webhook', {
			method: 'POST',
			headers: { 'x-github-event': 'workflow_job', 'x-hub-signature-256': await signature(storedSecret, body) },
			body: chunked,
		});
		await expectError(streamed, 400, 'bad_request', 'The body is larger than 256 KB.');
	});

	it('refuses a signed workflow_job delivery it cannot read', async () => {
		await ready();
		const event = jobEvent('queued');
		await expectError(
			await deliver('workflow_job', { ...event, workflow_job: { ...event.workflow_job, html_url: 'javascript:alert(1)' } }),
			400,
			'bad_request',
			'"workflow_job.html_url" is missing or malformed.',
		);
	});

	it('checks deliveries with WEBHOOK_SECRET when it is set', async () => {
		await ready();
		await withEnv({ WEBHOOK_SECRET: 'changed-in-github' }, async () => {
			expect((await deliver('ping', {}, storedSecret)).status).toBe(401);
			expect((await deliver('ping', {}, 'changed-in-github')).status).toBe(204);
		});
	});

	it('answers 503 before setup', async () => {
		await fakeGitHub();
		await expectError(await deliver('ping', {}), 503, 'not_configured', 'This Worker has no webhook secret; see docs/unserved-jobs.md.');
	});
});
