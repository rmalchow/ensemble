// Zero-dependency static-site generator for the ensemble marketing site.
//
//   node build.mjs            → renders ./dist (a fully self-contained static site)
//
// Output is plain HTML/CSS/woff2/png with RELATIVE paths, so ./dist can be served
// from any "dumb" web server (or opened from file://) with nothing else required.
// Edit content.mjs for the words; edit src/assets/styles.css for the look.

import { content as C } from "./content.mjs";
import { promises as fs } from "node:fs";
import { createHash } from "node:crypto";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.dirname(fileURLToPath(import.meta.url));
const SRC = path.join(root, "src");
const OUT = path.join(root, "dist");
const VERSION = process.env.ENSEMBLE_VERSION || "";

const esc = (s = "") =>
  String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

const eq = (n = 7) =>
  `<span class="eq" aria-hidden="true">${Array.from({ length: n }, (_, i) => `<i style="--i:${i}"></i>`).join("")}</span>`;

// Nav links, with in-page anchors prefixed so they also work from a sub-page
// (download.html → index.html#why). prefix is "" on the home page.
// GitHub mark, sized to sit inline with the nav text (fills currentColor).
const GITHUB_ICON =
  '<svg class="gh-mark" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" fill="currentColor"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.6 7.6 0 012-.27c.68 0 1.36.09 2 .27 1.53-1.03 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>';

