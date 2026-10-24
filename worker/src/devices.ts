import type { Box } from './box';
import { boxTokenHash, randomBytes, randomToken } from './crypto';
import { dashboardData } from './dashboard';
import { ApiError, errorResponse, json } from './http';
import { applyLimit } from './limits';
import type { Device } from './model';
import { escape, messagePage, page, seeOther } from './page';
import type { DeviceRoute } from './routes';
import type { Session } from './session';

/** How long a pairing code works, and how many may wait for approval at once. */
export const pairingMs = 10 * 60 * 1000;
const waitingAtMost = 5;
const codeAlphabet = 'ABCDEFGHJKMNPQRSTUVWXYZ23456789';
/** A device token starts with this, so nobody mistakes it for a box token. */
const tokenPrefix = 'brd_';
const maxBodyBytes = 4 * 1024;

const openDashboard = { href: '/', text: 'Open the dashboard' };

/** XXXX-XXXX from letters and digits nobody misreads. */
function pairingCode() {
	const chars = Array.from(randomBytes(8), (byte) => codeAlphabet[byte % codeAlphabet.length]!);
	return `${chars.slice(0, 4).join('')}-${chars.slice(4).join('')}`;
}

async function body(request: Request): Promise<Record<string, unknown>> {
	const bytes = await request.arrayBuffer();
	if (bytes.byteLength > maxBodyBytes) throw new ApiError('bad_request', 'The body is larger than 4 KB.');
	try {
		const value: unknown = JSON.parse(new TextDecoder().decode(bytes));
		if (typeof value === 'object' && value !== null && !Array.isArray(value)) return value as Record<string, unknown>;
	} catch {
		// Answered below.
	}
	throw new ApiError('bad_request', 'The body is not a JSON object.');
}

/** A device's name as people read it on the dashboard: 1 to 64 printable characters. */
function deviceName(value: unknown) {
	if (typeof value !== 'string') return undefined;
	const name = value.trim();
	return name.length >= 1 && name.length <= 64 && !/[\u0000-\u001f\u007f]/.test(name) ? name : undefined;
}

/**
 * The paired devices' API. A device starts a pairing, the person approves it
 * on the dashboard, and the device collects a token of its own once. With
 * it, the device reads what the dashboard shows and sets limits, as the
 * person who approved it, while that person may still open the dashboard.
 */
export async function handleDevice(box: Box, request: Request, route: DeviceRoute): Promise<Response> {
	try {
		if (!box.store.app()) throw new ApiError('not_configured', `Setup has not finished. Open ${new URL(request.url).origin}/setup.`);
		const now = Date.now();
		switch (route.name) {
			case 'device.pair':
				return await startPairing(box, request, now);
			case 'device.token':
				return await collectToken(box, request, now);
			case 'device.fleet': {
				const device = await authenticate(box, request, now);
				return json({ ...dashboardData(box, now), device: { id: device.id, name: device.name, by: device.by } });
			}
			case 'device.limit': {
				const device = await authenticate(box, request, now);
				const fields = await body(request);
				const max = fields.max === null ? '' : typeof fields.max === 'number' ? String(fields.max) : undefined;
				const result = applyLimit(box, fields.box, fields.class, max, device.by, now);
				if (!result.ok) throw new ApiError('bad_request', `${result.title}. ${result.text}`);
				return json({ limits: box.store.limits().filter((l) => l.box === fields.box).map((l) => ({ class: l.className, max: l.max })) });
			}
		}
	} catch (error) {
		if (error instanceof ApiError) return errorResponse(error);
		throw error;
	}
}

