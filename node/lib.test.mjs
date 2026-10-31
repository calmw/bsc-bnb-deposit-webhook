import assert from 'node:assert/strict'
import { createHmac } from 'node:crypto'
import { mkdtempSync } from 'node:fs'
import { createServer } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { backfill, events, Store, verify } from './lib.mjs'

const address = '0x1111111111111111111111111111111111111111'

function event(id, status, { direction = 'in', amountWei = '500000000000000000', kind = 'external' } = {}) {
  return { id, status, address, direction, amount: '0.5', amountWei, kind, txHash: '0xabc', blockNumber: 100, balance: '1' }
}

function newStore() {
  return new Store(join(mkdtempSync(join(tmpdir(), 'bnb-webhook-')), 'state.json'), () => {})
}

function sign(secret, timestamp, body) {
  return 'sha256=' + createHmac('sha256', secret).update(`${timestamp}.${body}`).digest('hex')
}

test('verify accepts a valid signature and rejects anything else', () => {
  const body = Buffer.from('{"events":[]}')
  const now = 1791100000
  const ts = String(now)
  assert.equal(verify('secret', ts, sign('secret', ts, body), body, now), true)
  assert.equal(verify('secret', ts, sign('other', ts, body), body, now), false)
  assert.equal(verify('secret', ts, sign('secret', ts, body), Buffer.from('{"events": []}'), now), false)
  assert.equal(verify('secret', ts, sign('secret', ts, body), body, now + 301), false)
  assert.equal(verify('secret', undefined, sign('secret', ts, body), body, now), false)
  assert.equal(verify('secret', ts, undefined, body, now), false)
})

test('events accepts single-block and batched payloads', () => {
  assert.deepEqual(events({ events: [1, 2] }), [1, 2])
  assert.deepEqual(events({ batches: [{ events: [1] }, { events: [2, 3] }] }), [1, 2, 3])
})

test('credits once, on finalized only', () => {
  const store = newStore()
  store.applyAll([event('a', 'pending')])
  assert.equal(store.state.balances[address], undefined)
  store.applyAll([event('a', 'finalized')])
  store.applyAll([event('a', 'finalized')])
  store.applyAll([event('a', 'pending')])
  assert.equal(store.state.balances[address], '500000000000000000')
})

test('removed is never credited and stays removed', () => {
  const store = newStore()
  store.applyAll([event('a', 'pending'), event('a', 'removed')])
  store.applyAll([event('a', 'finalized')])
  assert.equal(store.state.balances[address], undefined)
  assert.equal(store.state.events.a.status, 'removed')
})

test('outgoing transfers and fees are debited, test pushes ignored', () => {
  const store = newStore()
  store.applyAll([
    event('in', 'finalized', { amountWei: '1000000000000000000' }),
    event('out', 'finalized', { direction: 'out', amountWei: '300000000000000000' }),
    event('fee', 'finalized', { direction: 'out', amountWei: '21000000000000', kind: 'fee' }),
    event('t', 'finalized', { kind: 'test' }),
  ])
  assert.equal(store.state.balances[address], '699979000000000000')
  assert.equal(store.state.events.t, undefined)
})

test('a failing batch leaves no trace and is retried in full', () => {
  const store = newStore()
  store.applyAll([event('a', 'finalized')])
  assert.throws(() => store.applyAll([event('b', 'finalized'), event('c', 'finalized', { amountWei: 'oops' })]))
  assert.equal(store.state.events.b, undefined)
  assert.equal(store.state.balances[address], '500000000000000000')

  store.applyAll([event('b', 'finalized'), event('c', 'finalized')])
  assert.equal(store.state.balances[address], '1500000000000000000')
  assert.deepEqual(new Store(store.file).state, store.state)
})

test('backfill follows nextCursor and dedupes', async (t) => {
  const pages = {
    '': { data: [event('a', 'finalized'), event('b', 'pending')], nextCursor: 'p2' },
    p2: { data: [event('a', 'finalized'), event('b', 'finalized')], nextCursor: '' },
  }
  const seen = []
  const server = createServer((req, res) => {
    const url = new URL(req.url, 'http://x')
    seen.push({ auth: req.headers.authorization, since: url.searchParams.get('since') })
    res.setHeader('Content-Type', 'application/json')
    res.end(JSON.stringify(pages[url.searchParams.get('cursor') ?? '']))
  })
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
  t.after(() => server.close())

  const store = newStore()
  const api = `http://127.0.0.1:${server.address().port}`
  assert.equal(await backfill(store, { api, token: 'tok', hours: 2 }), 4)
  assert.equal(store.state.balances[address], '1000000000000000000')
  assert.equal(seen.length, 2)
  assert.equal(seen[0].auth, 'Bearer tok')
  assert.ok(Math.abs(Number(seen[0].since) - (Date.now() / 1000 - 7200)) < 5)
})
