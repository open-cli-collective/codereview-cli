// Negative control: deliberately violate the helper process-group contract.
if (process.argv[1]?.endsWith('/runner.mjs')) {
  const childProcess = require('node:child_process');
  const originalSpawn = childProcess.spawn;
  childProcess.spawn = (executable, args, options) =>
    originalSpawn(executable, args, { ...options, detached: true });
  require('node:module').syncBuiltinESMExports();
}
