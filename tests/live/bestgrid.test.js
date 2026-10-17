"use strict";
// bestGrid and bestGridFill are pure functions in ui/static/live.js (between the <bestGrid / <bestGridFill markers). Run: node tests/live/bestgrid.test.js
const fs = require("fs");
const path = require("path");
const src = fs.readFileSync(path.join(__dirname, "..", "..", "ui", "static", "live.js"), "utf8");
const body = src.slice(src.indexOf("// <bestGrid"), src.indexOf("// bestGrid>"));
const bestGrid = new Function(body + "\nreturn bestGrid;")();
const fillBody = src.slice(src.indexOf("// <bestGridFill"), src.indexOf("// bestGridFill>"));
const bestGridFill = new Function(fillBody + "\nreturn bestGridFill;")();

let failed = 0;
function check(label, got, want) {
  const ok = Object.keys(want).every((k) => got[k] === want[k]);
  if (!ok) failed++;
  console.log((ok ? "ok   " : "FAIL ") + label + " -> " + JSON.stringify(got) + (ok ? "" : " wanted " + JSON.stringify(want)));
}

// 1920x1080 window: about 976 px of height under the header. Six cameras: 3x2, width limited.
check("1920x976 n=6 gap=4", bestGrid(1908, 976, 6, 300, 4), { cols: 3, rows: 2, scroll: false });
check("2560x1336 n=6", bestGrid(2548, 1336, 6, 300, 4), { cols: 3, rows: 2, scroll: false });
check("800x900 n=6 -> 2 columns", bestGrid(788, 900, 6, 300, 4), { cols: 2, rows: 3, scroll: false });
check("390x760 n=6 -> 1 column, scroll, full width", bestGrid(378, 760, 6, 300, 4), { cols: 1, rows: 6, scroll: true, tile: 378 });
check("844x330 (phone landscape) -> 1 column", bestGrid(832, 330, 6, 300, 4), { cols: 1, scroll: true });
check("1 camera", bestGrid(1900, 900, 1, 300, 4), { cols: 1, rows: 1, scroll: false });
check("0 cameras", bestGrid(1900, 900, 0, 300, 4), { cols: 1, rows: 0 });
// Height limited: a very wide, short window prefers more columns.
check("3000x300 n=6 height limited", bestGrid(3000, 300, 6, 100, 0), { cols: 6, rows: 1 });
// Ties go to fewer columns: 4 cameras in a square window, 2x2 (1 and 4 columns would be smaller).
check("tie -> fewer columns (n=6 1600x300: 3, 4, 5 and 6 columns all give 266 px)", bestGrid(1600, 300, 6, 50, 0), { cols: 3, tile: 266 });
check("tile never exceeds the width", bestGrid(500, 5000, 1, 300, 0), { tile: 500 });
// Brute force: the answer is never beaten by another column count.
for (const [w, h, n] of [[1908, 976, 6], [1000, 1000, 5], [1280, 700, 9], [700, 1200, 4], [1600, 400, 3], [400, 400, 7]]) {
  const got = bestGrid(w, h, n, 1, 0);
  let beaten = false;
  for (let c = 1; c <= n; c++) { const t = Math.min(w / c, (h / Math.ceil(n / c)) * 16 / 9); if (Math.floor(t) > got.tile) beaten = true; }
  if (beaten) failed++;
  console.log((beaten ? "FAIL " : "ok   ") + "optimal " + w + "x" + h + " n=" + n + " -> " + JSON.stringify(got));
}

