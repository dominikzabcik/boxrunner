import type { AppKey } from './crypto';
import { idle, type Fallback } from './fallback';
import {
	billedMinutes,
	dayOf,
	startOfDay,
	type App,
	type BoxName,
	type BoxStatus,
	type ClassName,
	type FinishedJob,
	type Heartbeat,
	type JobEvent,
	type Login,
	type Pause,
	type QueuedJob,
	type ScaleSetId,
	type SessionId,
} from './model';
import { keptJobs, keptMs } from './unserved';

/** Jobs, their daily totals and boxes that stopped reporting are kept this many days. */
export const retentionDays = 30;

const schema = `
CREATE TABLE IF NOT EXISTS app (
	singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
	id INTEGER NOT NULL,
	slug TEXT NOT NULL,
	private_key TEXT NOT NULL,
	webhook_secret TEXT,
	box_token_sha256 TEXT NOT NULL
);
DROP TABLE IF EXISTS scale_sets;
CREATE TABLE IF NOT EXISTS repo_scale_sets (
	repo_id INTEGER NOT NULL,
	class TEXT NOT NULL,
	id INTEGER NOT NULL,
	PRIMARY KEY (repo_id, class),
	UNIQUE (repo_id, id)
);
CREATE TABLE IF NOT EXISTS session_holders (
	repo_id INTEGER NOT NULL,
	scale_set_id INTEGER NOT NULL,
	session_id TEXT NOT NULL,
	box TEXT NOT NULL,
	PRIMARY KEY (repo_id, scale_set_id)
);
CREATE TABLE IF NOT EXISTS cache (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS keys (
	name TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS boxes (
	name TEXT PRIMARY KEY,
	seen_at INTEGER NOT NULL,
	status TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
	runner TEXT PRIMARY KEY,
	job TEXT NOT NULL,
	finished_at INTEGER NOT NULL,
	minutes INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS fallback (
	singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
	state TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_by_finish ON jobs (finished_at);
CREATE TABLE IF NOT EXISTS job_days (
	day INTEGER PRIMARY KEY,
	jobs INTEGER NOT NULL,
	minutes INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS queued_jobs (
	id INTEGER PRIMARY KEY,
	state TEXT NOT NULL,
	queued_at INTEGER NOT NULL,
	job TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS paused_classes (
	class TEXT PRIMARY KEY,
	since INTEGER NOT NULL,
	by_login TEXT NOT NULL
);
`;

export type StoredBox = { status: BoxStatus; seenAt: number };

/** A status stored from an agent or Worker before draining, held classes and scale sets were reported lacks them. */
function statusFrom(json: string): BoxStatus {
	const status = JSON.parse(json) as BoxStatus;
	return { ...status, draining: status.draining ?? false, scaleSets: status.scaleSets ?? [], classes: status.classes.map((c) => ({ ...c, heldUntil: c.heldUntil ?? null })) };
}
export type StoredJob = FinishedJob & { minutes: number };
export type DayTotal = { day: number; jobs: number; minutes: number };

/** The Durable Object's SQLite tables. Every method is synchronous, so a caller's check and write cannot interleave with another request. */
export class Store {
	constructor(private readonly sql: SqlStorage) {
		sql.exec(schema);
		// Columns added after the first release; CREATE TABLE IF NOT EXISTS leaves an older table as it was.
		const columns = new Set(sql.exec<{ name: string }>("SELECT name FROM pragma_table_info('app')").toArray().map((c) => c.name));
		for (const column of ['client_id', 'client_secret', 'owner_login']) {
			if (!columns.has(column)) sql.exec(`ALTER TABLE app ADD COLUMN ${column} TEXT`);
		}
	}

