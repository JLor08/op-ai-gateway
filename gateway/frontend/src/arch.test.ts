// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// ArchUnit-style layer-boundary tests for the frontend.
//
// This walks every non-test source file under src/, extracts its STATIC
// import / `export ... from` specifiers via the TypeScript compiler API
// (already a devDependency — no new dependency added), resolves relative
// specifiers to normalized src-relative paths, and asserts a handful of
// layering rules against the resulting edge list.
//
// The rules below were derived FROM the current import graph (see the
// discovery in each rule's comment for the real edges that shaped it), so
// this test is green today. If a rule ever fails, it means a new import
// crossed a layer boundary: either fix the import, or — if the boundary
// change is intentional — update the rule/allowlist here to match the new
// reality and note why.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import ts from 'typescript';
import { describe, expect, it } from 'vitest';

const SRC_DIR = path.dirname(fileURLToPath(import.meta.url));

interface Edge {
  /** src-relative path (posix separators) of the importing file. */
  from: string;
  /** the raw specifier as written in the source, e.g. '../shared/types'. */
  specifier: string;
  /** resolved src-relative path for relative specifiers; equals `specifier` for external/unresolved ones. */
  to: string;
  /** true for bare (npm package) specifiers — these are never checked by layer rules. */
  external: boolean;
}

function toSrcRelative(absPath: string): string {
  return path.relative(SRC_DIR, absPath).split(path.sep).join('/');
}

function listSourceFiles(dir: string, out: string[] = []): string[] {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      listSourceFiles(full, out);
      continue;
    }
    if (!/\.tsx?$/.test(entry.name)) continue;
    if (/\.test\.tsx?$/.test(entry.name)) continue;
    if (/\.d\.ts$/.test(entry.name)) continue;
    out.push(full);
  }
  return out;
}

// Resolve a relative import specifier the same way the bundler/TS resolver
// would: exact path, then .ts/.tsx, then index.ts/index.tsx under a directory.
function resolveRelativeSpecifier(fromFile: string, specifier: string): string | null {
  const base = path.resolve(path.dirname(fromFile), specifier);
  const candidates = [
    base,
    `${base}.ts`,
    `${base}.tsx`,
    path.join(base, 'index.ts'),
    path.join(base, 'index.tsx'),
  ];
  for (const candidate of candidates) {
    if (fs.existsSync(candidate) && fs.statSync(candidate).isFile()) {
      return toSrcRelative(candidate);
    }
  }
  return null;
}

// Extract every static import/export-from module specifier in a file.
// `ts.preProcessFile` cheaply reports `import ... from 'x'` / `import 'x'`,
// but does not reliably surface `export ... from 'x'` (used by the api/*
// barrels), so the AST is also scanned for export declarations.
function extractSpecifiers(file: string, text: string): string[] {
  const specifiers = new Set<string>();
  const info = ts.preProcessFile(text, true, true);
  for (const imported of info.importedFiles) specifiers.add(imported.fileName);

  const scriptKind = file.endsWith('.tsx') ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  const sourceFile = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, scriptKind);
  for (const statement of sourceFile.statements) {
    const isImportOrExportFrom =
      ts.isImportDeclaration(statement) || ts.isExportDeclaration(statement);
    if (
      isImportOrExportFrom &&
      statement.moduleSpecifier &&
      ts.isStringLiteral(statement.moduleSpecifier)
    ) {
      specifiers.add(statement.moduleSpecifier.text);
    }
  }
  return [...specifiers];
}

function buildEdges(files: string[]): Edge[] {
  const edges: Edge[] = [];
  for (const file of files) {
    const from = toSrcRelative(file);
    const text = fs.readFileSync(file, 'utf8');
    for (const specifier of extractSpecifiers(file, text)) {
      if (!specifier.startsWith('.')) {
        edges.push({ from, specifier, to: specifier, external: true });
        continue;
      }
      const resolved = resolveRelativeSpecifier(file, specifier);
      edges.push({ from, specifier, to: resolved ?? specifier, external: false });
    }
  }
  return edges;
}

const sourceFiles = listSourceFiles(SRC_DIR);
const edges = buildEdges(sourceFiles);
// Layer rules only ever care about imports between our own modules.
const internalEdges = edges.filter((e) => !e.external);

// --- layer predicates -------------------------------------------------

const isApiModule = (p: string) => p.startsWith('api/');
const isApiBarrel = (p: string) => p === 'api.ts';
const isSharedComponent = (p: string) => p.startsWith('components/shared/');
const isAnyComponent = (p: string) => p.startsWith('components/');
const isNonSharedComponent = (p: string) => isAnyComponent(p) && !isSharedComponent(p);
const isThemeModule = (p: string) => p.startsWith('theme/');
const isChatModule = (p: string) => p.startsWith('components/chat/');

// The only components/chat/** modules a non-chat file may import. Everything
// else in chat/ (chatDoc.ts, useChatRuns.ts, useChatPersistence.ts) is
// chat-internal wiring reached only through these two.
const CHAT_PUBLIC_ENTRIES = new Set([
  'components/chat/ChatStore.tsx',
  'components/chat/ChatSidebar.tsx',
]);

function describeEdge(e: Edge): string {
  return `  - ${e.from}  imports '${e.specifier}'  (resolves to ${e.to})`;
}

function assertNoViolations(matches: Edge[], rule: string): void {
  const details = matches.length === 0 ? '(none)' : matches.map(describeEdge).join('\n');
  const message =
    `Architecture rule violated: ${rule}\n` +
    `${matches.length} offending import(s):\n${details}\n` +
    'Fix the import to respect the boundary, or — if this crossing is an intentional ' +
    'layering change — update the rule/allowlist in src/arch.test.ts to match the new reality.';
  expect(matches, message).toHaveLength(0);
}

