import type { Register } from 'claude-code'

// Agent Deck sessions as native Claude Code agents.
//
// Each live session is an agent type `deck:<title>`, and `deck:new` starts a fresh one.
// The agent's model step is answered here, not by a model: the brief goes to the session through
// `agent-deck session send --json --wait`, and the session's reply comes back as the agent's answer.
// Everything else is Claude Code's own: the agent row, foreground or background, the agents view.

const NEW = 'new'
const REFRESH_MS = 15_000
const SEND_TIMEOUT = '30m'
const READY_SECONDS = 120
const slug = (title: string) => title.replace(/[^A-Za-z0-9_-]/g, '-').slice(0, 64)

type Session = { id: string; title: string; status: string }
type Step = { turnId: string; index: number }

// One answer: the reply text, then the stop that ends the agent's only step.
async function* answer(e: Step, text: string) {
  if (text) yield { kind: 'text' as const, index: 0, text }
  yield { kind: 'stop' as const, stopReason: 'end_turn' as const, usage: null }
  return { turnId: e.turnId, index: e.index, answer: text, toolUses: [], stopReason: 'end_turn' as const, usage: null }
}

// Registers a type for each live session not yet known; the current session is never its own agent.
async function refresh($: any, types: Map<string, string>): Promise<void> {
  if (!types.has(NEW)) {
    await $.agent.register({ name: NEW, description: 'Start a new Agent Deck session for the task and return its reply. Use for long or durable work.',
      prompt: 'Answered by Agent Deck.', model: 'haiku', tools: [], omitClaudeMd: true })
    types.set(NEW, NEW)
  }
  const listed = await $.process.run(['agent-deck', 'list', '--json'])
  if (listed.exitCode !== 0) return
  const self = await $.env.get('AGENTDECK_INSTANCE_ID')
  for (const s of JSON.parse(listed.stdout) as Session[]) {
    const name = slug(s.title)
    if (s.id === self || s.status === 'stopped' || name === NEW || types.has(name)) continue
    await $.agent.register({ name, description: `Message the live Agent Deck session '${s.title}' and return its reply.`,
      prompt: 'Answered by Agent Deck.', model: 'haiku', tools: [], omitClaudeMd: true })
    types.set(name, s.title)
  }
}

// Sends the brief and waits for the session's turn to end; the reply, or why there is none.
async function send($: any, session: string, brief: string): Promise<string> {
  const argv = ['agent-deck', 'session', 'send', session, '--json', '--wait', '--defer-if-busy', '--no-tag', '--timeout', SEND_TIMEOUT, '--message-file', '-']
  let out = '', err = ''
  let end: { code: number | null } = { code: null }
  const it = $.process.spawn({ argv, input: brief })[Symbol.asyncIterator]()
  for (;;) {
    const step = await it.next()
    if (step.done) { end = step.value ?? end; break }
    if (step.value.stream === 'stdout') out += step.value.text; else err += step.value.text
  }
  let reply: { content?: string; error?: string } = {}
  try { reply = JSON.parse(out) } catch {}
  if (end.code === 0 && reply.content) return reply.content
  return `agent-deck: ${session}: ${reply.error ?? (err.trim().split('\n').at(-1) || `send exited ${end.code}`)}`
}

// Starts a session in the working directory and waits until it can take a message; its id, or why not.
async function launch($: any): Promise<{ id?: string; why?: string }> {
  const made = await $.process.run(['agent-deck', 'launch', await $.session.cwd(), '--json', '--no-parent'], { timeoutMs: 60_000 })
  let id: string | undefined
  try { const j = JSON.parse(made.stdout); id = j.id ?? j.session?.id ?? j.session_id } catch {}
  if (made.exitCode !== 0 || !id) return { why: String(made.stderr).trim().split('\n').at(-1) || 'launch failed' }
  // The wait runs in a child process, so it costs this hook no budget.
  const ready = await $.process.spawn({ argv: ['sh', '-c',
    'for i in $(seq "$2"); do s=$(agent-deck session show "$1" --json | jq -r .status); case "$s" in waiting|idle) exit 0;; error|stopped) exit 1;; esac; sleep 1; done; exit 1',
    'sh', id, String(READY_SECONDS)] })[Symbol.asyncIterator]()
  let end: { code: number | null } = { code: null }
  for (;;) { const step = await ready.next(); if (step.done) { end = step.value ?? end; break } }
  return end.code === 0 ? { id } : { why: `session ${id} never became ready` }
}

// Registered name → session title. Module state on purpose: an unloaded plugin's types go with it.
const types = new Map<string, string>()
let refreshedAt: number | undefined

async function sync($: any): Promise<void> {
  const now = await $.clock.now()
  if (refreshedAt !== undefined && now - refreshedAt < REFRESH_MS) return
  refreshedAt = now
  try { await refresh($, types) } catch {} // a missing or failing agent-deck leaves the types as they were
}

export const register: Register = (on) => {
  on('session.start', async ($, e, next) => {
    const done = await next(e)
    await sync($)
    return done
  })

  on('turn.start', async ($, e, next) => {
    const done = await next(e)
    await sync($)
    return done
  })

  // Only this plugin's agent types are answered here; every other step goes to its model.
  on('turn.step', async function* ($, e, next) {
    let name: string | undefined
    try {
      const type = e.agentId ? (await $.agent.list()).find((a: { id: string }) => a.id === e.agentId)?.type : undefined
      name = type?.startsWith('deck:') ? type.slice('deck:'.length) : undefined
    } catch {}
    if (!name) return yield* next(e)
    // The reply is the whole answer; a further step would reach the placeholder model.
    if (e.index > 0) return yield* answer(e, '')
    const brief = (await $.session.messages({ agentId: e.agentId }))?.find((m: { role: string }) => m.role === 'user')?.text ?? ''
    if (name === NEW) {
      const made = await launch($)
      return yield* answer(e, made.id ? await send($, made.id, brief) : `agent-deck: new session: ${made.why}`)
    }
    return yield* answer(e, await send($, types.get(name) ?? name, brief))
  })
}
