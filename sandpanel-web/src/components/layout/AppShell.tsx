import {
  ChevronDown,
  Cog,
  FileText,
  Gamepad2,
  LayoutDashboard,
  LogOut,
  Package,
  Radar,
  Server,
  Settings,
  Terminal,
  TerminalSquare,
  Users,
} from "lucide-react"
import { useMemo, useState } from "react"
import { NavLink, Outlet } from "react-router-dom"
import { cn } from "../../lib/utils"
import { useServerStore } from "../../store/useServerStore"
import { Button } from "../ui/button"
import { Input } from "../ui/input"
import sandpanelIcon from "../../assets/sandpanel-icon.png"

const roleLevel: Record<string, number> = { user: 1, moderator: 2, admin: 3, host: 4 }

const allNavSections = [
  {
    label: "Server",
    items: [
      { to: "/", label: "Dashboard", icon: LayoutDashboard, minRole: 1 },
      { to: "/players", label: "Players", icon: Users, minRole: 2 },
      { to: "/rcon", label: "RCON", icon: TerminalSquare, minRole: 2 },
      { to: "/server-control", label: "Server Control", icon: Gamepad2, minRole: 2 },
    ],
  },
  {
    label: "Configuration",
    items: [
      { to: "/configuration", label: "Configuration", icon: Cog, minRole: 3 },
      { to: "/profiles", label: "Profiles", icon: Server, minRole: 3 },
      { to: "/mods", label: "Mods", icon: Package, minRole: 3 },
      { to: "/mods/explorer", label: "Mod Explorer", icon: Radar, minRole: 3 },
    ],
  },
  {
    label: "System",
    items: [
      { to: "/steamcmd", label: "SteamCMD", icon: Terminal, minRole: 3 },
      { to: "/logs", label: "Logs", icon: FileText, minRole: 3 },
      { to: "/operations", label: "Operations", icon: Settings, minRole: 3 },
    ],
  },
]

