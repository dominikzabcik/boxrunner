import { signAppJwt } from './crypto';
import * as github from './github';
import { ApiError } from './http';
import { parseClasses, type App, type ClassName, type Classes, type Repo, type RepoName, type ScaleSetId } from './model';
import type { Store } from './store';

const minute = 60_000;
/** The App JWT and the Actions admin token both live ten minutes; a minute of margin covers clock drift. */
const tokenLifetime = 9 * minute;
const repoListLifetime = minute;

/**
 * Each box token may spend `burst` requests at once, then one every
 * `refillMs`. That allows 1,800 an hour, under half of the free plan's
 * 100,000 requests a day, so a runaway agent loop cannot spend it.
 */
export const budget = { burst: 120, refillMs: 2_000 };

/**
 * Everything the Worker derives from its stored state: GitHub credentials,
 * the served repositories and the scale sets. One Durable Object holds the
 * only instance, so the in-flight map makes every derivation single-flight.
 */
export class Box {
	private readonly inflight = new Map<string, Promise<unknown>>();
	private tokens = budget.burst;
	private refilledAt = 0;
	setupRunning = false;

	constructor(
		readonly store: Store,
		private readonly env: Env,
	) {}

	get setupSecret(): string | undefined {
		return this.env.SETUP_SECRET && this.env.SETUP_SECRET.length >= 32 ? this.env.SETUP_SECRET : undefined;
	}

	classes(): Classes {
		return parseClasses(this.env.CLASSES);
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

	/** Private, unarchived repositories across the App's installations. */
	repos(app: App): Promise<Repo[]> {
		return this.memo('repos', repoListLifetime, async () => {
			const jwt = await this.jwt(app);
			const served: Repo[] = [];
			for (const installationId of await github.installationIds(jwt)) {
				// Listing needs a token for the whole installation, so it gets metadata and nothing else.
				const token = await github.installationToken(jwt, installationId, { metadata: 'read' });
				for (const repo of await github.installationRepos(token)) {
					if (repo.private && !repo.archived) served.push({ name: repo.name, id: repo.id, installationId });
				}
			}
			return served;
		});
	}

	async servedRepo(app: App, name: RepoName): Promise<Repo> {
		const wanted = name.toLowerCase();
		const repo = (await this.repos(app)).find((candidate) => candidate.name.toLowerCase() === wanted);
		if (!repo) throw new ApiError('repo_ineligible', `${name} is not a private repository the App is installed on.`);
		return repo;
	}

	private async narrowToken(app: App, repo: Repo, permissions: github.Permissions) {
		return github.installationToken(await this.jwt(app), repo.installationId, permissions, repo.id);
	}

	private connection(app: App, repo: Repo): Promise<github.AdminConnection> {
		return this.memo(`admin:${repo.id}`, tokenLifetime, async () =>
			github.adminConnection(await this.narrowToken(app, repo, { administration: 'write' }), repo.name),
		);
	}

	/** Runs an Actions service call with the repository's admin connection, dropping the connection if GitHub no longer accepts it. */
	async actions<T>(app: App, repo: Repo, call: (connection: github.AdminConnection) => Promise<T>): Promise<T> {
		try {
			return await call(await this.connection(app, repo));
		} catch (error) {
			if (error instanceof github.GitHubError && error.status === 401) this.store.forget(`admin:${repo.id}`);
			throw error;
		}
	}

	ensureScaleSet(app: App, repo: Repo, className: ClassName): Promise<ScaleSetId> {
		const known = this.store.scaleSet(repo.id, className);
		if (known !== undefined) return Promise.resolve(known);
		return this.once(`scaleset:${repo.id}:${className}`, async () => {
			const id = await this.actions(
				app,
				repo,
				async (connection) =>
					(await github.findScaleSet(connection, className)) ?? (await github.createScaleSet(connection, className)),
			);
			this.store.saveScaleSet(repo.id, className, id);
			return id;
		});
	}

	/**
	 * Reads the repository's visibility from GitHub, never from a cache, right
	 * before a session opens or a runner is minted. A public repository loses
	 * every scale set the Worker made for it, which disconnects its runners.
	 */
	async assertEligible(app: App, repo: Repo): Promise<void> {
		let seen: github.Visibility;
		try {
			seen = await github.visibility(await this.narrowToken(app, repo, { metadata: 'read' }), repo.name);
		} catch (error) {
			if (!(error instanceof github.GitHubError && [401, 403, 404, 422].includes(error.status))) throw error;
			seen = 'gone';
		}
		if (seen === 'private') return;
		this.store.forget('repos');
		if (seen === 'public') {
			for (const id of this.store.scaleSetsOf(repo.id)) {
				await this.actions(app, repo, (connection) => github.deleteScaleSet(connection, id));
				this.store.forgetScaleSet(id);
			}
		}
		const why = { public: 'is public', archived: 'is archived', gone: 'no longer has the App installed' }[seen];
		throw new ApiError('repo_ineligible', `${repo.name} ${why}, so this Worker no longer serves it.`);
	}
}
