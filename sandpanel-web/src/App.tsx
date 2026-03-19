import { useEffect } from "react"
import { Navigate, Route, Routes } from "react-router-dom"
import { AppShell } from "./components/layout/AppShell"
import { ConfigurationEditor } from "./features/ConfigurationEditor"
import { ConsoleScreen } from "./features/Console"
import { Dashboard } from "./features/Dashboard"
import { LogsCenter } from "./features/LogsCenter"
import { ModExplorer } from "./features/ModExplorer"
import { ModManagement } from "./features/ModManagement"
import { OperationsCenter } from "./features/OperationsCenter"
import { PlayerManagement } from "./features/PlayerManagement"
import { ProfilesServerControl } from "./features/ProfilesServerControl"
import { ServerControl } from "./features/ServerControl"
import { SteamcmdCenter } from "./features/SteamcmdCenter"
import { useServerStore } from "./store/useServerStore"

const roleLevels: Record<string, number> = { user: 1, moderator: 2, admin: 3, host: 4 }

function RoleGuard({ minRole, children }: { minRole: number; children: React.ReactNode }) {
  const role = useServerStore((state) => state.currentUser?.role ?? "user")
  if ((roleLevels[role] ?? 1) < minRole) {
    return <Navigate to="/" replace />
  }
  return <>{children}</>
}

function App() {
  const init = useServerStore((state) => state.init)
  const startRealtime = useServerStore((state) => state.startRealtime)
  const stopRealtime = useServerStore((state) => state.stopRealtime)
  const currentUser = useServerStore((state) => state.currentUser)

  useEffect(() => {
    void init()
    return () => stopRealtime()
  }, [init, stopRealtime])

  useEffect(() => {
    if (!currentUser) {
      stopRealtime()
      return
    }
    startRealtime()
    return () => stopRealtime()
  }, [currentUser, startRealtime, stopRealtime])

  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<Dashboard />} />
        {/* Moderator+ */}
        <Route path="rcon" element={<RoleGuard minRole={2}><ConsoleScreen /></RoleGuard>} />
        <Route path="server-control" element={<RoleGuard minRole={2}><ServerControl /></RoleGuard>} />
        <Route path="players" element={<RoleGuard minRole={2}><PlayerManagement /></RoleGuard>} />
        {/* Admin+ */}
        <Route path="configuration" element={<RoleGuard minRole={3}><ConfigurationEditor /></RoleGuard>} />
        <Route path="mods" element={<RoleGuard minRole={3}><ModManagement /></RoleGuard>} />
        <Route path="mods/explorer" element={<RoleGuard minRole={3}><ModExplorer /></RoleGuard>} />
        <Route path="profiles" element={<RoleGuard minRole={3}><ProfilesServerControl /></RoleGuard>} />
        <Route path="steamcmd" element={<RoleGuard minRole={3}><SteamcmdCenter /></RoleGuard>} />
        <Route path="logs" element={<RoleGuard minRole={3}><LogsCenter /></RoleGuard>} />
        <Route path="operations" element={<RoleGuard minRole={3}><OperationsCenter /></RoleGuard>} />
        {/* Legacy redirects */}
        <Route path="console" element={<Navigate to="/rcon" replace />} />
        <Route path="server" element={<Navigate to="/server-control" replace />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  )
}

export default App
