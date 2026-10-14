// wrangler.jsonc can declare only required secrets, so the optional one is declared here.
interface Env {
	/** The App's client secret, for an App set up before the Worker kept it. */
	GITHUB_CLIENT_SECRET?: string;
}
