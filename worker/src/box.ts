import { boxTokenHash, randomToken, sameDigest, sha256, signAppJwt } from './crypto';
import { variable, type Alarms, type Watch } from './fallback';
import * as github from './github';
import { ApiError } from './http';
import {
	groupProblem,
	mayOpenDashboard,
	parseAlerts,
	parseClasses,
	parseFallback,
	parseOrg,
	repoFilter,
	scopeId,
	scopeName,
	type AlertConfig,
	type App,
	type ClassName,
	type Classes,
	type FallbackConfig,
	type ForkPullRequests,
	type Login,
	type Org,
	type OrgConfig,
	type Repo,
	type RepoName,
	type RunnerGroup,
	type ScaleSetId,
	type Scope,
} from './model';
import type { Store } from './store';
import type { Served } from './unserved';

const minute = 60_000;
/** The App JWT and the Actions admin token both live ten minutes; a minute of margin covers clock drift. */
const tokenLifetime = 9 * minute;
const repoListLifetime = minute;
const forkSettingsLifetime = 60 * minute;
const forkSettingsRetry = 10 * minute;
/** Each read takes two GitHub calls; the cap keeps one listing well under a Worker's subrequest limit, however many repositories it serves. */
const forkReadsPerListing = 5;
const forkKey = (repoId: number) => `forks:${repoId}`;

/**
 * Each box token may spend `burst` requests at once, then one every
 * `refillMs`. That allows 1,800 an hour, under half of the free plan's
 * 100,000 requests a day, so a runaway agent loop cannot spend it.
 */
export const budget = { burst: 120, refillMs: 2_000 };

/** The form that made a new box token; only that form's page shows it. */
export type TokenForm = 'setup' | 'dashboard';

/** A new box token waiting for the one page view that shows it. */
type NewToken = { idSha256: Uint8Array; form: TokenForm; token: string; until: number };

/**
 * Everything the Worker derives from its stored state: GitHub credentials,
 * the served repositories and the scale sets. One Durable Object holds the
 * only instance, so the in-flight map makes every derivation single-flight.
 */
export class Box {
	private readonly inflight = new Map<string, Promise<unknown>>();
	private tokens = budget.burst;
	private refilledAt = 0;
	/** Only in memory: storage's point-in-time recovery would keep it for 30 days. */
	private newToken: NewToken | undefined;
	setupRunning = false;

	constructor(
		readonly store: Store,
		private readonly env: Env,
		readonly alarms: Alarms,
	) {}

	get setupSecret(): string | undefined {
		return this.env.SETUP_SECRET && this.env.SETUP_SECRET.length >= 32 ? this.env.SETUP_SECRET : undefined;
	}

	classes(): Classes {
		return parseClasses(this.env.CLASSES);
	}

	orgConfig(): OrgConfig {
		return parseOrg(this.env.ORG, this.env.RUNNER_GROUP);
	}

	/** The organization the Worker serves, as its last listing found it; null while it serves repositories one by one or before the first listing. */
	org(): Org | null {
		const config = this.orgConfig();
		const stored = this.store.lastCached(orgKey);
		if (!config.ok || config.org === null || stored === undefined) return null;
		const org = JSON.parse(stored) as Org;
		return org.login.toLowerCase() === config.org.login.toLowerCase() && org.group === config.org.group ? org : null;
	}

	fallback(): FallbackConfig {
		return parseFallback(this.env.FALLBACK_MINUTES);
	}

	/** How long the box may stay silent before its jobs go to GitHub's runners; null when fallback is off or misconfigured. */
	fallbackAfterMs(): number | null {
		const config = this.fallback();
		return config.ok ? config.afterMs : null;
	}

	alerts(): AlertConfig {
		return parseAlerts(this.env.ALERT_WEBHOOK, this.env.ALERT_MINUTES);
	}

