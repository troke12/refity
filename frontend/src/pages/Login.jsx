import { useState } from 'react';
import { authAPI } from '../services/api';
import './Login.css';

function Login({ onLogin }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [showPassword, setShowPassword] = useState(false);
  const [isLoading, setIsLoading] = useState(false);
  const [error, setError] = useState('');

  const handleSubmit = async (e) => {
    e.preventDefault();
    if (!username || !password) return;

    setIsLoading(true);
    setError('');

    try {
      await authAPI.login(username, password);
      onLogin();
    } catch (err) {
      setError(err.response?.data?.message || 'Invalid username or password');
    } finally {
      setIsLoading(false);
    }
  };

  return (
    <div className="login-container">
      <div className="login-card">
        <div className="login-header">
          <div className="login-icon">
            <i className="bi bi-shield-lock"></i>
          </div>
          <h1 className="login-title">Refity Registry</h1>
          <p className="login-subtitle">Sign in to access your Docker registry</p>
        </div>

        {error && (
          <div className="alert alert-danger d-flex align-items-center mb-3" role="alert">
            <i className="bi bi-exclamation-triangle-fill me-2"></i>
            {error}
          </div>
        )}

        <form onSubmit={handleSubmit}>
          <div className="mb-3">
            <label htmlFor="username" className="form-label">
              <i className="bi bi-person me-2"></i>Username
            </label>
            <input
              type="text"
              id="username"
              className="form-control"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="Enter your username"
              required
              autoFocus
              disabled={isLoading}
            />
          </div>

          <div className="mb-4">
            <label htmlFor="password" className="form-label">
              <i className="bi bi-lock me-2"></i>Password
            </label>
            <div className="input-group">
              <input
                type={showPassword ? 'text' : 'password'}
                id="password"
                className="form-control"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="Enter your password"
                required
                disabled={isLoading}
              />
              <button
                className="btn btn-outline-secondary"
                type="button"
                onClick={() => setShowPassword(!showPassword)}
                disabled={isLoading}
                title="Toggle password visibility"
              >
                <i className={showPassword ? 'bi bi-eye-slash' : 'bi bi-eye'}></i>
              </button>
            </div>
          </div>

          <button
            type="submit"
            className="btn btn-login w-100"
            disabled={isLoading || !username || !password}
          >
            {isLoading ? (
              <>
                <span className="spinner-border spinner-border-sm me-2" role="status"></span>
                Signing in...
              </>
            ) : (
              <>
                <i className="bi bi-box-arrow-in-right me-2"></i>Sign In
              </>
            )}
          </button>
        </form>
      </div>
    </div>
  );
}

export default Login;

