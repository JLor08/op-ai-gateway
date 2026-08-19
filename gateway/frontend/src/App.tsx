import { useEffect, useState } from 'react'
import { Button } from 'twenty-ui/input'

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
    <main>
      <p className="eyebrow">ON-PREMISES AI PLATFORM</p>
      <h1>AI Gateway</h1>
      <p>Control approved local model access from one internal endpoint.</p>
      <Button title="Refresh gateway status" variant="secondary" onClick={() => void loadStatus()} />
      {error && <p className="error">{error}</p>}
      {status && (
        <section aria-label="Gateway status">
          <h2>{status.service}</h2>
          <p>{status.upstreams.length} configured upstream(s)</p>
        </section>
      )}
    </main>
  )
}
