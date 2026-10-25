import type { Box } from './box';
import { parse, type Login } from './model';

export type LimitResult = { ok: true; index: number } | { ok: false; title: string; text: string };

/**
 * Sets or, with an empty `max`, removes the limit of a class a box reports,
 * from 0 up to the class's max there. The dashboard's form and a paired
 * device both come through here; without a class, every class the box reports.
 * `index` is the box's place on the dashboard.
 */
export function applyLimit(box: Box, rawBox: unknown, rawClass: unknown, max: unknown, by: Login, now: number): LimitResult {
	const name = parse.boxName(rawBox);
	const boxes = box.store.boxes();
	const index = boxes.findIndex((b) => b.status.box === name);
	const reported = boxes[index]?.status.classes ?? [];
	// A form without a class is the dashboard's switch for the whole box.
	const classes = rawClass === null ? reported : reported.filter((c) => c.name === parse.className(rawClass));
	if (!name || classes.length === 0) return { ok: false, title: 'No such class on that box', text: 'The box has not reported that class, so nothing changed.' };
	const top = Math.min(...classes.map((c) => c.max));
	if (max !== '' && !(typeof max === 'string' && /^\d+$/.test(max) && Number(max) <= top)) {
		return { ok: false, title: 'Not a limit', text: `A limit is a whole number from 0 to ${top}, the class’s max on the box. Nothing changed.` };
	}
	for (const c of classes) {
		if (max === '') box.store.unlimit(name, c.name);
		else box.store.limit(name, c.name, Number(max), by, now);
	}
	return { ok: true, index };
}