	app(): { app: App; boxTokenSha256: string } | undefined {
		const row = this.sql
			.exec<{
				id: number;
				slug: string;
				private_key: string;
				webhook_secret: string | null;
				client_id: string | null;
				client_secret: string | null;
				owner_login: string | null;
				box_token_sha256: string;
			}>('SELECT id, slug, private_key, webhook_secret, client_id, client_secret, owner_login, box_token_sha256 FROM app')
			.toArray()[0];
		if (!row) return undefined;
		return {
			app: {
				id: row.id,
				slug: row.slug,
				key: row.private_key as AppKey,
				webhookSecret: row.webhook_secret,
				clientId: row.client_id,
				clientSecret: row.client_secret,
				owner: row.owner_login,
			},
			boxTokenSha256: row.box_token_sha256,
		};
	}

	saveApp(app: App, boxTokenSha256: string) {
		this.sql.exec(
			'INSERT INTO app (singleton, id, slug, private_key, webhook_secret, client_id, client_secret, owner_login, box_token_sha256) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)',
			app.id,
			app.slug,
			app.key,
			app.webhookSecret,
			app.clientId,
			app.clientSecret,
			app.owner,
			boxTokenSha256,
		);
	}

	/** Forgets the App, the box token, the scale sets and who holds their sessions, every cached token, the session key, the pauses and the queued jobs from the webhook, so setup can run again. */
	reset() {
		this.sql.exec(
			'DELETE FROM app; DELETE FROM repo_scale_sets; DELETE FROM session_holders; DELETE FROM cache; DELETE FROM keys; DELETE FROM paused_classes; DELETE FROM queued_jobs;',
		);
	}

	/** Fills in the OAuth client id and owner of an App set up before the Worker kept them. */
	saveAppProfile(clientId: string, owner: string) {
		this.sql.exec('UPDATE app SET client_id = ?, owner_login = ?', clientId, owner);
	}

	/** A random key made once and kept, such as the one that signs dashboard sessions. */
	key(name: string, make: () => string): string {
		const stored = this.sql.exec<{ value: string }>('SELECT value FROM keys WHERE name = ?', name).toArray()[0]?.value;
		if (stored !== undefined) return stored;
		const value = make();
		this.sql.exec('INSERT INTO keys (name, value) VALUES (?, ?)', name, value);
		return value;
	}

	replaceBoxToken(boxTokenSha256: string) {
		this.sql.exec('UPDATE app SET box_token_sha256 = ?', boxTokenSha256);
	}

	pauses(): Pause[] {
		return this.sql
			.exec<{ class: string; since: number; by_login: string }>('SELECT class, since, by_login FROM paused_classes ORDER BY class')
			.toArray()
			.map((row) => ({ className: row.class as ClassName, since: row.since, by: row.by_login as Login }));
	}

	/** Pausing a paused class keeps who paused it first, and when. */
	pause(className: ClassName, by: Login, at: number) {
		this.sql.exec('INSERT OR IGNORE INTO paused_classes (class, since, by_login) VALUES (?, ?, ?)', className, at, by);
	}

	resume(className: ClassName) {
		this.sql.exec('DELETE FROM paused_classes WHERE class = ?', className);
	}

	scaleSet(repoId: number, className: ClassName): ScaleSetId | undefined {
		return this.sql
			.exec<{ id: number }>('SELECT id FROM repo_scale_sets WHERE repo_id = ? AND class = ?', repoId, className)
			.toArray()[0]?.id as ScaleSetId | undefined;
	}

	scaleSetClass(repoId: number, id: ScaleSetId): ClassName | undefined {
		return this.sql
			.exec<{ class: string }>('SELECT class FROM repo_scale_sets WHERE repo_id = ? AND id = ?', repoId, id)
			.toArray()[0]?.class as ClassName | undefined;
	}

	/** Repositories the Worker holds scale sets for. */
	scaleSetRepos(): number[] {
		return this.sql
			.exec<{ repo_id: number }>('SELECT DISTINCT repo_id FROM repo_scale_sets')
			.toArray()
			.map((row) => row.repo_id);
	}

	scaleSetsOf(repoId: number): ScaleSetId[] {
		return this.sql
			.exec<{ id: number }>('SELECT id FROM repo_scale_sets WHERE repo_id = ?', repoId)
			.toArray()
			.map((row) => row.id as ScaleSetId);
	}

