// Shared fixture: every page records console errors, uncaught exceptions and
// CSP violations, and a test fails if it saw any (csp.spec relies on it).
// @ts-check
const base = require('@playwright/test');

const test = base.test.extend({
  // URLs whose failed requests a test expects (it checks how the page handles them).
  // A failed /api/home2 request is always expected: the probe before the server has it,
  // and the Home specs' unreachable and 404 cases.
  failOk: async ({}, use) => { await use([/\/api\/home2\b/]); },
  page: async ({ page, failOk }, use) => {
    /** @type {string[]} */
    const problems = [];
    page.on('console', (m) => {
      if (m.type() !== 'error') return;
      if (/Failed to load resource/.test(m.text()) && failOk.some((re) => re.test(m.location().url))) return;
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

module.exports = { test, expect: base.expect };