	/** One notion of down for fallback and alerts: with fallback on, its minutes decide. */
	watch(): Watch {
		const fallbackMs = this.fallbackAfterMs();
		const alerts = this.alerts();
		const webhook = alerts.ok ? alerts.webhook : null;
		const alertMs = alerts.ok && webhook !== null ? alerts.afterMs : null;
		return { afterMs: fallbackMs ?? alertMs, fallback: fallbackMs !== null, webhook };
	}

	/** What the Worker serves, for telling which queued jobs it should run. */
	served(): Served {
		const classes = this.classes();
		const all = classes.ok ? classes.classes : [];
		const paused = new Set(this.store.pauses().map((pause) => pause.className));
		const config = this.orgConfig();
		if (!config.ok || config.org === null) return { classes: all, paused: all.filter((name) => paused.has(name)), listed: repoFilter(this.env.REPOS) };
		// For an organization, the runner group decides: a repository it does not admit is not served.
		const cached = this.store.lastCached('repos');
		const served = new Set((cached === undefined ? [] : (JSON.parse(cached) as Repo[])).map((repo) => repo.name.toLowerCase()));
		return { classes: all, paused: all.filter((name) => paused.has(name)), listed: (repo) => served.has(repo.toLowerCase()) };
	}

	/** The `WEBHOOK_SECRET` secret when it is set, else the secret GitHub made when setup created the App. */
	webhookSecret(): string | undefined {
		return this.env.WEBHOOK_SECRET?.trim() || this.store.app()?.app.webhookSecret || undefined;
	}

	/** Signs dashboard sessions and their CSRF tokens; made once and forgotten by a reset. */
	sessionKey(): string {
		return this.store.key('session', randomToken);
	}

	mayOpenDashboard(owner: string | null): (login: Login) => boolean {
		return mayOpenDashboard(owner, this.env.ADMINS);
	}

	/** Setup keeps the App's client secret; an App set up before that needs the GITHUB_CLIENT_SECRET secret. */
	canSignIn(app: App): boolean {
		return this.clientSecret(app) !== undefined;
	}

	private clientSecret(app: App): string | undefined {
		return app.clientSecret ?? (this.env.GITHUB_CLIENT_SECRET || undefined);
	}

	/** The App's OAuth client and owner. An App set up before the Worker kept them gets them from GitHub once. */
	async oauthClient(app: App): Promise<{ client: github.OAuthClient; owner: string } | undefined> {
		const secret = this.clientSecret(app);
		if (secret === undefined) return undefined;
		if (app.clientId !== null && app.owner !== null) return { client: { id: app.clientId, secret }, owner: app.owner };
		const profile = await this.once('profile', async () => github.appProfile(await this.jwt(app)));
		this.store.saveAppProfile(profile.clientId, profile.owner);
		return { client: { id: profile.clientId, secret }, owner: profile.owner };
	}

	/** Takes one request from the budget, or answers how many milliseconds until one is free. */
	spend(now: number): number {
		this.tokens = Math.min(budget.burst, this.tokens + (now - this.refilledAt) / budget.refillMs);
		this.refilledAt = now;
		if (this.tokens >= 1) {
			this.tokens -= 1;
			return 0;
		}
		return Math.ceil((1 - this.tokens) * budget.refillMs);
	}

	resetBudget() {
		this.tokens = budget.burst;
	}

	/**
	 * Makes a new box token; the old one stops working at once. The Worker
	 * keeps its hash, and the token itself for a minute under the returned
	 * random id, so a redirect can show it without a reload replacing it again.
	 */
	async replaceBoxToken(form: TokenForm, now: number): Promise<string> {
		const token = randomToken();
		const id = randomToken();
		const [tokenSha256, idSha256] = await Promise.all([boxTokenHash(token), sha256(id)]);
		this.store.replaceBoxToken(tokenSha256);
		this.resetBudget();
		this.newToken = { idSha256, form, token, until: now + minute };
		return id;
	}

