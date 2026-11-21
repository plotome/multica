// @vitest-environment node
import { spawnSync } from "node:child_process";
import {
  copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, expect, it } from "vitest";

let root, scripts, bin, marker, voiceMarker;

beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), "multica-package-cli-"));
  scripts = join(root, "apps", "desktop", "scripts");
  bin = join(root, "bin");
  marker = join(root, "builder-args.txt");
  voiceMarker = join(root, "voice-args.jsonl");
  mkdirSync(scripts, { recursive: true });
  mkdirSync(bin);
  writeFileSync(marker, "");
  writeFileSync(voiceMarker, "");
  for (const script of ["bundle-cli.mjs", "package.mjs"]) {
    copyFileSync(fileURLToPath(new URL(script, import.meta.url)), join(scripts, script));
  }
  // Record staging without loading the native engine or fetching packages.
  writeFileSync(join(scripts, "bundle-voice-engine.mjs"), `
    import { appendFileSync } from "node:fs";
    appendFileSync(process.env.VOICE_MARKER, JSON.stringify(process.argv.slice(2)) + "\\n");
  `);

  // Isolate PATH so no Go toolchain or real Electron packager is used.
  if (process.platform === "win32") {
    copyFileSync(process.execPath, join(bin, "node.exe"));
    writeFileSync(join(bin, "electron-vite.cmd"), "@exit /b 0\r\n");
    writeFileSync(join(bin, "electron-builder.cmd"), '@echo %*>>"%BUILDER_MARKER%"\r\n');
  } else {
    symlinkSync(process.execPath, join(bin, "node"));
    writeFileSync(join(bin, "electron-vite"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    writeFileSync(join(bin, "electron-builder"), '#!/bin/sh\necho "$*" >> "$BUILDER_MARKER"\n', { mode: 0o755 });
  }
});

afterEach(() => rmSync(root, { recursive: true, force: true }));

function runScript(name, args) {
  const env = { ...process.env };
  for (const key of Object.keys(env)) {
    if (key.toUpperCase() === "PATH") delete env[key];
  }
  const result = spawnSync(process.execPath, [join(scripts, name), ...args], {
    env: { ...env, PATH: bin, BUILDER_MARKER: marker, VOICE_MARKER: voiceMarker },
    encoding: "utf8",
  });
  if (result.error) throw result.error;
  return result;
}

it("requires Go before development bundling or packaging a Fork target", () => {
  const dev = runScript("bundle-cli.mjs", ["--target-platform", "win32", "--target-arch", "x64"]);
  expect(dev.status).toBe(1);
  expect(dev.stderr).toContain("Go is required to build the matching Fork CLI");

  const result = runScript("package.mjs", ["--win", "--x64"]);
  expect(result.status).toBe(1);
  expect(result.stderr).toContain("Go is required to build the matching Fork CLI");
  expect(readFileSync(marker, "utf8")).toBe("");
  expect(readFileSync(voiceMarker, "utf8")).toBe("");
});

it("packages the built x64 CLI and stops before a failed arm64 build", () => {
  const prebuilt = join(root, "server", "bin", "windows-amd64");
  mkdirSync(prebuilt, { recursive: true });
  writeFileSync(join(prebuilt, "multica.exe"), "x64 fixture");
  const source = `
const fs = require("node:fs");
const args = process.argv.slice(2);
if (args[0] === "version") console.log("go version fake/fake");
else if (args[0] === "build") {
  if (process.env.GOARCH === "arm64") {
    console.error("fixture arm64 compiler unavailable");
    process.exit(1);
  }
  fs.writeFileSync(args[args.indexOf("-o") + 1], "built x64 fixture");
} else process.exit(2);
`;
  if (process.platform === "win32") {
    const fakeGo = join(bin, "fake-go.cjs");
    writeFileSync(fakeGo, source);
    writeFileSync(join(bin, "go.cmd"), `@"${process.execPath}" "${fakeGo}" %*\r\n`);
  } else {
    writeFileSync(join(bin, "go"), `#!${process.execPath}\n${source}`, { mode: 0o755 });
  }

  const result = runScript("package.mjs", ["--win", "--x64", "--arm64"]);
  expect(result.status).toBe(1);
  expect(result.stderr).toContain("fixture arm64 compiler unavailable");
  expect(readFileSync(join(root, "server", "bin", "windows-amd64", "multica.exe"), "utf8")).toBe("built x64 fixture");
  const calls = readFileSync(marker, "utf8").trim().split(/\r?\n/);
  expect(calls).toHaveLength(1);
  expect(calls[0].split(" ")).toContain("--x64");
  expect(calls[0]).not.toContain("--arm64");
  const voiceCalls = readFileSync(voiceMarker, "utf8").trim().split(/\r?\n/).map(line => JSON.parse(line));
  expect(voiceCalls).toEqual([
    ["--target-platform", "win32", "--target-arch", "x64", "--required"],
  ]);
});
