import { expect, mock, test } from 'claude-code/testing'

const ran = (exitCode: number, stdout = '', stderr = '') => ({ exitCode, stdout, stderr, isStdoutTruncated: false, isStderrTruncated: false })
const SESSIONS = [
  { id: 'self-1', title: 'mods', status: 'running' },
  { id: 'j-1', title: 'julia', status: 'waiting' },
  { id: 'p-1', title: 'conductor-piper', status: 'idle' },
  { id: 'f-1', title: 'flora', status: 'stopped' },
]
type Spawned = { argv: readonly string[]; input?: string }

// Agent Deck and the engine beneath the plugin: `send` is what `session send --json --wait` prints and how it exits.
function standIns(on: any, opts: { type?: string; send?: { code: number; out: string; err?: string } } = {}) {
  const registered: string[] = []
  const spawned: Spawned[] = []
  mock.clock(on)
  on('env.get', (_$: any, e: any) => ({ value: e.name === 'AGENTDECK_INSTANCE_ID' ? 'self-1' : '/home/test' }))
  on('session.cwd', () => ({ value: '/home/test/p' }))
  on('process.run', (_$: any, e: any) => ({
    value: e.argv[1] === 'list' ? ran(0, JSON.stringify(SESSIONS)) : e.argv[1] === 'launch' ? ran(0, JSON.stringify({ id: 'n-1' })) : ran(0),
  }))
  on('process.spawn', async function* (_$: any, e: any) {
    spawned.push({ argv: e.argv, input: e.input })
    if (e.argv[0] === 'sh') return { value: { code: 0, signal: null } } // the readiness wait
    const send = opts.send ?? { code: 0, out: JSON.stringify({ success: true, content: 'Halloumi bake tonight.' }) }
    if (send.out) yield { stream: 'stdout', text: send.out }
    if (send.err) yield { stream: 'stderr', text: send.err }
    return { value: { code: send.code, signal: null } }
  })
  on('agent.register', (_$: any, e: any) => { registered.push(e.name); return { value: { agent: `deck:${e.name}` } } })
  on('agent.list', () => ({ value: [{ id: 'ag-1', type: opts.type ?? 'deck:julia', description: 'd', status: 'running' }] }))
  on('session.messages', () => ({ value: [{ role: 'user', text: 'What is for dinner?', toolUses: [] }] }))
  on('turn.start', () => ({ turnId: 't' }))
  on('turn.step', async function* () {
    yield { kind: 'text', index: 0, text: 'from the model' }
    yield { kind: 'stop', stopReason: 'end_turn', usage: null }
    return { turnId: 't', index: 0, answer: 'from the model', toolUses: [], stopReason: 'end_turn', usage: null }
  })
  return { registered, spawned }
}

async function step($: any, index = 0) {
  const chunks: any[] = []
  for await (const c of $.turn.step({ turnId: 't', index, model: 'haiku', messageCount: 1, agentId: 'ag-1' })) chunks.push(c)
  return { chunks, text: chunks.filter((c) => c.kind === 'text').map((c) => c.text).join('') }
}

test('each live session but this one becomes an agent type, plus new', async ($, on) => {
  const { registered } = standIns(on)
  await $.turn.start({ turnId: 't' } as any)
  expect(registered.sort()).toEqual(['conductor-piper', 'julia', 'new'])
})

test('a resident agent sends its brief and answers with the session reply', async ($, on) => {
  const { spawned } = standIns(on)
  await $.turn.start({ turnId: 't' } as any)
  const { text, chunks } = await step($)
  expect(text).toBe('Halloumi bake tonight.')
  expect(chunks.at(-1)).toMatchObject({ kind: 'stop', stopReason: 'end_turn' })
  const send = spawned.find((s) => s.argv[1] === 'session')!
  expect(send.argv.slice(0, 6)).toEqual(['agent-deck', 'session', 'send', 'julia', '--json', '--wait'])
  expect(send.input).toBe('What is for dinner?')
})

test('a failed send says why instead of asking the placeholder model', async ($, on) => {
  standIns(on, { send: { code: 1, out: JSON.stringify({ success: false, error: 'session julia is not running' }) } })
  const { text } = await step($)
  expect(text).toBe('agent-deck: julia: session julia is not running')
  expect(text).not.toContain('from the model')
})

test('another agent type goes to its own model', async ($, on) => {
  standIns(on, { type: 'vault:curator' })
  expect((await step($)).text).toBe('from the model')
})

test('a further step ends the turn without asking anyone', async ($, on) => {
  const { spawned } = standIns(on)
  const { text, chunks } = await step($, 1)
  expect(text).toBe('')
  expect(chunks).toEqual([{ kind: 'stop', stopReason: 'end_turn', usage: null }])
  expect(spawned.length).toBe(0)
})

test('new starts a session, waits until it is ready, then sends the brief', async ($, on) => {
  const { spawned } = standIns(on, { type: 'deck:new' })
  const { text } = await step($)
  expect(text).toBe('Halloumi bake tonight.')
  expect(spawned.map((s) => s.argv[0])).toEqual(['sh', 'agent-deck'])
  expect(spawned[1].argv[3]).toBe('n-1')
})