export function AppShell() {
  const currentUser = useServerStore((state) => state.currentUser)
  const login = useServerStore((state) => state.login)
  const logout = useServerStore((state) => state.logout)
  const serverStatus = useServerStore((state) => state.serverStatus)
  const profiles = useServerStore((state) => state.profiles)
  const activeProfileId = useServerStore((state) => state.activeProfileId)
  const setActiveProfile = useServerStore((state) => state.setActiveProfile)

  const [username, setUsername] = useState("")
  const [password, setPassword] = useState("")
  const [loginError, setLoginError] = useState("")
  const [profileDropdownOpen, setProfileDropdownOpen] = useState(false)

  const userLevel = roleLevel[currentUser?.role ?? "user"] ?? 1

  const navSections = useMemo(() => {
    return allNavSections
      .map((section) => ({
        ...section,
        items: section.items.filter((item) => userLevel >= item.minRole),
      }))
      .filter((section) => section.items.length > 0)
  }, [userLevel])

  const allNavItems = useMemo(
    () => navSections.flatMap((section) => section.items),
    [navSections],
  )

  const activeProfile = useMemo(
    () => profiles.find((p) => p.id === activeProfileId) ?? profiles[0],
    [profiles, activeProfileId],
  )

  const runningServers = useMemo(() => {
    if (!serverStatus?.instances) return 0
    return Object.values(serverStatus.instances).filter((item) => item.running).length
  }, [serverStatus?.instances])

  const cpuPercent = useMemo(() => {
    return serverStatus?.server?.cpuPercent ?? 0
  }, [serverStatus?.server?.cpuPercent])

  if (!currentUser) {
    return (
      <div className="grid h-screen place-items-center overflow-hidden bg-zinc-950 px-4">
        <div className="w-full max-w-md rounded-2xl border border-zinc-700/45 bg-zinc-900/55 p-6 backdrop-blur-xl shadow-[0_18px_40px_rgba(2,6,23,0.28)]">
          <div className="mb-5 flex items-center gap-3">
            <img src={sandpanelIcon} alt="SandPanel" className="h-10 w-10 rounded-xl" />
            <h1 className="text-lg font-semibold">SandPanel Login</h1>
          </div>
          <div className="space-y-3">
            <Input value={username} onChange={(event) => setUsername(event.target.value)} placeholder="Username" />
            <Input
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              placeholder="Password"
              type="password"
              onKeyDown={(event) => {
                if (event.key === "Enter" && username.trim() && password.trim()) {
                  event.preventDefault()
                  setLoginError("")
                  void login(username.trim(), password)
                    .then(() => setPassword(""))
                    .catch((err: Error) => setLoginError(err.message || "Login failed"))
                }
              }}
            />
            {loginError ? <p className="text-sm text-red-400">{loginError}</p> : null}
            <Button
              className="w-full"
              onClick={() => {
                if (username.trim() && password.trim()) {
                  setLoginError("")
                  void login(username.trim(), password)
                    .then(() => setPassword(""))
                    .catch((err: Error) => setLoginError(err.message || "Login failed"))
                }
              }}
            >
              Login
            </Button>
          </div>
          <p className="mt-4 text-center text-xs text-zinc-500">
            <a href="https://github.com/jocxFIN/sandpanel" target="_blank" rel="noreferrer" className="text-zinc-400 hover:text-zinc-200 transition-colors">SandPanel</a>{" "}
            by{" "}
            <a href="https://github.com/jocxFIN" target="_blank" rel="noreferrer" className="text-zinc-400 hover:text-zinc-200 transition-colors">jocxfin</a>
          </p>
        </div>
      </div>
    )
  }

  const isProfileRunning = (profileId: string) => {
    if (!serverStatus?.instances) return false
    return Boolean(serverStatus.instances[profileId]?.running)
  }

  return (
    <div className="h-screen overflow-hidden bg-zinc-950 p-5 text-zinc-100">
      <div className="relative h-full">
        <aside className="absolute inset-y-0 left-0 z-20 hidden w-72 flex-col rounded-2xl border border-zinc-700/50 bg-zinc-900/50 p-5 backdrop-blur-xl shadow-[0_18px_40px_rgba(2,6,23,0.3)] lg:flex">
          <div className="mb-6 flex items-center gap-3">
            <img src={sandpanelIcon} alt="SandPanel" className="h-10 w-10 rounded-xl" />
            <h1 className="text-lg font-semibold">SandPanel</h1>
          </div>

          {/* Navigation sections */}
          <nav className="flex-1 space-y-5 overflow-y-auto">
            {navSections.map((section) => (
              <div key={section.label}>
                <p className="mb-1.5 px-3 text-[10px] font-semibold uppercase tracking-widest text-zinc-500">
                  {section.label}
                </p>
                <div className="space-y-0.5">
                  {section.items.map((item) => {
                    const Icon = item.icon
                    return (
                      <NavLink
                        key={item.to}
                        to={item.to}
                        end={item.to === "/"}
                        className={({ isActive }) =>
                          cn(
                            "flex items-center gap-3 rounded-xl px-3 py-2 text-sm font-medium transition-colors",
                            isActive
                              ? "bg-indigo-500/90 text-white shadow-[0_8px_18px_rgba(79,70,229,0.35)]"
                              : "text-zinc-300 hover:bg-zinc-800/50",
                          )
                        }
                      >
                        <Icon className="h-4 w-4" />
                        <span>{item.label}</span>
                      </NavLink>
                    )
                  })}
                </div>
              </div>
            ))}
          </nav>

          {/* Profile selector */}
          <div className="mb-3 mt-4">
            <p className="mb-1.5 px-1 text-[10px] font-semibold uppercase tracking-widest text-zinc-500">
              Active Profile
            </p>
            <div className="relative">
              <button
                type="button"
                className="flex w-full items-center justify-between rounded-xl border border-zinc-700/60 bg-zinc-800/50 px-3 py-2.5 text-left text-sm font-medium text-zinc-100 transition-colors hover:border-zinc-600 hover:bg-zinc-800/70"
                onClick={() => setProfileDropdownOpen(!profileDropdownOpen)}
              >
                <div className="flex items-center gap-2.5 truncate">
                  <span
                    className={cn(
                      "h-2 w-2 shrink-0 rounded-full",
                      isProfileRunning(activeProfileId)
                        ? "bg-emerald-400 shadow-[0_0_6px_rgba(52,211,153,0.5)]"
                        : "bg-zinc-500",
                    )}
                  />
                  <span className="truncate">{activeProfile?.name ?? activeProfileId}</span>
                </div>
                <ChevronDown
                  className={cn(
                    "h-4 w-4 shrink-0 text-zinc-400 transition-transform",
                    profileDropdownOpen && "rotate-180",
                  )}
                />
              </button>
              {profileDropdownOpen && (
                <div className="absolute bottom-full left-0 z-30 mb-1 w-full rounded-xl border border-zinc-700/60 bg-zinc-900 py-1 shadow-xl backdrop-blur-xl">
                  {profiles.map((profile) => (
                    <button
                      key={profile.id}
                      type="button"
                      className={cn(
                        "flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm transition-colors",
                        profile.id === activeProfileId
                          ? "bg-indigo-500/20 text-indigo-300"
                          : "text-zinc-300 hover:bg-zinc-800",
                      )}
                      onClick={() => {
                        setActiveProfile(profile.id)
                        setProfileDropdownOpen(false)
                      }}
                    >
                      <span
                        className={cn(
                          "h-2 w-2 shrink-0 rounded-full",
                          isProfileRunning(profile.id)
                            ? "bg-emerald-400 shadow-[0_0_6px_rgba(52,211,153,0.5)]"
                            : "bg-zinc-500",
                        )}
                      />
                      <span className="truncate">{profile.name}</span>
                    </button>
                  ))}
                </div>
              )}
            </div>
          </div>

          {/* Status & logout */}
          <div className="space-y-3 rounded-xl border border-zinc-700/55 bg-zinc-950/45 p-3 text-xs backdrop-blur-md">
            <div className="space-y-1">
              <p className="text-zinc-400">Signed in as</p>
              <p className="font-medium text-zinc-100">
                {currentUser.name}
              </p>
            </div>
            <div className="grid grid-cols-2 gap-2 text-zinc-300">
              <div className="rounded-md border border-zinc-800 px-2 py-1">
                <p className="text-zinc-500">Running</p>
                <p>{runningServers} srv</p>
              </div>
              <div className="rounded-md border border-zinc-800 px-2 py-1">
                <p className="text-zinc-500">CPU</p>
                <p>{cpuPercent}%</p>
              </div>
            </div>
            <Button size="sm" variant="secondary" className="w-full" onClick={logout}>
              <LogOut className="h-4 w-4" />
              Logout
            </Button>
          </div>

          {/* Attribution */}
          <p className="mt-3 text-center text-[10px] text-zinc-600">
            <a href="https://github.com/jocxFIN/sandpanel" target="_blank" rel="noreferrer" className="hover:text-zinc-400 transition-colors">SandPanel</a>{" "}
            by{" "}
            <a href="https://github.com/jocxFIN" target="_blank" rel="noreferrer" className="hover:text-zinc-400 transition-colors">jocxfin</a>
          </p>
        </aside>

        <main className="h-full min-h-0 lg:pl-[19rem]">
          <div className="flex h-full min-h-0 flex-col rounded-2xl border border-zinc-700/45 bg-zinc-950/28 p-5 backdrop-blur-[2px] sm:p-7">
            <div className="mb-4 flex gap-2 overflow-x-auto pb-1 lg:hidden">
            {allNavItems.map((item) => {
              const Icon = item.icon
              return (
                <NavLink
                  key={`mobile-${item.to}`}
                  to={item.to}
                  end={item.to === "/"}
                  className={({ isActive }) =>
                    cn(
                      "flex shrink-0 items-center gap-2 rounded-xl px-3 py-2 text-sm font-medium",
                      isActive ? "bg-indigo-500/90 text-white" : "bg-zinc-900/50 text-zinc-300",
                    )
                  }
                >
                  <Icon className="h-4 w-4" />
                  <span>{item.label}</span>
                </NavLink>
              )
            })}
            </div>
            <div className="min-h-0 flex-1 overflow-y-auto">
              <Outlet />
            </div>
          </div>
        </main>
      </div>
    </div>
  )
}