	/** The new token for the first view of its id within the minute; later views get nothing. */
	async takeNewToken(id: string, form: TokenForm, now: number): Promise<string | undefined> {
		const presented = await sha256(id);
		const waiting = this.newToken;
		if (waiting?.form !== form || !sameDigest(presented, waiting.idSha256)) return undefined;
		this.newToken = undefined;
		return now < waiting.until ? waiting.token : undefined;
	}

	private once<T>(key: string, derive: () => Promise<T>): Promise<T> {
		const running = this.inflight.get(key);
		if (running) return running as Promise<T>;
		const promise = derive().finally(() => this.inflight.delete(key));
		this.inflight.set(key, promise);
		return promise;
	}

	private memo<T>(key: string, lifetime: number, derive: () => Promise<T>): Promise<T> {
		const stored = this.store.cached(key, Date.now());
		if (stored !== undefined) return Promise.resolve(JSON.parse(stored) as T);
		return this.once(key, async () => {
			const value = await derive();
			this.store.cache(key, JSON.stringify(value), Date.now() + lifetime);
			return value;
		});
	}

	private jwt(app: App): Promise<string> {
		return this.memo('jwt', tokenLifetime, () => signAppJwt(app.id, app.key, Math.floor(Date.now() / 1000)));
	}

	/**
	 * Private, unarchived repositories across the App's installations, narrowed
	 * by `REPOS` when it is set. With `ORG` set, the private, unarchived
	 * repositories its runner group admits instead, narrowed the same way.
	 */
	repos(app: App): Promise<Repo[]> {
		return this.memo('repos', repoListLifetime, async () => {
			const jwt = await this.jwt(app);
			const listed = repoFilter(this.env.REPOS);
			const config = this.orgConfig();
			const all = await github.installations(jwt);
			if (config.ok && config.org !== null) {
				const { org, served } = await this.orgRepos(app, jwt, all, config.org, listed);
				this.store.cache(orgKey, JSON.stringify(org), Date.now() + repoListLifetime);
				// The installation's own list matters only for deleting repository scale sets made before the switch.
				const repoScopes = this.store.scaleSetRepos().some((id) => id > 0);
				const installed = repoScopes ? await this.installedRepos(jwt, all) : [];
				await this.dropUnserved(app, installed, [], org);
				return served;
			}
			const installed = await this.installedRepos(jwt, all);
			const served = installed.filter((repo) => repo.private && !repo.archived && listed(repo.name)).map(({ name, id, installationId }) => ({ name, id, installationId }));
			await this.dropUnserved(app, installed, served, null);
			return served;
		});
	}

	private async installedRepos(jwt: string, installations: github.Installation[]) {
		const installed: (Repo & { private: boolean; archived: boolean })[] = [];
		for (const installation of installations) {
			// Listing needs a token for the whole installation, so it gets metadata and nothing else.
			const token = await github.installationToken(jwt, installation.id, { metadata: 'read' });
			for (const repo of await github.installationRepos(token)) installed.push({ ...repo, installationId: installation.id });
		}
		return installed;
	}

