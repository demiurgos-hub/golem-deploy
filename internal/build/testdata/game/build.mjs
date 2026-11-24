// The fixture's web build: copies the client into web/dist like a bundler.
import { cpSync, mkdirSync } from 'node:fs';

mkdirSync('web/dist', { recursive: true });
cpSync('web/index.html', 'web/dist/index.html');
console.log('web client built');
