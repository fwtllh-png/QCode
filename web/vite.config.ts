import react from "@vitejs/plugin-react";
import {readdirSync, readFileSync, statSync, writeFileSync} from "node:fs";
import {join, resolve} from "node:path";
import {brotliCompressSync, constants} from "node:zlib";
import {defineConfig, type Plugin} from "vite";

export default defineConfig({
  plugins: [react(), precompress()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
    assetsDir: "assets",
    minify: "terser"
  },
  server: {
    host: "127.0.0.1",
    port: 4173
  }
});

function precompress(): Plugin {
  let outputRoot = "";
  return {
    name: "qcode-precompress",
    apply: "build",
    configResolved(config) {
      outputRoot = resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      // 只预压缩 brotli：宿主（internal/host/server.go）按 Accept-Encoding 协商，
      // 缺失对应编码时回落 identity 原始产物。再写一份 gzip 会让 webbundle 构建
      // 把同一资产内嵌三份（raw+gz+br）；gzip-only 客户端回落原始文件即可。
      for (const path of filesUnder(outputRoot)) {
        if (!path.endsWith(".js") && !path.endsWith(".css")) continue;
        const content = readFileSync(path);
        writeFileSync(`${path}.br`, brotliCompressSync(content, {
          params: {[constants.BROTLI_PARAM_QUALITY]: 11}
        }));
      }
    }
  };
}

function filesUnder(root: string): string[] {
  return readdirSync(root).flatMap((name) => {
    const path = join(root, name);
    return statSync(path).isDirectory() ? filesUnder(path) : [path];
  });
}
