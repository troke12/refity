import { BrowserRouter as Router, Routes, Route, Navigate } from 'react-router-dom';
import { useState, useEffect } from 'react';
import Login from './pages/Login';
import Dashboard from './pages/Dashboard';
import { isAuthenticated } from './services/api';

function App() {
  const [auth, setAuth] = useState(isAuthenticated());

  useEffect(() => {
    const checkAuth = () => {
      setAuth(isAuthenticated());
    };
    checkAuth();
    window.addEventListener('storage', checkAuth);
    return () => window.removeEventListener('storage', checkAuth);
  }, []);

  return (
    <Router>
      <Routes>
        <Route path="/login" element={auth ? <Navigate to="/" /> : <Login onLogin={() => setAuth(true)} />} />
        <Route path="/" element={auth ? <Dashboard /> : <Navigate to="/login" />} />
        <Route path="*" element={<Navigate to="/" />} />
      </Routes>
    </Router>
  );
}

export default App;

