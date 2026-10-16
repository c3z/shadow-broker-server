#!/usr/bin/env python3
"""Build a static, Docusaurus-style documentation site from this repo's Markdown.

    uv run --with markdown --with pygments python3 website/build.py
    open website/build/index.html

No server, no framework: every page is a self-contained HTML file with the
sidebar, "on this page" TOC, dark mode, client-side search and copy buttons.
Links to files in the repo point at GitHub (GH_REF); links between docs stay local.
"""
import datetime
import html
import json
import posixpath
import re
import shutil
from pathlib import Path

import markdown
from markdown.extensions.toc import slugify_unicode
from pygments.formatters import HtmlFormatter

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
OUT = HERE / "build"
GH = "https://github.com/c3z/shadow-broker-server"
GH_REF = "qos-unified"

NAV = [
    ("Getting started", [
        ("index", "Introduction", "README.md"),
        ("quickstart", "Quickstart", "docs/quickstart.md"),
        ("deployment", "Deployment", "docs/deployment.md"),
    ]),
    ("Reference", [
        ("protocol", "Wire protocol", "docs/protocol.md"),
        ("architecture", "Architecture", "docs/architecture.md"),
        ("security", "Security model", "docs/security.md"),
        ("clients", "Client libraries", "docs/clients.md"),
    ]),
    ("Project", [
        ("agent-skill", "Agent skill", "skill/SKILL.md"),
        ("changelog", "Changelog", "CHANGELOG.md"),
    ]),
]
FLAT = [(slug, title, src, section) for section, items in NAV for slug, title, src in items]
BY_SRC = {src: slug for slug, _, src, _ in FLAT}


def read_source(src: str) -> str:
    text = (ROOT / src).read_text(encoding="utf-8")
    if text.startswith("---\n"):  # YAML front matter (skill)
        end = text.index("\n---\n", 4)
        meta = text[4:end]
        text = text[end + 5:]
        desc = re.search(r"^description:\s*(.+)$", meta, re.M)
        if desc:
            text = text.replace("\n", "\n\n> " + desc.group(1).strip() + "\n", 1)
    return text


def split_title(md_text: str):
    m = re.match(r"\s*# (.+?)\n", md_text)
    if not m:
        return None, md_text
    return m.group(1).strip(), md_text[m.end():]


def rewrite_links(body: str, src: str) -> str:
    base = posixpath.dirname(src)

    def fix(m):
        href = m.group(1)
        if re.match(r"^(https?:|mailto:|#)", href):
            return m.group(0)
        path, _, anchor = href.partition("#")
        target = posixpath.normpath(posixpath.join(base, path)) if path else src
        if target in BY_SRC:
            new = BY_SRC[target] + ".html" + ("#" + anchor if anchor else "")
        else:
            kind = "tree" if (ROOT / target).is_dir() else "blob"
            new = f"{GH}/{kind}/{GH_REF}/{target}"
        return f'href="{new}"'

    return re.sub(r'href="([^"]+)"', fix, body)


def section_index(slug: str, page_title: str, md_text: str):
    """Rough per-section plain text for client-side search."""
    entries = []
    current = {"page": page_title, "slug": slug, "section": page_title, "anchor": "", "text": []}
    for line in md_text.splitlines():
        h = re.match(r"^(#{2,3})\s+(.+)$", line)
        if h:
            if current["text"]:
                entries.append(current)
            title = h.group(2).strip()
            anchor = re.sub(r"[^a-z0-9]+", "-", re.sub(r"`|\*", "", title).lower()).strip("-")
            current = {"page": page_title, "slug": slug, "section": title, "anchor": anchor, "text": []}
            continue
        if line.startswith("```") or line.startswith("|---") or not line.strip():
            continue
        current["text"].append(re.sub(r"[`*_>#|\[\]]", " ", line).strip())
    if current["text"]:
        entries.append(current)
    for e in entries:
        e["text"] = re.sub(r"\s+", " ", " ".join(e["text"]))[:400]
    return entries


def toc_html(tokens) -> str:
    out = []
    for t in tokens:
        if t["level"] > 3:
            continue
        out.append(f'<li class="toc-l{t["level"]}"><a href="#{t["id"]}">{t["name"]}</a>')
        if t.get("children"):
            inner = toc_html(t["children"])
            if inner:
                out.append(f"<ul>{inner}</ul>")
        out.append("</li>")
    return "".join(out)


def sidebar_html(active: str) -> str:
    parts = []
    for section, items in NAV:
        parts.append(f'<li class="sb-section"><div class="sb-section-title">{section}</div><ul>')
        for slug, title, _ in items:
            cls = ' class="active"' if slug == active else ""
            parts.append(f'<li><a{cls} href="{slug}.html">{title}</a></li>')
        parts.append("</ul></li>")
    return "".join(parts)


