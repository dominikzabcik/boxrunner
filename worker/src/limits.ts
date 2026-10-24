import type { Box } from './box';
import { parse, type Login } from './model';

export type LimitResult = { ok: true; index: number } | { ok: false; title: string; text: string };

/**
 * Sets or, with an empty `max`, removes the limit of a class a box reports,
 * from 0 up to the class's max there. The dashboard's form and a paired
 * device both come through here. `index` is the box's place on the dashboard.
 */
export function applyLimit(box: Box, rawBox: unknown, rawClass: unknown, max: unknown, by: Login, now: number): LimitResult {
	const name = parse.boxName(rawBox);
	const boxes = box.store.boxes();
	const index = boxes.findIndex((b) => b.status.box === name);
	const className = parse.className(rawClass);
	const c = boxes[index]?.status.classes.find((c) => c.name === className);
	if (!name || !className || !c) return { ok: false, title: 'No such class on that box', text: 'The box has not reported that class, so nothing changed.' };
	if (max === '') {
		box.store.unlimit(name, className);
	} else if (typeof max === 'string' && /^\d+$/.test(max) && Number(max) <= c.max) {
		box.store.limit(name, className, Number(max), by, now);
	} else {
		return { ok: false, title: 'Not a limit', text: `A limit is a whole number from 0 to ${c.max}, the class’s max on the box. Nothing changed.` };
	}
	return { ok: true, index };
}
