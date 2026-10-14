// Builds the Go server (../server) for this machine and puts it where Tauri bundles it:
// src-tauri/binaries/photo-backup-<target triple>[.exe] (Tauri's "sidecar" naming).
import { execFileSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const triple = execFileSync("rustc", ["-vV"], { encoding: "utf8" })
  .split("\n").find((l) => l.startsWith("host:")).slice(5).trim();
const ext = process.platform === "win32" ? ".exe" : "";
const outDir = join(here, "..", "src-tauri", "binaries");
mkdirSync(outDir, { recursive: true });
const out = join(outDir, `photo-backup-${triple}${ext}`);

// cgo on macOS for native Bonjour; plain builds elsewhere.
const env = { ...process.env, CGO_ENABLED: process.platform === "darwin" ? "1" : "0" };
execFileSync("go", ["build", "-trimpath", "-ldflags", "-s -w", "-o", out, "."], {
  cwd: join(here, "..", "..", "server"), env, stdio: "inherit",
});
console.log(`server built: ${out}`);
