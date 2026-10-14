import type { Box } from './box';
import { matchesBoxToken } from './crypto';
import * as github from './github';
import { ApiError, errorResponse, json, noContent } from './http';
import { parse, type App, type ClassName, type OwnedScaleSet, type RepoName, type ScaleSetId } from './model';
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
	const major = /^(\d+)\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]*)?$/.exec(header ?? '')?.[1];
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
	if (Number(request.headers.get('content-length') ?? 0) > maxBodyBytes) {
		throw new ApiError('bad_request', 'The body is larger than 16 KB.');
	}
	const value: unknown = await request.json().catch(() => undefined);
	if (typeof value !== 'object' || value === null || Array.isArray(value)) {
		throw new ApiError('bad_request', 'The body must be a JSON object.');
	}
	return value as Record<string, unknown>;
}

function field<T>(value: T | undefined, name: string): T {
	if (value === undefined) throw new ApiError('bad_request', `"${name}" is missing or malformed.`);
	return value;
}

function query(request: Request) {
	const params = new URL(request.url).searchParams;
	return {
		repo: field(parse.repo(params.get('repo')), 'repo'),
		scaleSetId: field(parse.scaleSetId(params.get('scaleSetId')), 'scaleSetId'),
	};
}

function servedClass({ classes }: Context, className: ClassName) {
	if (!classes.includes(className)) {
		throw new ApiError('unknown_class', `"${className}" is not one of this Worker's classes: ${classes.join(', ')}.`);
	}
}

/** The scale set, if the Worker created it for this served repository and one of its classes. */
async function owned(context: Context, name: RepoName, id: ScaleSetId): Promise<OwnedScaleSet> {
	const repo = await context.box.servedRepo(context.app, name);
	const className = context.box.store.scaleSetClass(repo.id, id);
	if (className === undefined) throw new ApiError('not_found', `Scale set ${id} is not one this Worker made for ${repo.name}.`);
	servedClass(context, className);
	return { repo, className, id };
}

function gone(context: Context, scaleSet: OwnedScaleSet): never {
	context.box.store.forgetScaleSet(scaleSet.repo.id, scaleSet.id);
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
	}
}

async function repos({ box, app, classes }: Context) {
	const repos = await box.repos(app);
	return json({ repos: repos.map(({ name, id }) => ({ name, id })), classes });
}

async function ensureScaleSet(context: Context) {
	const input = await body(context.request);
	const name = field(parse.repo(input.repo), 'repo');
	const className = field(parse.className(input.class), 'class');
	servedClass(context, className);
	const repo = await context.box.servedRepo(context.app, name);
	const id = await context.box.ensureScaleSet(context.app, repo, className);
	return json({ id, name: className });
}

async function openSession(context: Context) {
	const input = await body(context.request);
	const name = field(parse.repo(input.repo), 'repo');
	const id = field(parse.scaleSetId(input.scaleSetId), 'scaleSetId');
	const owner = field(parse.boxName(input.owner), 'owner');
	const scaleSet = await owned(context, name, id);
	await context.box.assertEligible(context.app, scaleSet.repo);
	const result = await context.box.actions(context.app, scaleSet.repo, (c) => github.openSession(c, id, owner));
	return sessionResponse(context, scaleSet, result, false);
}

async function refreshSession(context: Context, routeId: string) {
	const sessionId = field(parse.sessionId(routeId), 'sessionId');
	const input = await body(context.request);
	const name = field(parse.repo(input.repo), 'repo');
	const id = field(parse.scaleSetId(input.scaleSetId), 'scaleSetId');
	const scaleSet = await owned(context, name, id);
	await context.box.assertEligible(context.app, scaleSet.repo);
	const result = await context.box.actions(context.app, scaleSet.repo, (c) => github.refreshSession(c, id, sessionId));
	return sessionResponse(context, scaleSet, result, true);
}

async function closeSession(context: Context, routeId: string) {
	const sessionId = field(parse.sessionId(routeId), 'sessionId');
	const { repo, scaleSetId } = query(context.request);
	const scaleSet = await owned(context, repo, scaleSetId);
	await context.box.actions(context.app, scaleSet.repo, (c) => github.closeSession(c, scaleSetId, sessionId));
	return noContent();
}

async function mintRunner(context: Context) {
	const input = await body(context.request);
	const name = field(parse.repo(input.repo), 'repo');
	const id = field(parse.scaleSetId(input.scaleSetId), 'scaleSetId');
	const runnerName = field(parse.runnerName(input.name), 'name');
	const scaleSet = await owned(context, name, id);
	await context.box.assertEligible(context.app, scaleSet.repo);
	const result = await context.box.actions(context.app, scaleSet.repo, (c) => github.generateJitConfig(c, id, runnerName));
	if (result.kind === 'gone') return gone(context, scaleSet);
	return json({ runnerId: result.runnerId, jitConfig: result.jitConfig });
}

async function removeRunner(context: Context, routeId: string) {
	const runnerId = field(parse.runnerId(routeId), 'runnerId');
	const { repo, scaleSetId } = query(context.request);
	const scaleSet = await owned(context, repo, scaleSetId);
	const outcome = await context.box.actions(context.app, scaleSet.repo, async (c) => {
		const belongsTo = await github.runnerScaleSet(c, runnerId);
		if (belongsTo === undefined) return 'removed';
		if (belongsTo !== scaleSetId) return 'foreign';
		return github.removeRunner(c, runnerId);
	});
	if (outcome === 'foreign') throw new ApiError('not_found', `Runner ${runnerId} does not belong to scale set ${scaleSetId}.`);
	if (outcome === 'busy') throw new ApiError('runner_busy', `Runner ${runnerId} is running a job.`);
	return noContent();
}
