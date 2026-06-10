// Zero-dependency static-site generator for the ensemble marketing site.
//
//   node build.mjs            → renders ./dist (a fully self-contained static site)
//
// Output is plain HTML/CSS/woff2/png with RELATIVE paths, so ./dist can be served
// from any "dumb" web server (or opened from file://) with nothing else required.
// Edit content.mjs for the words; edit src/assets/styles.css for the look.

import { content as C } from "./content.mjs";
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.dirname(fileURLToPath(import.meta.url));
const SRC = path.join(root, "src");
const OUT = path.join(root, "dist");

const esc = (s = "") =>
  String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

const eq = (n = 7) =>
  `<span class="eq" aria-hidden="true">${Array.from({ length: n }, (_, i) => `<i style="--i:${i}"></i>`).join("")}</span>`;

const navLinks = C.nav
  .map((l) => `<a href="${esc(l.href)}"${l.href.startsWith("#") ? "" : ' rel="noopener"'}>${esc(l.label)}</a>`)
  .join("");

const features = C.why.features
  .map(
    (f) => `
      <article class="feat">
        <div class="feat-top"><span class="feat-n">${esc(f.n)}</span><span class="tag">${esc(f.tag)}</span></div>
        <h3>${esc(f.title)}</h3>
        <p>${esc(f.body)}</p>
      </article>`
  )
  .join("");

const screens = C.screens.items
  .map(
    (s, i) => `
      <article class="screen${i % 2 ? " flip" : ""}">
        <figure class="screen-shot">
          <img src="${esc(s.src)}" alt="${esc(s.alt)}" loading="lazy" decoding="async" />
        </figure>
        <div class="screen-copy">
          <span class="kicker">${eq(5)}${esc(s.kicker)}</span>
          <h3>${esc(s.title)}</h3>
          <p>${esc(s.body)}</p>
        </div>
      </article>`
  )
  .join("");

const steps = C.how.steps
  .map(
    (s) => `
      <li class="step">
        <span class="step-n">${esc(s.n)}</span>
        <h3>${esc(s.title)}</h3>
        <p>${esc(s.body)}</p>
      </li>`
  )
  .join("");

const qsSteps = C.quickstart.steps
  .map((s) => {
    const code = s.code
      ? `<pre class="qs-code"><code>${esc(s.code)}</code></pre>`
      : "";
    const params = s.params
      ? `<div class="qs-params">${s.params
          .map(
            (p) =>
              `<code class="qs-flag">${esc(p.flag)}</code><span>${esc(p.what)} <em>· default ${esc(p.def)}</em></span>`
          )
          .join("")}</div>`
      : "";
    const methods = s.methods
      ? `<div class="qs-methods">${s.methods
          .map((m) => `<span class="qs-mlabel">${esc(m.label)}</span><code>${esc(m.cmd)}</code>`)
          .join("")}</div>`
      : "";
    const action = s.action
      ? `<a class="btn btn-solid qs-dl" href="${esc(s.action.href)}" rel="noopener">${esc(s.action.label)}<span class="arrow">→</span></a>`
      : "";
    const doc = s.doc
      ? `<a class="qs-doc" href="${esc(s.doc.href)}" rel="noopener">for further details — ${esc(s.doc.label)}<span class="arrow">→</span></a>`
      : "";
    return `
      <article class="qs">
        <div class="qs-top">
          <div class="qs-h"><span class="qs-n">${esc(s.n)}</span><h3>${esc(s.title)}</h3></div>
          <span class="tag">${esc(s.tag)}</span>
        </div>
        <p class="qs-body">${esc(s.body)}</p>
        ${code}${params}${methods}
        <div class="qs-foot">${action}${doc}</div>
      </article>`;
  })
  .join("");

const techItems = C.tech.items
  .map(
    (t) => `
      <article class="tech">
        <span class="tag">${esc(t.tag)}</span>
        <p class="tech-problem">${esc(t.problem)}</p>
        <p class="tech-solution"><span class="tech-arrow" aria-hidden="true">→</span>${esc(t.solution)}</p>
      </article>`
  )
  .join("");

const footLinks = C.footer.links
  .map((l) => `<a href="${esc(l.href)}" rel="noopener">${esc(l.label)}</a>`)
  .join("");

const page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>${esc(C.meta.title)}</title>
<meta name="description" content="${esc(C.meta.description)}" />
<meta name="theme-color" content="${esc(C.meta.themeColor)}" />
<meta property="og:title" content="${esc(C.meta.title)}" />
<meta property="og:description" content="${esc(C.meta.description)}" />
<meta property="og:type" content="website" />
<meta property="og:image" content="assets/img/overview.png" />
<link rel="preload" href="assets/fonts/fraunces-wght.woff2" as="font" type="font/woff2" crossorigin />
<link rel="preload" href="assets/fonts/plex-sans-400.woff2" as="font" type="font/woff2" crossorigin />
<link rel="stylesheet" href="assets/styles.css" />
</head>
<body>
<div class="grain" aria-hidden="true"></div>

