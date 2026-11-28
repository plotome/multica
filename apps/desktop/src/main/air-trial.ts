import { existsSync, mkdirSync, readFileSync, realpathSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { join } from "node:path";
import { deriveProfileName, desktopCliRoot, profileConfigPath } from "./daemon-profile";

export const AIR_TRIAL_NAME = "MulticaIsolatedAirTrial";
export const AIR_TRIAL_APP_ID = "cc.ploto.multica.isolated.multicaisolatedairtrial";
export const AIR_TRIAL_API = "http://127.0.0.1:29424";

const trialToken = "air-desktop-trial-local-only";

function seedFile(path: string, value: object): void {
  if (existsSync(path)) {
    if (realpathSync(path) !== path) throw new Error("Air trial state must not be a symlink");
    return;
  }
  writeFileSync(path, JSON.stringify(value), { flag: "wx", mode: 0o600 });
}

export async function startAirTrialFixture(): Promise<void> {
  // This smoke backend exposes no workspaces, so the real daemon cannot claim work.
  const server = createServer((request, response) => {
    const route = `${request.method} ${request.url}`;
    const authorized = request.headers.authorization === `Bearer ${trialToken}`;
    const payload = authorized && route === "GET /api/daemon/workspaces" ? []
      : authorized && route === "POST /api/tokens/current/renew" ? { renewed: false }
        : null;
    response.writeHead(payload === null ? 404 : 200, { "Content-Type": "application/json" });
    response.end(JSON.stringify(payload));
  });
  const root = desktopCliRoot();
  if (existsSync(root) && realpathSync(root) !== root) throw new Error("Air trial CLI state must not be a symlink");
  const config = profileConfigPath(deriveProfileName(AIR_TRIAL_API));
  const profile = join(root, "profiles", deriveProfileName(AIR_TRIAL_API));
  for (const directory of [root, join(root, "profiles"), profile]) {
    if (existsSync(directory) && realpathSync(directory) !== directory) {
      throw new Error("Air trial profiles must not be symlinks");
    }
    mkdirSync(directory, { recursive: true, mode: 0o700 });
  }
  seedFile(config, { server_url: AIR_TRIAL_API, token: trialToken });
  const actual = JSON.parse(readFileSync(config, "utf8"));
  if (actual.server_url !== AIR_TRIAL_API || actual.token !== trialToken) {
    throw new Error("Air trial profile does not match its loopback fixture");
  }
  seedFile(join(root, "desktop_prefs.json"), { autoStart: true, autoStop: true });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(29424, "127.0.0.1", resolve);
  });
  server.unref();
}