const renderNav = (prefix = "") =>
  C.nav
    .map((l) => {
      const href = l.href.startsWith("#") ? prefix + l.href : l.href;
      const rel = l.href.startsWith("#") ? "" : ' rel="noopener"';
      const icon = l.icon === "github" ? GITHUB_ICON : "";
      const cls = icon ? ' class="nav-gh"' : "";
      return `<a${cls} href="${esc(href)}"${rel}>${icon}${esc(l.label)}</a>`;
    })
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
          <img class="lb-thumb" data-lb="${i}" role="button" tabindex="0" aria-label="Open “${esc(s.title)}” full size" src="${esc(s.src)}" alt="${esc(s.alt)}" loading="lazy" decoding="async" />
        </figure>
        <div class="screen-copy">
          <span class="kicker">${eq(5)}${esc(s.kicker)}</span>
          <h3>${esc(s.title)}</h3>
          <p>${esc(s.body)}</p>
        </div>
      </article>`
  )
  .join("");

// Lightbox carousel — the screenshot gallery followed by the measured-coherence
// graphs, in order. data-lb indices on the thumbs are global across both sets, so
// the proof graphs open at C.screens.items.length + their own index. The graphs are
// wide (not portrait phone shots), so they carry a `wide` flag for a larger cap.
const lbItems = [
  ...C.screens.items.map((s) => ({ src: s.src, alt: s.alt, cap: `${s.kicker} — ${s.title}` })),
  ...C.proof.items.map((s) => ({ src: s.src, alt: s.alt, cap: `${s.kicker} — ${s.title}`, wide: true })),
];

const lbSlides = lbItems
  .map(
    (s) => `
      <figure class="lb-slide${s.wide ? " wide" : ""}" data-cap="${esc(s.cap)}">
        <img src="${esc(s.src)}" alt="${esc(s.alt)}" loading="lazy" decoding="async" />
      </figure>`
  )
  .join("");

const lbDots = lbItems
  .map(
    (s, i) =>
      `<button class="lb-dot" type="button" data-i="${i}" aria-label="View “${esc(s.cap)}”"></button>`
  )
  .join("");

// The hero reuses one of the gallery shots; open the lightbox at its slide.
const heroLbIdx = Math.max(0, C.screens.items.findIndex((s) => s.src === C.hero.shot.src));

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

// Measured-coherence proof: a branded graph (bare PNG from tools/calib/) with the
// headline + honest judgement set in the brand font. Alternates side like screens.
const proofLbBase = C.screens.items.length;
const proof = C.proof.items
  .map(
    (s, i) => `
      <article class="proof-item${i % 2 ? " flip" : ""}">
        <figure class="proof-shot">
          <img class="lb-thumb" data-lb="${proofLbBase + i}" role="button" tabindex="0" aria-label="Open “${esc(s.title)}” full size" src="${esc(s.src)}" alt="${esc(s.alt)}" loading="lazy" decoding="async" />
        </figure>
        <div class="proof-copy">
          <span class="kicker">${eq(5)}${esc(s.kicker)}</span>
          ${s.metric ? `<span class="proof-metric">${esc(s.metric)}</span>` : ""}
          <h3>${esc(s.title)}</h3>
          <p>${esc(s.body)}</p>
        </div>
      </article>`
  )
  .join("");

const testimonials = C.testimonials.items
  .map(
    (t) => `
      <figure class="quote">
        <img class="quote-photo" src="${esc(t.img)}" alt="${esc(t.name)}" loading="lazy" decoding="async" width="72" height="72" />
        <blockquote>“${esc(t.quote)}”</blockquote>
        <figcaption><span class="quote-name">${esc(t.name)}</span><span class="quote-role">${esc(t.role)}</span></figcaption>
      </figure>`
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
  <nav class="nav-links">${renderNav("")}</nav>
  <a class="btn btn-solid nav-cta" href="${esc(C.hero.primary.href)}" rel="noopener">${esc(C.hero.primary.label)}</a>
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
      <div class="frame"><img class="lb-thumb" data-lb="${heroLbIdx}" role="button" tabindex="0" aria-label="Open screenshots full size" src="${esc(C.hero.shot.src)}" alt="${esc(C.hero.shot.alt)}" /></div>
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

  <section id="proof" class="proof">
    <header class="sec-head">
      <span class="eyebrow">${esc(C.proof.eyebrow)}</span>
      <h2>${esc(C.proof.title)}</h2>
      <p class="sec-intro">${esc(C.proof.intro)}</p>
    </header>
    <div class="proof-list">${proof}</div>
  </section>

  <section id="praise" class="praise">
    <header class="sec-head">
      <span class="eyebrow">${esc(C.testimonials.eyebrow)}</span>
      <h2>${esc(C.testimonials.title)}</h2>
      <p class="sec-intro">${esc(C.testimonials.note)}</p>
    </header>
    <div class="quote-grid">${testimonials}</div>
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

<div class="lightbox" id="lightbox" role="dialog" aria-modal="true" aria-label="Screenshots" hidden>
  <button class="lb-btn lb-close" type="button" aria-label="Close (Esc)">✕</button>
  <button class="lb-btn lb-nav lb-prev" type="button" aria-label="Previous">‹</button>
  <button class="lb-btn lb-nav lb-next" type="button" aria-label="Next">›</button>
  <div class="lb-track">${lbSlides}</div>
  <p class="lb-cap" aria-live="polite"></p>
  <div class="lb-dots">${lbDots}</div>
</div>

<script>
(function () {
  var lb = document.getElementById("lightbox");
  if (!lb) return;
  var track = lb.querySelector(".lb-track");
  var slides = [].slice.call(lb.querySelectorAll(".lb-slide"));
  var dots = [].slice.call(lb.querySelectorAll(".lb-dot"));
  var capEl = lb.querySelector(".lb-cap");
  var caps = slides.map(function (s) { return s.getAttribute("data-cap") || ""; });
  var n = slides.length, idx = 0, lastFocus = null, raf = 0;

  function render() {
    dots.forEach(function (d, k) { d.setAttribute("aria-current", k === idx ? "true" : "false"); });
    capEl.textContent = caps[idx];
  }
  function goTo(i, smooth) {
    idx = Math.max(0, Math.min(n - 1, i));
    track.scrollTo({ left: slides[idx].offsetLeft, behavior: smooth ? "smooth" : "auto" });
    render();
  }
  function syncFromScroll() {
    var i = Math.round(track.scrollLeft / track.clientWidth);
    if (i !== idx && i >= 0 && i < n) { idx = i; render(); }
  }
  function open(i) {
    lastFocus = document.activeElement;
    lb.hidden = false;
    document.documentElement.style.overflow = "hidden";
    requestAnimationFrame(function () { goTo(i, false); });
    lb.querySelector(".lb-close").focus();
  }
  function close() {
    lb.hidden = true;
    document.documentElement.style.overflow = "";
    if (lastFocus && lastFocus.focus) lastFocus.focus();
  }

  [].slice.call(document.querySelectorAll("[data-lb]")).forEach(function (el) {
    el.addEventListener("click", function (e) { e.preventDefault(); open(+el.getAttribute("data-lb") || 0); });
    el.addEventListener("keydown", function (e) {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); open(+el.getAttribute("data-lb") || 0); }
    });
  });

  track.addEventListener("scroll", function () {
    cancelAnimationFrame(raf);
    raf = requestAnimationFrame(syncFromScroll);
  }, { passive: true });

  lb.querySelector(".lb-prev").addEventListener("click", function () { goTo(idx - 1, true); });
  lb.querySelector(".lb-next").addEventListener("click", function () { goTo(idx + 1, true); });
  lb.querySelector(".lb-close").addEventListener("click", close);
  dots.forEach(function (d) {
    d.addEventListener("click", function () { goTo(+d.getAttribute("data-i") || 0, true); });
  });
  // Click outside the image (backdrop / slide padding) closes.
  lb.addEventListener("click", function (e) {
    if (e.target === lb || e.target.classList.contains("lb-slide") || e.target.classList.contains("lb-track")) close();
  });
  document.addEventListener("keydown", function (e) {
    if (lb.hidden) return;
    if (e.key === "Escape") close();
    else if (e.key === "ArrowLeft") goTo(idx - 1, true);
    else if (e.key === "ArrowRight") goTo(idx + 1, true);
  });
})();
</script>
</body>
</html>
`;