<header class="nav">
  <a class="brand" href="#top">${esc(C.brand.name)}<span class="brand-dot"></span></a>
  <nav class="nav-links">${navLinks}</nav>
  <a class="btn btn-ghost nav-cta" href="${esc(C.hero.primary.href)}" rel="noopener">${esc(C.hero.primary.label)}</a>
</header>

<main id="top">
  <section class="hero">
    <div class="hero-copy">
      <span class="eyebrow">${eq(6)}${esc(C.hero.eyebrow)}</span>
      <h1>${C.hero.title.map((t) => `<span>${esc(t)}</span>`).join("")}</h1>
      <p class="lede">${esc(C.hero.lede)}</p>
      <div class="actions">
        <a class="btn btn-solid" href="${esc(C.hero.primary.href)}" rel="noopener">${esc(C.hero.primary.label)}<span class="arrow">→</span></a>
        <a class="btn btn-ghost" href="${esc(C.hero.secondary.href)}" rel="noopener">${esc(C.hero.secondary.label)}</a>
      </div>
      <div class="snippet">
        <code><span class="prompt">$</span> ${esc(C.hero.snippet.cmd)}</code>
        <span class="snippet-cap">${esc(C.hero.snippet.caption)}</span>
      </div>
    </div>
    <figure class="hero-shot">
      <div class="frame"><img src="${esc(C.hero.shot.src)}" alt="${esc(C.hero.shot.alt)}" /></div>
    </figure>
  </section>

  <section id="why" class="why">
    <header class="sec-head">
      <span class="eyebrow">${esc(C.why.eyebrow)}</span>
      <h2>${esc(C.why.title)}</h2>
    </header>
    <div class="feat-grid">${features}</div>
  </section>

  <section id="screens" class="screens">
    <header class="sec-head">
      <span class="eyebrow">${esc(C.screens.eyebrow)}</span>
      <h2>${esc(C.screens.title)}</h2>
    </header>
    <div class="screen-list">${screens}</div>
  </section>

  <section id="how" class="how">
    <header class="sec-head">
      <span class="eyebrow">${esc(C.how.eyebrow)}</span>
      <h2>${esc(C.how.title)}</h2>
    </header>
    <ol class="steps">${steps}</ol>
  </section>

  <section id="quickstart" class="quickstart">
    <header class="sec-head">
      <span class="eyebrow">${esc(C.quickstart.eyebrow)}</span>
      <h2>${esc(C.quickstart.title)}</h2>
      <p class="sec-intro">${esc(C.quickstart.intro)}</p>
    </header>
    <div class="qs-list">${qsSteps}</div>
  </section>

  <section id="tech" class="tech-sec">
    <header class="sec-head">
      <span class="eyebrow">${esc(C.tech.eyebrow)}</span>
      <h2>${esc(C.tech.title)}</h2>
      <p class="sec-intro">${esc(C.tech.intro)}</p>
    </header>
    <div class="tech-grid">${techItems}</div>
  </section>

  <section class="cta">
    <h2>${esc(C.cta.title)}</h2>
    <p>${esc(C.cta.body)}</p>
    <div class="actions">
      <a class="btn btn-solid" href="${esc(C.cta.primary.href)}" rel="noopener">${esc(C.cta.primary.label)}<span class="arrow">→</span></a>
      <a class="btn btn-ghost" href="${esc(C.cta.secondary.href)}" rel="noopener">${esc(C.cta.secondary.label)}</a>
    </div>
  </section>
</main>

<footer class="foot">
  <div class="foot-brand">${esc(C.brand.name)}${eq(4)}</div>
  <p class="foot-note">${esc(C.footer.note)}</p>
  <nav class="foot-links">${footLinks}</nav>
</footer>
</body>
</html>
`;

async function copyDir(from, to) {
  await fs.mkdir(to, { recursive: true });
  for (const e of await fs.readdir(from, { withFileTypes: true })) {
    const s = path.join(from, e.name);
    const d = path.join(to, e.name);
    if (e.isDirectory()) await copyDir(s, d);
    else await fs.copyFile(s, d);
  }
}

async function main() {
  await fs.rm(OUT, { recursive: true, force: true });
  await fs.mkdir(OUT, { recursive: true });
  await copyDir(path.join(SRC, "assets"), path.join(OUT, "assets"));
  await fs.writeFile(path.join(OUT, "index.html"), page);
  console.log("built ./dist (" + (page.length / 1024).toFixed(1) + " kB html)");
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
