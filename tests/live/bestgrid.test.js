"use strict";
// bestGrid is a pure function in ui/static/live.js (between the <bestGrid markers). Run: node tests/live/bestgrid.test.js
const fs = require("fs");
const path = require("path");
const src = fs.readFileSync(path.join(__dirname, "..", "..", "ui", "static", "live.js"), "utf8");
const body = src.slice(src.indexOf("// <bestGrid"), src.indexOf("// bestGrid>"));
const bestGrid = new Function(body + "\nreturn bestGrid;")();

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
process.exit(failed ? 1 : 0);
