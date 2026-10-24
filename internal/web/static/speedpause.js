// Pausing the speed-test schedule (POST api/speed/config {"pause"}): a
// dialog for how long, and the words for a pause. Scheduled tests are
// skipped while paused; Test now and Test every server still run. Shared by
// the Speed page and, later, the Storage page.
// Plain ES module, no build step.
// @ts-check

import { h, dialog, clock, toLocalInput, fromLocalInput } from './core.js';

/** @typedef {{paused:boolean,since:number|null,until:number|null}} SpeedPauseState */

/** Bounds the server accepts for an end time (scheduler.MinPause, MaxPause). */
const MIN_MS = 60e3, MAX_MS = 30 * 86400e3;

/** "9:30 pm" today, else "Fri 9:30 pm" within a week, else "Oct 31, 9:30 pm". @param {number} ms */
export function when(ms) {
  const d = new Date(ms), now = new Date();
  if (d.toDateString() === now.toDateString()) return clock(ms);
  if (ms - now.getTime() < 6 * 86400e3) return `${d.toLocaleDateString([], { weekday: 'short' })} ${clock(ms)}`;
  return `${d.toLocaleDateString([], { month: 'short', day: 'numeric' })}, ${clock(ms)}`;
}

/** The pause in force (a timed one that has ended is none, before the next poll says so). @param {SpeedPauseState|undefined} p */
export function activePause(p) {
  return p && p.paused && !(p.until && p.until <= Date.now()) ? p : null;
}

/** "paused until you resume them", "paused until 9:30 pm". @param {SpeedPauseState} p */
export function pauseWords(p) {
  return p.until ? `paused until ${when(p.until)}` : 'paused until you resume them';
}

/**
 * Asks how long to pause. Resolves with the request body, or null when
 * cancelled. @param {string} [lead] an extra first line (why pause now)
 * @returns {Promise<{pause:true,pause_until:number|null}|null>}
 */
export async function askPause(lead = '') {
  const opts = [
    { v: 'resume', label: 'Until I resume them' },
    { v: '1h', label: 'For 1 hour' },
    { v: '2h', label: 'For 2 hours' },
    { v: 'at', label: 'Until a time' },
  ];
  // No min/max on the time field: the browser's own check would block Pause
  // (even with another choice picked) before ours could explain.
  const at = /** @type {HTMLInputElement} */ (h('input', { type: 'datetime-local', id: 'sp-pause-time', value: toLocalInput(Date.now() + 2 * 3600e3), 'aria-describedby': 'sp-pause-err' }));
  const err = h('p', { class: 'sp-note', id: 'sp-pause-err', role: 'alert', hidden: true });
  const radios = opts.map((o, i) => h('div', { class: 'sp-radio' },
    h('input', { type: 'radio', name: 'sp-pause', id: `sp-pause-${o.v}`, value: o.v, checked: i === 0 }),
    h('label', { for: `sp-pause-${o.v}` }, h('b', { text: o.label })),
    o.v === 'at' ? h('label', { class: 'fld', for: 'sp-pause-time' }, h('span', { text: 'End time' }), at) : null));
  const form = h('fieldset', { class: 'sp-modes' }, h('legend', { text: 'How long?' }), ...radios, err);
  // Changing the time picks "Until a time".
  at.addEventListener('input', () => { /** @type {HTMLInputElement} */ (form.querySelector('#sp-pause-at')).checked = true; });
  /** @type {HTMLElement|undefined} */
  let focus;
  for (;;) {
    const v = await dialog({
      title: 'Pause scheduled speed tests?',
      body: [...(lead ? [h('p', { text: lead })] : []),
        h('p', { text: 'Scheduled tests are skipped while paused, so they don’t compete with other tests or slow your line. Test now and Test every server still run.' })],
      fields: [form],
      buttons: [{ label: 'Cancel', value: 'cancel' }, { label: 'Pause', value: 'ok', primary: true }],
      focus,
    });
    if (v !== 'ok') return null;
    const pick = /** @type {HTMLInputElement|null} */ (form.querySelector('input[name="sp-pause"]:checked'))?.value;
    const now = Date.now();
    let until = null;
    if (pick === '1h') until = now + 3600e3;
    else if (pick === '2h') until = now + 2 * 3600e3;
    else if (pick === 'at') {
      until = fromLocalInput(at.value);
      if (!(until >= now + MIN_MS && until <= now + MAX_MS)) {
        err.hidden = false;
        err.textContent = 'Pick a time between 1 minute and 30 days from now.';
        at.setAttribute('aria-invalid', 'true');
        focus = at;
        continue;
      }
    }
    return { pause: true, pause_until: until };
  }
}
