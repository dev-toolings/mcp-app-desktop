// Release tooling for the Claude Code plugin, the GitHub release and the
// (optional) npm distribution.
//
//   bun scripts/release.ts version X.Y.Z      set the version everywhere it is pinned
//   bun scripts/release.ts check [vX.Y.Z]     verify pinned versions agree (and match the tag)
//   bun scripts/release.ts build [X.Y.Z]      cross-compile every target in parallel into dist/bin
//   bun scripts/release.ts stage X.Y.Z        write the npm packages and release archives into dist/
//   bun scripts/release.ts plugin X.Y.Z       assemble the installable plugin with its binaries in dist/plugin
//   bun scripts/release.ts push-plugin X.Y.Z  replace the `dist` branch (what the marketplace serves) with dist/plugin
//   bun scripts/release.ts github X.Y.Z       create or update the GitHub release with the archives
//   bun scripts/release.ts publish X.Y.Z      publish the npm packages (not run by the release workflow)
//
// The binaries are pure Go (CGO_ENABLED=0), so one Linux runner builds every
// target; the Go linker ad-hoc signs darwin/arm64 binaries itself.
import { chmod, cp, mkdir, rm } from "node:fs/promises";
import { join } from "node:path";

const ROOT = join(import.meta.dir, "..");
const DIST = join(ROOT, "dist");
const SCOPE = "@dev-toolings";
const MAIN = `${SCOPE}/mcp-app-desktop`;
const PLUGIN_DIR = join(ROOT, "plugin");
const PLUGIN_JSON = join(PLUGIN_DIR, ".claude-plugin/plugin.json");
const MCP_JSON = join(PLUGIN_DIR, ".mcp.json");
// One command for every OS: macOS runs the universal binary bin/mcp-app-desktop,
// Windows resolves the extension-less name to bin/mcp-app-desktop.exe.
const PLUGIN_COMMAND = "${CLAUDE_PLUGIN_ROOT}/bin/mcp-app-desktop";
const DIST_BRANCH = "dist";
const NPM_MAIN = join(ROOT, "npm/mcp-app-desktop");
const SEMVER = /^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/;

// Keep in sync with TARGETS in npm/mcp-app-desktop/bin/mcp-app-desktop.js.
const TARGETS = [
  { key: "darwin-arm64", goos: "darwin", goarch: "arm64", os: "darwin", cpu: "arm64", label: "macOS arm64" },
  { key: "darwin-x64", goos: "darwin", goarch: "amd64", os: "darwin", cpu: "x64", label: "macOS x64" },
  { key: "win32-arm64", goos: "windows", goarch: "arm64", os: "win32", cpu: "arm64", label: "Windows arm64" },
  { key: "win32-x64", goos: "windows", goarch: "amd64", os: "win32", cpu: "x64", label: "Windows x64" },
] as const;
type Target = (typeof TARGETS)[number];

const exe = (t: Target) => (t.goos === "windows" ? "mcp-app-desktop.exe" : "mcp-app-desktop");
const platformPackage = (t: Target) => `${MAIN}-${t.key}`;

function die(message: string): never {
  console.error(`release: ${message}`);
  process.exit(1);
}

function parseVersion(raw: string | undefined): string {
  if (!raw) die("missing version argument");
  const v = raw.replace(/^v/, "");
  if (!SEMVER.test(v)) die(`invalid version ${JSON.stringify(raw)} (expected X.Y.Z or X.Y.Z-pre)`);
  return v;
}

async function readJSON(path: string): Promise<any> {
  return JSON.parse(await Bun.file(path).text());
}

async function writeJSON(path: string, value: unknown) {
  await Bun.write(path, JSON.stringify(value, null, 2) + "\n");
}

async function run(cmd: string[], opts: { cwd?: string; env?: Record<string, string> } = {}) {
  const proc = Bun.spawn(cmd, {
    cwd: opts.cwd ?? ROOT,
    env: { ...process.env, ...opts.env },
    stdout: "inherit",
    stderr: "inherit",
  });
  const code = await proc.exited;
  if (code !== 0) die(`${cmd.join(" ")} exited with ${code}`);
}

