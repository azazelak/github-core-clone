// Launcher: the trial runs Node 22 main.ts; this spawns the prebuilt
// static Go binary ./forge. The entry files may be stored read-only and
// without the exec bit (browser upload sets 644), so the binary is copied
// to a tmp file, chmod 755, and spawned from there. main.ts binds no port;
// the Go binary owns the lifecycle (bind port 0, register, poll /done).
import { spawn } from 'node:child_process';
import { chmodSync, copyFileSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
let bin: string;
try {
  const dir = mkdtempSync(join(tmpdir(), 'forge-'));
  bin = join(dir, 'forge');
  copyFileSync(join(here, 'forge'), bin);
  chmodSync(bin, 0o755);
} catch (err) {
  process.stderr.write(`[launcher] cannot stage forge binary: ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}

const child = spawn(bin, [], { stdio: 'inherit', env: process.env });
child.on('error', (err) => {
  process.stderr.write(`[launcher] spawn failed: ${err.message}\n`);
  process.exit(1);
});
for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP'] as const) {
  process.on(sig, () => {
    child.kill(sig);
  });
}
child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