	/**
	 * Finds the organization's installation and runner group, and the repositories
	 * the group admits. A group that admits public repositories, or every
	 * repository rather than selected ones, serves nothing; one that admits
	 * public repositories also loses its scale sets, which disconnects their runners.
	 */
	private async orgRepos(app: App, jwt: string, installations: github.Installation[], wanted: { login: Login; group: string }, listed: (name: string) => boolean) {
		const org: Org = { login: wanted.login, id: null, installationId: null, group: wanted.group, groupId: null, problem: null };
		const none = (problem: string) => ({ org: { ...org, problem }, served: [] as Repo[] });
		const installation = installations.find((candidate) => candidate.organization && candidate.login.toLowerCase() === wanted.login.toLowerCase());
		if (installation === undefined) return none(`The App is not installed on the organization ${wanted.login}.`);
		org.id = installation.accountId;
		org.installationId = installation.id;
		let token: string;
		try {
			token = await github.installationToken(jwt, installation.id, { organization_self_hosted_runners: 'read' });
		} catch (error) {
			if (!(error instanceof github.GitHubError && error.status === 422)) throw error;
			return none(`The App lacks the organization permission Self-hosted runners (read and write), or ${wanted.login} has not approved it.`);
		}
		const group = await github.findRunnerGroup(token, wanted.login, wanted.group);
		const scope: Scope = { kind: 'org', org: { ...org, id: installation.accountId, installationId: installation.id } };
		// A scale set made in another group, one since renamed or deleted, or before RUNNER_GROUP changed, is in a group the Worker no longer checks.
		await this.dropOtherGroups(app, scope, group?.id ?? null);
		if (group === undefined) return none(`${wanted.login} has no runner group named “${wanted.group}”.`);
		org.groupId = group.id;
		const problem = groupProblem(wanted.login, group);
		if (problem !== null) {
			if (group.allowsPublicRepositories) await this.deleteScaleSets(app, scope);
			return none(problem);
		}
		const served = (await github.runnerGroupRepos(token, wanted.login, group.id))
			.filter((repo) => repo.private && !repo.archived && listed(repo.name))
			.map(({ name, id }) => ({ name, id, installationId: installation.id }));
		return { org, served };
	}

	/**
	 * Deletes the scale sets of repositories the Worker no longer serves (public,
	 * archived or left out of `REPOS`), so they leave the repository's runner
	 * settings. With `ORG` set, every repository scale set goes, and the scale
	 * sets of any organization but `org`. Where the App is gone, or lacks the
	 * permission, there is nothing to delete with, and a failure waits for the
	 * next listing.
	 */
	private async dropUnserved(app: App, installed: Repo[], served: Repo[], org: Org | null) {
		const servedIds = new Set(served.map((repo) => repo.id));
		const keptOrg = org?.id === null || org?.id === undefined ? undefined : -org.id;
		for (const scope of this.store.scaleSetRepos()) {
			if (servedIds.has(scope) || scope === keptOrg) continue;
			const repo = installed.find((candidate) => candidate.id === scope);
			for (const id of this.store.scaleSetsOf(scope)) {
				if (repo) {
					try {
						await this.actions(app, { kind: 'repo', repo }, (connection) => github.deleteScaleSet(connection, id));
					} catch (error) {
						if (!(error instanceof github.GitHubError && [403, 404, 422].includes(error.status))) continue;
					}
				}
				this.store.forgetScaleSet(scope, id);
			}
		}
	}

	/**
	 * Deletes the organization's scale sets that the Worker did not make in the
	 * group `groupId`, or all of them when it is null. A failure leaves the
	 * scale set for the next listing; until then the group check before every
	 * session and runner refuses it.
	 */
	private async dropOtherGroups(app: App, scope: Scope, groupId: number | null) {
		const key = scopeId(scope);
		for (const id of this.store.scaleSetsOf(key)) {
			if (groupId !== null && this.store.scaleSetGroup(key, id) === groupId) continue;
			try {
				await this.actions(app, scope, (connection) => github.deleteScaleSet(connection, id));
			} catch (error) {
				if (!(error instanceof github.GitHubError)) throw error;
				if (![403, 404, 422].includes(error.status)) continue;
			}
			this.store.forgetScaleSet(key, id);
		}
	}

