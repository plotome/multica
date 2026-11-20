export interface UpdaterPreferences {
  automaticUpdates: boolean;
  mode?: "manual" | "coordinated";
}

export type ManualUpdateCheckResult =
  | {
      ok: true;
      currentVersion: string;
      latestVersion: string;
      available: boolean;
    }
  | { ok: false; error: string };
