# End-to-end tests

Browser-based end-to-end tests are implemented with Playwright. The Playwright
configuration starts the Go backend and React/Vite frontend automatically.

```bash
cd gateway/e2e
npm install
npx playwright install chromium
npm test
```

Keep test fixtures synthetic and never store credentials, customer prompts, or
production model output here.
