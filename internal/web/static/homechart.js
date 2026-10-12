// fyisp Home (v0.9.0): the 24-hour chart in a tile's detail panel. One uPlot
// line (how quickly the app answered, 288 five-minute points, gaps left
// open), a dashed "usual" line, problem bands and hatched "not running"
// bands. The text alternative and legend live in the panel (hometiles.js).
// @ts-check

import { css, rgba, hatch, hpClock } from './core.js';

/** @typedef {import('./hometiles.js').HomeTileDetail} HomeTileDetail */

const uPlot = /** @type {any} */ (window).uPlot;
const FONT = '11px system-ui, -apple-system, "Segoe UI", sans-serif';
const NARROW = 480;

/** "2 pm", "midnight", "6 am" for whole hours. */
function hourLabel(/** @type {number} */ ms) {
  const d = new Date(ms);
  if (d.getHours() === 0) return 'midnight';
  if (d.getHours() === 12) return 'noon';
  return hpClock(ms).replace(':00', '');
}

/**
 * x ticks (seconds): the start, a few local hours (2 pm, 7 pm, midnight,
 * 6 am; only midnight when narrow) and now. Hours too close to either end
 * are left out so labels never overlap.
 */
function xTicks(/** @type {number} */ from, /** @type {number} */ to, /** @type {boolean} */ narrow) {
  const hours = narrow ? [0] : [14, 19, 0, 6];
  const out = [from];
  const gap = (to - from) * (narrow ? 0.25 : 0.1);
  const d = new Date(from);
  d.setMinutes(0, 0, 0);
  for (let t = d.getTime() + 3600e3; t < to; t += 3600e3) {
    if (hours.includes(new Date(t).getHours()) && t - from >= gap && to - t >= gap) out.push(t);
  }
  out.push(to);
  return out;
}

export class TileChart {
  /** @param {HTMLElement} box */
  constructor(box) {
    this.box = box;
    /** @type {any} */
    this.u = null;
    /** @type {HomeTileDetail|null} */
    this.d = null;
    this.narrow = false;
  }

  width() { return Math.max(260, this.box.clientWidth || 0); }

  /** Draws (or redraws) the chart for one part=tile answer. @param {HomeTileDetail} d */
  set(d) {
    const len = (d.ms || []).length;
    const xs = new Float64Array(len);
    for (let i = 0; i < len; i++) xs[i] = (d.from + i * d.step) / 1000;
    const data = [xs, d.ms.map((v) => (v == null ? null : v))];
    this.d = d;
    if (!this.u || (this.width() < NARROW) !== this.narrow) this.build(data);
    else this.u.setData(data);
  }

  /** @param {any[]} data */
  build(data) {
    this.u?.destroy();
    this.narrow = this.width() < NARROW;
    const self = this;
    const ink2 = css('--ink-2'), grid = css('--grid'), axis = css('--axis');
    const opts = {
      width: this.width(),
      height: this.narrow ? 150 : 170,
      padding: [8, 10, 0, 0],
      legend: { show: false },
      cursor: { drag: { x: false, y: false }, points: { show: false } },
      scales: {
        x: { time: true },
        y: {
          range: (/** @type {any} */ u, /** @type {number} */ min, /** @type {number} */ max) => {
            const usual = self.d && self.d.usual_ms;
            const top = Math.max(usual ? usual * 2.2 : 10, isFinite(max) ? max : 0) * 1.05;
            return [0, top];
          },
        },
      },
      axes: [
        {
          stroke: ink2, font: FONT, grid: { show: false }, ticks: { stroke: axis, width: 1, size: 4 },
          splits: (/** @type {any} */ u) => {
            const d = self.d;
            if (!d || !d.ms.length) return [];
            return xTicks(d.from, d.from + (d.ms.length - 1) * d.step, self.narrow).map((t) => t / 1000);
          },
          values: (/** @type {any} */ u, /** @type {number[]} */ splits) => splits.map((s, i) =>
            i === splits.length - 1 ? 'now' : i === 0 ? hpClock(s * 1000) : hourLabel(s * 1000)),
        },
        { stroke: ink2, font: FONT, size: 40, label: 'ms', labelSize: 14, labelFont: FONT, grid: { stroke: grid, width: 1 }, ticks: { stroke: axis, width: 1, size: 4 } },
      ],
      series: [{}, { label: 'ms', stroke: css('--accent'), width: 2, spanGaps: false, points: { show: false } }],
      hooks: {
        drawClear: [(/** @type {any} */ u) => self.drawBands(u)],
        draw: [(/** @type {any} */ u) => self.drawUsual(u)],
      },
    };
    this.u = new uPlot(opts, data, this.box);
    // Axis ticks are text drawn on the canvas: the wrapper's role=img and description carry it.
  }

  /** Problem bands (tinted, with a stronger top edge) and hatched "not running" bands, behind the line. */
  drawBands(/** @type {any} */ u) {
    const d = this.d;
    if (!d || !d.bands || !d.bands.length) return;
    const ctx = u.ctx;
    const { left, top, width, height } = u.bbox;
    const dpr = window.devicePixelRatio || 1;
    const down = css('--down');
    ctx.save();
    ctx.beginPath();
    ctx.rect(left, top, width, height);
    ctx.clip();
    for (const b of d.bands) {
      let x0 = u.valToPos(b.from / 1000, 'x', true), x1 = u.valToPos(b.to / 1000, 'x', true);
      if (x1 - x0 < 4 * dpr) { const m = (x0 + x1) / 2; x0 = m - 2 * dpr; x1 = m + 2 * dpr; }
      if (b.kind === 'off') {
        ctx.fillStyle = hatch(ctx);
        ctx.fillRect(x0, top, x1 - x0, height);
      } else {
        ctx.fillStyle = rgba(down, 0.15);
        ctx.fillRect(x0, top, x1 - x0, height);
        ctx.fillStyle = down;
        ctx.fillRect(x0, top, x1 - x0, 2 * dpr);
      }
    }
    ctx.restore();
  }

  /** The usual time, a dashed line (none while fyisp is still learning). */
  drawUsual(/** @type {any} */ u) {
    const usual = this.d && this.d.usual_ms;
    if (usual == null) return;
    const ctx = u.ctx;
    const { left, width } = u.bbox;
    const dpr = window.devicePixelRatio || 1;
    const y = Math.round(u.valToPos(usual, 'y', true)) + 0.5;
    ctx.save();
    ctx.strokeStyle = css('--muted');
    ctx.lineWidth = 1.5 * dpr;
    ctx.setLineDash([4 * dpr, 4 * dpr]);
    ctx.beginPath();
    ctx.moveTo(left, y);
    ctx.lineTo(left + width, y);
    ctx.stroke();
    ctx.restore();
  }

  resize() {
    if (!this.u || !this.d) return;
    if ((this.width() < NARROW) !== this.narrow) { this.set(this.d); return; }
    this.u.setSize({ width: this.width(), height: this.narrow ? 150 : 170 });
  }

  /** The palette changed: rebuild with the new colours. */
  themeChanged() {
    if (!this.u || !this.d) return;
    const d = this.d;
    this.u.destroy();
    this.u = null;
    this.set(d);
  }

  clear() {
    this.u?.destroy();
    this.u = null;
    this.d = null;
  }
}
