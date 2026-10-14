import { DurableObject } from 'cloudflare:workers';
import { handleApi } from './api';
import { Box } from './box';
import { notFound } from './http';
import { isSetup, isWeb, match, type Route } from './routes';
import { handleSetup } from './setup';
import { Store } from './store';
import { handleWeb } from './web';

/** The one Durable Object that holds the App, the box token's hash, the scale sets, the token cache and what the box reports. */
export class BoxObject extends DurableObject<Env> {
	private readonly box: Box;

	constructor(ctx: DurableObjectState, env: Env) {
		super(ctx, env);
		this.box = new Box(new Store(ctx.storage.sql), env);
	}

	handle(request: Request, route: Route): Promise<Response> {
		if (isSetup(route)) return handleSetup(this.box, request, route);
		if (isWeb(route)) return handleWeb(this.box, request, route);
		return handleApi(this.box, request, route);
	}
}

export default {
	async fetch(request, env): Promise<Response> {
		const route = match(request.method, new URL(request.url).pathname);
		if (!route) return notFound();
		return env.BOX.getByName('box').handle(request, route);
	},
} satisfies ExportedHandler<Env>;
