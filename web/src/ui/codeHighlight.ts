// Prism 只做语法分词（highlight 输出前会转义源码），着色由
// MarkdownMessage.css 的 token 主题按品牌色板完成。
// 语言清单按仓库常见内容精选，控制包体；未覆盖语言按纯文本渲染。
import Prism from "prismjs";
import "prismjs/components/prism-go";
import "prismjs/components/prism-typescript";
import "prismjs/components/prism-bash";
import "prismjs/components/prism-json";
import "prismjs/components/prism-python";
import "prismjs/components/prism-rust";
import "prismjs/components/prism-sql";
import "prismjs/components/prism-yaml";
import "prismjs/components/prism-toml";
import "prismjs/components/prism-diff";

const aliases: Record<string, string> = {
  sh: "bash",
  shell: "bash",
  zsh: "bash",
  yml: "yaml",
  ts: "typescript",
  js: "javascript",
  jsx: "javascript",
  tsx: "typescript",
  py: "python",
  golang: "go",
  md: "markdown"
};

function escapeHtml(value: string): string {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

export function highlightCode(code: string, language: string): string {
  const name = aliases[language] ?? language;
  const grammar = Prism.languages[name];
  if (!grammar) return escapeHtml(code);
  return Prism.highlight(code, grammar, name);
}
