import { useEffect, useState } from 'react'
import RefreshIcon from '@mui/icons-material/Refresh'
import {
  Alert,
  Box,
  Button,
  Chip,
  Container,
  Paper,
  Stack,
  Typography,
} from '@mui/material'

type Status = { service: string; upstreams: string[] }

export default function App() {
  const [status, setStatus] = useState<Status | null>(null)
  const [error, setError] = useState('')

  const loadStatus = async () => {
    setError('')
    try {
      const response = await fetch('/api/v1/status')
      if (!response.ok) throw new Error(response.statusText)
      setStatus(await response.json())
    } catch {
      setError('Gateway is not reachable. Start the Go backend to view its status.')
    }
  }

  useEffect(() => {
    void loadStatus()
  }, [])

  return (
    <Container component="main" maxWidth="md" sx={{ py: { xs: 6, md: 10 } }}>
      <Stack spacing={4}>
        <Box>
          <Typography
            component="p"
            variant="overline"
            color="primary"
            sx={{ fontWeight: 700 }}
          >
            ON-PREMISES AI PLATFORM
          </Typography>
          <Typography component="h1" variant="h2" sx={{ mt: 1, fontWeight: 800 }}>
            AI Gateway
          </Typography>
          <Typography color="text.secondary" sx={{ mt: 2, maxWidth: 560 }}>
            Control approved local model access from one internal endpoint.
          </Typography>
        </Box>

        <Box>
          <Button
            aria-label="Refresh gateway status"
            title="Refresh gateway status"
            variant="contained"
            startIcon={<RefreshIcon />}
            onClick={() => void loadStatus()}
          >
            Refresh status
          </Button>
        </Box>

        {error && <Alert severity="error">{error}</Alert>}

        {status && (
          <Paper
            component="section"
            aria-label="Gateway status"
            variant="outlined"
            sx={{ p: 3 }}
          >
            <Stack spacing={2}>
              <Stack
                direction={{ xs: 'column', sm: 'row' }}
                spacing={2}
                sx={{
                  alignItems: { xs: 'flex-start', sm: 'center' },
                  justifyContent: 'space-between',
                }}
              >
                <Box>
                  <Typography component="h2" variant="h5" sx={{ fontWeight: 700 }}>
                    {status.service}
                  </Typography>
                  <Typography color="text.secondary">
                    {status.upstreams.length} configured upstream(s)
                  </Typography>
                </Box>
                <Chip color="primary" label="Reachable" />
              </Stack>
            </Stack>
          </Paper>
        )}
      </Stack>
    </Container>
  )
}
