import type { AppKey } from './crypto';
import type { App, ClassName, ScaleSetId } from './model';

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
`;

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

	/** Forgets the App, the box token, the scale sets and every cached token, so setup can run again. */
	reset() {
		this.sql.exec('DELETE FROM app; DELETE FROM repo_scale_sets; DELETE FROM cache;');
	}

	replaceBoxToken(boxTokenSha256: string) {
		this.sql.exec('UPDATE app SET box_token_sha256 = ?', boxTokenSha256);
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

	cache(key: string, value: string, expiresAt: number) {
		this.sql.exec('INSERT OR REPLACE INTO cache (key, value, expires_at) VALUES (?, ?, ?)', key, value, expiresAt);
	}

	forget(key: string) {
		this.sql.exec('DELETE FROM cache WHERE key = ?', key);
	}
}
