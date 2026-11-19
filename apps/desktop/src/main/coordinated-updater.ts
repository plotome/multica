import { app, ipcMain, type BrowserWindow } from "electron";
import { existsSync } from "node:fs";
import { writeFile, rename } from "node:fs/promises";
import { join, dirname } from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import {
  createForkControlServer,
  parseForkControlPolicy,
  readPrivateJSON,
} from "./fork-update-control";
import type { UpdaterPreferences } from "../shared/updater-types";
const execute = promisify(execFile);
// Reuse the native updater startup/hourly schedule. Installation remains an
// authenticated owner action in the shared ledger, never a timer side effect.
const STARTUP_CHECK_DELAY_MS = 5_000;
const PERIODIC_CHECK_INTERVAL_MS = 60 * 60 * 1000;
export function setupCoordinatedUpdater(
  windows: () => readonly BrowserWindow[],
  prepareQuit: (
    profile: string,
    lease: string,
    digest: string,
  ) => Promise<void>,
): boolean {
  const path = join(app.getPath("userData"), "fork-update-control.json");
  if (!existsSync(path)) return false;
  const ready = readPrivateJSON(path)
    .then(parseForkControlPolicy)
    .then(async (policy) => {
      const server = await createForkControlServer(policy, prepareQuit, () =>
        app.quit(),
      );
      app.on("will-quit", () => server.close());
      return policy;
    });
  const broadcast = () => {
    for (const win of windows())
      if (!win.isDestroyed() && !win.webContents.isDestroyed())
        win.webContents.send("updater:fork-check");
  };
  const worker = async () => {
    const policy = await ready;
    const config = (await readPrivateJSON(policy.worker_config)) as {
      automatic_checks?: boolean;
      state_dir?: string;
    };
    if (typeof config.automatic_checks !== "boolean")
      throw new Error("Worker check preference is invalid");
    return { policy, config };
  };
  const preferences = async (): Promise<UpdaterPreferences> => {
    const { config } = await worker();
    return {
      mode: "coordinated",
      automaticUpdates: config.automatic_checks === true,
    };
  };
  const check = async () => {
    const { policy, config } = await worker();
    if (typeof config.state_dir !== "string")
      throw new Error("Invalid update worker storage");
    const file = join(config.state_dir, "check-request.json");
    const temporary = join(dirname(file), ".check-request.json");
    await writeFile(
      temporary,
      JSON.stringify({ requested_at: new Date().toISOString() }),
      { mode: 0o600 },
    );
    await rename(temporary, file);
    await execute("/bin/launchctl", [
      "kickstart",
      `gui/${process.getuid!()}/${policy.launchd_label}`,
    ]);
    broadcast();
  };
  ipcMain.handle("updater:get-preferences", preferences);
  ipcMain.handle(
    "updater:set-automatic-updates",
    async (_event, enabled: unknown) => {
      if (typeof enabled !== "boolean")
        throw new Error("Expected boolean update preference");
      const { policy, config } = await worker();
      const temp = policy.worker_config + ".tmp";
      await writeFile(
        temp,
        JSON.stringify({ ...config, automatic_checks: enabled }),
        { mode: 0o600 },
      );
      await rename(temp, policy.worker_config);
      broadcast();
      return preferences();
    },
  );
  ipcMain.handle("updater:fork-check", check);
  for (const channel of [
    "updater:check",
    "updater:download",
    "updater:install",
  ])
    ipcMain.handle(channel, () => {
      throw new Error(
        "Use the coordinated update plan and its authenticated owner approval",
      );
    });
  const automatic = async () => {
    if ((await preferences()).automaticUpdates) broadcast();
  };
  const startup = setTimeout(() => {
    void automatic().catch((error) =>
      console.error("Fork update check failed", error),
    );
  }, STARTUP_CHECK_DELAY_MS);
  const periodic = setInterval(() => {
    void automatic().catch((error) =>
      console.error("Fork update check failed", error),
    );
  }, PERIODIC_CHECK_INTERVAL_MS);
  app.on("will-quit", () => {
    clearTimeout(startup);
    clearInterval(periodic);
  });
  // Observe startup failures without enabling an upstream source as fallback.
  void ready.catch((error) =>
    console.error("Fork update control unavailable", error),
  );
  return true;
}
