// The two static checks the Go suite does on the front end, run here because a
// machine without a Go toolchain still has to be able to check its own work:
//   TestEveryKeyTheUIRefersToExists — every key index.html asks for is in the
//     tables, in every language;
//   TestTranslationsCoverEveryKey — no language is missing a key another has.
//
// Run it from anywhere: node tools/check-static.cjs
const fs = require("fs");
const path = require("path");
const root = path.resolve(__dirname, "..");
const html = fs.readFileSync(path.join(root, "internal/web/static/index.html"), "utf8");
const dict = JSON.parse(fs.readFileSync(path.join(root, "internal/web/static/i18n.json"), "utf8"));

const keys = new Set();
// A key that is built from a prefix at run time ("stage." + name). The bare
// prefix is recorded here and expanded below; it is not itself a key.
const prefixes = new Set();
const addKey = (k) => {
  if (/[.$^]|\.$/.test(k) && k.endsWith(".")) prefixes.add(k);
  else keys.add(k);
};
for (const m of html.matchAll(/\bt\(\s*"([A-Za-z0-9_.]+)"/g)) addKey(m[1]);
for (const m of html.matchAll(/\bt\(\s*'([A-Za-z0-9_.]+)'/g)) addKey(m[1]);
for (const m of html.matchAll(/data-i18n="([^"]+)"/g)) addKey(m[1].trim());
for (const m of html.matchAll(/data-i18n-attr="([^"]+)"/g)) {
  for (const pair of m[1].split(",")) {
    const at = pair.indexOf(":");
    if (at >= 0) addKey(pair.slice(at + 1).trim());
  }
}
for (const prefix of prefixes) {
  for (const k of Object.keys(dict["en"])) if (k.startsWith(prefix)) keys.add(k);
}

const langs = Object.keys(dict);
const missing = [];
for (const k of keys) for (const l of langs) if (!(k in dict[l])) missing.push(`${l}:${k}`);

console.log(`UI refers to ${keys.size} keys; languages: ${langs.join(", ")}`);
console.log(missing.length ? `MISSING: ${missing.join(" ")}` : "all present in every language");

const all = new Set();
for (const l of langs) for (const k of Object.keys(dict[l])) all.add(k);
const uneven = [];
for (const l of langs) {
  const miss = [...all].filter((k) => !(k in dict[l]));
  if (miss.length) uneven.push(`${l} missing ${miss.length}: ${miss.slice(0, 5).join(",")}`);
}
console.log(`tables hold ${all.size} keys; ${uneven.length ? uneven.join(" | ") : "every language complete"}`);

// The keys this session added, so their presence is explicit rather than
// implied by the totals.
const added = [
  "settings.minSize", "settings.minSizeHint", "settings.minSizePlaceholder", "settings.minSizeOff",
  "source.tooSmall", "source.skipped",
  "covers.fill", "covers.fillHint", "covers.fillConfirm", "covers.fillStop", "covers.fillStopping",
  "covers.fillProgress", "covers.fillDone", "covers.fillNone", "covers.fillToast", "covers.fillStopped",
  "covers.finding", "covers.filled", "covers.alreadyHas",
];
const absent = added.filter((k) => !(k in dict["en"]));
console.log(absent.length ? `ADDED KEYS MISSING: ${absent.join(" ")}` : `all ${added.length} new keys present`);
