export type Effort = 'low' | 'medium' | 'high' | 'xhigh' | 'max'
export type Slot = 'main' | 'inherit' | 'fable' | 'opus' | 'sonnet' | 'haiku'
/** One override; a field left out passes the engine's own choice through. */
export type Rule = { model?: string; effort?: Effort; fallback?: string[]; by?: 'session' }
/** Who decided one field of a spawn's route. */
export type Who = 'you' | 'session' | 'card' | 'tier' | 'main' | 'engine'
/** One subagent's route, frozen when it spawned, plus what really ran. */
export type Spawn = {
  agentId: string; toolUseId: string; type: string; description: string
  model: string; effort?: Effort; fallback?: string[]; by: { model: Who; effort: Who }
  real?: string; provider?: string; trail: string[]; tokens: number; at: number
}
/** What one subagent (or main) really ran on, from its last request. */
export type Seen = { key: string; type: string; description: string; model: string; effort?: string; provider?: string; fellBackFrom?: string; at: number }
export type Routes = { slots: Partial<Record<Slot, Rule>>; agents: Record<string, Rule> }
export type Provider = { name: string; ids: string[] }

declare module 'claude-code' {
  interface PluginState {
    'model-router': { routes: Routes; catalogue: Provider[]; profiles: Record<string, Routes>; seen: Record<string, Seen>; spawns: Record<string, Spawn>; pending: Spawn[]; health: Record<string, { ok: number; failed: number }>; 'pane-provider': Record<string, string> }
  }
}
