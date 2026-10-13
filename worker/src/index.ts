import { DurableObject } from 'cloudflare:workers';
import { handleApi } from './api';
import { Box } from './box';
import { notFound } from './http';
import { isSetup, match, type Route } from './routes';
import { handleSetup } from './setup';
import { Store } from './store';

/** The one Durable Object that holds the App, the box token's hash, the scale sets and the token cache. */
export class BoxObject extends DurableObject<Env> {
	private readonly box: Box;

	constructor(ctx: DurableObjectState, env: Env) {
		super(ctx, env);
		this.box = new Box(new Store(ctx.storage.sql), env);
	}

	handle(request: Request, route: Route): Promise<Response> {
		return isSetup(route) ? handleSetup(this.box, request, route) : handleApi(this.box, request, route);
	}
}

export default {
	async fetch(request, env): Promise<Response> {
		const route = match(request.method, new URL(request.url).pathname);
		if (!route) return notFound();
		return env.BOX.getByName('box').handle(request, route);
	},
} satisfies ExportedHandler<Env>;
