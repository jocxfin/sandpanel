import { create } from "zustand"
import { api, type CommandTarget } from "../lib/api"
import type {
  AppSettings,
  ConfigField,
  LivePlayer,
  ModDetail,
  ModExploreResponse,
  Monitor,
  ParsedConfigDocument,
  Profile,
  PublicUser,
  ServerStatusResponse,
} from "../types/server"

type ConnectionState = "connecting" | "connected" | "disconnected"

type LogEntry = {
  time: string
  line: string
  type: string
}

const ACTIVE_PROFILE_KEY = "sandpanel:activeProfile"

function loadActiveProfile(): string {
  try {
    return localStorage.getItem(ACTIVE_PROFILE_KEY) ?? "default"
  } catch { return "default" }
}

type StoreState = {
  connectionState: ConnectionState
  currentUser: PublicUser | null
  activeProfileId: string
  serverStatus: ServerStatusResponse | null
  livePlayers: LivePlayer[]
  mods: ModDetail[]
  exploreMods: ModExploreResponse["mods"]
  exploreTotal: number
  explorePage: number
  explorePageSize: number
  profiles: Profile[]
  users: PublicUser[]
  monitors: Monitor[]
  instances: Record<string, { running: boolean; pid?: number; threads?: number }>
  appSettings: AppSettings | null
  wrapperLogs: Array<{ time: string; line: string }>
  steamcmdStatus: Record<string, unknown> | null
  configFiles: string[]
  activeConfigName: string
  activeConfig: ParsedConfigDocument | null
  logs: LogEntry[]
  commandResponse: string
  fetchError: string
  loading: boolean
  setActiveProfile: (id: string) => void
  login: (name: string, password: string) => Promise<void>
  logout: () => void
  init: () => Promise<void>
  startRealtime: () => void
  stopRealtime: () => void
  loadConfig: (name: string) => Promise<void>
  parseConfigFromRaw: (name: string, raw: string) => Promise<void>
  saveConfig: (name: string, raw: string, updates: ConfigField[]) => Promise<void>
  runCommand: (command: string, target: CommandTarget) => Promise<void>
  moderatePlayer: (action: "kick" | "ban" | "unban", steamId: string, reason: string) => Promise<void>
  addMod: (id: string) => Promise<void>
  toggleMod: (id: string, enabled: boolean) => Promise<void>
  loadModExplorer: (opts: { q?: string; sort?: string; page?: number; pageSize?: number }) => Promise<void>
  saveProfile: (profile: Profile) => Promise<void>
  controlProfile: (profileId: string, action: "start" | "stop" | "restart") => Promise<void>
  refreshAdminData: () => Promise<void>
  createUser: (payload: { name: string; role: string; password?: string }) => Promise<void>
  updateUser: (id: string, payload: { name: string; role: string; password?: string }) => Promise<void>
  deleteUser: (id: string) => Promise<void>
  createMonitor: (payload: Omit<Monitor, "id">) => Promise<void>
  updateMonitor: (id: string, payload: Omit<Monitor, "id">) => Promise<void>
  deleteMonitor: (id: string) => Promise<void>
  monitorAction: (id: string, action: "start" | "stop") => Promise<void>
  saveSettings: (settings: AppSettings) => Promise<void>
  refreshWrapperLogs: () => Promise<void>
  refreshSteamcmdStatus: () => Promise<void>
  steamcmdInstall: (validate: boolean) => Promise<void>
  steamcmdCheckUpdate: () => Promise<void>
  steamcmdStop: () => Promise<void>
  wrapperRestart: () => Promise<void>
  wrapperUpdate: () => Promise<void>
}

let pollTimer: number | null = null
let logsSocket: WebSocket | null = null

const maxLogLines = 1200

function wsBase() {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:"
  return `${protocol}//${window.location.host}`
}

function pushLog(set: (fn: (state: StoreState) => Partial<StoreState>) => void, line: string, type = "log") {
  const entry: LogEntry = { time: new Date().toISOString(), line, type }
  set((state) => ({
    logs: [...state.logs, entry].slice(-maxLogLines),
  }))
}

