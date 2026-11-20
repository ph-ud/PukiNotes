// Builds the UI into ../cmd/casa/ui, which the Go binary embeds.
import { build } from 'esbuild';
import { copyFileSync, mkdirSync } from 'node:fs';
mkdirSync('../cmd/casa/ui', { recursive: true });
await build({ entryPoints: ['src/main.js'], bundle: true, minify: true, format: 'iife', target: 'es2020', outfile: '../cmd/casa/ui/app.js', legalComments: 'none' });
copyFileSync('src/index.html', '../cmd/casa/ui/index.html');
console.log('built ../cmd/casa/ui');
