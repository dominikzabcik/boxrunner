import { DurableObject } from 'cloudflare:workers';
import { handleApi } from './api';
import { Box } from './box';
import { check } from './fallback';
import { notFound } from './http';
import { isSetup, isWeb, isWebhook, match, type Route } from './routes';
import { handleSetup } from './setup';
import { Store } from './store';
import { handleWebhook } from './unserved';
import { handleWeb } from './web';

/** The one Durable Object that holds the App, the box token's hash, the scale sets, the token cache, what the box reports, the jobs GitHub says wait and whether its jobs fall back to GitHub's runners. */
export class BoxObject extends DurableObject<Env> {
	private readonly box: Box;

	constructor(ctx: DurableObjectState, env: Env) {
		super(ctx, env);
		this.box = new Box(new Store(ctx.storage.sql), env, ctx.storage);
	}

	handle(request: Request, route: Route): Promise<Response> {
		if (isSetup(route)) return handleSetup(this.box, request, route);
		if (isWeb(route)) return handleWeb(this.box, request, route);
		if (isWebhook(route)) return handleWebhook(this.box, request);
		return handleApi(this.box, request, route);
	}

	/** Set only while fallback to GitHub's runners is on or still has a variable to delete; see docs/fallback.md. */
	override alarm(): Promise<void> {
		return check(this.box, Date.now());
	}
}

export default {
	async fetch(request, env): Promise<Response> {
		const route = match(request.method, new URL(request.url).pathname);
		if (!route) return notFound();
		return env.BOX.getByName('box').handle(request, route);
	},
} satisfies ExportedHandler<Env>;
