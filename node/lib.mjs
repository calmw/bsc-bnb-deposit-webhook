import { createHmac, timingSafeEqual } from 'node:crypto'
import { existsSync, readFileSync, renameSync, writeFileSync } from 'node:fs'

const maxSkewSeconds = 300

// The signature covers the raw request body: parsing and re-serializing the JSON changes the bytes.
export function verify(secret, timestamp, signature, rawBody, now = Date.now() / 1000) {
  const ts = Number(timestamp)
  if (!Number.isInteger(ts) || Math.abs(now - ts) > maxSkewSeconds) return false
  const expected = 'sha256=' + createHmac('sha256', secret).update(`${timestamp}.`).update(rawBody).digest('hex')
  const got = Buffer.from(String(signature ?? ''))
  const want = Buffer.from(expected)
  return got.length === want.length && timingSafeEqual(got, want)
}

// With "multi-block delivery" enabled the body is {"batches": [...]}; accept both shapes.
export function events(payload) {
  const blocks = Array.isArray(payload.batches) ? payload.batches : [payload]
  return blocks.flatMap((block) => block.events)
}

// Pull every change whose status moved in the last `hours` hours and apply the ones that were
// never delivered. Overlapping an earlier window is harmless because apply dedupes by id.
export async function backfill(store, { api, token, hours }) {
  const since = Math.floor(Date.now() / 1000) - hours * 3600
  let cursor = ''
  let total = 0
  do {
    const url = new URL('/v1/bnb/events', api)
    url.search = new URLSearchParams({ since: String(since), limit: '500', ...(cursor && { cursor }) })
    const res = await fetch(url, {
      headers: { Authorization: `Bearer ${token}`, 'User-Agent': 'bsc-bnb-deposit-webhook/1' },
      signal: AbortSignal.timeout(30_000),
    })
    if (!res.ok) throw new Error(`backfill failed: HTTP ${res.status} ${await res.text()}`)
    const page = await res.json()
    store.applyAll(page.data)
    total += page.data.length
    cursor = page.nextCursor
  } while (cursor)
  return total
}

// A JSON file keeps the demo restartable. In production use a database and handle each request
// in one transaction.
export class Store {
  constructor(file, log = console.log) {
    this.file = file
    this.log = log
    this.reload()
  }

  reload() {
    this.state = existsSync(this.file) ? JSON.parse(readFileSync(this.file, 'utf8')) : { events: {}, balances: {} }
  }

  // All or nothing: if any event fails, drop the half-applied state so the platform's retry
  // replays the whole request against the last saved state.
  applyAll(events) {
    try {
      for (const event of events) this.apply(event)
      this.save()
    } catch (err) {
      this.reload()
      throw err
    }
  }

  apply(event) {
    if (event.kind === 'test') {
      this.log(`test push received ${event.id}`)
      return
    }
    const known = this.state.events[event.id]
    // finalized and removed are terminal. Skipping them makes retries, duplicates and backfill
    // overlaps credit each event exactly once.
    if (known && known.status !== 'pending') return
    this.state.events[event.id] = {
      status: event.status, address: event.address, direction: event.direction, kind: event.kind,
      amountWei: event.amountWei, txHash: event.txHash, blockNumber: event.blockNumber,
    }
    if (event.status === 'finalized') {
      this.credit(event)
    } else if (event.status === 'pending') {
      this.log(`confirming ${event.address} ${event.direction} ${event.amount} BNB (${event.kind}) tx=${event.txHash}`)
    } else if (event.status === 'removed') {
      this.log(`reorged, dropping ${event.id} tx=${event.txHash}`)
    }
  }

  credit(event) {
    const delta = BigInt(event.amountWei) * (event.direction === 'in' ? 1n : -1n)
    const balance = BigInt(this.state.balances[event.address] ?? '0') + delta
    this.state.balances[event.address] = balance.toString()
    this.log(`credited ${event.address} ${event.direction} ${event.amount} BNB (${event.kind}) ` +
      `on-chain balance ${event.balance} BNB tx=${event.txHash}`)
  }

  save() {
    const tmp = `${this.file}.tmp`
    writeFileSync(tmp, JSON.stringify(this.state, null, 2))
    renameSync(tmp, this.file)
  }
}
