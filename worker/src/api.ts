import type { Box } from './box';
import { matchesBoxToken } from './crypto';
import { offlineAfterSeconds } from './dashboard';
import { onHeartbeat } from './fallback';
import { shares } from './fleet';
import * as github from './github';
import { ApiError, errorResponse, json, noContent } from './http';
import { parse, parseHeartbeat, scopeId, scopeName, semverPattern, splitPaused, type App, type BoxName, type ClassName, type Login, type OwnedScaleSet, type RepoName, type ScaleSetId } from './model';
import type { ApiRoute } from './routes';

/** Agent major versions this Worker speaks protocol 1 with. */
const servedMajors = [0, 1];
const maxBodyBytes = 16 * 1024;

type Context = { box: Box; app: App; classes: readonly ClassName[]; request: Request };

export async function handleApi(box: Box, request: Request, route: ApiRoute): Promise<Response> {
	try {
		checkVersion(request.headers.get('boxrunner-version'));
		const stored = box.store.app();
		if (!stored) throw new ApiError('not_configured', `Setup has not finished. Open ${new URL(request.url).origin}/setup.`);
		const classes = box.classes();
		if (!classes.ok) throw new ApiError('not_configured', `The Worker's ${classes.problem}.`);
		const org = box.orgConfig();
		if (!org.ok) {
			// The value may name the organization; only the Worker's log, not an unauthenticated caller, sees it.
			console.error(`The Worker's ${org.problem}.`);
			throw new ApiError('not_configured', "The Worker's ORG or RUNNER_GROUP variable is not valid; the Worker's log says why.");
		}
		await authenticate(request, stored.boxTokenSha256);
		const wait = box.spend(Date.now());
		if (wait > 0) {
			throw new ApiError('rate_limited', 'Too many requests for this box token.', {
				'retry-after': String(Math.ceil(wait / 1000)),
			});
		}
		return await dispatch({ box, app: stored.app, classes: classes.classes, request }, route);
	} catch (error) {
		if (error instanceof ApiError) return errorResponse(error);
		if (error instanceof github.GitHubError) return errorResponse(new ApiError('github_error', error.message));
		throw error;
	}
}

function checkVersion(header: string | null) {
	const major = semverPattern.exec(header ?? '')?.[1];
	if (major !== undefined && servedMajors.includes(Number(major))) return;
	throw new ApiError(
		'upgrade_required',
		`This Worker serves agent versions ${servedMajors.map((m) => `${m}.x`).join(' and ')}; the agent sent ${
			header ? `"${header}"` : 'no Boxrunner-Version'
		}. Upgrade whichever is older.`,
	);
}

async function authenticate(request: Request, boxTokenSha256: string) {
	const presented = /^Bearer (\S+)$/.exec(request.headers.get('authorization') ?? '')?.[1];
	if (presented === undefined || !(await matchesBoxToken(presented, boxTokenSha256))) throw new ApiError('unauthorized', 'The box token is missing or wrong.');
}

async function body(request: Request): Promise<Record<string, unknown>> {
	const tooLarge = new ApiError('bad_request', 'The body is larger than 16 KB.');
	if (Number(request.headers.get('content-length') ?? 0) > maxBodyBytes) throw tooLarge;
	// A chunked body has no Content-Length, so the bytes are counted too.
	const bytes = await request.arrayBuffer();
	if (bytes.byteLength > maxBodyBytes) throw tooLarge;
	let value: unknown;
	try {
		value = JSON.parse(new TextDecoder().decode(bytes));
	} catch {
		value = undefined;
	}
	if (typeof value !== 'object' || value === null || Array.isArray(value)) {
		throw new ApiError('bad_request', 'The body must be a JSON object.');
	}
	return value as Record<string, unknown>;
}

function field<T>(value: T | undefined, name: string): T {
	if (value === undefined) throw new ApiError('bad_request', `"${name}" is missing or malformed.`);
	return value;
}

type Asked = { repo: RepoName } | { org: Login };

/** A scale set's scope as a request names it: `org` for an organization scale set, else `repo`. */
function scope(values: { repo?: unknown; org?: unknown }): Asked {
	if (values.org !== undefined && values.org !== null) return { org: field(parse.login(values.org), 'org') };
	return { repo: field(parse.repo(values.repo), 'repo') };
}

function query(request: Request) {
	const params = new URL(request.url).searchParams;
	return {
		asked: scope({ repo: params.get('repo'), org: params.get('org') }),
		scaleSetId: field(parse.scaleSetId(params.get('scaleSetId')), 'scaleSetId'),
	};
}

function servedClass({ classes }: Context, className: ClassName) {
	if (!classes.includes(className)) {
		throw new ApiError('unknown_class', `"${className}" is not one of this Worker's classes: ${classes.join(', ')}.`);
	}
}

