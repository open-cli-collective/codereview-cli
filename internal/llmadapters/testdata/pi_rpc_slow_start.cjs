// Delay only the process-group test's runner, not its helper.
if (process.argv[1]?.endsWith('/runner.mjs')) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 3100);
}