	/**
	 * Reads the fork pull request settings of up to `forkReadsPerListing`
	 * repositories whose last read is over an hour old, or ten minutes after
	 * a failed one, then answers what is known for each, null before a read
	 * succeeded. A failed read never fails the listing that asked.
	 */
	async forkPullRequests(app: App, repos: Repo[]): Promise<(ForkPullRequests | null)[]> {
		// An App that serves an organization has no repository Administration permission to read them with.
		const config = this.orgConfig();
		if (config.ok && config.org !== null) return repos.map(() => null);
		await this.once('forks', async () => {
			const now = Date.now();
			const due = repos.filter((repo) => this.store.cached(forkKey(repo.id), now) === undefined).slice(0, forkReadsPerListing);
			await Promise.all(
				due.map(async (repo) => {
					try {
						const token = await this.narrowToken(app, repo, { administration: 'read' });
						const settings = await github.forkPullRequests(token, repo.name);
						this.store.cache(forkKey(repo.id), JSON.stringify(settings), Date.now() + forkSettingsLifetime);
					} catch (error) {
						if (!(error instanceof github.GitHubError)) throw error;
						this.store.cache(forkKey(repo.id), this.store.lastCached(forkKey(repo.id)) ?? 'null', Date.now() + forkSettingsRetry);
					}
				}),
			);
		});
		return repos.map((repo) => this.lastForkPullRequests(repo.id));
	}

	/** The settings as last read, without asking GitHub. */
	lastForkPullRequests(repoId: number): ForkPullRequests | null {
		return JSON.parse(this.store.lastCached(forkKey(repoId)) ?? 'null') as ForkPullRequests | null;
	}

	/**
	 * Refuses a runner for an organization scale set while any job the agent
	 * acquired for it is of a repository the Worker does not serve, as the
	 * listing narrowed by `REPOS` says, at most a minute old. A runner takes any
	 * job its scale set holds, so one such job is enough.
	 */
	async assertJobRepos(app: App, names: readonly RepoName[]): Promise<void> {
		const served = new Set((await this.repos(app)).map((repo) => repo.name.toLowerCase()));
		const unserved = names.find((name) => !served.has(name.toLowerCase()));
		if (unserved !== undefined) throw new ApiError('repo_ineligible', `${unserved} is not a repository this Worker serves, so it mints no runner for its job.`);
	}

	async servedRepo(app: App, name: RepoName): Promise<Repo> {
		const wanted = name.toLowerCase();
		const repo = (await this.repos(app)).find((candidate) => candidate.name.toLowerCase() === wanted);
		if (!repo) throw new ApiError('repo_ineligible', `${name} is not a private repository the App is installed on.`);
		return repo;
	}

	/** What a request's scale set serves: the repository it names, or with `ORG` set, the organization. */
	async servedScope(app: App, asked: { repo: RepoName } | { org: Login }): Promise<Scope> {
		const config = this.orgConfig();
		const org = config.ok ? config.org : null;
		if (org === null) {
			if ('org' in asked) throw new ApiError('repo_ineligible', `This Worker serves repositories one by one, not the organization ${asked.org}.`);
			return { kind: 'repo', repo: await this.servedRepo(app, asked.repo) };
		}
		if ('repo' in asked) {
			throw new ApiError(
				'repo_ineligible',
				`This Worker serves ${org.login} through organization scale sets, so it makes none for ${asked.repo} alone. Upgrade the agent to one that knows organization scale sets.`,
			);
		}
		if (asked.org.toLowerCase() !== org.login.toLowerCase()) throw new ApiError('repo_ineligible', `This Worker serves the organization ${org.login}, not ${asked.org}.`);
		await this.repos(app);
		const found = this.org();
		if (found === null || found.id === null || found.installationId === null || found.problem !== null) {
			throw new ApiError('group_ineligible', found?.problem ?? `The Worker has not found the organization ${org.login} yet.`);
		}
		return { kind: 'org', org: { ...found, id: found.id, installationId: found.installationId } };
	}

	private async narrowToken(app: App, repo: Repo, permissions: github.Permissions) {
		return github.installationToken(await this.jwt(app), repo.installationId, permissions, repo.id);
	}

