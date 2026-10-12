// Shared fixture: every page records console errors, uncaught exceptions and
// CSP violations, and a test fails if it saw any.
// @ts-check
const base = require('@playwright/test');

const test = base.test.extend({
  page: async ({ page }, use) => {
    /** @type {string[]} */
    const problems = [];
    page.on('console', (m) => { if (m.type() === 'error') problems.push(`console: ${m.text()}`); });
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
