/** Every error the agent API answers with, and its status. docs/api.md holds the same table. */
export const errorStatus = {
	bad_request: 400,
	unknown_class: 400,
	unauthorized: 401,
	not_found: 404,
	session_exists: 409,
	runner_busy: 409,
	repo_ineligible: 410,
	upgrade_required: 426,
	rate_limited: 429,
	github_error: 502,
	not_configured: 503,
} as const;

export type ErrorCode = keyof typeof errorStatus;

export class ApiError extends Error {
	constructor(
		readonly code: ErrorCode,
		message: string,
		readonly headers: Record<string, string> = {},
	) {
		super(message);
	}
}

const apiHeaders = { 'boxrunner-protocol': '1', 'cache-control': 'no-store' };

export const json = (body: unknown, status = 200) => Response.json(body, { status, headers: apiHeaders });

export const noContent = () => new Response(null, { status: 204, headers: apiHeaders });

export const errorResponse = (error: ApiError) =>
	Response.json(
		{ error: error.code, message: error.message },
		{ status: errorStatus[error.code], headers: { ...apiHeaders, ...error.headers } },
	);

export const notFound = () => errorResponse(new ApiError('not_found', 'No such endpoint.'));
