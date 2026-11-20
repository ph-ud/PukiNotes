// Builds the UI into ../cmd/pukinotes/ui, which the Go binary embeds.
import { build } from 'esbuild';
import { copyFileSync, mkdirSync } from 'node:fs';
mkdirSync('../cmd/pukinotes/ui', { recursive: true });
await build({ entryPoints: ['src/main.js'], bundle: true, minify: true, format: 'iife', target: 'es2020', outfile: '../cmd/pukinotes/ui/app.js', legalComments: 'none' });
copyFileSync('src/index.html', '../cmd/pukinotes/ui/index.html');
console.log('built ../cmd/pukinotes/ui');
