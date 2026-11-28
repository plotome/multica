import { execFileSync } from "node:child_process";
import { join } from "node:path";

export default function airTrialProtocol({ appOutDir, electronPlatformName, packager }) {
  if (electronPlatformName !== "darwin") throw new Error("Air trial packaging requires macOS");
  const name = packager.appInfo.productFilename;
  if (name !== "MulticaIsolatedAirTrial") throw new Error("Unexpected Air trial product");
  // Electron Builder merges the parent and child protocol arrays. Replace the
  // merged plist entry while the bundle is still unsigned.
  const urlTypes = [{ CFBundleTypeRole: "Editor", CFBundleURLName: "Multica Air Trial",
    CFBundleURLSchemes: ["multica-air-trial"] }];
  execFileSync("/usr/bin/plutil", ["-replace", "CFBundleURLTypes", "-json",
    JSON.stringify(urlTypes), join(appOutDir, name + ".app", "Contents", "Info.plist")]);
}