async function startPairing(box: Box, request: Request, now: number) {
	const name = deviceName((await body(request)).name);
	if (!name) throw new ApiError('bad_request', '"name" is missing or malformed: 1 to 64 printable characters.');
	const code = pairingCode();
	const secret = randomToken();
	if (!box.store.startPairing(code, await boxTokenHash(secret), name, now, now - pairingMs, waitingAtMost)) {
		throw new ApiError('rate_limited', `${waitingAtMost} pairings already wait for approval. Approve or let them expire first.`, { 'retry-after': '60' });
	}
	const url = `${new URL(request.url).origin}/pair?code=${code}`;
	return json({ code, secret, url, expiresIn: pairingMs / 1000 }, 201);
}

async function collectToken(box: Box, request: Request, now: number) {
	const secret = (await body(request)).secret;
	const pairing = typeof secret === 'string' ? box.store.pairingBySecret(await boxTokenHash(secret), now - pairingMs) : undefined;
	if (!pairing) throw new ApiError('not_found', 'No such pairing, or it expired. Pair again.');
	if (!pairing.approvedBy) return json({ status: 'pending' }, 202);
	const token = `${tokenPrefix}${randomToken()}`;
	const id = randomToken().slice(0, 12);
	box.store.finishPairing(pairing.code, id, await boxTokenHash(token), now);
	return json({ token, device: { id, name: pairing.name, by: pairing.approvedBy } });
}

async function authenticate(box: Box, request: Request, now: number): Promise<Device> {
	const presented = /^Bearer (brd_\S+)$/.exec(request.headers.get('authorization') ?? '')?.[1];
	const device = presented === undefined ? undefined : box.store.deviceByToken(await boxTokenHash(presented), now);
	if (!device || !box.mayOpenDashboard(device.by)) throw new ApiError('unauthorized', 'The device token is missing or wrong, or whoever paired it may no longer open the dashboard. Pair again.');
	return device;
}

/** The page a pairing's link opens: what asks, and the button that approves it. */
export function pairingPage(box: Box, code: string | null, session: Session | undefined, now: number) {
	const back = { href: `/pair?code=${encodeURIComponent(code ?? '')}`, text: 'Open the pairing again' };
	if (!session) return messagePage(401, 'Sign in first', 'Sign in to the dashboard, then open the link from the app again.', { href: '/login', text: 'Sign in' });
	const pairing = code === null ? undefined : box.store.pairing(code, now - pairingMs);
	if (!pairing) return messagePage(404, 'No such pairing', 'The code is wrong or expired after 10 minutes. Pair again from the app.', openDashboard);
	if (pairing.approvedBy) return messagePage(200, 'Paired', `${pairing.name} is paired. Go back to it.`, openDashboard);
	return page({
		status: 200,
		title: 'Pair a device',
		body: `<h1>Pair ${escape(pairing.name)}?</h1>
<p>A device asks to see this Worker’s dashboard and to set limits on its boxes, as you, <strong>${escape(session.login)}</strong>.</p>
<p>Check that the app shows the same code:</p>
<p class="code"><code>${escape(pairing.code)}</code></p>
<form method="post" action="/pair">
<input type="hidden" name="csrf" value="${escape(session.csrf)}">
<input type="hidden" name="code" value="${escape(pairing.code)}">
<button type="submit">Pair ${escape(pairing.name)}</button>
</form>
<p class="hint">Unpair it on the dashboard at any time. It stops working too once you can no longer open the dashboard.</p>
<p><a href="${back.href}">Reload</a></p>`,
	});
}

export function approvePairing(box: Box, form: FormData, session: Session, now: number) {
	const code = form.get('code');
	const pairing = typeof code === 'string' ? box.store.pairing(code, now - pairingMs) : undefined;
	if (!pairing) return messagePage(404, 'No such pairing', 'The code is wrong or expired after 10 minutes. Pair again from the app.', openDashboard);
	box.store.approvePairing(pairing.code, session.login);
	return messagePage(200, 'Paired', `${pairing.name} is paired. Go back to it; it shows the fleet within a few seconds.`, openDashboard);
}

export function unpair(box: Box, form: FormData) {
	const id = form.get('id');
	if (typeof id === 'string') box.store.unpair(id);
	return seeOther('/#devices');
}
