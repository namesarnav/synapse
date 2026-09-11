import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { AuthProvider, useAuth } from "./state/auth";
import { Shell } from "./components/Shell";
import { Login } from "./pages/Login";
import { Landing } from "./pages/Landing";
import { Dashboard } from "./pages/Dashboard";
import { Workflows } from "./pages/Workflows";
import { Editor } from "./pages/Editor";
import { Executions } from "./pages/Executions";
import { ExecutionDetail } from "./pages/ExecutionDetail";
import { Versions } from "./pages/Versions";
import { Settings } from "./pages/Settings";

function Gate() {
  const { session, loading, workspace } = useAuth();
  if (loading) return <div className="page muted">Loading…</div>;
  if (!session) return <Navigate to="/login" replace />;
  if (!workspace) return <div className="page">This account has no workspace.</div>;
  return <Shell />;
}

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/" element={<Landing />} />
      <Route path="/welcome" element={<Navigate to="/" replace />} />
      <Route element={<Gate />}>
        <Route path="dashboard" element={<Dashboard />} />
        <Route path="workflows" element={<Workflows />} />
        <Route path="workflows/:id" element={<Editor />} />
        <Route path="workflows/:id/versions" element={<Versions />} />
        <Route path="executions" element={<Executions />} />
        <Route path="executions/:id" element={<ExecutionDetail />} />
        <Route path="settings" element={<Settings />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

export function App() {
  return (
    <BrowserRouter>
      <AuthProvider>
        <AppRoutes />
      </AuthProvider>
    </BrowserRouter>
  );
}

export default App;
