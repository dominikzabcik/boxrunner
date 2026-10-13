export type SetupRoute = { name: 'setup.form' } | { name: 'setup.start' } | { name: 'setup.callback' } | { name: 'setup.rotate' } | { name: 'setup.reset' };

export type ApiRoute =
	| { name: 'repos' }
	| { name: 'scalesets.ensure' }
	| { name: 'sessions.open' }
	| { name: 'sessions.refresh'; id: string }
	| { name: 'sessions.close'; id: string }
	| { name: 'runners.mint' }
	| { name: 'runners.remove'; id: string };

export type Route = SetupRoute | ApiRoute;

const table: ReadonlyArray<[method: string, path: RegExp, route: (id: string) => Route]> = [
	['GET', /^\/setup$/, () => ({ name: 'setup.form' })],
	['POST', /^\/setup$/, () => ({ name: 'setup.start' })],
	['GET', /^\/setup\/callback$/, () => ({ name: 'setup.callback' })],
	['POST', /^\/setup\/token$/, () => ({ name: 'setup.rotate' })],
	['POST', /^\/setup\/reset$/, () => ({ name: 'setup.reset' })],
	['GET', /^\/v1\/repos$/, () => ({ name: 'repos' })],
	['POST', /^\/v1\/scalesets$/, () => ({ name: 'scalesets.ensure' })],
	['POST', /^\/v1\/sessions$/, () => ({ name: 'sessions.open' })],
	['PATCH', /^\/v1\/sessions\/([^/]+)$/, (id) => ({ name: 'sessions.refresh', id })],
	['DELETE', /^\/v1\/sessions\/([^/]+)$/, (id) => ({ name: 'sessions.close', id })],
	['POST', /^\/v1\/runners$/, () => ({ name: 'runners.mint' })],
	['DELETE', /^\/v1\/runners\/([^/]+)$/, (id) => ({ name: 'runners.remove', id })],
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
