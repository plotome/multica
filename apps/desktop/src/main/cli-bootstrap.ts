import { app } from "electron";
import { join } from "path";

// Existing development installations may resolve this path. Packaged Fork
// builds require their bundled CLI and never repair it from a remote release.
export function managedCliPath(): string {
  return join(app.getPath("userData"), "bin", process.platform === "win32" ? "multica.exe" : "multica");
}
