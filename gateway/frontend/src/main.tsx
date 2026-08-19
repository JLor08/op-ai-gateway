// SPDX-License-Identifier: AGPL-3.0-only
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { ThemeProvider } from 'twenty-ui/theme-constants'
import App from './App'
import './styles.css'
import 'twenty-ui/style.css'
import 'twenty-ui/theme-dark.css'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ThemeProvider colorScheme="dark">
      <App />
    </ThemeProvider>
  </StrictMode>
)