/** The scale set, if the Worker created it for this served repository or organization and one of its classes. */
async function owned(context: Context, asked: Asked, id: ScaleSetId): Promise<OwnedScaleSet> {
	const scope = await context.box.servedScope(context.app, asked);
	const className = context.box.store.scaleSetClass(scopeId(scope), id);
	if (className === undefined) throw new ApiError('not_found', `Scale set ${id} is not one this Worker made for ${scopeName(scope)}.`);
	servedClass(context, className);
	return { scope, className, id };
}

function gone(context: Context, scaleSet: OwnedScaleSet): never {
	context.box.store.forgetScaleSet(scopeId(scaleSet.scope), scaleSet.id);
	throw new ApiError('not_found', `Scale set ${scaleSet.id} no longer exists at GitHub; find or create it again.`);
}

function sessionResponse(context: Context, scaleSet: OwnedScaleSet, result: github.SessionResult, refresh: boolean) {
	switch (result.kind) {
		case 'open':
			return json(result.session);
		case 'conflict':
			throw new ApiError('session_exists', `Another session holds scale set ${scaleSet.id}.`);
		case 'gone':
			if (refresh) throw new ApiError('not_found', 'The session is gone; open a new one.');
			return gone(context, scaleSet);
	}
}

function dispatch(context: Context, route: ApiRoute): Promise<Response> {
	switch (route.name) {
		case 'heartbeat':
			return heartbeat(context);
		case 'repos':
			return repos(context);
		case 'scalesets.ensure':
			return ensureScaleSet(context);
		case 'sessions.open':
			return openSession(context);
		case 'sessions.refresh':
			return refreshSession(context, route.id);
		case 'sessions.close':
			return closeSession(context, route.id);
		case 'runners.mint':
			return mintRunner(context);
		case 'runners.remove':
			return removeRunner(context, route.id);
		case 'jobs':
			return acquirableJobs(context);
	}
}

/**
 * The answer names the paused classes, so an agent learns of a pause within one heartbeat instead of at its next `GET /v1/repos`.
 * While two or more boxes serve one of the box's classes, it also carries the box's share of each of that class's scale sets.
 * `limits` caps the box's classes as set for it on the dashboard.
 */
async function heartbeat(context: Context) {
	const parsed = parseHeartbeat(await body(context.request));
	if (!parsed.ok) throw new ApiError('bad_request', `"${parsed.field}" is missing or malformed.`);
	const now = Date.now();
	await onHeartbeat(context.box, parsed.heartbeat, now);
	context.box.store.saveHeartbeat(parsed.heartbeat, now);
	const { paused } = splitPaused(context.classes, context.box.store.pauses());
	const all = context.box.store.limits();
	const limits = Object.fromEntries(all.filter((l) => l.box === parsed.heartbeat.box).map((l) => [l.className, l.max]));
	const fleet = shares(context.box.store.boxes(), new Set(paused), all, now).get(parsed.heartbeat.box);
	return json({ paused, limits, ...(fleet === undefined ? {} : { fleet: fleet.map(({ repo, className, jobs, repos }) => ({ repo, class: className, jobs, repos })) }) });
}

/**
 * `classes` leaves out the paused ones, so an agent that does not know `paused`
 * stops serving them too. `organization` is there only while the Worker serves one.
 */
async function repos({ box, app, classes }: Context) {
	const repos = await box.repos(app);
	const forks = await box.forkPullRequests(app, repos);
	const { serving, paused } = splitPaused(classes, box.store.pauses());
	const org = box.org();
	const organization = org === null ? {} : { organization: { name: org.login, group: org.group, problem: org.problem } };
	return json({ repos: repos.map(({ name, id }, i) => ({ name, id, forkPullRequests: forks[i] })), classes: serving, paused, ...organization });
}

async function ensureScaleSet(context: Context) {
	const input = await body(context.request);
	const asked = scope(input);
	const className = field(parse.className(input.class), 'class');
	servedClass(context, className);
	const served = await context.box.servedScope(context.app, asked);
	const id = await context.box.ensureScaleSet(context.app, served, className);
	return json({ id, name: className });
}

async function openSession(context: Context) {
	const input = await body(context.request);
	const asked = scope(input);
	const id = field(parse.scaleSetId(input.scaleSetId), 'scaleSetId');
	const owner = field(parse.boxName(input.owner), 'owner');
	const scaleSet = await owned(context, asked, id);
	await context.box.assertEligible(context.app, scaleSet.scope, id);
	const result = await context.box.actions(context.app, scaleSet.scope, (c) => github.openSession(c, id, owner));
	if (result.kind === 'conflict') throw sessionExists(context, scaleSet, owner);
	if (result.kind === 'open') context.box.store.holdSession(scopeId(scaleSet.scope), id, result.session.sessionId, owner);
	return sessionResponse(context, scaleSet, result, false);
}