	private async scopeToken(app: App, scope: Scope, level: 'read' | 'write') {
		if (scope.kind === 'repo') return this.narrowToken(app, scope.repo, { administration: level });
		return github.installationToken(await this.jwt(app), scope.org.installationId, { organization_self_hosted_runners: level });
	}

	private connection(app: App, scope: Scope): Promise<github.AdminConnection> {
		return this.memo(`admin:${scopeId(scope)}`, tokenLifetime, async () => github.adminConnection(await this.scopeToken(app, scope, 'write'), scopeName(scope)));
	}

	/** Runs an Actions service call with the scope's admin connection, dropping the connection if GitHub no longer accepts it. */
	async actions<T>(app: App, scope: Scope, call: (connection: github.AdminConnection) => Promise<T>): Promise<T> {
		try {
			return await call(await this.connection(app, scope));
		} catch (error) {
			if (error instanceof github.GitHubError && error.status === 401) this.store.forget(`admin:${scopeId(scope)}`);
			throw error;
		}
	}

	/**
	 * Sets the fallback variable in every served repository, or deletes it.
	 * Each repository is tried even when another fails; the answer says what
	 * went wrong, or is null when every repository now agrees.
	 */
	async writeFallback(down: boolean, since: number): Promise<string | null> {
		const app = this.store.app()?.app;
		if (!app) return 'setup has not finished';
		let repos: Repo[];
		try {
			repos = await this.repos(app);
		} catch (error) {
			if (error instanceof github.GitHubError) return error.message;
			throw error;
		}
		const problems = new Set<string>();
		for (const repo of repos) {
			try {
				const token = await this.narrowToken(app, repo, { actions_variables: 'write' });
				if (down) await github.setVariable(token, repo.name, variable, new Date(since).toISOString());
				else await github.deleteVariable(token, repo.name, variable);
			} catch (error) {
				if (!(error instanceof github.GitHubError)) throw error;
				// GitHub refuses a token for a permission the installation has not granted.
				const unpermitted = error.status === 422 && error.call.endsWith('/access_tokens');
				problems.add(unpermitted ? 'the App lacks the Variables permission (read and write), or an installation has not approved it' : `${repo.name}: ${error.message}`);
			}
		}
		return problems.size === 0 ? null : [...problems].join('; ');
	}

	/**
	 * Finds or creates the class's scale set. An organization's goes in the
	 * runner group the listing checked, by the id GitHub's REST API gave it: the
	 * Actions service must find the same id under the group's name, or a group
	 * renamed since could take the scale set. A recorded scale set made in
	 * another group is deleted first.
	 */
	ensureScaleSet(app: App, scope: Scope, className: ClassName): Promise<ScaleSetId> {
		const key = scopeId(scope);
		const checked = scope.kind === 'org' ? scope.org.groupId : null;
		const known = this.store.scaleSet(key, className);
		if (known !== undefined && this.store.scaleSetGroup(key, known) === checked) return Promise.resolve(known);
		return this.once(`scaleset:${key}:${className}`, async () => {
			const id = await this.actions(app, scope, async (connection) => {
				if (known !== undefined) await github.deleteScaleSet(connection, known);
				let group = github.defaultRunnerGroup;
				if (scope.kind === 'org') {
					const found = await github.actionsRunnerGroupId(connection, scope.org.group);
					if (found === undefined) throw new ApiError('group_ineligible', `${scope.org.login} has no runner group named “${scope.org.group}”.`);
					if (found !== checked) {
						throw new ApiError(
							'group_ineligible',
							`GitHub's Actions service names runner group ${found} “${scope.org.group}”, but the Worker checked group ${checked}; it makes no scale set until they agree.`,
						);
					}
					group = found;
				}
				return (await github.findScaleSet(connection, className, group)) ?? (await github.createScaleSet(connection, className, group));
			});
			if (known !== undefined) this.store.forgetScaleSet(key, known);
			this.store.saveScaleSet(key, className, id, checked);
			return id;
		});
	}

