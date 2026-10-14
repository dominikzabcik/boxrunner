// wrangler.jsonc can declare only required secrets, so the optional one is declared here.
interface Env {
	/** The App's client secret, for an App set up before the Worker kept it. */
	GITHUB_CLIENT_SECRET?: string;
	/** Where the Worker posts when the box goes silent or jobs wait, and when that ends; a secret, since the address is the credential. */
	ALERT_WEBHOOK?: string;
	/** Checks GitHub's webhook deliveries, for an App whose secret setup did not keep or that was changed since. */
	WEBHOOK_SECRET?: string;
}