const footLinksHtml = C.footer.links
  .map((l) => `<a href="${esc(l.href)}" rel="noopener">${esc(l.label)}</a>`)
  .join("");

const fmtBytes = (n) => {
  if (!n) return "";
  const mb = n / (1024 * 1024);
  return mb >= 1 ? mb.toFixed(1) + " MB" : Math.round(n / 1024) + " KB";
};

// resolveDownloads enriches each download option that has a `file` with the
// staged binary's SHA-256 + size (computed here, so local and CI builds get the
// real hash). A missing binary renders as "not built" rather than failing — so a
// plain `node build.mjs` (no binaries staged) still produces the page.
async function resolveDownloads() {
  const out = [];
  for (const o of C.download.options) {
    if (!o.file) {
      out.push({ ...o });
      continue;
    }
    try {
      const buf = await fs.readFile(path.join(SRC, o.file));
      out.push({ ...o, present: true, size: buf.length, hash: createHash("sha256").update(buf).digest("hex") });
    } catch {
      out.push({ ...o, present: false });
    }
  }
  return out;
}

// Simplified, brand-coloured inline SVG marks (self-contained — no external
// assets). Recognisable rather than pixel-exact; ~22px, drawn on a 24px grid.
const LOGOS = {
  raspberrypi: `<svg class="dl-logo" viewBox="0 0 24 24" width="22" height="22" role="img" aria-label="Raspberry Pi">
    <g fill="#75A928"><ellipse cx="9.6" cy="6.6" rx="1.5" ry="2.9" transform="rotate(-32 9.6 6.6)"/><ellipse cx="14.4" cy="6.6" rx="1.5" ry="2.9" transform="rotate(32 14.4 6.6)"/></g>
    <g fill="#C7203E"><circle cx="11" cy="12" r="2"/><circle cx="14.4" cy="12.3" r="2"/><circle cx="9.2" cy="14.6" r="2"/><circle cx="12.6" cy="14.8" r="2"/><circle cx="15.6" cy="15" r="1.8"/><circle cx="11" cy="17.4" r="1.9"/><circle cx="14" cy="17.6" r="1.7"/></g>
  </svg>`,
  docker: `<svg class="dl-logo" viewBox="0 0 24 24" width="22" height="22" role="img" aria-label="Docker" fill="#2496ED">
    <g><rect x="6" y="9.4" width="2.4" height="2.4"/><rect x="8.8" y="9.4" width="2.4" height="2.4"/><rect x="11.6" y="9.4" width="2.4" height="2.4"/><rect x="8.8" y="6.6" width="2.4" height="2.4"/><rect x="11.6" y="6.6" width="2.4" height="2.4"/></g>
    <path d="M4 12.4h15.2c.2 1.4-.4 2.6-1.7 2.6.1-1.1-2-1.5-2.4-.4-1 1.9-3.4 3.1-6.3 3.1C5.6 17.7 4 15.4 4 12.4z"/>
  </svg>`,
  fedora: `<svg class="dl-logo" viewBox="0 0 24 24" width="22" height="22" role="img" aria-label="Fedora">
    <circle cx="12" cy="12" r="10" fill="#51A2DA"/>
    <path d="M13.7 6.6a3.2 3.2 0 0 0-3.2 3.2v1.4H8.8v2.3h1.7v4h2.4v-4h2v-2.3h-2v-1.4c0-.5.4-.9.9-.9h1.7V6.6z" fill="#fff"/>
  </svg>`,
  ubuntu: `<svg class="dl-logo" viewBox="0 0 24 24" width="22" height="22" role="img" aria-label="Ubuntu">
    <circle cx="12" cy="12" r="5.4" fill="none" stroke="#E95420" stroke-width="1.7"/>
    <g fill="#E95420"><circle cx="12" cy="5.3" r="1.8"/><circle cx="6.2" cy="15.3" r="1.8"/><circle cx="17.8" cy="15.3" r="1.8"/></g>
  </svg>`,
  debian: `<svg class="dl-logo" viewBox="0 0 24 24" width="22" height="22" role="img" aria-label="Debian" fill="none" stroke="#A80030" stroke-width="1.7" stroke-linecap="round">
    <path d="M15.6 8a5.2 5.2 0 1 0 1.5 4.7"/>
  </svg>`,
  arch: `<svg class="dl-logo" viewBox="0 0 24 24" width="22" height="22" role="img" aria-label="Arch Linux" fill="#1793D1">
    <path d="M12 3.5 4.5 20l7.5-3.6L19.5 20z"/>
  </svg>`,
  manjaro: `<svg class="dl-logo" viewBox="0 0 24 24" width="22" height="22" role="img" aria-label="Manjaro" fill="#35BF5C">
    <rect x="4" y="4" width="5" height="16" rx="1"/><rect x="10.5" y="9.5" width="5" height="10.5" rx="1"/><rect x="17" y="4" width="3" height="16" rx="1"/>
  </svg>`,
};

