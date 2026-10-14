import { hmac, sameSecret } from './crypto';
import { parse, type Login } from './model';
import { readCookie } from './page';

const sessionCookie = '__Host-boxrunner-session';
const lifetime = 12 * 60 * 60 * 1000;

/** A signed-in account, until `expiresAt`. `csrf` goes into every form that changes something. */
export type Session = { login: Login; expiresAt: number; csrf: string };

const sign = (key: string, purpose: 'session' | 'csrf', login: Login, expiresAt: number) => hmac(key, `${purpose}:${login}.${expiresAt}`);

/** The cookie value is the login, its expiry and an HMAC of both, so the Worker keeps no session table. */
export async function startSession(key: string, login: Login, now: number): Promise<string> {
	const expiresAt = now + lifetime;
	const value = `${login}.${expiresAt}.${await sign(key, 'session', login, expiresAt)}`;
	return `${sessionCookie}=${value}; Path=/; Secure; HttpOnly; SameSite=Strict; Max-Age=${lifetime / 1000}`;
}

export const endSession = `${sessionCookie}=; Path=/; Secure; HttpOnly; SameSite=Strict; Max-Age=0`;

/** The session the request's cookie carries, if it is signed with `key`, unexpired and its account may still sign in. */
export async function readSession(key: string, request: Request, now: number, allowed: (login: Login) => boolean) {
	const cookie = readCookie(request, sessionCookie) ?? '';
	const [, rawLogin, rawExpiry, mac] = /^([^.]+)\.(\d{13})\.([A-Za-z0-9_-]{43})$/.exec(cookie) ?? [];
	const login = parse.login(rawLogin);
	const expiresAt = Number(rawExpiry);
	if (!login || !mac || !(expiresAt > now && expiresAt <= now + lifetime)) return undefined;
	if (!(await sameSecret(mac, await sign(key, 'session', login, expiresAt)))) return undefined;
	if (!allowed(login)) return undefined;
	const session: Session = { login, expiresAt, csrf: await sign(key, 'csrf', login, expiresAt) };
	return session;
}
