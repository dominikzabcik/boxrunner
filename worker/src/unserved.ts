import type { Box } from './box';
import { sameHmacSha256 } from './crypto';
import { ApiError, errorResponse, noContent } from './http';
import { parseJobEvent, type ClassName, type QueuedJob } from './model';

/**
 * Why nobody here runs a job that waits: no class is named by its `runs-on`
 * (`label`), its repository is one the Worker does not serve, or it waits on
 * a served class longer than `slowAfterMs`, paused or not.
 */
export type UnservedReason = 'label' | 'public' | 'archived' | 'unlisted' | 'paused' | 'slow';

/** What the Worker serves, as `unservedReason` needs it. */
export type Served = { classes: readonly ClassName[]; paused: readonly ClassName[]; listed: (repo: string) => boolean };

/** A job on a served class that waits this long is reported: the box may be down, full or gone. */
export const slowAfterMs = 10 * 60_000;
/** GitHub cancels a job that waited 24 hours; the Worker no longer lists it then. */
export const keptMs = 24 * 60 * 60_000;
/** The most jobs the Worker keeps, queued or running; the oldest go first. A job whose `completed` delivery was lost stays until then. */
export const keptJobs = 200;
export const maxPayloadBytes = 256 * 1024;

// A job for one of GitHub's own images is for GitHub's runners, never this Worker's.
const githubImage = /^(ubuntu|windows|macos)-/i;

/** Why the job, after waiting `waitedMs`, is one to report, or null while it is not. */
export function unservedReason(job: QueuedJob, served: Served, waitedMs: number): UnservedReason | null {
	const className = job.labels.length === 1 ? served.classes.find((name) => name === job.labels[0]!.toLowerCase()) : undefined;
	const repoProblem = !job.private ? 'public' : job.archived ? 'archived' : !served.listed(job.repo) ? 'unlisted' : null;
	if (className === undefined) return repoProblem === null && !job.labels.some((label) => githubImage.test(label)) ? 'label' : null;
	if (repoProblem !== null) return repoProblem;
	if (waitedMs < slowAfterMs) return null;
	return served.paused.includes(className) ? 'paused' : 'slow';
}

/** Whether the job could ever be reported, so whether the Worker keeps it. */
export const reportable = (job: QueuedJob, served: Served) => unservedReason(job, served, Number.POSITIVE_INFINITY) !== null;

/**
 * `POST /webhook`: GitHub's deliveries for the App. The signature is checked
 * before anything is read from the body, and the body is never logged.
 * Events other than `workflow_job` are acknowledged and dropped, since GitHub
 * sends an App its installation events whatever it subscribes to.
 */
export async function handleWebhook(box: Box, request: Request): Promise<Response> {
	try {
		const secret = box.webhookSecret();
		if (secret === undefined) throw new ApiError('not_configured', 'This Worker has no webhook secret; see docs/unserved-jobs.md.');
		const tooLarge = new ApiError('bad_request', `The body is larger than ${maxPayloadBytes / 1024} KB.`);
		if (Number(request.headers.get('content-length') ?? 0) > maxPayloadBytes) throw tooLarge;
		const bytes = await request.arrayBuffer();
		if (bytes.byteLength > maxPayloadBytes) throw tooLarge;
		if (!(await sameHmacSha256(secret, request.headers.get('x-hub-signature-256') ?? '', bytes))) {
			throw new ApiError('unauthorized', 'The X-Hub-Signature-256 header is missing or does not match the webhook secret.');
		}
		box.store.noteDelivery(Date.now());
		if (request.headers.get('x-github-event') !== 'workflow_job') return noContent();
		let body: unknown;
		try {
			body = JSON.parse(new TextDecoder().decode(bytes));
		} catch {
			throw new ApiError('bad_request', 'The body must be JSON.');
		}
		const parsed = parseJobEvent(body);
		if (!parsed.ok) throw new ApiError('bad_request', `"${parsed.field}" is missing or malformed.`);
		if (parsed.event !== null && reportable(parsed.event.job, box.served())) box.store.recordJob(parsed.event);
		return noContent();
	} catch (error) {
		if (error instanceof ApiError) return errorResponse(error);
		throw error;
	}
}
