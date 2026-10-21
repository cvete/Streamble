import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';
import { SessionProvider, useSession } from './lib/session';
import { LiveProvider } from './lib/live';
import { ToastProvider } from './lib/hooks';
import { Layout } from './components/Layout';
import { ChangePassword, Login } from './pages/Login';
import { Overview } from './pages/Overview';
import { StreamDetail, Streams } from './pages/Streams';
import { AppEditor, Apps } from './pages/Apps';
import { Transcoding } from './pages/Transcoding';
import { Restreams } from './pages/Restreams';
import { Sources } from './pages/Sources';
import { Stats } from './pages/Stats';
import { Webhooks } from './pages/Webhooks';
import { Audit, Users } from './pages/Admin';
import { Account, Settings } from './pages/Settings';
import { Empty } from './components/ui';

function Gate() {
  const { user, loading, can } = useSession();
  if (loading) return null;
  if (!user) return <Login />;
  if (user.must_change_password) return <ChangePassword forced />;
  const admin = can('admin');
  return (
    <LiveProvider>
      <Layout>
        <Routes>
          <Route path="/" element={<Overview />} />
          <Route path="/streams" element={<Streams />} />
          <Route path="/streams/:app/:stream" element={<StreamDetail />} />
          <Route path="/stats" element={<Stats />} />
          <Route path="/apps" element={<Apps />} />
          <Route path="/apps/:name" element={<AppEditor />} />
          <Route path="/transcoding" element={<Transcoding />} />
          <Route path="/restreams" element={<Restreams />} />
          <Route path="/sources" element={<Sources />} />
          {admin && <Route path="/webhooks" element={<Webhooks />} />}
          {admin && <Route path="/users" element={<Users />} />}
          {admin && <Route path="/audit" element={<Audit />} />}
          <Route path="/settings" element={<Settings />} />
          <Route path="/account" element={<Account />} />
          <Route path="/login" element={<Navigate to="/" replace />} />
          <Route path="*" element={<div className="page"><div className="panel"><Empty title="Page not found"><p>Pick a page from the menu.</p></Empty></div></div>} />
        </Routes>
      </Layout>
    </LiveProvider>
  );
}

export function App() {
  return (
    <BrowserRouter basename="/panel">
      <ToastProvider>
        <SessionProvider>
          <Gate />
        </SessionProvider>
      </ToastProvider>
    </BrowserRouter>
  );
}