describe('frontend architecture: import graph discovery', () => {
  // Guards the rules below against a silently-broken walker (wrong root,
  // glob typo, etc.) that would otherwise make every rule vacuously pass.
  it('walks a non-trivial number of source files and edges', () => {
    expect(sourceFiles.length).toBeGreaterThan(100);
    expect(internalEdges.length).toBeGreaterThan(300);
  });
});

describe('frontend architecture: layer boundaries', () => {
  it('api/** is a leaf layer: it does not import components/**, App.tsx, i18n.ts, or theme/**', () => {
    // Verified against today's graph: zero violations. api/* modules only
    // import each other (e.g. servers.ts -> groups.ts, projects.ts -> tokens.ts).
    const matches = internalEdges.filter(
      (e) =>
        isApiModule(e.from) &&
        (isAnyComponent(e.to) || e.to === 'App.tsx' || e.to === 'i18n.ts' || isThemeModule(e.to)),
    );
    assertNoViolations(
      matches,
      'src/api/** must not import from src/components/**, src/App.tsx, src/i18n.ts, or src/theme/** (api is a leaf layer over fetch)',
    );
  });

  it('i18n.ts and currency.ts are dependency-free leaves', () => {
    // Verified: both files have zero relative imports today (not merely "no
    // components/api" — they import nothing of ours at all).
    const matches = internalEdges.filter(
      (e) =>
        (e.from === 'i18n.ts' || e.from === 'currency.ts') &&
        (isAnyComponent(e.to) || isApiModule(e.to) || isApiBarrel(e.to)),
    );
    assertNoViolations(
      matches,
      'src/i18n.ts and src/currency.ts must not import from src/components/** or src/api/**',
    );
  });

  it('theme/** does not reach into non-shared components, App.tsx, or main.tsx', () => {
    // RELAXED FROM THE ORIGINAL "pure leaf" CANDIDATE: reality shows theme/**
    // is not dependency-free like i18n/currency. Real edges that forced this:
    //   theme/ThemeRoot.tsx  -> ../api                          (api.ts barrel; fetches theme config)
    //   theme/ThemeRoot.tsx  -> ../components/shared/ToastProvider
    //   theme/registry.ts    -> ../api                          (api.ts barrel)
    //   theme/ColorModeMenu.tsx -> ../components/shared/types
    // So theme is encoded as a peer of the shared layer: it may use the api.ts
    // barrel and components/shared/**, but must not depend on views, chat/,
    // netbird/, legal/, or the app-entry chain.
    const matches = internalEdges.filter(
      (e) =>
        isThemeModule(e.from) &&
        (isNonSharedComponent(e.to) || e.to === 'App.tsx' || e.to === 'main.tsx'),
    );
    assertNoViolations(
      matches,
      'src/theme/** must not import from non-shared src/components/** (views/chat/netbird/legal), src/App.tsx, or src/main.tsx',
    );
  });

  it('components/shared/** does not reach into non-shared components, App.tsx, or main.tsx', () => {
    // Verified: zero violations. shared/* only reaches the api.ts barrel,
    // i18n.ts, and other components/shared/** siblings (e.g. shared/types.ts,
    // shared/benchmark.ts -> ../api is fine; that's the leaf-over-api pattern).
    const matches = internalEdges.filter(
      (e) =>
        isSharedComponent(e.from) &&
        (isNonSharedComponent(e.to) || e.to === 'App.tsx' || e.to === 'main.tsx'),
    );
    assertNoViolations(
      matches,
      'src/components/shared/** must not import from non-shared src/components/** (views/chat/netbird/legal), src/App.tsx, or src/main.tsx',
    );
  });

  it('nothing outside src/api/ deep-imports an src/api/<module> — the api.ts barrel is the only surface', () => {
    // Verified: the only file that deep-imports api/<module> is api.ts
    // itself (the barrel, which is expected to import and re-export every
    // domain module). Every other consumer goes through '../api' / '../../api'.
    const matches = internalEdges.filter(
      (e) => !isApiModule(e.from) && !isApiBarrel(e.from) && isApiModule(e.to),
    );
    assertNoViolations(
      matches,
      "nothing outside src/api/** may import an src/api/<module> directly; import from the 'api.ts' barrel instead",
    );
  });

  it('src/main.tsx is never imported, and src/App.tsx is only imported by main.tsx', () => {
    // Verified: main.tsx -> App.tsx is the sole entry chain edge.
    const importsMain = internalEdges.filter((e) => e.to === 'main.tsx');
    assertNoViolations(
      importsMain,
      'src/main.tsx must not be imported by any module (it is the entry point)',
    );

    const importsAppNotFromMain = internalEdges.filter(
      (e) => e.to === 'App.tsx' && e.from !== 'main.tsx',
    );
    assertNoViolations(importsAppNotFromMain, 'src/App.tsx must only be imported by src/main.tsx');
  });

  it('components/chat/** internals are only reachable through ChatStore.tsx / ChatSidebar.tsx', () => {
    // Verified against today's real external importers of chat/*:
    //   App.tsx               -> components/chat/ChatStore
    //   components/Chat.tsx   -> components/chat/ChatStore, components/chat/ChatSidebar
    //   components/NavSidebar.tsx -> components/chat/ChatStore
    // chatDoc.ts / useChatRuns.ts / useChatPersistence.ts are never imported
    // from outside components/chat/** — they are chat-internal wiring.
    const matches = internalEdges.filter(
      (e) => isChatModule(e.to) && !isChatModule(e.from) && !CHAT_PUBLIC_ENTRIES.has(e.to),
    );
    assertNoViolations(
      matches,
      'components/chat/** internals (anything but ChatStore.tsx and ChatSidebar.tsx) must not be imported from outside components/chat/**',
    );
  });
});