	saveScaleSet(repoId: number, className: ClassName, id: ScaleSetId) {
		this.sql.exec('INSERT OR REPLACE INTO repo_scale_sets (repo_id, class, id) VALUES (?, ?, ?)', repoId, className, id);
	}

	forgetScaleSet(repoId: number, id: ScaleSetId) {
		this.sql.exec('DELETE FROM repo_scale_sets WHERE repo_id = ? AND id = ?', repoId, id);
		this.sql.exec('DELETE FROM session_holders WHERE repo_id = ? AND scale_set_id = ?', repoId, id);
	}

	holdSession(repoId: number, id: ScaleSetId, sessionId: SessionId, box: BoxName) {
		this.sql.exec(
			'INSERT OR REPLACE INTO session_holders (repo_id, scale_set_id, session_id, box) VALUES (?, ?, ?, ?)',
			repoId,
			id,
			sessionId.toLowerCase(),
			box,
		);
	}

	releaseSession(repoId: number, id: ScaleSetId, sessionId: SessionId) {
		this.sql.exec('DELETE FROM session_holders WHERE repo_id = ? AND scale_set_id = ? AND session_id = ?', repoId, id, sessionId);
	}

	/**
	 * The box other than `asker` that opened the scale set's session, if it
	 * reported since `seenSince`. A box that stopped reporting may have died
	 * with its session open, which GitHub releases on its own.
	 */
	sessionHolder(repoId: number, id: ScaleSetId, asker: BoxName, seenSince: number): BoxName | undefined {
		return this.sql
			.exec<{ box: string }>(
				'SELECT h.box FROM session_holders h JOIN boxes b ON b.name = h.box WHERE h.repo_id = ? AND h.scale_set_id = ? AND h.box != ? AND b.seen_at >= ?',
				repoId,
				id,
				asker,
				seenSince,
			)
			.toArray()[0]?.box as BoxName | undefined;
	}

	cached(key: string, now: number): string | undefined {
		return this.sql
			.exec<{ value: string }>('SELECT value FROM cache WHERE key = ? AND expires_at > ?', key, now)
			.toArray()[0]?.value;
	}

	/** The last value cached under `key`, expired or not, for pages that must not ask GitHub. */
	lastCached(key: string): string | undefined {
		return this.sql.exec<{ value: string }>('SELECT value FROM cache WHERE key = ?', key).toArray()[0]?.value;
	}

	cache(key: string, value: string, expiresAt: number) {
		this.sql.exec('INSERT OR REPLACE INTO cache (key, value, expires_at) VALUES (?, ?, ?)', key, value, expiresAt);
	}

	forget(key: string) {
		this.sql.exec('DELETE FROM cache WHERE key = ?', key);
	}

	/**
	 * Keeps the box's latest status and upserts its finished jobs. The agent
	 * sends the same jobs every 30 seconds, so a job is written only when it
	 * is new or changed: the free plan allows 100,000 rows written a day.
	 */
	saveHeartbeat({ jobs, ...status }: Heartbeat, receivedAt: number) {
		this.sql.exec('INSERT OR REPLACE INTO boxes (name, seen_at, status) VALUES (?, ?, ?)', status.box, receivedAt, JSON.stringify(status));
		const changedDays = new Set<number>();
		for (const job of jobs) {
			const json = JSON.stringify(job);
			const old = this.sql.exec<{ job: string; finished_at: number }>('SELECT job, finished_at FROM jobs WHERE runner = ?', job.runner).toArray()[0];
			if (old?.job === json) continue;
			if (old) changedDays.add(dayOf(old.finished_at));
			changedDays.add(dayOf(job.finishedAt));
			this.sql.exec(
				'INSERT OR REPLACE INTO jobs (runner, job, finished_at, minutes) VALUES (?, ?, ?, ?)',
				job.runner,
				json,
				job.finishedAt,
				billedMinutes(job),
			);
		}
		for (const day of changedDays) {
			this.sql.exec(
				'INSERT OR REPLACE INTO job_days (day, jobs, minutes) SELECT ?, COUNT(*), COALESCE(SUM(minutes), 0) FROM jobs WHERE finished_at >= ? AND finished_at < ?',
				day,
				startOfDay(day),
				startOfDay(day + 1),
			);
		}
		const firstKeptDay = dayOf(receivedAt) - retentionDays + 1;
		this.sql.exec('DELETE FROM jobs WHERE finished_at < ?', startOfDay(firstKeptDay));
		this.sql.exec('DELETE FROM job_days WHERE day < ?', firstKeptDay);
		this.sql.exec('DELETE FROM boxes WHERE seen_at < ?', startOfDay(firstKeptDay));
	}

