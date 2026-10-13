import { describe, expect, it } from 'vitest';
import { parseClasses } from '../src/model';

describe('CLASSES', () => {
	it('reads a comma-separated list, trimmed and without repeats', () => {
		expect(parseClasses(' boxrunner, boxrunner-light ,boxrunner,')).toEqual({ ok: true, classes: ['boxrunner', 'boxrunner-light'] });
	});

	it("refuses labels GitHub's own runners answer to", () => {
		for (const label of ['ubuntu-latest', 'windows-2025', 'macos-15', 'self-hosted']) {
			expect(parseClasses(`boxrunner,${label}`)).toEqual({ ok: false, problem: `CLASSES has "${label}", a label GitHub's own runners use` });
		}
	});

	it('refuses an empty list and names that are not lower-case letters, digits and dashes', () => {
		expect(parseClasses('')).toEqual({ ok: false, problem: 'CLASSES is empty' });
		expect(parseClasses('Box Runner')).toEqual({
			ok: false,
			problem: 'CLASSES has "Box Runner", which is not lower-case letters, digits and dashes',
		});
	});
});