async function setVersion(v: string) {
  const plugin = await readJSON(PLUGIN_JSON);
  plugin.version = v;
  await writeJSON(PLUGIN_JSON, plugin);

  const pkgPath = join(NPM_MAIN, "package.json");
  const pkg = await readJSON(pkgPath);
  pkg.version = v;
  pkg.optionalDependencies = Object.fromEntries(TARGETS.map((t) => [platformPackage(t), v]));
  await writeJSON(pkgPath, pkg);
  await check(undefined);
  console.log(`version set to ${v}; commit, then tag v${v}`);
}

async function check(tag: string | undefined) {
  const errors: string[] = [];
  const v = (await readJSON(PLUGIN_JSON)).version as string;
  if (!SEMVER.test(v)) errors.push(`plugin.json version ${JSON.stringify(v)} is not semver`);

  const command = (await readJSON(MCP_JSON)).mcpServers?.["mcp-app-desktop"]?.command;
  if (command !== PLUGIN_COMMAND) errors.push(`plugin/.mcp.json must run ${PLUGIN_COMMAND} (command: ${JSON.stringify(command)})`);

  const pkg = await readJSON(join(NPM_MAIN, "package.json"));
  if (pkg.name !== MAIN) errors.push(`npm package name is ${pkg.name}, expected ${MAIN}`);
  if (pkg.version !== v) errors.push(`npm package version ${pkg.version} != plugin version ${v}`);
  const deps: Record<string, string> = pkg.optionalDependencies ?? {};
  const expected = TARGETS.map(platformPackage).sort();
  if (JSON.stringify(Object.keys(deps).sort()) !== JSON.stringify(expected)) {
    errors.push(`optionalDependencies must be exactly ${expected.join(", ")}`);
  }
  for (const [name, dv] of Object.entries(deps)) if (dv !== v) errors.push(`${name} pinned at ${dv}, expected ${v}`);

  if (tag !== undefined && parseVersion(tag) !== v) errors.push(`tag ${tag} does not match version ${v}`);
  if (errors.length) die(errors.join("\n         "));
  console.log(`versions consistent: ${v}`);
}

async function build(v: string) {
  await rm(join(DIST, "bin"), { recursive: true, force: true });
  // One go build per target, concurrently: they share the build cache and
  // each one is mostly single-threaded at link time.
  await Promise.all(
    TARGETS.map((t) =>
      run(
        [
          "go", "build", "-trimpath",
          "-ldflags", `-s -w -X main.version=${v}`,
          "-o", join(DIST, "bin", t.key, exe(t)),
          "./cmd/mcp-app-desktop",
        ],
        { env: { CGO_ENABLED: "0", GOOS: t.goos, GOARCH: t.goarch } },
      ),
    ),
  );
  console.log(`built ${TARGETS.map((t) => t.key).join(", ")} (${v})`);
}

async function stage(v: string) {
  const npmDir = join(DIST, "npm");
  const relDir = join(DIST, "release");
  await rm(npmDir, { recursive: true, force: true });
  await rm(relDir, { recursive: true, force: true });
  await mkdir(relDir, { recursive: true });
  await check(v);
  const repository = (await readJSON(join(NPM_MAIN, "package.json"))).repository;

  for (const t of TARGETS) {
    const src = join(DIST, "bin", t.key, exe(t));
    if (!(await Bun.file(src).exists())) die(`${src} missing; run build ${v} first`);
    const dir = join(npmDir, `mcp-app-desktop-${t.key}`);
    await mkdir(join(dir, "bin"), { recursive: true });
    await cp(src, join(dir, "bin", exe(t)));
    await chmod(join(dir, "bin", exe(t)), 0o755);
    await writeJSON(join(dir, "package.json"), {
      name: platformPackage(t),
      version: v,
      description: `mcp-app-desktop binary for ${t.label}. Install ${MAIN} instead.`,
      repository,
      os: [t.os],
      cpu: [t.cpu],
      files: ["bin/"],
      preferUnplugged: true,
    });

    const archive = join(relDir, `mcp-app-desktop_${v}_${t.key}${t.goos === "windows" ? ".zip" : ".tar.gz"}`);
    if (t.goos === "windows") await run(["zip", "-jq", archive, src]);
    else await run(["tar", "-czf", archive, "-C", join(DIST, "bin", t.key), exe(t)]);
  }

  const main = join(npmDir, "mcp-app-desktop");
  await cp(NPM_MAIN, main, { recursive: true });
  await chmod(join(main, "bin", "mcp-app-desktop.js"), 0o755);
  await cp(join(ROOT, "README.md"), join(main, "README.md"));

  const sums: string[] = [];
  for (const name of [...new Bun.Glob("*").scanSync(relDir)].sort()) {
    const hasher = new Bun.CryptoHasher("sha256");
    hasher.update(await Bun.file(join(relDir, name)).arrayBuffer());
    sums.push(`${hasher.digest("hex")}  ${name}`);
  }
  await Bun.write(join(relDir, "SHA256SUMS"), sums.join("\n") + "\n");
  console.log(`staged npm packages in ${npmDir} and archives in ${relDir}`);
}