// ---- bestGridFill ----
// Independent crop figure: scale the 16:9 picture to cover the cell, the visible share is cell area / scaled picture area.
const cropOf = (w, h) => { const k = Math.max(w / 16, h / 9); return 1 - (w * h) / (16 * k * 9 * k); };
const pct = (x) => (100 * x).toFixed(1) + " %";
// Content areas (grid slot: window minus header, toolbar and the page padding) for the five window sizes, gap 4.
const areas = { "1920x1080": [1908, 970], "2560x1440": [2548, 1330], "800x1000": [788, 900], "390x844": [378, 750], "1600x300": [1588, 290] };
const gap = 4;
const want = {
  "1920x1080": { cols: 3, rows: 2, scroll: false },
  "2560x1440": { cols: 3, rows: 2, scroll: false },
  "800x1000": { cols: 2, rows: 3, scroll: false },     // 3 columns would be 262 px wide, 1 column 146 px tall
  "390x844": { cols: 1, rows: 6, scroll: true },       // 6 stacked tiles would be 121 px tall: the 16:9 scroll fallback
  "1600x300": { cols: 1, rows: 6, scroll: true },      // no cell reaches 180 px tall
};
for (const [size, [w, h]] of Object.entries(areas)) {
  const g = bestGridFill(w, h, 6, 300, gap);
  check("fill " + size + " n=6", g, want[size]);
  console.log("     " + size + " n=6: " + g.cols + "x" + g.rows + " cells " + g.w + "x" + g.h + " crop " + pct(cropOf(g.w, g.h)) + (g.scroll ? " (scroll fallback, 16:9, nothing cropped)" : ""));
}
check("fill phone n=6 fallback tile is 16:9 and as wide as the screen", bestGridFill(378, 750, 6, 300, gap), { w: 378, h: 213, crop: 0 });
check("fill phone n=2 stays one column and fills the height", bestGridFill(378, 750, 2, 300, gap), { cols: 1, rows: 2, scroll: false, h: 373 });
check("fill 1 camera = the whole area", bestGridFill(1900, 900, 1, 300, 4), { cols: 1, rows: 1, w: 1900, h: 900, scroll: false });
check("fill 0 cameras", bestGridFill(1900, 900, 0, 300, 4), { cols: 1, rows: 0 });
// A 16:9 area with n=4 is 2x2 with no crop at all.
check("fill 1920x1080 n=4 -> 2x2, no crop", bestGridFill(1920, 1080, 4, 100, 0), { cols: 2, rows: 2, crop: 0 });
check("fill tie -> fewer columns (16:9 area, n=2: 1x2 and 2x1 both crop 50 %)", bestGridFill(1920, 1080, 2, 100, 0, 100), { cols: 1, rows: 2 });
// Brute force: every size x n=1..8 x gap 0 and 4: either nothing qualifies (scroll fallback) or no qualifying column count crops less, and a
// equally-cropping one with fewer columns would have been chosen.
let cases = 0, bad = 0;
for (const [, [w, h]] of Object.entries(areas)) for (const gp of [0, 4]) for (let n = 1; n <= 8; n++) {
  const got = bestGridFill(w, h, n, 300, gp);
  let min = null;
  for (let c = 1; c <= n; c++) {
    const r = Math.ceil(n / c), cw = (w - gp * (c - 1)) / c, ch = (h - gp * (r - 1)) / r;
    if (cw < 300 || ch < 180) continue;
    const crop = cropOf(cw, ch);
    if (!min || crop < min.crop - 1e-9) min = { c, crop };
  }
  cases++;
  let ok;
  if (!min) ok = got.scroll === true && got.cols === 1 && got.rows === n && got.w <= w && got.w <= Math.floor(h * 16 / 9) && Math.abs(got.h - got.w * 9 / 16) <= 1;
  else ok = got.scroll === false && got.cols === min.c && Math.abs(got.crop - min.crop) < 1e-9 && got.cols * got.w + gp * (got.cols - 1) <= w + 1e-6 && got.rows * got.h + gp * (got.rows - 1) <= h + 1e-6 && got.cols * got.rows >= n;
  if (!ok) { bad++; console.log("FAIL optimal fill " + w + "x" + h + " gap " + gp + " n=" + n + " -> " + JSON.stringify(got) + " brute force " + JSON.stringify(min)); }
}
if (bad) failed++;
console.log((bad ? "FAIL " : "ok   ") + "fill brute force: " + cases + " cases (5 sizes x gap 0/4 x n=1..8), " + bad + " wrong");
// Crop table for the report: Fill at 1920x1080 and 2560x1440, n=1..8 (gap 4).
for (const [size, [w, h]] of [["1920x1080", areas["1920x1080"]], ["2560x1440", areas["2560x1440"]]]) {
  console.log("     " + size + " crop by n: " + [1, 2, 3, 4, 5, 6, 7, 8].map((n) => { const g = bestGridFill(w, h, n, 300, gap); return n + "->" + g.cols + "x" + g.rows + " " + pct(g.crop); }).join(", "));
}
process.exit(failed ? 1 : 0);
