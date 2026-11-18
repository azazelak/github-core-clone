// Entry lifecycle: bind an ephemeral port, announce it to the env, wait for
// the env to finish, then exit. The server itself lives in app.ts.
import { createApp } from './app.ts';

const envUrl = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (envUrl === '') {
  process.stderr.write('[forge] MAKEFUN_ENV_URL is not set\n');
  process.exit(1);
}

const wait = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

async function announce(port: number): Promise<void> {
  let attempt = 0;
  for (;;) {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(30_000),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register refused with HTTP ${res.status}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register refused')) throw err;
    }
    await wait(Math.min(2000, 250 * 2 ** Math.min(attempt++, 8)));
  }
}

async function waitDone(): Promise<void> {
  let attempt = 0;
  for (;;) {
    try {
      const res = await fetch(`${envUrl}/done`, { signal: AbortSignal.timeout(30_000) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // env not answering yet: keep polling.
    }
    await wait(Math.min(2000, 250 * 2 ** Math.min(attempt++, 8)));
  }
}

async function run(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const addr = server.address();
  if (addr === null || typeof addr === 'string') throw new Error('no TCP port bound');
  process.stdout.write(`[forge] listening on ${addr.port}\n`);
  await announce(addr.port);
  process.stdout.write('[forge] registered\n');
  await waitDone();
  server.close();
}

try {
  await run();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
