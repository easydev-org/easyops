// easyops ops.v1 client over Connect's plain-JSON protocol:
//   POST /easyops.v1.OpsService/<Method>   {json} -> {json}
// Auth is the cluster service token (the operator pastes it; localStorage).
export interface Service {
  name: string
  image?: string
  phase?: string
  ready?: boolean
  replicas?: number
  readyReplicas?: number
  url?: string
  owner?: string
  session?: string
  podPhase?: string
  restarts?: number
  message?: string
}
export interface Sandbox {
  name: string
  image?: string
  phase?: string
  ready?: boolean
  url?: string
  owner?: string
  session?: string
  restarts?: number
  message?: string
}
export interface PVC {
  name: string
  size?: string
  storageClass?: string
  phase?: string
  owner?: string
  mountedBy?: string[]
}
export interface Helm {
  name: string
  namespace?: string
  revision?: number
  status?: string
  chartVersion?: string
  appVersion?: string
  owner?: string
  updatedAt?: string
  slot?: string
  router?: string
  activeSlot?: string
}
export interface HelmObject {
  kind?: string
  name?: string
  ready?: boolean
  phase?: string
  restarts?: number
  message?: string
}

export class Api {
  constructor(private token: () => string) {}
  private async rpc<T>(method: string, body: unknown = {}): Promise<T> {
    const r = await fetch('/easyops.v1.OpsService/' + method, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + this.token().trim() },
      body: JSON.stringify(body),
    })
    const j = await r.json().catch(() => ({}))
    if (!r.ok) throw new Error((j as { message?: string }).message || 'HTTP ' + r.status)
    return j as T
  }
  services = () => this.rpc<{ services?: Service[] }>('ListServices')
  sandboxes = () => this.rpc<{ sandboxes?: Sandbox[] }>('ListSandboxes')
  pvcs = () => this.rpc<{ pvcs?: PVC[] }>('ListPVCs')
  helm = () => this.rpc<{ releases?: Helm[] }>('HelmList')
  serviceLogs = (owner: string, name: string, tailLines = 300) =>
    this.rpc<{ lines?: string[] }>('ServiceLogs', { owner, name, tailLines })
  sandboxLogs = (owner: string, name: string, tailLines = 300) =>
    this.rpc<{ lines?: string[] }>('SandboxLogs', { owner, name, tailLines })
  helmObjects = (owner: string, release: string) =>
    this.rpc<{ objects?: HelmObject[] }>('HelmObjects', { owner, release })
  helmHistory = (owner: string, release: string) =>
    this.rpc<{ revisions?: { revision?: number; status?: string; chartVersion?: string; appVersion?: string; updatedAt?: string }[] }>(
      'HelmHistory',
      { owner, release },
    )
  helmObjectLogs = (owner: string, release: string, kind: string, name: string, tailLines = 200) =>
    this.rpc<{ lines?: string[] }>('HelmObjectLogs', { owner, release, kind, name, tailLines })
}
