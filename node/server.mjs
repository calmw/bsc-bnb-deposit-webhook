import { createServer } from 'node:http'
import { backfill, events, Store, verify } from './lib.mjs'

const maxBodyBytes = 32 << 20

const secret = process.env.BSCG_WEBHOOK_SECRET
const token = process.env.BSCG_BNB_TOKEN
const api = process.env.BSCG_API || 'https://api.bscquery.com'
const port = Number(process.env.PORT || 8080)
if (!secret) {
  console.error('Set BSCG_WEBHOOK_SECRET (the signing secret under "Webhook" on the BNB plan page)')
  process.exit(1)
}
const store = new Store(process.env.STATE_FILE || 'state.json')

createServer((req, res) => {
  if (req.method !== 'POST' || req.url !== '/bnb-webhook') {
    res.writeHead(404).end()
    return
  }
  const chunks = []
  let size = 0
  req.on('data', (chunk) => {
    size += chunk.length
    if (size <= maxBodyBytes) chunks.push(chunk)
  })
  req.on('end', () => {
    if (size > maxBodyBytes) {
      res.writeHead(413).end()
      return
    }
    const raw = Buffer.concat(chunks)
    if (!verify(secret, req.headers['x-bscg-timestamp'], req.headers['x-bscg-signature'], raw)) {
      res.writeHead(401).end()
      return
    }
    let payload
    try {
      payload = JSON.parse(raw)
    } catch {
      res.writeHead(400).end()
      return
    }
    try {
      store.applyAll(events(payload))
      res.writeHead(200).end()
    } catch (err) {
      console.error('processing failed, waiting for the platform to retry', req.headers['x-bscg-delivery-id'], err)
      res.writeHead(500).end()
    }
  })
}).listen(port, () => console.log(`listening on http://0.0.0.0:${port}/bnb-webhook`))

async function backfillLoop(hours) {
  try {
    console.log(`backfill done: ${await backfill(store, { api, token, hours })} changes in the last ${hours}h`)
  } catch (err) {
    console.error(err.message)
  }
  setTimeout(() => backfillLoop(2), 3600 * 1000)
}

if (token) {
  backfillLoop(Number(process.env.BACKFILL_HOURS || 24))
} else {
  console.log('BSCG_BNB_TOKEN not set, backfill disabled')
}