/** Names the box whose session holds the scale set when another box that still reports opened it. */
function sessionExists(context: Context, { scope, className, id }: OwnedScaleSet, asker: BoxName) {
	const holder = context.box.store.sessionHolder(scopeId(scope), id, asker, Date.now() - offlineAfterSeconds * 1000);
	if (holder === undefined) return new ApiError('session_exists', `Another session holds scale set ${id}.`);
	return new ApiError('session_exists', `${holder} holds the session of scale set ${id}, so it serves ${scopeName(scope)} ${className}.`, {}, { heldBy: holder });
}

async function refreshSession(context: Context, routeId: string) {
	const sessionId = field(parse.sessionId(routeId), 'sessionId');
	const input = await body(context.request);
	const asked = scope(input);
	const id = field(parse.scaleSetId(input.scaleSetId), 'scaleSetId');
	// Older agents send no owner. A session opened before the Worker kept holders gets one here.
	const owner = input.owner === undefined ? undefined : field(parse.boxName(input.owner), 'owner');
	const scaleSet = await owned(context, asked, id);
	await context.box.assertEligible(context.app, scaleSet.scope, id);
	const result = await context.box.actions(context.app, scaleSet.scope, (c) => github.refreshSession(c, id, sessionId));
	if (result.kind === 'open' && owner !== undefined) context.box.store.holdSession(scopeId(scaleSet.scope), id, result.session.sessionId, owner);
	return sessionResponse(context, scaleSet, result, true);
}

async function closeSession(context: Context, routeId: string) {
	const sessionId = field(parse.sessionId(routeId), 'sessionId');
	const { asked, scaleSetId } = query(context.request);
	const scaleSet = await owned(context, asked, scaleSetId);
	await context.box.actions(context.app, scaleSet.scope, (c) => github.closeSession(c, scaleSetId, sessionId));
	context.box.store.releaseSession(scopeId(scaleSet.scope), scaleSetId, sessionId);
	return noContent();
}

async function mintRunner(context: Context) {
	const input = await body(context.request);
	const asked = scope(input);
	const id = field(parse.scaleSetId(input.scaleSetId), 'scaleSetId');
	const runnerName = field(parse.runnerName(input.name), 'name');
	const scaleSet = await owned(context, asked, id);
	if (scaleSet.scope.kind === 'org') await context.box.assertJobRepos(context.app, field(parse.jobRepos(input.jobRepos), 'jobRepos'));
	await context.box.assertEligible(context.app, scaleSet.scope, id);
	const result = await context.box.actions(context.app, scaleSet.scope, (c) => github.generateJitConfig(c, id, runnerName));
	if (result.kind === 'gone') return gone(context, scaleSet);
	return json({ runnerId: result.runnerId, jitConfig: result.jitConfig });
}

async function removeRunner(context: Context, routeId: string) {
	const runnerId = field(parse.runnerId(routeId), 'runnerId');
	const { asked, scaleSetId } = query(context.request);
	const scaleSet = await owned(context, asked, scaleSetId);
	const outcome = await context.box.actions(context.app, scaleSet.scope, async (c) => {
		const belongsTo = await github.runnerScaleSet(c, runnerId);
		if (belongsTo === undefined) return 'removed';
		if (belongsTo !== scaleSetId) return 'foreign';
		return github.removeRunner(c, runnerId);
	});
	if (outcome === 'foreign') throw new ApiError('not_found', `Runner ${runnerId} does not belong to scale set ${scaleSetId}.`);
	if (outcome === 'busy') throw new ApiError('runner_busy', `Runner ${runnerId} is running a job.`);
	return noContent();
}

/**
 * The jobs an organization scale set may still acquire, of repositories the
 * Worker serves. GitHub offers a job once; the agent asks here when a session
 * opens and when its list of repositories changes, for the jobs it turned down.
 */
async function acquirableJobs(context: Context) {
	const { asked, scaleSetId } = query(context.request);
	const scaleSet = await owned(context, asked, scaleSetId);
	const jobs = await context.box.actions(context.app, scaleSet.scope, (c) => github.acquirableJobs(c, scaleSetId));
	if (jobs === undefined) return gone(context, scaleSet);
	const { scope } = scaleSet;
	if (scope.kind === 'repo') return json({ jobs });
	const served = new Set((await context.box.repos(context.app)).map((repo) => repo.name.toLowerCase()));
	const name = (job: github.AcquirableJob) => (job.repositoryName.includes('/') ? job.repositoryName : `${job.ownerName || scope.org.login}/${job.repositoryName}`);
	return json({ jobs: jobs.filter((job) => job.repositoryName !== '' && served.has(name(job).toLowerCase())) });
}
