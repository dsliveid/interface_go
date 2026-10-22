// 轻量 Markdown 渲染器：无任何外部依赖，适配隔离网环境。
// 支持：标题、粗斜体、删除线、行内代码、代码块、有序/无序列表、
// 引用、表格、分割线、链接、图片。

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

export function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ESC[c]);
}

function inline(text) {
  const codes = [];
  let out = text.replace(/`([^`]+)`/g, (_, code) => {
    codes.push(code);
    return '\u0000C' + (codes.length - 1) + '\u0000';
  });

  out = out.replace(/!\[([^\]]*)\]\(([^)\s]+)\)/g, (_, alt, src) => {
    if (!/^(https?:\/\/|data:image\/)/i.test(src)) return alt;
    return '<img src="' + src + '" alt="' + alt + '">';
  });

  out = out.replace(/\[([^\]]*)\]\(([^)\s]+)\)/g, (_, txt, href) => {
    const safe = /^(https?:\/\/|mailto:|#|\/)/i.test(href) ? href : '#';
    return '<a href="' + safe + '" target="_blank" rel="noreferrer">' + txt + '</a>';
  });

  out = out.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  out = out.replace(/(^|[^*\w])\*([^*\n]+)\*/g, '$1<em>$2</em>');
  out = out.replace(/~~([^~]+)~~/g, '<del>$1</del>');

  return out.replace(/\u0000C(\d+)\u0000/g, (_, i) => '<code>' + codes[Number(i)] + '</code>');
}

function splitRow(row) {
  let s = row.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|')) s = s.slice(0, -1);
  return s.split('|').map((c) => c.trim());
}

const RE_LIST = /^\s*([-*+]|\d+\.)\s+/;
const RE_HEADING = /^(#{1,6})\s+(.*)$/;
// 注意：源码先经过 HTML 转义，">" 已成为 "&gt;"，引用块需同时兼容两种写法
const RE_QUOTE = /^\s*(?:&gt;|>)\s?/;
const RE_DIVIDER = /^\s*(-{3,}|\*{3,}|_{3,})\s*$/;

export function renderMarkdown(src) {
  if (!src || !String(src).trim()) {
    return '<p class="md-empty">暂无文档。可在左侧用 Markdown 记录接口说明、字段含义、示例报文等。</p>';
  }
  const lines = escapeHtml(src).replace(/\r\n/g, '\n').split('\n');
  const out = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // 代码块
    if (/^\s*```/.test(line)) {
      const lang = line.replace(/^\s*```/, '').trim();
      const buf = [];
      i++;
      while (i < lines.length && !/^\s*```/.test(lines[i])) {
        buf.push(lines[i]);
        i++;
      }
      i++;
      out.push('<pre><code class="lang-' + lang + '">' + buf.join('\n') + '</code></pre>');
      continue;
    }

    if (!line.trim()) {
      i++;
      continue;
    }

    if (RE_DIVIDER.test(line)) {
      out.push('<hr>');
      i++;
      continue;
    }

    const h = line.match(RE_HEADING);
    if (h) {
      const lv = h[1].length;
      out.push('<h' + lv + '>' + inline(h[2].trim()) + '</h' + lv + '>');
      i++;
      continue;
    }

    if (RE_QUOTE.test(line)) {
      const buf = [];
      while (i < lines.length && RE_QUOTE.test(lines[i])) {
        buf.push(inline(lines[i].replace(RE_QUOTE, '')));
        i++;
      }
      out.push('<blockquote>' + buf.join('<br>') + '</blockquote>');
      continue;
    }

    // 表格
    if (line.includes('|') && i + 1 < lines.length && /^\s*\|?[\s:|-]*-[\s:|-]*\|/.test(lines[i + 1])) {
      const head = splitRow(line);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes('|') && lines[i].trim()) {
        rows.push(splitRow(lines[i]));
        i++;
      }
      let html = '<table><thead><tr>';
      head.forEach((c) => { html += '<th>' + inline(c) + '</th>'; });
      html += '</tr></thead><tbody>';
      rows.forEach((r) => {
        html += '<tr>';
        for (let c = 0; c < head.length; c++) html += '<td>' + inline(r[c] || '') + '</td>';
        html += '</tr>';
      });
      html += '</tbody></table>';
      out.push(html);
      continue;
    }

    // 列表
    if (RE_LIST.test(line)) {
      const ordered = /^\s*\d+\./.test(line);
      const items = [];
      while (i < lines.length && RE_LIST.test(lines[i])) {
        items.push(inline(lines[i].replace(RE_LIST, '')));
        i++;
      }
      const tag = ordered ? 'ol' : 'ul';
      out.push('<' + tag + '>' + items.map((t) => '<li>' + t + '</li>').join('') + '</' + tag + '>');
      continue;
    }

    // 段落
    const buf = [];
    while (
      i < lines.length &&
      lines[i].trim() &&
      !/^\s*```/.test(lines[i]) &&
      !RE_HEADING.test(lines[i]) &&
      !RE_QUOTE.test(lines[i]) &&
      !RE_LIST.test(lines[i]) &&
      !RE_DIVIDER.test(lines[i])
    ) {
      buf.push(inline(lines[i]));
      i++;
    }
    out.push('<p>' + buf.join('<br>') + '</p>');
  }

  return out.join('\n');
}
