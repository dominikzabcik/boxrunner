// Calls GitHub's cache and artifact services as a JavaScript action does,
// for scripts/e2e/run.sh, which runs it in a job container:
//
//   node cache-client.mjs save|restore|artifact <key>
//
// It prints the ACTIONS_RESULTS_URL the action sees and what each call did.
import * as cache from '@actions/cache';
import { DefaultArtifactClient } from '@actions/artifact';
import { readFileSync, writeFileSync } from 'node:fs';

const [what, key] = process.argv.slice(2);
console.log(`results-url ${process.env.ACTIONS_RESULTS_URL}`);
switch (what) {
  case 'save':
    writeFileSync('payload.txt', `saved by ${key}\n`);
    console.log(`saved ${await cache.saveCache(['payload.txt'], key)}`);
    break;
  case 'restore':
    console.log(`restored ${await cache.restoreCache(['payload.txt'], key)}`);
    console.log(`payload ${readFileSync('payload.txt', 'utf8').trim()}`);
    break;
  case 'miss':
    console.log(`restored ${await cache.restoreCache(['payload.txt'], key)}`);
    break;
  case 'artifact': {
    const { id } = await new DefaultArtifactClient().uploadArtifact(key, ['payload.txt'], '.', { retentionDays: 1 });
    console.log(`artifact ${id}`);
    break;
  }
  default:
    throw new Error(`unknown call ${what}`);
}
