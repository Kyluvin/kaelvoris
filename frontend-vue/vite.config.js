import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

// Wails 桌面端前端构建配置：
//   - build.outDir 指向 ../frontend/dist（Wails //go:embed 路径，保持不变）
//   - base: "./" 相对路径（Wails asset server 直挂 dist 根，不支持绝对路径）
//   - 桥接：window.go.main.App 由 Wails 运行时注入；缺失时前端走 mock 桩（浏览器预览）
export default defineConfig({
  base: "./",
  plugins: [vue()],
  build: {
    outDir: "../frontend/dist",
    emptyOutDir: true, // 每次 build 清空 dist，防止 hash 产物跨构建堆积（曾致 94 个残留 js/css 全量打入 exe，+109MB）
    target: "chrome110",
    cssCodeSplit: false,
  },
  server: {
    port: 5175,
    strictPort: true,
  },
});
