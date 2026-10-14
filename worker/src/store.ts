import type { AppKey } from './crypto';
import {
	billedMinutes,
	dayOf,
	startOfDay,
	type App,
	type BoxStatus,
	type ClassName,
	type FinishedJob,
	type Heartbeat,
	type Login,
	type Pause,
	type ScaleSetId,
} from './model';

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
CREATE INDEX IF NOT EXISTS jobs_by_finish ON jobs (finished_at);
CREATE TABLE IF NOT EXISTS job_days (
	day INTEGER PRIMARY KEY,
	jobs INTEGER NOT NULL,
	minutes INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS paused_classes (
	class TEXT PRIMARY KEY,
	since INTEGER NOT NULL,
	by_login TEXT NOT NULL
);
`;

export type StoredBox = { status: BoxStatus; seenAt: number };
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

	/** Forgets the App, the box token, the scale sets, every cached token, the session key and the pauses, so setup can run again. */
	reset() {
		this.sql.exec('DELETE FROM app; DELETE FROM repo_scale_sets; DELETE FROM cache; DELETE FROM keys; DELETE FROM paused_classes;');
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

	boxes(): StoredBox[] {
		return this.sql
			.exec<{ seen_at: number; status: string }>('SELECT seen_at, status FROM boxes ORDER BY name')
			.toArray()
			.map((row) => ({ status: JSON.parse(row.status) as BoxStatus, seenAt: row.seen_at }));
	}

	recentJobs(limit: number): StoredJob[] {
		return this.sql
			.exec<{ job: string; minutes: number }>('SELECT job, minutes FROM jobs ORDER BY finished_at DESC, runner LIMIT ?', limit)
			.toArray()
			.map((row) => ({ ...(JSON.parse(row.job) as FinishedJob), minutes: row.minutes }));
	}

	daysFrom(firstDay: number): DayTotal[] {
		return this.sql.exec<DayTotal>('SELECT day, jobs, minutes FROM job_days WHERE day >= ?', firstDay).toArray();
	}
}
