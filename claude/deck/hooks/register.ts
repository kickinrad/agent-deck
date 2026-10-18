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
// A silent step is killed by the 600s stall watchdog, so the send beats on stderr while it waits.
// `<&0`: bash gives a background job /dev/null for stdin, and the brief arrives on stdin.
// The beat's sleep stays off the pipes: the spawn ends when its pipes close, and the orphaned sleep would hold them up to 30s.
const BEAT = '@@deck-beat'
const BEAT_SECONDS = 30
const withBeat = (argv: string[]) => ['bash', '-c',
  `"$@" <&0 & p=$!; ( while sleep ${BEAT_SECONDS} >/dev/null 2>&1; do printf '${BEAT}\\n' >&2; done ) & b=$!; wait $p; c=$?; kill $b 2>/dev/null; exit $c`,
  'bash', ...argv]
// Which sessions become agent types: the `sessions` option, titles or globs.
let offered: RegExp[] = [/^.*$/]
const globs = (spec: string) => spec.split(/\s+/).filter(Boolean)
  .map((g) => new RegExp(`^${g.replace(/[.+?^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*')}$`))
const slug = (title: string) => title.replace(/[^A-Za-z0-9_-]/g, '-').slice(0, 64)

type Session = { id: string; title: string; status: string }
type Step = { turnId: string; index: number }

// A teammate's first message arrives in its team envelope; the session gets the brief alone.
const unwrap = (text: string) => text.match(/^\s*<teammate-message[^>]*>\s*([\s\S]*?)\s*<\/teammate-message>\s*$/)?.[1] ?? text

// One answer: the reply as text and as a SubagentHandback call, then the stop that ends the agent's step.
// Under auto mode only the hand-back reaches the caller, through its review; elsewhere the text does.
// Index 0 belongs to the heartbeat's thinking.
const HANDBACK = 'SubagentHandback'
async function* answer(e: Step, text: string) {
  if (!text) {
    yield { kind: 'stop' as const, stopReason: 'end_turn' as const, usage: null }
    return { turnId: e.turnId, index: e.index, answer: '', toolUses: [], stopReason: 'end_turn' as const, usage: null }
  }
  const input = { message: text }
  yield { kind: 'text' as const, index: 1, text }
  yield { kind: 'tool' as const, index: 2, id: `toolu_deck_${e.turnId}_${e.index}`, name: HANDBACK }
  yield { kind: 'input' as const, index: 2, json: JSON.stringify(input) }
  yield { kind: 'stop' as const, stopReason: 'tool_use' as const, usage: null }
  return { turnId: e.turnId, index: e.index, answer: text, toolUses: [{ name: HANDBACK, input }], stopReason: 'tool_use' as const, usage: null }
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
    if (!offered.some((g) => g.test(s.title))) continue
    await $.agent.register({ name, description: `Message the live Agent Deck session '${s.title}' and return its reply.`,
      prompt: 'Answered by Agent Deck.', model: 'haiku', tools: [], omitClaudeMd: true })
    types.set(name, s.title)
  }
}

// Sends the brief and waits for the session's turn to end, thinking a beat while it waits; the reply, or why there is none.
async function* send($: any, session: string, brief: string) {
  const argv = ['agent-deck', 'session', 'send', session, '--json', '--wait', '--defer-if-busy', '--no-tag', '--timeout', SEND_TIMEOUT, '--message-file', '-']
  let out = '', err = ''
  let end: { code: number | null } = { code: null }
  const startedAt = await $.clock.now()
  const it = $.process.spawn({ argv: withBeat(argv), input: brief })[Symbol.asyncIterator]()
  for (;;) {
    const step = await it.next()
    if (step.done) { end = step.value ?? end; break }
    const { stream, text } = step.value
    if (stream === 'stdout') { out += text; continue }
    if (text.includes(BEAT)) {
      const waited = Math.round(((await $.clock.now()) - startedAt) / 60_000)
      yield { kind: 'thinking' as const, index: 0, text: `${session} · waiting ${waited}m` }
    }
    err += text.split('\n').filter((l: string) => !l.includes(BEAT)).join('\n')
  }
  // The payload is one JSON object; read the last line that parses, in case anything printed before it.
  let reply: { content?: string; error?: string } = {}
  for (const line of out.trim().split('\n').reverse()) { try { reply = JSON.parse(line); break } catch {} }
  if (!reply.content && !reply.error) try { reply = JSON.parse(out) } catch {}
  if (end.code === 0 && reply.content?.trim()) return reply.content
  const why = reply.error ?? err.trim().split('\n').at(-1) ?? ''
  return `agent-deck: ${session}: ${why || (end.code === 0 ? `finished without a text reply (${out.trim().slice(0, 160) || 'no output'})` : `send exited ${end.code}`)}`
}

// Starts a Claude session in the working directory and waits until it can take a message; its id, or why not.
async function launch($: any): Promise<{ id?: string; why?: string }> {
  const made = await $.process.run(['agent-deck', 'launch', await $.session.cwd(), '-c', 'claude', '--json', '--no-parent'], { timeoutMs: 60_000 })
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

export const register: Register = (on, options) => {
  offered = globs(String((options as { sessions?: string })?.sessions ?? '*'))
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
    const first = (await $.session.messages({ agentId: e.agentId }))?.find((m: { role: string }) => m.role === 'user')?.text ?? ''
    const brief = unwrap(first)
    if (name === NEW) {
      const made = await launch($)
      return yield* answer(e, made.id ? yield* send($, made.id, brief) : `agent-deck: new session: ${made.why}`)
    }
    return yield* answer(e, yield* send($, types.get(name) ?? name, brief))
  })
}
