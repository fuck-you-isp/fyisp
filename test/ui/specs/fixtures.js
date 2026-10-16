// Shared fixture: every page records console errors, uncaught exceptions and
// CSP violations, and a test fails if it saw any (csp.spec relies on it).
// webdev() starts a test's own fyisp-webdev (a scenario) on a port.
// @ts-check
const { spawn } = require('child_process');
const base = require('@playwright/test');

const test = base.test.extend({
  // Failed requests a test expects (it checks how the page handles them), matched
  // against "<console text> <url>". Specs add their own (unreachable, 404 tiles).
  failOk: async ({}, use) => { await use([]); },
  page: async ({ page, failOk }, use) => {
    /** @type {string[]} */
    const problems = [];
    page.on('console', (m) => {
      if (m.type() !== 'error') return;
      const line = `${m.text()} ${m.location().url}`;
      if (/Failed to load resource/.test(line) && failOk.some((re) => re.test(line))) return;
      problems.push(`console: ${m.text()}`);
    });
    page.on('pageerror', (e) => problems.push(`exception: ${e.message}`));
    await page.exposeFunction('__fyispCSP', (/** @type {string} */ s) => problems.push(`CSP: ${s}`));
    await page.addInitScript(() => {
      document.addEventListener('securitypolicyviolation', (e) => {
        /** @type {any} */ (window).__fyispCSP(`${e.violatedDirective} blocked ${e.blockedURI || 'inline'} (${e.sourceFile}:${e.lineNumber})`);
      });
    });
    await use(page);
    base.expect(problems, 'console errors, exceptions or CSP violations').toEqual([]);
  },
});

/** Starts fyisp-webdev on port with args and waits until it answers. @param {number} port @param {string[]} args */
async function webdev(port, args) {
  const proc = spawn('fyisp-webdev', ['-listen', `127.0.0.1:${port}`, ...args], { stdio: 'ignore' });
  const failed = new Promise((_, reject) => proc.on('error', reject));
  const up = (async () => {
    for (let i = 0; i < 100; i++) {
      try {
        if ((await fetch(`http://127.0.0.1:${port}/api/home?part=now`)).ok) return;
      } catch { /* not yet */ }
      await new Promise((r) => setTimeout(r, 100));
    }
    throw new Error(`fyisp-webdev ${args.join(' ')} did not start`);
  })();
  await Promise.race([up, failed]);
  return proc;
}

module.exports = { test, expect: base.expect, webdev };