const logosHtml = (keys = []) => {
  const svgs = keys.map((k) => LOGOS[k]).filter(Boolean).join("");
  return svgs ? `<div class="dl-logos">${svgs}</div>` : "";
};

function downloadCard(o) {
  const head = `
        <div class="dl-card-head">
          <div class="dl-card-head-l">
            <h3>${esc(o.name)}</h3>
            <p class="dl-rec">${esc(o.rec)}</p>
          </div>
          <div class="dl-card-meta">
            <span class="tag">${esc(o.arch)}</span>
            ${logosHtml(o.logos)}
          </div>
        </div>`;
  // o.note is trusted author HTML from content.mjs (may carry <code>/<strong>),
  // so it is intentionally not run through esc(). Rendered full-width under head.
  const note = o.note ? `\n        <p class="dl-note">${o.note}</p>` : "";
  if (o.docker) {
    return `
      <article class="dl-card">${head}${note}
        <div class="dl-cmd">
          <code>${esc(o.docker)}</code>
          <button class="dl-copy" type="button" data-copy="${esc(o.docker)}" aria-label="Copy command">copy</button>
        </div>
      </article>`;
  }
  const fname = o.file.split("/").pop();
  if (!o.present) {
    return `
      <article class="dl-card">${head}${note}
        <p class="dl-missing">Binary not staged — run <code>site/build.sh</code> (or build from a tagged CI pipeline).</p>
      </article>`;
  }
  return `
      <article class="dl-card">${head}${note}
        <div class="dl-card-foot">
          <div class="dl-file">
            <span class="dl-fname"><code>${esc(fname)}</code> <span class="dl-size">${esc(fmtBytes(o.size))}</span></span>
            <span class="dl-sha"><span class="dl-sha-label">SHA-256</span><code>${esc(o.hash)}</code></span>
          </div>
          <a class="btn btn-solid dl-dl" href="${esc(o.file)}" download>Download<span class="arrow">↓</span></a>
        </div>
      </article>`;
}