// universal joins thin 64-bit Mach-O binaries into one fat (universal)
// binary, as `lipo -create` does, which Linux runners do not have. Each slice
// keeps its own code signature.
function universal(slices: Uint8Array[]): Uint8Array {
  const ALIGN = 14; // 16 KiB, what lipo uses for arm64 slices
  const step = 1 << ALIGN;
  const up = (n: number) => Math.ceil(n / step) * step;
  let offset = up(8 + 20 * slices.length);
  const entries = slices.map((s) => {
    const dv = new DataView(s.buffer, s.byteOffset, s.byteLength);
    if (dv.getUint32(0, true) !== 0xfeedfacf) die("universal: slice is not a thin 64-bit Mach-O");
    const entry = { cputype: dv.getUint32(4, true), cpusubtype: dv.getUint32(8, true), offset, size: s.byteLength };
    offset = up(offset + s.byteLength);
    return entry;
  });
  const last = entries[entries.length - 1];
  const out = new Uint8Array(last.offset + last.size);
  const dv = new DataView(out.buffer);
  dv.setUint32(0, 0xcafebabe); // FAT_MAGIC, big-endian like the whole fat header
  dv.setUint32(4, entries.length);
  entries.forEach((e, i) => {
    const at = 8 + 20 * i;
    dv.setUint32(at, e.cputype);
    dv.setUint32(at + 4, e.cpusubtype);
    dv.setUint32(at + 8, e.offset);
    dv.setUint32(at + 12, e.size);
    dv.setUint32(at + 16, ALIGN);
    out.set(slices[i], e.offset);
  });
  return out;
}

async function binary(key: Target["key"], v: string): Promise<Uint8Array> {
  const t = TARGETS.find((t) => t.key === key)!;
  const path = join(DIST, "bin", t.key, exe(t));
  if (!(await Bun.file(path).exists())) die(`${path} missing; run build ${v} first`);
  return new Uint8Array(await Bun.file(path).arrayBuffer());
}

// plugin writes the installable plugin: manifest, .mcp.json, skill and two
// binaries (universal macOS, Windows x64; Windows on arm64 runs x64 binaries).
async function plugin(v: string) {
  await check(v);
  const out = join(DIST, "plugin");
  await rm(out, { recursive: true, force: true });
  await mkdir(join(out, "bin"), { recursive: true });
  await cp(join(PLUGIN_DIR, ".claude-plugin"), join(out, ".claude-plugin"), { recursive: true });
  await cp(MCP_JSON, join(out, ".mcp.json"));
  await cp(join(PLUGIN_DIR, "skills/mcp-app-desktop"), join(out, "skills/mcp-app-desktop"), { recursive: true });

  const mac = join(out, "bin", "mcp-app-desktop");
  await Bun.write(mac, universal([await binary("darwin-x64", v), await binary("darwin-arm64", v)]));
  await chmod(mac, 0o755);
  await Bun.write(join(out, "bin", "mcp-app-desktop.exe"), await binary("win32-x64", v));
  console.log(`assembled plugin ${v} in ${out}`);
}

function git(args: string[], env: Record<string, string> = {}): string {
  const r = Bun.spawnSync(["git", ...args], { cwd: ROOT, env: { ...process.env, ...env }, stderr: "pipe" });
  if (r.exitCode !== 0) die(`git ${args.join(" ")}: ${r.stderr.toString().trim()}`);
  return r.stdout.toString().trim();
}

