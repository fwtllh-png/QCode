import {execFileSync} from "node:child_process";
import path from "node:path";
import {fileURLToPath} from "node:url";

export default function sandboxPreflight(): void {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
  execFileSync("go", ["run", "./scripts/sandbox-preflight.go"], {
    cwd: root,
    stdio: "inherit"
  });
}