	/** When a box last reported, or undefined before any did. */
	lastSeen(): number | undefined {
		return this.sql.exec<{ seen: number | null }>('SELECT MAX(seen_at) AS seen FROM boxes').toArray()[0]?.seen ?? undefined;
	}

	fallback(): Fallback {
		const state = this.sql.exec<{ state: string }>('SELECT state FROM fallback').toArray()[0]?.state;
		// A row from before alerts has no `alert`, one from before waits no `waits` or `alert.waiting`, and `idle` fills them in.
		if (state === undefined) return idle;
		const stored = JSON.parse(state) as Partial<Fallback>;
		return { ...idle, ...stored, alert: { ...idle.alert, ...stored.alert } };
	}

	saveFallback(state: Fallback) {
		this.sql.exec('INSERT OR REPLACE INTO fallback (singleton, state) VALUES (1, ?)', JSON.stringify(state));
	}

	boxes(): StoredBox[] {
		return this.sql
			.exec<{ seen_at: number; status: string }>('SELECT seen_at, status FROM boxes ORDER BY name')
			.toArray()
			.map((row) => ({ status: statusFrom(row.status), seenAt: row.seen_at }));
	}

	recentJobs(limit: number): StoredJob[] {
		return this.sql
			.exec<{ job: string; minutes: number }>('SELECT job, minutes FROM jobs ORDER BY finished_at DESC, runner LIMIT ?', limit)
			.toArray()
			.map((row) => ({ ...(JSON.parse(row.job) as FinishedJob), minutes: row.minutes }));
	}

	/** When GitHub's last signed webhook delivery arrived, kept beside the cache so a reset forgets it. */
	noteDelivery(at: number) {
		this.cache('webhook:delivered', String(at), at);
	}

	lastDelivery(): number | undefined {
		const at = this.lastCached('webhook:delivered');
		return at === undefined ? undefined : Number(at);
	}

	/**
	 * Keeps a job from `queued` until a runner takes it, then until it is gone,
	 * so a `queued` delivery that arrives after `in_progress` does not list it
	 * again. All but the newest `keptJobs` go.
	 */
	recordJob({ change, job }: JobEvent) {
		if (change === 'gone') this.sql.exec('DELETE FROM queued_jobs WHERE id = ?', job.id);
		else {
			this.sql.exec(
				`INSERT INTO queued_jobs (id, state, queued_at, job) VALUES (?, ?, ?, ?) ON CONFLICT (id) DO ${change === 'started' ? "UPDATE SET state = 'started'" : 'NOTHING'}`,
				job.id,
				change,
				job.queuedAt,
				JSON.stringify(job),
			);
		}
		this.sql.exec('DELETE FROM queued_jobs WHERE id NOT IN (SELECT id FROM queued_jobs ORDER BY queued_at DESC, id DESC LIMIT ?)', keptJobs);
	}

	/** Jobs no runner has taken, queued within the last day, oldest first. */
	queuedJobs(now: number): QueuedJob[] {
		return this.sql
			.exec<{ job: string }>("SELECT job FROM queued_jobs WHERE state = 'queued' AND queued_at >= ? ORDER BY queued_at, id", now - keptMs)
			.toArray()
			.map((row) => JSON.parse(row.job) as QueuedJob);
	}

	daysFrom(firstDay: number): DayTotal[] {
		return this.sql.exec<DayTotal>('SELECT day, jobs, minutes FROM job_days WHERE day >= ?', firstDay).toArray();
	}
}
