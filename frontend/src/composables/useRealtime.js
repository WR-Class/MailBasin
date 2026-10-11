import { reactive } from 'vue'

export const realtimeState = reactive({
  status: 'closed',
  lastSequence: 0,
})

let source = null
let listenerID = 0
const listeners = new Map()

function dispatchChange(event) {
  let change
  try {
    change = JSON.parse(event.data)
  } catch {
    return
  }
  const sequence = Number(change.sequence || event.lastEventId || 0)
  if (sequence > 0 && sequence <= realtimeState.lastSequence) return
  if (sequence > 0) realtimeState.lastSequence = sequence
  for (const listener of listeners.values()) {
    if (listener.resources && !listener.resources.has(change.resource)) continue
    try {
      listener.callback(change)
    } catch {
      // 单个页面刷新失败时保留实时连接，页面仍会由低频轮询兜底。
    }
  }
}

export function connectRealtime() {
  if (source || typeof window === 'undefined' || typeof window.EventSource !== 'function') return
  realtimeState.status = 'connecting'
  realtimeState.lastSequence = 0
  source = new window.EventSource('/api/realtime')
  source.addEventListener('open', () => {
    realtimeState.status = 'connected'
  })
  source.addEventListener('error', () => {
    if (source) realtimeState.status = 'reconnecting'
  })
  source.addEventListener('change', dispatchChange)
}

export function disconnectRealtime() {
  if (source) {
    source.removeEventListener('change', dispatchChange)
    source.close()
    source = null
  }
  realtimeState.status = 'closed'
  realtimeState.lastSequence = 0
}

export function subscribeRealtime(resources, callback) {
  const normalizedResources = resources == null
    ? null
    : new Set((Array.isArray(resources) ? resources : [resources]).map((item) => String(item || '').trim()).filter(Boolean))
  const id = ++listenerID
  listeners.set(id, { resources: normalizedResources, callback })
  return () => listeners.delete(id)
}

export function useRealtime() {
  return { realtimeState, connectRealtime, disconnectRealtime, subscribeRealtime }
}
