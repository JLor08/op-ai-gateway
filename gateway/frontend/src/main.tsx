// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { createRoot } from 'react-dom/client';

import App from './App';
import { ThemeRoot } from './theme/ThemeRoot';

createRoot(document.getElementById('root') as HTMLElement).render(
  <ThemeRoot>
    <App />
  </ThemeRoot>,
);