async function refreshCore(set: (fn: (state: StoreState) => Partial<StoreState>) => void) {
  // Only poll lightweight endpoints. Mod details (mod.io API) are fetched
  // separately on init and after explicit user actions to avoid flickering.
  const [status, players, profiles] = await Promise.allSettled([
    api.getServerStatus(),
    api.getLivePlayers(),
    api.getProfiles(),
  ])

  set((state) => ({
    serverStatus: status.status === "fulfilled" ? status.value : state.serverStatus,
    livePlayers:
      players.status === "fulfilled" && Array.isArray(players.value.live)
        ? players.value.live
        : state.livePlayers,
    profiles:
      profiles.status === "fulfilled" && Array.isArray(profiles.value.profiles)
        ? profiles.value.profiles
        : state.profiles,
  }))
}

async function refreshMods(set: (fn: (state: StoreState) => Partial<StoreState>) => void) {
  try {
    const mods = await api.getModsDetails()
    set((state) => ({
      mods: Array.isArray(mods.mods) ? mods.mods : state.mods,
      fetchError: mods.fetchError ?? "",
    }))
  } catch (err) {
    // Don't clear mods on error — keep showing cached data.
    set(() => ({ fetchError: err instanceof Error ? err.message : "Mod details fetch failed" }))
  }
}

async function refreshAdminCore(set: (fn: (state: StoreState) => Partial<StoreState>) => void) {
  const [users, monitors, instances, settings, wrapperLogs, steamcmdStatus] = await Promise.all([
    api.getUsers(),
    api.getMonitors(),
    api.getInstances(),
    api.getSettings(),
    api.getWrapperLogs(),
    api.getSteamcmdStatus(),
  ])

  set(() => ({
    users: Array.isArray(users.users) ? users.users : [],
    monitors: Array.isArray(monitors.monitors) ? monitors.monitors : [],
    instances: instances.instances ?? {},
    appSettings: settings,
    wrapperLogs: Array.isArray(wrapperLogs.logs) ? wrapperLogs.logs : [],
    steamcmdStatus,
  }))
}

