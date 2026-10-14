// wrangler.jsonc can declare only required secrets, so the optional one is declared here.
interface Env {
	/** The App's client secret, for an App set up before the Worker kept it. */
	GITHUB_CLIENT_SECRET?: string;
	/** Where the Worker posts when the box goes silent and when it is back; a secret, since the address is the credential. */
	ALERT_WEBHOOK?: string;
}
