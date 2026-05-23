import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

/**
 * Regression guard for the dual ESM + CommonJS distribution (v0.10.1+).
 *
 * The adapter ships as a dual-package per the standard Node.js pattern:
 *   - dist/index.js          — ESM (`type: module` inherits from root)
 *   - dist/cjs/index.js      — CJS (overridden by `dist/cjs/package.json`)
 *   - dist/cjs/package.json  — `{ "type": "commonjs" }` to flip the
 *                              subdir's module system
 *
 * CommonJS consumers like n8n's community-node loader require this — they
 * can't `require()` a pure-ESM package. The exports field's "require"
 * key points at the CJS bundle.
 *
 * The tests don't import the built artefacts at runtime (vitest works
 * directly against src/). Instead they verify the package.json shape and
 * the build-output filesystem, which is what npm publishes.
 */

const PKG_ROOT = resolve(__dirname, '..');
const pkg = JSON.parse(readFileSync(resolve(PKG_ROOT, 'package.json'), 'utf8'));

describe('package.json exports — dual build', () => {
	it('declares "type": "module" so .js files default to ESM', () => {
		expect(pkg.type).toBe('module');
	});

	it('main points at the CJS build (for legacy require()-only consumers)', () => {
		expect(pkg.main).toBe('./dist/cjs/index.js');
	});

	it('module points at the ESM build (legacy bundler hint)', () => {
		expect(pkg.module).toBe('./dist/index.js');
	});

	it('exports[".#types"] is the .d.ts (typed-modern resolvers)', () => {
		expect(pkg.exports['.'].types).toBe('./dist/index.d.ts');
	});

	it('exports[".#import"] is the ESM entry', () => {
		expect(pkg.exports['.'].import).toBe('./dist/index.js');
	});

	it('exports[".#require"] is the CJS entry — fixes n8n community-node loader (v0.10.1+)', () => {
		expect(pkg.exports['.'].require).toBe('./dist/cjs/index.js');
	});

	it('files whitelist includes dist (carries cjs subdir along)', () => {
		expect(pkg.files).toContain('dist');
	});

	it('build script emits both ESM and CJS', () => {
		const build = pkg.scripts.build;
		expect(build).toContain('tsc'); // ESM build
		expect(build).toContain('tsconfig.cjs.json'); // CJS build
		expect(build).toContain("type:'commonjs'"); // marker file
	});
});
