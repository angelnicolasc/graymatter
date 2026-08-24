// Syncs canonical documentation from the repo root into the Starlight site.
//
// The files under /docs are the single source of truth (agents and humans read
// them in the repo). This script projects them into src/content/docs/reference/
// at build time so the site always renders the latest version with zero
// duplication in git. It also copies .github/assets into public/assets.
//
// Run automatically by `npm run build` (prebuild), or manually: `npm run sync`.

import {
  readFileSync,
  writeFileSync,
  mkdirSync,
  readdirSync,
  cpSync,
  rmSync,
} from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { posix } from 'node:path';
import { fileURLToPath } from 'node:url';

const WWW = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const REPO = resolve(WWW, '..');
const DOCS = join(REPO, 'docs');
const OUT = join(WWW, 'src', 'content', 'docs', 'reference');
const PUBLIC = join(WWW, 'public');
const GITHUB_BLOB = 'https://github.com/angelnicolasc/graymatter/blob/main/';

/** Pages copied from docs/ with explicit frontmatter. */
const PAGES = [
  {
    src: 'AGENTS.md',
    out: 'agents-guide.md',
    title: 'Agent guide',
    description:
      'Operational guide for AI agents using GrayMatter as long-term memory: tool reference, patterns, hygiene.',
  },
  {
    src: 'benchmarks.md',
    out: 'benchmarks.md',
    title: 'Benchmarks',
    description:
      'Methodology and per-query results for the token-reduction and fact-relevance benchmarks.',
  },
  {
    src: 'api-stability.md',
    out: 'api-stability.md',
    title: 'API stability',
    description: 'Public API surface and the stability promises behind it.',
  },
  {
    src: 'plugin-protocol.md',
    out: 'plugin-protocol.md',
    title: 'Plugin protocol',
    description: 'How GrayMatter plugins are structured, installed, and loaded.',
  },
  {
    src: 'threat-model.md',
    out: 'threat-model.md',
    title: 'Threat model',
    description:
      'What GrayMatter defends, what it does not, and where the boundary sits.',
  },
];

/** Maps a repo-relative docs/*.md path to its site route. */
function siteSlug(repoRel) {
  const p = repoRel.replaceAll('\\', '/');
  if (p === 'docs/AGENTS.md') return '/reference/agents-guide/';
  if (p === 'docs/decisions/README.md') return '/reference/decisions/';
  if (p.startsWith('docs/decisions/'))
    return `/reference/decisions/${p.slice('docs/decisions/'.length, -'.md'.length)}/`;
  if (p.startsWith('docs/'))
    return `/reference/${p.slice('docs/'.length, -'.md'.length)}/`;
  return null;
}

/** Rewrites relative .md links: internal docs links become site routes,
 *  everything else (source files, root README) links to GitHub. */
function rewriteLinks(body, srcRepoRel) {
  const srcDir = posix.dirname(srcRepoRel.replaceAll('\\', '/'));
  return body.replace(
    /\]\(([^)\s]+\.md)((#[^)\s]*)?)\)/g,
    (full, target, anchor) => {
      if (/^[a-z]+:/i.test(target) || target.startsWith('/')) return full;
      const resolved = posix.normalize(posix.join(srcDir, target));
      const slug = siteSlug(resolved);
      if (slug) return `](${slug}${anchor})`;
      return `](${GITHUB_BLOB}${resolved}${anchor})`;
    },
  );
}

/** Strips the first H1 (Starlight renders the frontmatter title instead). */
function stripFirstH1(body) {
  return body.replace(/^#\s+.+\r?\n/, '');
}

function frontmatter(title, description) {
  const esc = (s) => s.replaceAll('"', '\\"');
  return `---\ntitle: "${esc(title)}"\ndescription: "${esc(description)}"\n---\n\n`;
}

function syncPage({ src, out, title, description }) {
  const repoRel = `docs/${src}`;
  const raw = readFileSync(join(DOCS, src), 'utf8');
  const body = rewriteLinks(stripFirstH1(raw), repoRel);
  writeFileSync(join(OUT, out), frontmatter(title, description) + body);
  return out;
}

function syncDecisions() {
  const decisionsDir = join(DOCS, 'decisions');
  const outDir = join(OUT, 'decisions');
  mkdirSync(outDir, { recursive: true });
  for (const file of readdirSync(decisionsDir)) {
    if (!file.endsWith('.md')) continue;
    const repoRel = `docs/decisions/${file}`;
    const raw = readFileSync(join(decisionsDir, file), 'utf8');
    const heading = raw.match(/^#\s+(.+)\r?\n/);
    const isIndex = file === 'README.md';
    const title = isIndex
      ? 'Design decisions'
      : (heading?.[1] ?? file.replace(/\.md$/, ''));
    const description = isIndex
      ? 'Architecture decision records: tradeoffs written down rather than left as folklore.'
      : `Architecture decision record: ${title.replace(/^\d+\s*[—–-]\s*/, '')}`;
    const outName = isIndex ? 'index.md' : file;
    const body = rewriteLinks(stripFirstH1(raw), repoRel);
    writeFileSync(join(outDir, outName), frontmatter(title, description) + body);
  }
}

function syncAssets() {
  const assets = join(REPO, '.github', 'assets');
  const out = join(PUBLIC, 'assets');
  mkdirSync(out, { recursive: true });
  for (const file of readdirSync(assets)) {
    if (/\.(png|jpe?g|svg|webp)$/i.test(file)) {
      cpSync(join(assets, file), join(out, file));
    }
  }
}

rmSync(OUT, { recursive: true, force: true });
mkdirSync(OUT, { recursive: true });
for (const page of PAGES) syncPage(page);
syncDecisions();
syncAssets();
console.log('sync-docs: reference pages and assets synced.');
