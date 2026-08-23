// SPDX-License-Identifier: AGPL-3.0-only
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { CssBaseline, ThemeProvider, createTheme } from '@mui/material'
import App from './App'
import './styles.css'

const theme = createTheme({
  palette: {
    mode: 'dark',
    background: {
      default: '#141414',
      paper: '#1f2220',
    },
    primary: {
      main: '#4dd4ac',
    },
    secondary: {
      main: '#f6c85f',
    },
    error: {
      main: '#ff8a80',
    },
  },
  shape: {
    borderRadius: 8,
  },
  typography: {
    fontFamily: ['Inter', 'system-ui', 'sans-serif'].join(','),
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ThemeProvider theme={theme}>
      <CssBaseline />
      <App />
    </ThemeProvider>
  </StrictMode>
)