// pushPlugin replaces the dist branch with a single commit holding
// dist/plugin. It goes through a throwaway index so the checkout, its branch
// and its credentials are used as they are; replacing the branch keeps
// binaries of past releases out of every clone.
async function pushPlugin(v: string) {
  const dir = join(DIST, "plugin");
  if ((await readJSON(join(dir, ".claude-plugin/plugin.json"))).version !== v) die(`dist/plugin is not ${v}; run plugin ${v} first`);
  const index = join(DIST, "plugin.index");
  await rm(index, { force: true });
  const env = { GIT_INDEX_FILE: index };
  git(["-C", dir, "--work-tree", ".", "add", "--all", "--force", "."], env);
  const tree = git(["write-tree"], env);
  const source = git(["rev-parse", "HEAD"]);
  const commit = git(["commit-tree", tree, "-m", `mcp-app-desktop ${v}\n\nBuilt from ${source}.`], {
    GIT_AUTHOR_NAME: "github-actions[bot]",
    GIT_AUTHOR_EMAIL: "41898282+github-actions[bot]@users.noreply.github.com",
    GIT_COMMITTER_NAME: "github-actions[bot]",
    GIT_COMMITTER_EMAIL: "41898282+github-actions[bot]@users.noreply.github.com",
  });
  await rm(index, { force: true });
  git(["push", "--force", "origin", `${commit}:refs/heads/${DIST_BRANCH}`]);
  console.log(`pushed ${commit} to ${DIST_BRANCH}`);
}

async function published(name: string, v: string): Promise<boolean> {
  const res = await fetch(`https://registry.npmjs.org/${name.replace("/", "%2f")}/${v}`);
  if (res.status === 200) return true;
  if (res.status === 404) return false;
  die(`registry lookup for ${name}@${v} failed: HTTP ${res.status}`);
}

async function publish(v: string) {
  if (!process.env.NPM_CONFIG_TOKEN) die("NPM_CONFIG_TOKEN is not set");
  const tag = v.includes("-") ? "next" : "latest";
  // Platform packages first: the main package must never resolve to an
  // optional dependency that is not on the registry yet. Re-running after a
  // partial failure skips what is already published.
  const dirs = [...TARGETS.map((t) => [platformPackage(t), `mcp-app-desktop-${t.key}`]), [MAIN, "mcp-app-desktop"]];
  for (const [name, dir] of dirs) {
    if (await published(name, v)) {
      console.log(`skip ${name}@${v}: already published`);
      continue;
    }
    await run(["bun", "publish", "--access", "public", "--tag", tag], { cwd: join(DIST, "npm", dir) });
  }
}

async function github(v: string) {
  const tag = `v${v}`;
  const assets = [...new Bun.Glob("*").scanSync(join(DIST, "release"))].map((f) => join(DIST, "release", f));
  if (assets.length === 0) die(`no archives in dist/release; run stage ${v} first`);
  const exists = Bun.spawnSync(["gh", "release", "view", tag], { stdout: "ignore", stderr: "ignore" }).exitCode === 0;
  if (exists) await run(["gh", "release", "upload", tag, ...assets, "--clobber"]);
  else {
    const pre = v.includes("-") ? ["--prerelease"] : [];
    await run(["gh", "release", "create", tag, ...assets, "--verify-tag", "--generate-notes", ...pre]);
  }
}

const [cmd, arg] = process.argv.slice(2);
switch (cmd) {
  case "version":
    await setVersion(parseVersion(arg));
    break;
  case "check":
    await check(arg);
    break;
  case "build":
    await build(arg ? parseVersion(arg) : "dev");
    break;
  case "stage":
    await stage(parseVersion(arg));
    break;
  case "plugin":
    await plugin(parseVersion(arg));
    break;
  case "push-plugin":
    await pushPlugin(parseVersion(arg));
    break;
  case "publish":
    await publish(parseVersion(arg));
    break;
  case "github":
    await github(parseVersion(arg));
    break;
  default:
    die(`unknown command ${JSON.stringify(cmd)}; see the header of scripts/release.ts`);
}
