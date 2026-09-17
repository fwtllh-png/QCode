#!/usr/bin/env node

import {mkdirSync, readFileSync, writeFileSync} from "node:fs";
import {dirname, join, resolve} from "node:path";
import {brotliCompressSync, gzipSync} from "node:zlib";

const root = resolve(process.argv[2] ?? ".");
const options = parseOptions(process.argv.slice(3));
const policyPath = join(root, "testdata/contracts/web-supply-chain-policy.json");
const lockPath = join(root, "web/package-lock.json");
const manifestPath = join(root, "web/dist/asset-manifest.json");
const reportPath = resolve(
  options.report ?? join(root, ".tmp/web-supply-chain-report.json")
);
const measureOnly = options["measure-only"] === true;

const policy = readJSON(policyPath);
const lock = readJSON(lockPath);
const manifest = readJSON(manifestPath);
if (policy.version !== 1 || !Array.isArray(policy.allowed_licenses)) {
  fail("invalid Web supply-chain policy");
}

const allowed = new Set(policy.allowed_licenses);
const attestations = policy.license_attestations ?? {};
const dependencies = Object.entries(lock.packages ?? {})
  .filter(([path]) => path !== "")
  .map(([path, value]) => ({
    path,
    license: value.license ??
      attestations[`${packageName(path)}@${value.version ?? ""}`] ??
      "",
    version: value.version ?? ""
  }));
const rejected = dependencies.filter(({license}) => !licenseAllowed(license));
const problems = [];
if (rejected.length > 0) problems.push(
  `disallowed or missing dependency licenses: ${JSON.stringify(rejected)}`
);

const assets = (manifest.files ?? []).filter(
  ({path}) => !path.endsWith(".gz") && !path.endsWith(".br")
);
const javascript = assets.filter(({path}) => path.endsWith(".js"));
const css = assets.filter(({path}) => path.endsWith(".css"));
const javascriptBuffers = javascript.map(({path}) =>
  readFileSync(join(root, "web/dist", path))
);
const index = readFileSync(join(root, "web/dist/index.html"), "utf8");
const initialPaths = new Set(
  [...index.matchAll(/(?:src|href)="\/([^"]+\.(?:js|css))"/g)]
    .map((match) => match[1])
);
const initialBuffers = assets
  .filter(({path}) => initialPaths.has(path))
  .map(({path}) => readFileSync(join(root, "web/dist", path)));
const measurements = {
  total_raw_bytes: assets.reduce((total, asset) => total + asset.bytes, 0),
  javascript_raw_bytes: javascript.reduce(
    (total, asset) => total + asset.bytes,
    0
  ),
  javascript_gzip_bytes: javascriptBuffers.reduce(
    (total, value) => total + gzipSync(value, {level: 9}).length,
    0
  ),
  javascript_brotli_bytes: javascriptBuffers.reduce(
    (total, value) => total + brotliCompressSync(value).length,
    0
  ),
  css_raw_bytes: css.reduce((total, asset) => total + asset.bytes, 0),
  initial_gzip_bytes: initialBuffers.reduce(
    (total, value) => total + gzipSync(value, {level: 9}).length,
    0
  ),
  initial_brotli_bytes: initialBuffers.reduce(
    (total, value) => total + brotliCompressSync(value).length,
    0
  )
};

for (const [name, maximum] of Object.entries(policy.bundle_budgets ?? {})) {
  const actual = measurements[name];
  if (!Number.isSafeInteger(maximum) || maximum <= 0 ||
      !Number.isSafeInteger(actual)) {
    problems.push(`invalid bundle budget ${name}`);
    continue;
  }
  if (actual > maximum) {
    problems.push(`bundle budget ${name} exceeded: ${actual} > ${maximum}`);
  }
}

const report = {
  version: 1,
  dependency_count: dependencies.length,
  allowed_licenses: [...new Set(dependencies.map(({license}) => license))].sort(),
  bundle: measurements,
  budgets: policy.bundle_budgets,
  problems,
  status: problems.length === 0 ? "passed" : "failed"
};
mkdirSync(dirname(reportPath), {recursive: true});
writeFileSync(reportPath, `${JSON.stringify(report, null, 2)}\n`);
if (problems.length > 0 && !measureOnly) fail(problems.join("\n"));
process.stdout.write(
  `Web supply chain ${report.status}: ${dependencies.length} dependencies, ` +
  `${measurements.total_raw_bytes} raw bytes\n`
);

function readJSON(path) {
  return JSON.parse(readFileSync(path, "utf8"));
}

function licenseAllowed(license) {
  if (!license) return false;
  if (allowed.has(license)) return true;
  const alternatives = spdxOrAlternatives(license);
  // SPDX "OR" lets the consumer pick a branch, so any allowed branch is enough.
  return alternatives.some((id) => allowed.has(id));
}

function spdxOrAlternatives(license) {
  const trimmed = license.trim();
  const inner = trimmed.slice(1, -1);
  const unwrapped = trimmed.startsWith("(") && trimmed.endsWith(")") &&
    !inner.includes("(")
    ? inner
    : trimmed;
  return unwrapped.split(" OR ").map((part) => part.trim());
}

function packageName(path) {
  const parts = path.split("node_modules/");
  return parts[parts.length - 1];
}

function fail(message) {
  process.stderr.write(`${message}\n`);
  process.exit(1);
}

function parseOptions(args) {
  const result = {};
  for (let index = 0; index < args.length; index += 1) {
    const name = args[index];
    if (name === "--measure-only") {
      result["measure-only"] = true;
      continue;
    }
    if (name === "--report" && args[index + 1]) {
      result.report = args[index + 1];
      index += 1;
      continue;
    }
    fail(`unknown option ${name}`);
  }
  return result;
}
