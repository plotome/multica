import { createConnection, createServer, type Server } from "node:net";
import { lstat, readFile, unlink, chmod } from "node:fs/promises";
import { isAbsolute } from "node:path";
import { timingSafeEqual } from "node:crypto";

export interface ForkControlPolicy {
  profile: string;
  socket: string;
  secret_file: string;
  worker_config: string;
  launchd_label: string;
  request_limit_bytes: number;
  request_timeout_ms: number;
}
export async function readPrivateJSON(path: string): Promise<unknown> {
  if (!isAbsolute(path))
    throw new Error("Fork update policy requires absolute paths");
  const info = await lstat(path);
  if (!info.isFile() || (info.mode & 0o077) !== 0)
    throw new Error("Fork update policy must be a private regular file");
  return JSON.parse(await readFile(path, "utf8"));
}
export function parseForkControlPolicy(value: unknown): ForkControlPolicy {
  const p = value as Partial<ForkControlPolicy>;
  if (
    !p ||
    typeof p.profile !== "string" ||
    !p.profile ||
    typeof p.socket !== "string" ||
    !isAbsolute(p.socket) ||
    typeof p.secret_file !== "string" ||
    !isAbsolute(p.secret_file) ||
    typeof p.worker_config !== "string" ||
    !isAbsolute(p.worker_config) ||
    typeof p.launchd_label !== "string" ||
    !/^com\.ploto\.multica\.updater\.[a-f0-9]+$/.test(p.launchd_label) ||
    !Number.isSafeInteger(p.request_limit_bytes) ||
    p.request_limit_bytes! <= 0 ||
    !Number.isSafeInteger(p.request_timeout_ms) ||
    p.request_timeout_ms! <= 0
  )
    throw new Error("Invalid coordinated update policy");
  return p as ForkControlPolicy;
}
async function removeStaleSocket(path: string): Promise<void> {
  try {
    const info = await lstat(path);
    if (!info.isSocket())
      throw new Error("Fork update socket path is occupied");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return;
    throw error;
  }
  await new Promise<void>((resolve, reject) => {
    const client = createConnection(path);
    client.once("connect", () => {
      client.destroy();
      reject(new Error("Fork update control socket is already active"));
    });
    client.once("error", (error) => {
      if (
        ["ECONNREFUSED", "ENOENT"].includes(
          (error as NodeJS.ErrnoException).code ?? "",
        )
      )
        resolve();
      else reject(error);
    });
  });
  await unlink(path).catch((error) => {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  });
}
export async function createForkControlServer(
  policy: ForkControlPolicy,
  prepare: (profile: string, lease: string, digest: string) => Promise<void>,
  quit: () => void,
): Promise<Server> {
  const secretInfo = await lstat(policy.secret_file);
  if (!secretInfo.isFile() || (secretInfo.mode & 0o077) !== 0)
    throw new Error("Fork update secret must be private");
  const secret = (await readFile(policy.secret_file, "utf8")).trim();
  if (!/^[a-f0-9]{64}$/.test(secret))
    throw new Error("Invalid Fork update secret");
  await removeStaleSocket(policy.socket);
  const server = createServer((socket) => {
    let data = Buffer.alloc(0),
      handled = false;
    socket.setTimeout(policy.request_timeout_ms, () => socket.destroy());
    socket.on("error", () => {
      socket.destroy();
    });
    socket.on("data", (chunk) => {
      if (handled) return;
      data = Buffer.concat([
        data,
        typeof chunk === "string" ? Buffer.from(chunk) : chunk,
      ]);
      if (data.length > policy.request_limit_bytes) {
        handled = true;
        socket.destroy();
        return;
      }
      const end = data.indexOf(10);
      if (end < 0) return;
      handled = true;
      void (async () => {
        const req = JSON.parse(
          data.subarray(0, end).toString("utf8"),
        ) as Record<string, unknown>;
        if (
          Object.keys(req).some(
            (key) => !["secret", "action", "lease", "digest"].includes(key),
          ) ||
          req.action !== "quit-for-update" ||
          typeof req.secret !== "string" ||
          !/^[a-f0-9]{64}$/.test(req.secret) ||
          !timingSafeEqual(Buffer.from(req.secret), Buffer.from(secret)) ||
          typeof req.lease !== "string" ||
          !/^[a-f0-9-]{36}$/.test(req.lease) ||
          typeof req.digest !== "string" ||
          !/^[a-f0-9]{64}$/.test(req.digest)
        )
          throw new Error("Unauthorized update request");
        await prepare(policy.profile, req.lease, req.digest);
        socket.end(JSON.stringify({ ok: true }) + "\n", quit);
      })().catch(() => socket.end(JSON.stringify({ ok: false }) + "\n"));
    });
  });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(policy.socket, () => {
      server.off("error", reject);
      resolve();
    });
  });
  await chmod(policy.socket, 0o600);
  const identity = await lstat(policy.socket);
  server.on("close", () => {
    void lstat(policy.socket)
      .then((info) => {
        if (info.ino === identity.ino && info.dev === identity.dev) {
          return unlink(policy.socket);
        }
        return undefined;
      })
      .catch(() => {});
  });
  server.unref();
  return server;
}
