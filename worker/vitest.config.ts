import { cloudflareTest } from '@cloudflare/vitest-plugin';
import { defineConfig } from 'vitest/config';
import { setupSecret } from './test/secret.ts';

export default defineConfig({
	plugins: [
		cloudflareTest({
			wrangler: { configPath: './wrangler.jsonc' },
			miniflare: { bindings: { SETUP_SECRET: setupSecret, ADMINS: ' Hubot , mona ' } },
		}),
	],
});