def render(slug, title, src, section, index_all, prev_, next_):
    raw = read_source(src)
    h1, body_md = split_title(raw)
    page_title = h1 or title
    md = markdown.Markdown(extensions=["fenced_code", "tables", "toc", "codehilite", "sane_lists", "md_in_html"],
                           extension_configs={
                               "toc": {"toc_depth": "2-3", "permalink": "​", "permalink_class": "hash", "slugify": slugify_unicode},
                               "codehilite": {"css_class": "codeblock", "guess_lang": False, "linenums": False},
                           })
    body = md.convert(body_md)
    body = rewrite_links(body, src)
    toc = toc_html(md.toc_tokens)
    edit = f"{GH}/edit/{GH_REF}/{src}"
    pag = ""
    if prev_ or next_:
        pag = '<nav class="pagination">'
        pag += f'<a class="pag prev" href="{prev_[0]}.html"><span>Previous</span><strong>« {prev_[1]}</strong></a>' if prev_ else "<span></span>"
        pag += f'<a class="pag next" href="{next_[0]}.html"><span>Next</span><strong>{next_[1]} »</strong></a>' if next_ else "<span></span>"
        pag += "</nav>"
    return TEMPLATE.format(
        title=html.escape(page_title), nav_title=html.escape(title), section=html.escape(section), body=body, toc=toc, sidebar=sidebar_html(slug),
        edit=edit, index=json.dumps(index_all, ensure_ascii=False), pagination=pag,
        built=datetime.date.today().isoformat(), gh=GH, ref=GH_REF, src=src,
        toc_block=f'<aside class="toc"><div class="toc-title">On this page</div><ul>{toc}</ul></aside>' if toc else "",
    )


TEMPLATE = """<!doctype html>
<html lang="en" data-theme="light">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{nav_title} · Shadow Broker</title>
<meta name="description" content="Shadow Broker documentation: {section} · {title}">
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='8' fill='%235b4bdb'/%3E%3Cpath d='M9 11h14M9 16h10M9 21h14' stroke='white' stroke-width='3' stroke-linecap='round'/%3E%3C/svg%3E">
<link rel="stylesheet" href="theme.css">
<script>try{{var t=localStorage.getItem('theme');if(!t)t=matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light';document.documentElement.dataset.theme=t;}}catch(e){{}}</script>
</head>
<body>
<nav class="navbar">
  <button class="burger" aria-label="Toggle sidebar" onclick="document.body.classList.toggle('sidebar-open')">☰</button>
  <a class="brand" href="index.html"><span class="logo"></span><b>Shadow Broker</b></a>
  <a class="nav-link" href="index.html">Docs</a>
  <a class="nav-link" href="clients.html">Clients</a>
  <a class="nav-link" href="changelog.html">Changelog</a>
  <div class="spacer"></div>
  <div class="search"><input id="q" type="search" placeholder="Search docs…" autocomplete="off" aria-label="Search"><div id="results" class="results" hidden></div></div>
  <button class="theme" id="theme" aria-label="Toggle dark mode" title="Toggle dark mode"></button>
  <a class="nav-link gh" href="{gh}" target="_blank" rel="noopener">GitHub</a>
</nav>
<div class="layout">
  <aside class="sidebar"><ul class="sb">{sidebar}</ul></aside>
  <main class="main">
    <div class="doc">
      <nav class="crumbs"><a href="index.html">Docs</a> › <span>{section}</span> › <span>{title}</span></nav>
      <article class="markdown">
        <h1>{title}</h1>
        {body}
      </article>
      <footer class="doc-footer">
        <a href="{edit}" target="_blank" rel="noopener">✎ Edit this page</a>
        <span>Source: <code>{src}</code> · built {built}</span>
      </footer>
      {pagination}
    </div>
    {toc_block}
  </main>
</div>
<footer class="footer">
  <div><b>Shadow Broker</b> — a minimal, self-hostable message broker for agent-to-agent communication.</div>
  <div><a href="{gh}">GitHub</a> · <a href="{gh}/blob/{ref}/LICENSE">MIT license</a> · <a href="protocol.html">Protocol</a> · <a href="security.html">Security</a></div>
</footer>
<script id="search-index" type="application/json">{index}</script>
<script src="site.js"></script>
</body>
</html>
"""


def main():
    OUT.mkdir(exist_ok=True)
    pages = []
    index_all = []
    for slug, title, src, section in FLAT:
        raw = read_source(src)
        h1, body_md = split_title(raw)
        index_all += section_index(slug, h1 or title, body_md)
    for i, (slug, title, src, section) in enumerate(FLAT):
        prev_ = (FLAT[i - 1][0], FLAT[i - 1][1]) if i > 0 else None
        next_ = (FLAT[i + 1][0], FLAT[i + 1][1]) if i + 1 < len(FLAT) else None
        (OUT / f"{slug}.html").write_text(render(slug, title, src, section, index_all, prev_, next_), encoding="utf-8")
        pages.append(slug)
    css = (HERE / "theme.css").read_text(encoding="utf-8")
    try:
        light = HtmlFormatter(style="friendly").get_style_defs(".codeblock")
        dark = HtmlFormatter(style="github-dark").get_style_defs('[data-theme="dark"] .codeblock')
    except Exception:
        light = HtmlFormatter(style="friendly").get_style_defs(".codeblock")
        dark = HtmlFormatter(style="monokai").get_style_defs('[data-theme="dark"] .codeblock')
    (OUT / "theme.css").write_text(css + "\n/* pygments */\n" + light + "\n" + dark, encoding="utf-8")
    shutil.copy(HERE / "site.js", OUT / "site.js")
    print(f"built {len(pages)} pages → {OUT}")


if __name__ == "__main__":
    main()
