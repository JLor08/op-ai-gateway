import { expect, test } from '@playwright/test'

test('shows the reachable gateway and its configured upstream count', async ({ page }) => {
  await page.goto('/')

  await expect(page.getByRole('heading', { name: 'AI Gateway' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'onprem-ai-gateway' })).toBeVisible()
  await expect(page.getByText('0 configured upstream(s)')).toBeVisible()
})