	/**
	 * Reads the repository's visibility, or the organization's runner group, from
	 * GitHub, never from a cache, right before a session opens or a runner is
	 * minted. A public repository, or a group that allows public repositories,
	 * loses every scale set the Worker made for it, which disconnects its runners.
	 */
	async assertEligible(app: App, scope: Scope, scaleSetId: ScaleSetId): Promise<void> {
		if (scope.kind === 'org') return this.assertGroup(app, scope, scaleSetId);
		const { repo } = scope;
		let seen: github.Visibility;
		try {
			seen = await github.visibility(await this.narrowToken(app, repo, { metadata: 'read' }), repo.name);
		} catch (error) {
			if (!(error instanceof github.GitHubError && [401, 403, 404, 422].includes(error.status))) throw error;
			seen = 'gone';
		}
		if (seen === 'private') return;
		this.store.forget('repos');
		if (seen === 'public') await this.deleteScaleSets(app, scope);
		const why = { public: 'is public', archived: 'is archived', gone: 'no longer has the App installed' }[seen];
		throw new ApiError('repo_ineligible', `${repo.name} ${why}, so this Worker no longer serves it.`);
	}

	/**
	 * Reads the group by the id the listing found it under, then the scale
	 * set's group at GitHub. A group that is gone (GitHub moves its runners to
	 * the Default group), renamed or open to public repositories loses the
	 * organization's scale sets; a scale set in any other group than the one
	 * read, by the Worker's record or GitHub's, is deleted.
	 */
	private async assertGroup(app: App, scope: Scope & { kind: 'org' }, scaleSetId: ScaleSetId): Promise<void> {
		const { org } = scope;
		let group: RunnerGroup | undefined;
		let deleted = false;
		try {
			group = org.groupId === null ? undefined : await github.runnerGroup(await this.scopeToken(app, scope, 'read'), org.login, org.groupId);
			deleted = org.groupId !== null && group === undefined;
		} catch (error) {
			if (!(error instanceof github.GitHubError && [401, 403, 404, 422].includes(error.status))) throw error;
			group = undefined;
		}
		const renamed = group !== undefined && group.name.toLowerCase() !== org.group.toLowerCase();
		let problem: string | null;
		if (deleted) problem = `The runner group “${org.group}” of ${org.login} was deleted, which moves its runners to the Default group, so the Worker deleted its scale sets.`;
		else if (group === undefined) problem = `The runner group “${org.group}” of ${org.login} is gone, or the App can no longer read it.`;
		else if (renamed) problem = `The runner group “${org.group}” of ${org.login} is now named “${group.name}”, so the Worker deleted its scale sets.`;
		else problem = groupProblem(org.login, group);
		if (problem !== null || group === undefined) {
			this.store.forget('repos');
			if (deleted || renamed || group?.allowsPublicRepositories) await this.deleteScaleSets(app, scope);
			throw new ApiError('group_ineligible', problem ?? '');
		}
		const recorded = this.store.scaleSetGroup(scopeId(scope), scaleSetId);
		const actual = await this.actions(app, scope, (connection) => github.scaleSetGroup(connection, scaleSetId));
		if (recorded === group.id && actual === group.id) return;
		if (actual !== undefined) await this.actions(app, scope, (connection) => github.deleteScaleSet(connection, scaleSetId));
		this.store.forgetScaleSet(scopeId(scope), scaleSetId);
		throw new ApiError('not_found', `Scale set ${scaleSetId} is not in the runner group “${org.group}”, so the Worker deleted it; find or create it again.`);
	}

	private async deleteScaleSets(app: App, scope: Scope) {
		for (const id of this.store.scaleSetsOf(scopeId(scope))) {
			await this.actions(app, scope, (connection) => github.deleteScaleSet(connection, id));
			this.store.forgetScaleSet(scopeId(scope), id);
		}
	}
}

const orgKey = 'organization';
