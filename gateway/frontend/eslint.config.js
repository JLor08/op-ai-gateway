// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import js from '@eslint/js';
import tseslint from 'typescript-eslint';
import react from 'eslint-plugin-react';
import reactHooks from 'eslint-plugin-react-hooks';
import prettier from 'eslint-config-prettier';
import globals from 'globals';

// Google-aligned, React-friendly ESLint (flat config). Prettier owns formatting;
// `prettier` (eslint-config-prettier) is applied LAST to disable any stylistic
// rules that would fight the formatter.
export default tseslint.config(
  { ignores: ['dist/**', 'node_modules/**', 'coverage/**', '.worktrees/**', '**/.worktrees/**'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      ecmaVersion: 2023,
      sourceType: 'module',
      globals: { ...globals.browser },
      parserOptions: { ecmaFeatures: { jsx: true } },
    },
    plugins: { react, 'react-hooks': reactHooks },
    settings: { react: { version: 'detect' } },
    rules: {
      ...react.configs.recommended.rules,
      // React 17+ automatic JSX runtime: no `import React` needed in scope.
      ...react.configs['jsx-runtime'].rules,
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      // TypeScript provides prop typing; PropTypes are not used.
      'react/prop-types': 'off',
      // Intentionally-unused bindings are allowed when prefixed with `_`.
      '@typescript-eslint/no-unused-vars': [
        'error',
        {
          argsIgnorePattern: '^_',
          varsIgnorePattern: '^_',
          caughtErrorsIgnorePattern: '^_',
          ignoreRestSiblings: true,
        },
      ],
      // `any` is discouraged but surfaced as advisory: eliminating existing uses
      // is tracked for the architecture refactor phase, not the tooling phase.
      '@typescript-eslint/no-explicit-any': 'warn',
    },
  },
  // Test + config files also see Node globals.
  {
    files: ['**/*.test.{ts,tsx}', '**/*.config.{ts,js}', 'src/setupTests.ts'],
    languageOptions: { globals: { ...globals.node } },
  },
  prettier,
);
