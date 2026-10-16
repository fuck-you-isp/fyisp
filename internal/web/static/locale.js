// fyisp: loaded before uPlot, as a classic script. uPlot builds
// new Intl.NumberFormat(navigator.language) when it loads, and Intl rejects
// some tags browsers report (en-US@posix, the default in some containers):
// uPlot would then never load and no chart would draw. Report the default
// locale instead.
// @ts-check
try {
  new Intl.NumberFormat(navigator.language);
} catch {
  const lang = new Intl.NumberFormat().resolvedOptions().locale;
  Object.defineProperty(navigator, 'language', { get: () => lang, configurable: true });
}
