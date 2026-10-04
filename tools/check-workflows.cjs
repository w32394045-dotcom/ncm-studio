// Sanity checks over the workflow files.
//
// A YAML parser is deliberately not used: the repository has no dependencies
// and a CI check is not a good enough reason to add one. These checks read the
// file as text and look for structure a workflow cannot do without, which is
// exactly what a hand-written workflow gets wrong.
const fs = require("fs");
const path = require("path");

const dir = path.join(path.resolve(__dirname, ".."), ".github/workflows");
let failures = 0;

// check(good, message): `good` true means the check passed.
function check(good, message) {
  if (!good) failures++;
  console.log(`${good ? "ok  " : "FAIL"} ${message}`);
}

for (const file of fs.readdirSync(dir)) {
  const text = fs.readFileSync(path.join(dir, file), "utf8");
  console.log(`\n--- ${file} ---`);

  // A tab inside a heredoc's shell code is fine; a tab used as YAML indentation
  // is not, which is why only leading tabs are counted.
  const tabIndent = text.split("\n").filter((l) => /^\t/.test(l)).length;
  check(tabIndent === 0, `no tab-indented lines (${tabIndent} found)`);

  const top = [...text.matchAll(/^([a-zA-Z][\w-]*):/gm)].map((m) => m[1]);
  check(top.includes("on"), `has a trigger block (top-level: ${top.join(", ")})`);
  check(top.includes("jobs"), "has a jobs block");

  // Jobs are the two-space indented keys. The trigger's own keys are indented
  // four spaces, so they are not picked up here.
  const jobNames = [...text.matchAll(/^ {2}([a-zA-Z][\w-]*):\s*$/gm)].map((m) => m[1])
    .filter((n) => !["push", "pull_request", "workflow_dispatch", "schedule"].includes(n));
  check(jobNames.length > 0, `declares jobs: ${jobNames.join(", ")}`);

  const uses = [...text.matchAll(/uses:\s*(\S+)/g)].map((m) => m[1]);
  const badUses = uses.filter((u) => !/^[\w.-]+\/[\w.-]+@[\w.-]+$/.test(u));
  check(badUses.length === 0, `every action is owner/repo@ref (${uses.length})${badUses.length ? ": " + badUses : ""}`);
  const floating = uses.filter((u) => /@(main|master|HEAD)$/.test(u));
  check(floating.length === 0, `no action on a moving branch${floating.length ? ": " + floating : ""}`);

  // Per job: tools are set up where they are used, and a job that publishes
  // waits for the jobs whose artifacts it uploads.
  const starts = jobNames.map((n) => ({ n, i: text.indexOf(`\n  ${n}:`) })).sort((a, b) => a.i - b.i);
  for (let k = 0; k < starts.length; k++) {
    const { n, i } = starts[k];
    const end = k + 1 < starts.length ? starts[k + 1].i : text.length;
    const body = text.slice(i, end);

    if (/\bgo (build|test|vet|run)\b/.test(body)) {
      check(/actions\/setup-go@/.test(body), `job ${n}: sets up Go before running it`);
    }
    if (/(^|\s)node [\w./-]/.test(body)) {
      check(/actions\/setup-node@/.test(body), `job ${n}: sets up Node before running it`);
    }
    if (/download-artifact@/.test(body) && !/needs:/.test(body)) {
      check(false, `job ${n}: downloads an artifact but declares no needs`);
    }
    if (/action-gh-release/.test(body)) {
      check(/\n\s+needs:/.test(body), `job ${n}: waits for the build jobs`);
      check(/\n\s+files:/.test(body), `job ${n}: declares the files to upload`);
      check(/\n\s+tag_name:/.test(body), `job ${n}: declares the tag`);
      check(/\n\s+body_path:|\n\s+body:/.test(body), `job ${n}: declares release notes`);
    }
  }

  // A workflow that wants to be run by hand has to say so in the trigger, and
  // a publishing job behind a manual trigger needs the input it reads.
  if (/github\.event\.inputs\./.test(text)) {
    check(/workflow_dispatch:/.test(text), "reads workflow inputs, so it declares workflow_dispatch");
  }
}

console.log(failures ? `\n${failures} problem(s)` : "\nworkflow checks passed");
process.exitCode = failures ? 1 : 0;
