export type SetupRoute =
	| { name: 'setup.form' }
	| { name: 'setup.start' }
	| { name: 'setup.callback' }
	| { name: 'setup.rotate' }
	| { name: 'setup.showToken'; id: string }
	| { name: 'setup.reset' };

export type WebRoute =
	| { name: 'web.dashboard' }
	| { name: 'web.data' }
	| { name: 'web.login' }
	| { name: 'web.callback' }
	| { name: 'web.logout' }
	| { name: 'web.token' }
	| { name: 'web.showToken'; id: string }
	| { name: 'web.pause' }
	| { name: 'web.resume' }
	| { name: 'web.limit' };

export type ApiRoute =
	| { name: 'heartbeat' }
	| { name: 'repos' }
	| { name: 'scalesets.ensure' }
	| { name: 'sessions.open' }
	| { name: 'sessions.refresh'; id: string }
	| { name: 'sessions.close'; id: string }
	| { name: 'runners.mint' }
	| { name: 'runners.remove'; id: string }
	| { name: 'jobs' };

/** GitHub's webhook deliveries for the App. */
export type WebhookRoute = { name: 'webhook' };

export type Route = SetupRoute | WebRoute | ApiRoute | WebhookRoute;

const table: ReadonlyArray<[method: string, path: RegExp, route: (id: string) => Route]> = [
	['GET', /^\/setup$/, () => ({ name: 'setup.form' })],
	['POST', /^\/setup$/, () => ({ name: 'setup.start' })],
	['GET', /^\/setup\/callback$/, () => ({ name: 'setup.callback' })],
	['POST', /^\/setup\/token$/, () => ({ name: 'setup.rotate' })],
	['GET', /^\/setup\/token\/([^/]+)$/, (id) => ({ name: 'setup.showToken', id })],
	['POST', /^\/setup\/reset$/, () => ({ name: 'setup.reset' })],
	['GET', /^\/$/, () => ({ name: 'web.dashboard' })],
	['GET', /^\/dashboard\.json$/, () => ({ name: 'web.data' })],
	['GET', /^\/login$/, () => ({ name: 'web.login' })],
	['GET', /^\/login\/callback$/, () => ({ name: 'web.callback' })],
	['POST', /^\/logout$/, () => ({ name: 'web.logout' })],
	['POST', /^\/box-token$/, () => ({ name: 'web.token' })],
	['GET', /^\/box-token\/([^/]+)$/, (id) => ({ name: 'web.showToken', id })],
	['POST', /^\/classes\/pause$/, () => ({ name: 'web.pause' })],
	['POST', /^\/classes\/resume$/, () => ({ name: 'web.resume' })],
	['POST', /^\/boxes\/limit$/, () => ({ name: 'web.limit' })],
	['POST', /^\/webhook$/, () => ({ name: 'webhook' })],
	['POST', /^\/v1\/heartbeat$/, () => ({ name: 'heartbeat' })],
	['GET', /^\/v1\/repos$/, () => ({ name: 'repos' })],
	['POST', /^\/v1\/scalesets$/, () => ({ name: 'scalesets.ensure' })],
	['POST', /^\/v1\/sessions$/, () => ({ name: 'sessions.open' })],
	['PATCH', /^\/v1\/sessions\/([^/]+)$/, (id) => ({ name: 'sessions.refresh', id })],
	['DELETE', /^\/v1\/sessions\/([^/]+)$/, (id) => ({ name: 'sessions.close', id })],
	['POST', /^\/v1\/runners$/, () => ({ name: 'runners.mint' })],
	['DELETE', /^\/v1\/runners\/([^/]+)$/, (id) => ({ name: 'runners.remove', id })],
	['GET', /^\/v1\/jobs$/, () => ({ name: 'jobs' })],
];

const decode = (segment: string) => {
	try {
		return decodeURIComponent(segment);
	} catch {
		return segment;
	}
};

export function match(method: string, pathname: string): Route | undefined {
	for (const [routeMethod, path, route] of table) {
		const found = method === routeMethod ? path.exec(pathname) : null;
		if (found) return route(decode(found[1] ?? ''));
	}
	return undefined;
}

export const isSetup = (route: Route): route is SetupRoute => route.name.startsWith('setup.');

export const isWeb = (route: Route): route is WebRoute => route.name.startsWith('web.');

export const isWebhook = (route: Route): route is WebhookRoute => route.name === 'webhook';