function downloadPage(options) {
  const cards = options.map(downloadCard).join("");
  const links = C.download.links
    .map(
      (l) => `
      <div class="dl-link-row">
        <p class="dl-link-desc">${esc(l.desc)}</p>
        <a class="btn btn-ghost dl-link-btn" href="${esc(l.href)}" rel="noopener">${esc(l.label)}<span class="arrow">→</span></a>
      </div>`
    )
    .join("");
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>Download — ${esc(C.meta.title)}</title>
<meta name="description" content="Download ensemble — pure-Go binaries for Raspberry Pi (32/64-bit) and x86-64 Linux, plus the Docker image. SHA-256 for every build." />
<meta name="theme-color" content="${esc(C.meta.themeColor)}" />
<link rel="preload" href="assets/fonts/fraunces-wght.woff2" as="font" type="font/woff2" crossorigin />
<link rel="preload" href="assets/fonts/plex-sans-400.woff2" as="font" type="font/woff2" crossorigin />
<link rel="stylesheet" href="assets/styles.css" />
</head>
<body>
<div class="grain" aria-hidden="true"></div>

<header class="nav">
  <a class="brand" href="index.html">${esc(C.brand.name)}<span class="brand-dot"></span></a>
  <nav class="nav-links">${renderNav("index.html")}</nav>
  <a class="btn btn-ghost nav-cta" href="index.html">← Home</a>
</header>

<main id="top">
  <section class="dl">
    <header class="sec-head">
      <span class="eyebrow">${eq(6)}${esc(C.download.eyebrow)}${VERSION ? " · " + esc(VERSION) : ""}</span>
      <h1>${esc(C.download.title)}</h1>
      <p class="sec-intro">${esc(C.download.intro)}</p>
    </header>
    <div class="dl-list">${cards}</div>
    <div class="dl-links">${links}</div>
  </section>
</main>

<footer class="foot">
  <div class="foot-brand">${esc(C.brand.name)}${eq(4)}</div>
  <p class="foot-note">${esc(C.footer.note)}</p>
  <nav class="foot-links">${footLinksHtml}</nav>
</footer>

<script>
(function () {
  document.querySelectorAll(".dl-copy").forEach(function (b) {
    b.addEventListener("click", function () {
      var t = b.getAttribute("data-copy") || "";
      (navigator.clipboard ? navigator.clipboard.writeText(t) : Promise.reject()).then(
        function () { var o = b.textContent; b.textContent = "copied"; setTimeout(function () { b.textContent = o; }, 1200); },
        function () {}
      );
    });
  });
})();
</script>
</body>
</html>
`;
}

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

  const downloads = await resolveDownloads();
  const dl = downloadPage(downloads);
  await fs.writeFile(path.join(OUT, "download.html"), dl);

  // Serve the installer at /get.sh (the "curl … | sudo bash" one-liner). Source of
  // truth is ../scripts/get.sh; the CI docker-site job stages a copy to site/get.sh
  // for the Docker build context.
  let getSh = false;
  for (const p of [path.join(root, "..", "scripts", "get.sh"), path.join(root, "get.sh")]) {
    try {
      await fs.writeFile(path.join(OUT, "get.sh"), await fs.readFile(p), { mode: 0o755 });
      getSh = true;
      break;
    } catch {
      // not here; try the next location
    }
  }

  const staged = downloads.filter((o) => o.present).length;
  const total = downloads.filter((o) => o.file).length;
  console.log(
    `built ./dist (index ${(page.length / 1024).toFixed(1)} kB, download ${(dl.length / 1024).toFixed(1)} kB; ${staged}/${total} binaries staged; get.sh ${getSh ? "✓" : "—"})`
  );
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