export const useServerStore = create<StoreState>((set, get) => ({
  connectionState: "disconnected",
  currentUser: null,
  activeProfileId: loadActiveProfile(),
  serverStatus: null,
  livePlayers: [],
  mods: [],
  exploreMods: [],
  exploreTotal: 0,
  explorePage: 1,
  explorePageSize: 24,
  profiles: [],
  users: [],
  monitors: [],
  instances: {},
  appSettings: null,
  wrapperLogs: [],
  steamcmdStatus: null,
  configFiles: [],
  activeConfigName: "Game.ini",
  activeConfig: null,
  logs: [],
  commandResponse: "",
  fetchError: "",
  loading: false,

  setActiveProfile: (id: string) => {
    try { localStorage.setItem(ACTIVE_PROFILE_KEY, id) } catch { /* ignore */ }
    set(() => ({ activeProfileId: id }))
  },

  login: async (name: string, password: string) => {
    const user = await api.login(name, password)
    set(() => ({ currentUser: user }))
    const level = ({ user: 1, moderator: 2, admin: 3, host: 4 } as Record<string, number>)[user.role] ?? 1

    // Always fetch core data (server status, etc.)
    const corePromises: Promise<void>[] = [refreshCore(set)]

    // Only fetch admin-level data for admin+ users
    if (level >= 3) {
      const files = await api.getConfigFiles()
      const fileList = Array.isArray(files.files) ? files.files : []
      const activeName = fileList.includes("Game.ini") ? "Game.ini" : fileList[0] ?? "Game.ini"
      corePromises.push(
        refreshMods(set),
        get().refreshAdminData(),
        get().loadModExplorer({ page: 1, pageSize: 24 }),
        get().loadConfig(activeName),
      )
      await Promise.all(corePromises)
      set(() => ({ configFiles: fileList, activeConfigName: activeName }))
    } else {
      await Promise.all(corePromises)
    }
  },

  logout: () => {
    void api.logout()
    set(() => ({
      currentUser: null,
      serverStatus: null,
      livePlayers: [],
      mods: [],
      profiles: [],
      users: [],
      monitors: [],
      instances: {},
      appSettings: null,
      wrapperLogs: [],
      exploreMods: [],
      exploreTotal: 0,
      logs: [],
    }))
  },

  init: async () => {
    set(() => ({ loading: true }))
    try {
      let currentUser: PublicUser | null = null
      try {
        currentUser = await api.getMe()
      } catch {
        set(() => ({ loading: false, currentUser: null }))
        return
      }

      const level = ({ user: 1, moderator: 2, admin: 3, host: 4 } as Record<string, number>)[currentUser.role] ?? 1

      // Always fetch core data
      const corePromises: Promise<void>[] = [refreshCore(set)]

      let fileList: string[] = []
      let activeName = "Game.ini"

      // Only fetch admin-level data for admin+ users
      if (level >= 3) {
        try {
          const files = await api.getConfigFiles()
          fileList = Array.isArray(files.files) ? files.files : []
          activeName = fileList.includes("Game.ini") ? "Game.ini" : fileList[0] ?? "Game.ini"
        } catch { /* user lacks permission, skip */ }
        corePromises.push(
          refreshMods(set),
          get().loadConfig(activeName),
          get().loadModExplorer({ page: 1, pageSize: 24 }),
          refreshAdminCore(set),
        )
      }

      await Promise.all(corePromises)

      set(() => ({
        configFiles: fileList,
        activeConfigName: activeName,
        currentUser,
        loading: false,
      }))
    } catch (error) {
      set(() => ({
        loading: false,
        fetchError: error instanceof Error ? error.message : "Failed to initialize",
      }))
    }
  },

  startRealtime: () => {
    if (pollTimer !== null) {
      return
    }

    set(() => ({ connectionState: "connecting" }))

    logsSocket = new WebSocket(`${wsBase()}/ws/logs`)
    logsSocket.onopen = () => {
      set(() => ({ connectionState: "connected" }))
      logsSocket?.send("subscribe")
    }
    logsSocket.onclose = () => {
      if (get().connectionState !== "disconnected") {
        set(() => ({ connectionState: "disconnected" }))
      }
    }
    logsSocket.onmessage = (event) => {
      try {
        const payload = JSON.parse(String(event.data)) as Record<string, unknown>
        if (typeof payload.line === "string") {
          pushLog(set, payload.line, String(payload.type ?? "log"))
          return
        }
        if (typeof payload.message === "string") {
          pushLog(set, payload.message, String(payload.type ?? "event"))
          return
        }
        pushLog(set, JSON.stringify(payload), String(payload.type ?? "event"))
      } catch {
        pushLog(set, String(event.data), "log")
      }
    }

    void refreshCore(set).catch(() => undefined)
    pollTimer = window.setInterval(() => {
      void refreshCore(set).catch(() => undefined)
    }, 3500)
  },

  stopRealtime: () => {
    if (pollTimer !== null) {
      window.clearInterval(pollTimer)
      pollTimer = null
    }
    logsSocket?.close()
    logsSocket = null
    set(() => ({ connectionState: "disconnected" }))
  },

  loadConfig: async (name: string) => {
    const doc = await api.getConfigFile(name)
    set(() => ({ activeConfigName: name, activeConfig: doc }))
  },

  parseConfigFromRaw: async (name: string, raw: string) => {
    const parsed = await api.parseConfigRaw(name, raw)
    set(() => ({ activeConfig: parsed }))
  },

  saveConfig: async (name: string, raw: string, updates: ConfigField[]) => {
    const saved = await api.saveConfigFile(name, raw, updates)
    set(() => ({ activeConfig: saved }))
  },

  runCommand: async (command: string, target: CommandTarget) => {
    const response = await api.executeCommand(command, target)
    set(() => ({ commandResponse: response.response }))
    pushLog(set, `> ${command}`, "command")
    if (response.response.trim()) {
      pushLog(set, response.response, "response")
    }
  },

  moderatePlayer: async (action: "kick" | "ban" | "unban", steamId: string, reason: string) => {
    if (action === "kick") {
      await api.kickPlayer(steamId, reason)
    } else if (action === "ban") {
      await api.banPlayer(steamId, reason)
    } else {
      await api.unbanPlayer(steamId)
    }
    await refreshCore(set)
  },

  addMod: async (id: string) => {
    await api.addMod(id)
    await refreshMods(set)
  },

  toggleMod: async (id: string, enabled: boolean) => {
    await api.toggleMod(id, enabled)
    await refreshMods(set)
  },

  loadModExplorer: async (opts) => {
    const data = await api.exploreMods(opts)
    set(() => ({
      exploreMods: Array.isArray(data.mods) ? data.mods : [],
      exploreTotal: data.total,
      explorePage: data.page,
      explorePageSize: data.pageSize,
    }))
  },

  saveProfile: async (profile: Profile) => {
    if (!profile.id.trim()) {
      await api.createProfile({ ...profile, id: undefined })
    } else {
      await api.saveProfile(profile)
    }
    const profiles = await api.getProfiles()
    set(() => ({ profiles: Array.isArray(profiles.profiles) ? profiles.profiles : [] }))
  },

  controlProfile: async (profileId: string, action: "start" | "stop" | "restart") => {
    await api.controlProfile(profileId, action)
    await refreshCore(set)
  },

  refreshAdminData: async () => {
    await refreshAdminCore(set)
  },

  createUser: async (payload) => {
    await api.createUser(payload)
    const users = await api.getUsers()
    set(() => ({ users: Array.isArray(users.users) ? users.users : [] }))
  },

  updateUser: async (id, payload) => {
    await api.updateUser(id, payload)
    const users = await api.getUsers()
    set(() => ({ users: Array.isArray(users.users) ? users.users : [] }))
  },

  deleteUser: async (id) => {
    await api.deleteUser(id)
    const users = await api.getUsers()
    set(() => ({ users: Array.isArray(users.users) ? users.users : [] }))
  },

  createMonitor: async (payload) => {
    await api.createMonitor(payload)
    const monitors = await api.getMonitors()
    set(() => ({ monitors: Array.isArray(monitors.monitors) ? monitors.monitors : [] }))
  },

  updateMonitor: async (id, payload) => {
    await api.updateMonitor(id, payload)
    const monitors = await api.getMonitors()
    set(() => ({ monitors: Array.isArray(monitors.monitors) ? monitors.monitors : [] }))
  },

  deleteMonitor: async (id) => {
    await api.deleteMonitor(id)
    const monitors = await api.getMonitors()
    set(() => ({ monitors: Array.isArray(monitors.monitors) ? monitors.monitors : [] }))
  },

  monitorAction: async (id, action) => {
    await api.monitorAction(id, action)
    const monitors = await api.getMonitors()
    set(() => ({ monitors: Array.isArray(monitors.monitors) ? monitors.monitors : [] }))
  },

  saveSettings: async (settings) => {
    const next = await api.updateSettings(settings)
    set(() => ({ appSettings: next }))
  },

  refreshWrapperLogs: async () => {
    const data = await api.getWrapperLogs()
    set(() => ({ wrapperLogs: Array.isArray(data.logs) ? data.logs : [] }))
  },

  refreshSteamcmdStatus: async () => {
    const status = await api.getSteamcmdStatus()
    set(() => ({ steamcmdStatus: status }))
  },

  steamcmdInstall: async (validate) => {
    const status = await api.runSteamcmdInstall(validate)
    set(() => ({ steamcmdStatus: status }))
  },

  steamcmdCheckUpdate: async () => {
    const status = await api.runSteamcmdUpdateCheck()
    set(() => ({ steamcmdStatus: status }))
  },

  steamcmdStop: async () => {
    const status = await api.stopSteamcmd()
    set(() => ({ steamcmdStatus: status }))
  },

  wrapperRestart: async () => {
    await api.wrapperRestart()
  },

  wrapperUpdate: async () => {
    await api.wrapperUpdate()
  },
}))
